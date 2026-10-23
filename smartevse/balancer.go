package smartevse

// Balancer is a port of the Smart-mode load balancing of the SmartEVSE-3
// firmware (CalcBalancedCurrent, IsCurrentAvailable, processAllNodeStates),
// with one addition: a per-EVSE external limit (Max) that evcc controls.
//
// All currents are in 0.1 A, like the firmware. Slot 0 is the master itself;
// this master has no EVSE of its own, so slot 0 stays in state A.
type Balancer struct {
	MaxMains   int // A per phase
	MaxCircuit int // A, sum of all EVSE currents per phase; 0 = disabled
	MinCurrent int // A, minimum the EV accepts (normally 6)

	State    [NrEVSEs]int  // BalancedState
	Answered [NrEVSEs]bool // node answered its last status poll (set by caller)
	Max      [NrEVSEs]int  // BalancedMax incl. external limit; < MinCurrent*10 means "do not charge"
	Balanced [NrEVSEs]int  // currents broadcast to the nodes

	// PhasesUpdated is set by the caller when a new mains measurement arrived;
	// the first Calc after that consumes it (firmware: phasesLastUpdateFlag).
	// This prevents a second ramp step in a cycle where a new EVSE was added.
	PhasesUpdated bool

	// StartedThisCycle limits starts to one EVSE per cycle (reset by caller).
	// Deviation from the firmware: it approves every waiting node in the same
	// loop against one measurement taken before any of them started, so two
	// cars plugged in together each get the full headroom.
	StartedThisCycle bool

	iset      int // IsetBalanced: total current for all charging EVSEs
	noCurrent int // consecutive hard shortage counter
}

// active reports whether slot n takes part in balancing.
// Deviation from firmware: an EVSE that evcc disabled (Max below MinCurrent)
// is excluded, so it does not trigger shortages for the others.
func (b *Balancer) active(n int) bool {
	return b.State[n] == StateC && b.Max[n] >= b.MinCurrent*10
}

func (b *Balancer) totals() (activeEVSE, activeMax, total int) {
	for n := 0; n < NrEVSEs; n++ {
		if b.active(n) {
			activeEVSE++
			activeMax += b.Max[n]
			total += b.Balanced[n]
		}
	}
	return
}

// Available ports IsCurrentAvailable (Smart mode): is there room for one more
// EVSE at MinCurrent without exceeding MaxMains / MaxCircuit?
func (b *Balancer) Available(imeasured int) bool {
	activeEVSE, _, total := b.totals()
	activeEVSE++
	if activeEVSE > NrEVSEs {
		activeEVSE = NrEVSEs
	}
	baseload := imeasured - total
	if activeEVSE*b.MinCurrent*10+baseload > b.MaxMains*10 {
		return false
	}
	// As master without circuit meter the firmware's Baseload_Circuit is 0.
	if b.MaxCircuit > 0 && activeEVSE*b.MinCurrent*10 > b.MaxCircuit*10 {
		return false
	}
	return true
}

// Calc ports CalcBalancedCurrent for Smart mode.
//
//	newEVSE: an EVSE just started charging (firmware mod=1)
func (b *Balancer) Calc(imeasured int, newEVSE bool) {
	fresh := b.PhasesUpdated
	b.PhasesUpdated = false

	// Caps always apply, also in cycles without a fresh measurement (the
	// firmware only applies BalancedMax during distribution).
	for n := 0; n < NrEVSEs; n++ {
		if b.State[n] == StateC && b.Balanced[n] > b.Max[n] {
			b.Balanced[n] = b.Max[n]
		}
	}

	activeEVSE, activeMax, total := b.totals()
	baseload := imeasured - total
	idiff := b.MaxMains*10 - imeasured

	if !newEVSE {
		if fresh {
			if idiff > 0 {
				b.iset += idiff / 4 // increase slowly
			} else {
				b.iset += idiff // decrease immediately
			}
		}
		b.iset = clamp(b.iset, 0, 800)
	} else if activeEVSE > 0 {
		b.iset = b.MaxMains*10 - baseload
		if b.MaxCircuit > 0 {
			b.iset = min(b.iset, b.MaxCircuit*10)
		}
	}

	b.iset = min(b.iset, b.MaxMains*10-baseload)
	if b.MaxCircuit > 0 {
		b.iset = min(b.iset, b.MaxCircuit*10)
	}

	// A charging EVSE that evcc disabled gets 0, so the node goes to C1.
	// Other non-charging slots keep their value (the firmware does the same:
	// a node in state B uses it for its PWM advertisement).
	for n := 0; n < NrEVSEs; n++ {
		if b.State[n] == StateC && !b.active(n) {
			b.Balanced[n] = 0
		}
	}

	if activeEVSE == 0 {
		b.noCurrent = 0
		return
	}
	// Deviation: the firmware only distributes with fresh measurements, even
	// for a new EVSE, which would leave it at 0 A. Distributing anyway is safe
	// because all limits above have been applied.
	if !fresh && !newEVSE {
		return
	}

	if b.iset < activeEVSE*b.MinCurrent*10 {
		b.iset = activeEVSE * b.MinCurrent * 10 // keep MinCurrent per EVSE ...
		hard := b.iset > b.MaxMains*10-baseload ||
			(b.MaxCircuit > 0 && b.iset > b.MaxCircuit*10)
		if hard {
			b.noCurrent++ // ... but count the shortage; Cycle stops all after > 2
		}
	} else {
		b.noCurrent = 0
	}

	if b.iset > activeMax {
		b.iset = activeMax
	}

	old := b.Balanced

	// Distribute: first give EVSEs whose max is below the average their max,
	// then split the rest evenly (same algorithm as the firmware).
	var set [NrEVSEs]bool
	remaining := b.iset
	left := activeEVSE
	for changed := true; changed && left > 0; {
		changed = false
		avg := remaining / left
		for n := 0; n < NrEVSEs; n++ {
			if b.active(n) && !set[n] && avg >= b.Max[n] {
				b.Balanced[n] = b.Max[n]
				set[n] = true
				left--
				remaining -= b.Max[n]
				changed = true
				break
			}
		}
	}
	for n := 0; n < NrEVSEs && left > 0; n++ {
		if b.active(n) && !set[n] {
			b.Balanced[n] = remaining / left
			set[n] = true
			remaining -= b.Balanced[n]
			left--
		}
	}

	b.limitIncreases(old, imeasured, newEVSE)
}

// limitIncreases is a safety addition, not in the firmware.
//
// The firmware redistributes on paper: lowering one EVSE and raising another in
// the same broadcast. If a node misses that broadcast (bus fault) it keeps its
// old current while the others go up, overloading the mains until the node's
// own comm timeout. Here an EVSE may only go up by the measured headroom plus
// the decreases of nodes that answered their last poll. Nodes that did not
// answer are never raised and their decreases are not counted on.
//
// A newly started EVSE always gets MinCurrent: nodes treat anything below 6 A
// as 6 A, and IsCurrentAvailable already checked that it fits.
func (b *Balancer) limitIncreases(old [NrEVSEs]int, imeasured int, newEVSE bool) {
	credit := max(0, b.MaxMains*10-imeasured)
	for n := 0; n < NrEVSEs; n++ {
		if b.Balanced[n] < old[n] && b.Answered[n] {
			credit += old[n] - b.Balanced[n]
		}
	}
	for n := 0; n < NrEVSEs; n++ {
		inc := b.Balanced[n] - old[n]
		if inc <= 0 || !b.active(n) {
			continue
		}
		if !b.Answered[n] {
			b.Balanced[n] = old[n]
			continue
		}
		if newEVSE && old[n] == 0 {
			// new EVSE: at least MinCurrent, more only within credit
			allowed := max(b.MinCurrent*10, min(inc, credit))
			b.Balanced[n] = allowed
			credit -= allowed
			continue
		}
		if inc > credit {
			b.Balanced[n] = old[n] + max(0, credit)
		}
		credit -= b.Balanced[n] - old[n]
	}
}

// Shortage reports (and resets) a persistent hard shortage: the firmware then
// broadcasts LESS_6A so all EVSEs stop.
func (b *Balancer) Shortage() bool {
	if b.noCurrent > 2 {
		b.noCurrent = 0
		return true
	}
	return false
}

// NodeUpdate ports processAllNodeStates for one node. It returns the register
// values to write to 0x0000.. of the node, or nil if nothing needs writing.
//
//	err:       error register as read from the node
//	mode:      mode register as read from the node
//	allowed:   evcc allows this node to charge
//	paused:    the node's access register is PAUSE (set by the master for evcc)
//	fault:     mains measurement lost (CT_NOCOMM)
func (b *Balancer) NodeUpdate(n, err, mode int, imeasured int, allowed, paused, fault bool) []uint16 {
	available := !fault && b.Available(imeasured)
	current := available && allowed
	write := false
	state := b.State[n]
	values := []uint16{uint16(state), 0}

	// Clear LESS_6A once there is room again, also for a node evcc disabled:
	// that node is paused instead (below), without a ChargeDelay.
	if available && err&ErrLess6A != 0 {
		err &^= ErrLess6A
		write = true
	}
	if fault && err&ErrCTNoComm == 0 {
		err |= ErrCTNoComm
		write = true
	}
	if !fault && err&ErrCTNoComm != 0 {
		err &^= ErrCTNoComm
		write = true
	}

	// Deviation from the firmware: a request from a node that evcc disabled is
	// not rejected with LESS_6A, which would cost a 60 s ChargeDelay once it is
	// enabled again. Once the node is paused (its access register), it is sent
	// back to B1: pausing alone does not move a node out of COMM_B/COMM_C, and a
	// paused node stays in B1 until it is set to ON again. Until then (the
	// access write failed) the request is left unanswered.
	switch {
	case (state == StateCommB || state == StateCommC) && !allowed:
		b.Balanced[n] = 0
		if paused {
			b.State[n] = StateB1
			values[0] = StateB1
			write = true
		}
	case state == StateCommB:
		if current {
			b.State[n] = StateB
			b.Balanced[n] = b.MinCurrent * 10
			values[0] = StateCommBOK
			write = true
		} else {
			b.Balanced[n] = 0
			if !available && err&ErrLess6A == 0 {
				err |= ErrLess6A
				write = true
			}
		}
	case state == StateCommC:
		b.Balanced[n] = 0
		if current && b.StartedThisCycle {
			break // wait one cycle for a measurement that includes the other start
		}
		if current {
			b.StartedThisCycle = true
			b.State[n] = StateC
			b.Calc(imeasured, true)
			values[0] = StateCommCOK
			write = true
		} else if !available && err&ErrLess6A == 0 {
			err |= ErrLess6A
			write = true
		}
	}

	values[1] = uint16(err)

	// This master only does Smart mode; correct a node switched via its display.
	if mode != ModeSmart {
		values = append(values, 0, ModeSmart)
		write = true
	}

	if !write {
		return nil
	}
	return values
}

func clamp(v, lo, hi int) int {
	return max(lo, min(v, hi))
}
