package smartevse

import (
	"io"
	"log/slog"
	"math"
	"os"
	"testing"
	"time"
)

// harness runs a Master against a simulated bus with two nodes.
type harness struct {
	t   *testing.T
	sim *Sim
	m   *Master
	cfg Config

	// worst observed overload of the real mains, and for how long
	peakOver  float64
	peakAt    string // situation at the worst overload, for debugging
	overSince time.Time
	longest   time.Duration
}

func newHarness(t *testing.T, mutate ...func(*Config)) *harness {
	t.Helper()
	cfg := Config{
		MaxMains:       25,
		MinCurrent:     6,
		Nodes:          []NodeConfig{{ID: 1, MaxA: 16, Host: "node1"}, {ID: 2, MaxA: 16, Host: "node2"}},
		DefaultEnabled: true,
	}
	for _, f := range mutate {
		f(&cfg)
	}

	sim := NewSim(1, 2)
	sim.House = [3]float64{5, 5, 5}

	bus := NewSimBus(sim)

	level := slog.LevelError
	var w io.Writer = io.Discard
	if os.Getenv("SIM_DEBUG") != "" {
		level, w = slog.LevelDebug, os.Stderr
	}
	m, err := NewMaster(bus, cfg, slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})))
	if err != nil {
		t.Fatal(err)
	}
	m.SetClock(sim.Now)
	m.SetHTTPTransport(sim)
	return &harness{t: t, sim: sim, m: m, cfg: cfg}
}

// run advances time in master cycles and checks the safety invariants every cycle.
func (h *harness) run(d time.Duration) {
	h.t.Helper()
	for elapsed := time.Duration(0); elapsed < d; elapsed += Interval {
		h.step()
	}
}

// step advances one cycle: the Sensorbox web api is polled in between, as
// its background poller would.
func (h *harness) step() {
	h.sim.Advance(Interval)
	if h.m.sbWeb != nil {
		h.m.sbWeb.poll(h.sim.Now())
	}
	h.m.Cycle()
	h.check()
}

// runUntil runs until cond is true, failing after max.
func (h *harness) runUntil(max time.Duration, what string, cond func() bool) time.Duration {
	h.t.Helper()
	for elapsed := time.Duration(0); elapsed <= max; elapsed += Interval {
		if cond() {
			return elapsed
		}
		h.step()
	}
	h.t.Fatalf("%s: not reached within %v\n%s", what, max, h.dump())
	return 0
}

// check enforces invariants that must hold at all times.
func (h *harness) check() {
	h.t.Helper()
	if h.sim.Merged > 0 {
		h.t.Fatalf("%d requests sent without the 3.5 character gap", h.sim.Merged)
	}
	for id := 1; id <= 2; id++ {
		n := h.sim.Node(id)
		if n.State != StateC || n.Offline {
			continue // a node cut off from the bus cannot be corrected; it stops on its timeout
		}
		info := h.m.Node(id).Info()
		limit := math.Min(float64(n.MaxA), float64(h.cfg.Nodes[id-1].MaxA))
		if info.LimitA > 0 {
			limit = math.Min(limit, info.LimitA)
		}
		if got := float64(n.PWM) / 10; got > limit+0.05 {
			h.t.Fatalf("node %d advertises %.1f A, above its cap %.1f A", id, got, limit)
		}
	}

	over := h.sim.MaxPhase() - float64(h.cfg.MaxMains)
	now := h.sim.Now()
	if over > 0.05 {
		if over > h.peakOver {
			h.peakOver = over
			h.peakAt = h.dump()
		}
		if h.overSince.IsZero() {
			h.overSince = now
		}
		if d := now.Sub(h.overSince); d > h.longest {
			h.longest = d
		}
	} else {
		h.overSince = time.Time{}
	}
}

func (h *harness) plugIn(id int)       { n := h.sim.Node(id); n.Connected, n.Wants = true, true }
func (h *harness) state(id int) int    { return h.sim.Node(id).State }
func (h *harness) amps(id int) float64 { return h.sim.Node(id).EVCurrent() }
func (h *harness) charging(id int) func() bool {
	return func() bool { return h.state(id) == StateC && h.amps(id) > 0 }
}

func (h *harness) dump() string {
	s := ""
	for id := 1; id <= 2; id++ {
		n := h.sim.Node(id)
		s += "  node " + itoa(id) + ": state=" + StateName(n.State) + " err=" + itoa(n.Err) +
			" delay=" + itoa(n.ChargeDelay) + " pwm=" + ftoa(float64(n.PWM)/10) + " draw=" + ftoa(n.EVCurrent()) + "\n"
	}
	m := h.sim.Mains()
	return s + "  mains: " + ftoa(m[0]) + " " + ftoa(m[1]) + " " + ftoa(m[2])
}

func itoa(i int) string      { return slogValue(i) }
func ftoa(f float64) string  { return slogValue(math.Round(f*10) / 10) }
func slogValue(v any) string { return slog.AnyValue(v).String() }

func near(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.1f, want %.1f ±%.1f", what, got, want, tol)
	}
}

// ---------------------------------------------------------------------------------

// Two cars plug in one after another: both charge, the headroom (25 - 5 = 20 A)
// is split evenly and the mains limit is never exceeded.
func TestScenarioTwoCarsShareHeadroom(t *testing.T) {
	h := newHarness(t)

	h.plugIn(1)
	h.runUntil(10*time.Second, "node 1 charging", h.charging(1))
	h.run(20 * time.Second)
	near(t, "node 1 alone", h.amps(1), 16, 0.1) // capped at 16 A, mains 21 A

	h.plugIn(2)
	h.runUntil(10*time.Second, "node 2 charging", h.charging(2))
	h.run(30 * time.Second)

	near(t, "node 1 shared", h.amps(1), 10, 0.5)
	near(t, "node 2 shared", h.amps(2), 10, 0.5)
	if h.peakOver > 0 {
		t.Errorf("mains exceeded by %.1f A for %v", h.peakOver, h.longest)
	}
}

// Two cars plugged in at the same moment must not both get the full headroom.
func TestScenarioSimultaneousPlugIn(t *testing.T) {
	h := newHarness(t)
	h.plugIn(1)
	h.plugIn(2)
	h.runUntil(20*time.Second, "both charging", func() bool { return h.charging(1)() && h.charging(2)() })
	h.run(30 * time.Second)
	near(t, "node 1", h.amps(1), 10, 0.5)
	near(t, "node 2", h.amps(2), 10, 0.5)
	if h.peakOver > 0 {
		t.Errorf("mains exceeded by %.1f A\n%s", h.peakOver, h.peakAt)
	}
}

// evcc limits node 1 to 6 A: node 2 gets the rest of the headroom.
func TestScenarioEvccLimit(t *testing.T) {
	h := newHarness(t)
	h.plugIn(1)
	h.plugIn(2)
	h.runUntil(20*time.Second, "both charging", func() bool { return h.charging(1)() && h.charging(2)() })

	if err := h.m.Node(1).MaxCurrent(6); err != nil {
		t.Fatal(err)
	}
	h.run(40 * time.Second)

	near(t, "node 1 limited", h.amps(1), 6, 0.1)
	near(t, "node 2 takes rest", h.amps(2), 14, 0.5)
}

// The node reports a lower maximum than configured: the master must respect it,
// because a node does not enforce its own maximum when driven by a master.
func TestScenarioNodeReportedMaxIsRespected(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxMains = 40 })
	h.sim.Node(1).MaxA = 10
	h.plugIn(1)
	h.runUntil(10*time.Second, "charging", h.charging(1))
	h.run(30 * time.Second)
	near(t, "node 1", h.amps(1), 10, 0.1) // check() also fails on any PWM above 10 A
}

// A moderate household spike is absorbed within one cycle by lowering the EVs.
func TestScenarioHouseholdSpike(t *testing.T) {
	h := newHarness(t)
	h.plugIn(1)
	h.plugIn(2)
	h.runUntil(20*time.Second, "both charging", func() bool { return h.charging(1)() && h.charging(2)() })
	h.run(30 * time.Second)

	h.sim.House[0] = 10 // kettle on L1: 10 + 2×10 = 30 A
	h.run(Interval)     // the master reacts on the next measurement
	h.run(Interval)
	if mp := h.sim.MaxPhase(); mp > 25.05 {
		t.Errorf("mains %.1f A after two cycles, want ≤ 25", mp)
	}
	near(t, "node 1", h.amps(1), 7.5, 0.5)
	near(t, "node 2", h.amps(2), 7.5, 0.5)
	if h.longest > Interval {
		t.Errorf("overload lasted %v (peak %.1f A), want ≤ one cycle", h.longest, h.peakOver)
	}

	h.sim.House[0] = 5 // kettle off: slow ramp back (¼ of headroom per cycle)
	h.run(40 * time.Second)
	near(t, "node 1 recovered", h.amps(1), 10, 0.5)
}

// Not enough room for 2×6 A: after three cycles of hard shortage all nodes stop
// (firmware behaviour: NoCurrent > 2 → broadcast LESS_6A).
func TestScenarioHardShortageStopsAll(t *testing.T) {
	h := newHarness(t)
	h.plugIn(1)
	h.plugIn(2)
	h.runUntil(20*time.Second, "both charging", func() bool { return h.charging(1)() && h.charging(2)() })

	h.sim.House[0] = 18 // 18 + 2×6 = 30 > 25
	h.runUntil(12*time.Second, "both stopped", func() bool { return h.amps(1) == 0 && h.amps(2) == 0 })

	if h.sim.Node(1).Err&ErrLess6A == 0 {
		t.Errorf("node 1 should show LESS_6A")
	}
}

// A car plugging in when there is no room does not start.
func TestScenarioNoRoomAtPlugIn(t *testing.T) {
	h := newHarness(t)
	h.sim.House = [3]float64{22, 5, 5}
	h.plugIn(1)
	h.run(30 * time.Second)
	if h.amps(1) != 0 {
		t.Fatalf("node 1 charges %.1f A without headroom", h.amps(1))
	}
	if h.sim.Node(1).Err&ErrLess6A == 0 {
		t.Errorf("node 1 should show LESS_6A")
	}
}

// evcc disables node 1: it is paused (access register), stops within a cycle
// and node 2 gets more. The node waits in B1 without error, so enabling it
// again is quick (no 60 s ChargeDelay, unlike the firmware's LESS_6A rejection).
func TestScenarioDisableEnable(t *testing.T) {
	h := newHarness(t)
	h.plugIn(1)
	h.plugIn(2)
	h.runUntil(20*time.Second, "both charging", func() bool { return h.charging(1)() && h.charging(2)() })
	h.run(30 * time.Second)

	h.m.Node(1).Enable(false)
	h.runUntil(2*Interval, "node 1 stopped", func() bool { return h.amps(1) == 0 })
	h.run(40 * time.Second)
	near(t, "node 2 alone", h.amps(2), 16, 0.5)
	if s, _ := h.m.Node(1).Status(); s != "B" {
		t.Errorf("disabled node status %q, want B", s)
	}
	if n := h.sim.Node(1); n.Err != 0 || n.ChargeDelay != 0 || n.Access != AccessPause || n.State != StateB1 {
		t.Errorf("disabled node should be paused in B1 without error: state=%s access=%s err=%d delay=%d",
			StateName(n.State), AccessName(n.Access), n.Err, n.ChargeDelay)
	}

	h.m.Node(1).Enable(true)
	took := h.runUntil(20*time.Second, "node 1 resumed", h.charging(1))
	t.Logf("node 1 resumed %v after enable", took)
	if a := h.sim.Node(1).Access; a != AccessOn {
		t.Errorf("enabled node access %s, want on", AccessName(a))
	}
	h.run(40 * time.Second)
	near(t, "node 1 shared again", h.amps(1), 10, 0.5)
	if h.peakOver > 0 {
		t.Errorf("mains exceeded by %.1f A\n%s", h.peakOver, h.peakAt)
	}
}

// Disabled before plugging in: a node without a car is not paused, or it would
// not notice the car and evcc would never see it connect. Once plugged in it is
// paused, evcc sees B, and the car starts as soon as evcc enables.
func TestScenarioPlugInWhileDisabled(t *testing.T) {
	h := newHarness(t)
	h.m.Node(1).Enable(false)
	h.run(10 * time.Second)
	if a := h.sim.Node(1).Access; a != AccessOn {
		t.Fatalf("disabled node without car: access %s, want on", AccessName(a))
	}

	h.plugIn(1)
	h.run(30 * time.Second)
	n := h.sim.Node(1)
	if h.amps(1) != 0 || n.Err != 0 || n.Access != AccessPause {
		t.Fatalf("disabled node: draw %.1f A, err %d, access %s", h.amps(1), n.Err, AccessName(n.Access))
	}
	if s, _ := h.m.Node(1).Status(); s != "B" {
		t.Errorf("disabled node with car: status %q, want B", s)
	}

	h.m.Node(1).Enable(true)
	took := h.runUntil(20*time.Second, "node 1 charging", h.charging(1))
	t.Logf("node 1 started %v after enable", took)

	// unplugged while disabled: back to ON, ready for the next car
	h.m.Node(1).Enable(false)
	h.run(10 * time.Second)
	n.Connected, n.Wants = false, false
	h.run(2 * Interval)
	if n.State != StateA || n.Access != AccessOn {
		t.Errorf("unplugged disabled node: state %s access %s, want A on", StateName(n.State), AccessName(n.Access))
	}
}

// evcc wins over a change made on the node itself (button, web UI, MQTT).
func TestScenarioAccessChangedOnNode(t *testing.T) {
	h := newHarness(t)
	h.plugIn(1)
	h.runUntil(10*time.Second, "node 1 charging", h.charging(1))

	// paused on the node while evcc allows charging: resumed
	h.sim.Node(1).SetAccess(AccessPause)
	took := h.runUntil(20*time.Second, "node 1 resumed", h.charging(1))
	t.Logf("node 1 resumed %v after a pause on the node", took)

	// switched on at the node while evcc has it disabled: paused again, no charging
	h.m.Node(1).Enable(false)
	h.run(10 * time.Second)
	h.sim.Node(1).SetAccess(AccessOn)
	for i := 0; i < 10; i++ {
		h.run(Interval)
		if h.amps(1) != 0 {
			t.Fatalf("node 1 charges %.1f A while evcc has it disabled", h.amps(1))
		}
	}
	if a := h.sim.Node(1).Access; a != AccessPause {
		t.Errorf("node 1 access %s, want pause", AccessName(a))
	}
}

// evcc switches a charging car from 3P to 1P and back. The node only switches
// its contactors on entering C, so the master pauses it until it has stopped
// and then lets it restart. The node's web server is busy at first (503).
func TestScenarioPhaseSwitchWhileCharging(t *testing.T) {
	h := newHarness(t)
	h.plugIn(1)
	h.runUntil(10*time.Second, "node 1 charging", h.charging(1))
	h.run(20 * time.Second)
	n := h.sim.Node(1)
	if p, err := h.m.Node(1).GetPhases(); err != nil || p != 3 || n.ActivePhases() != 3 {
		t.Fatalf("initial phases %d (%v), active %d", p, err, n.ActivePhases())
	}

	for _, want := range []int{1, 3} {
		n.WebBusy = 2
		if err := h.m.Node(1).Phases1p3p(want); err != nil {
			t.Fatalf("switch to %dP: %v", want, err)
		}
		if !h.m.Node(1).Info().Switch || n.WebBusy != 0 {
			t.Errorf("switch to %dP: node should be held for the switch after retrying (busy %d)", want, n.WebBusy)
		}
		stopped := false
		took := h.runUntil(40*time.Second, "charging on new phases", func() bool {
			stopped = stopped || h.amps(1) == 0
			return h.charging(1)() && n.ActivePhases() == want
		})
		t.Logf("charging on %dP %v after the switch", want, took)
		if !stopped {
			t.Errorf("switch to %dP: the car never stopped", want)
		}
		if info := h.m.Node(1).Info(); info.Switch || info.Phases != want || n.Access != AccessOn || n.Err != 0 {
			t.Errorf("after switch to %dP: %+v, node access %s err %d", want, info, AccessName(n.Access), n.Err)
		}
		h.run(20 * time.Second)
		if m := h.sim.Mains(); want == 1 && (m[1] != 5 || m[2] != 5) {
			t.Errorf("1P: L2/L3 should carry only the house load, mains %v", m)
		}
	}
	if h.peakOver > 0 {
		t.Errorf("mains exceeded by %.1f A\n%s", h.peakOver, h.peakAt)
	}

	// the same setting again does not interrupt charging
	if err := h.m.Node(1).Phases1p3p(3); err != nil || h.m.Node(1).Info().Switch {
		t.Errorf("repeated 3P: err %v, held %v", err, h.m.Node(1).Info().Switch)
	}
}

// A switch while no car charges just changes the setting: the next charge
// starts on the new phases.
func TestScenarioPhaseSwitchIdle(t *testing.T) {
	h := newHarness(t)
	h.run(2 * Interval)
	if err := h.m.Node(1).Phases1p3p(1); err != nil || h.m.Node(1).Info().Switch {
		t.Fatalf("switch: err %v, held %v", err, h.m.Node(1).Info().Switch)
	}
	h.plugIn(1)
	h.runUntil(10*time.Second, "node 1 charging", h.charging(1))
	if p := h.sim.Node(1).ActivePhases(); p != 1 {
		t.Errorf("charging on %d phases, want 1", p)
	}
}

// Sensorbox stops answering: after Stale the master flags CT_NOCOMM, stops
// broadcasting and the nodes stop. When it comes back, charging resumes.
func TestScenarioSensorboxLost(t *testing.T) {
	h := newHarness(t)
	h.plugIn(1)
	h.plugIn(2)
	h.runUntil(20*time.Second, "both charging", func() bool { return h.charging(1)() && h.charging(2)() })

	h.sim.SensorboxOffline = true
	took := h.runUntil(16*time.Second, "both stopped", func() bool { return h.amps(1) == 0 && h.amps(2) == 0 })
	t.Logf("nodes stopped %v after sensorbox loss", took)
	if !h.m.Snapshot().Fault {
		t.Errorf("master should report fault")
	}
	since := h.sim.Now()
	h.run(10 * time.Second)
	if n := h.sim.Broadcasts(since); n != 0 {
		t.Errorf("%d current broadcasts during fault, want 0", n)
	}

	h.sim.SensorboxOffline = false
	h.runUntil(90*time.Second, "both resumed", func() bool { return h.charging(1)() && h.charging(2)() })
}

// Sensorbox stops answering on the bus while its WiFi keeps working: the
// master balances on the REST API instead, still within MaxMains. When the
// Sensorbox also loses its meter, the REST stamp freezes and the nodes stop
// after Stale as without the fallback.
func TestScenarioSensorboxWebFallback(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.SensorboxHost = "sensorbox" })
	h.plugIn(1)
	h.plugIn(2)
	h.runUntil(20*time.Second, "both charging", func() bool { return h.charging(1)() && h.charging(2)() })

	h.sim.SensorboxOffline = true
	h.run(60 * time.Second)
	if s := h.m.Snapshot(); s.Fault || s.Source != "REST" {
		t.Fatalf("fault=%v source=%q, want web api mains\n%s", s.Fault, s.Source, h.dump())
	}
	if !h.charging(1)() || !h.charging(2)() {
		t.Fatalf("nodes stopped on web api mains\n%s", h.dump())
	}

	// a household load must still be balanced out
	h.sim.House = [3]float64{10, 10, 10}
	h.run(30 * time.Second)
	if over := h.sim.MaxPhase() - float64(h.cfg.MaxMains); over > 0.05 {
		t.Fatalf("mains %.1f A over MaxMains after 30 s\n%s", over, h.dump())
	}
	if !h.charging(1)() || !h.charging(2)() || h.amps(1)+h.amps(2) > 15.05 {
		t.Fatalf("nodes not reduced to share 15 A\n%s", h.dump())
	}
	if h.longest > 2*Interval {
		t.Errorf("overload lasted %v (peak %.1f A)\n%s", h.longest, h.peakOver, h.peakAt)
	}

	h.sim.SensorboxP1Lost = true
	took := h.runUntil(16*time.Second, "both stopped", func() bool { return h.amps(1) == 0 && h.amps(2) == 0 })
	t.Logf("nodes stopped %v after P1 loss", took)
	if !h.m.Snapshot().Fault {
		t.Errorf("master should report fault")
	}

	h.sim.SensorboxP1Lost = false
	h.runUntil(90*time.Second, "both resumed", func() bool { return h.charging(1)() && h.charging(2)() })
	if s := h.m.Snapshot(); s.Source != "REST" {
		t.Errorf("source %q, want REST", s.Source)
	}

	h.sim.SensorboxOffline = false
	h.run(4 * time.Second)
	if s := h.m.Snapshot(); s.Source != "P1" {
		t.Errorf("source %q after the bus recovered, want P1", s.Source)
	}
}

// Sensorbox gone on the bus and over WiFi: nodes stop after Stale.
func TestScenarioSensorboxWebAlsoLost(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.SensorboxHost = "sensorbox" })
	h.plugIn(1)
	h.runUntil(20*time.Second, "charging", h.charging(1))

	h.sim.SensorboxOffline = true
	h.sim.SensorboxWebOffline = true
	took := h.runUntil(16*time.Second, "stopped", func() bool { return h.amps(1) == 0 })
	t.Logf("node stopped %v after losing both", took)
	if !h.m.Snapshot().Fault {
		t.Errorf("master should report fault")
	}
}

// The master process dies: nodes must stop by themselves (CT_NOCOMM after ~11 s).
func TestScenarioMasterDies(t *testing.T) {
	h := newHarness(t)
	h.plugIn(1)
	h.plugIn(2)
	h.runUntil(20*time.Second, "both charging", func() bool { return h.charging(1)() && h.charging(2)() })

	h.sim.Advance(time.Duration(COMMTimeout+1) * time.Second) // no more cycles
	for id := 1; id <= 2; id++ {
		n := h.sim.Node(id)
		if n.EVCurrent() != 0 || n.Err&ErrCTNoComm == 0 {
			t.Errorf("node %d still charging %.1f A after master loss (state %s, err %d)",
				id, n.EVCurrent(), StateName(n.State), n.Err)
		}
	}
}

// Node 2 loses its bus connection: the master marks it offline and gives node 1
// the headroom; node 2 stops by itself on its comm timeout. Node 2's last current
// is still drawn until then, which the master sees as household load, so the mains
// stays protected.
func TestScenarioNodeLosesBus(t *testing.T) {
	h := newHarness(t)
	h.plugIn(1)
	h.plugIn(2)
	h.runUntil(20*time.Second, "both charging", func() bool { return h.charging(1)() && h.charging(2)() })
	h.run(30 * time.Second)

	h.sim.Node(2).Offline = true
	h.run(30 * time.Second)

	if h.amps(2) != 0 {
		t.Errorf("isolated node 2 still draws %.1f A", h.amps(2))
	}
	if _, err := h.m.Node(2).Status(); err == nil {
		t.Errorf("node 2 should be reported offline")
	}
	near(t, "node 1", h.amps(1), 16, 0.5)
	if h.longest > Interval {
		t.Errorf("overload lasted %v (peak %.1f A)", h.longest, h.peakOver)
	}
}

// Worst case: node 2 drops off the bus while drawing 16 A next to node 1. Its
// stale current leaves less than 6 A for node 1, so node 1 is stopped (hard
// shortage). Once node 2 times out, node 1 resumes after its ChargeDelay.
func TestScenarioNodeLosesBusAtHighCurrent(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxMains = 32 })
	h.sim.House = [3]float64{10, 10, 10}
	h.plugIn(1)
	h.plugIn(2)
	h.runUntil(20*time.Second, "both charging", func() bool { return h.charging(1)() && h.charging(2)() })
	h.run(30 * time.Second) // 10 + 2×11 = 32

	h.m.Node(2).MaxCurrent(16)
	h.m.Node(1).MaxCurrent(6)
	h.run(20 * time.Second) // node 2 at 16, node 1 at 6
	near(t, "node 2 before", h.amps(2), 16, 0.5)

	h.sim.Node(2).Offline = true
	h.m.Node(1).MaxCurrent(16)
	took := h.runUntil(120*time.Second, "node 1 at full current", func() bool { return h.amps(1) > 15.5 })
	t.Logf("node 1 reached 16 A %v after node 2 left the bus", took)
	if h.longest > Interval {
		t.Errorf("overload lasted %v (peak %.1f A)", h.longest, h.peakOver)
	}
}

// MaxCircuit limits the sum of the EVSE currents even with plenty of mains.
func TestScenarioMaxCircuit(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxMains = 50; c.MaxCircuit = 20 })
	h.plugIn(1)
	h.plugIn(2)
	h.runUntil(20*time.Second, "both charging", func() bool { return h.charging(1)() && h.charging(2)() })
	h.run(30 * time.Second)
	near(t, "sum", h.amps(1)+h.amps(2), 20, 0.5)
}

// Someone switches a node to Normal on its display: the master puts it back to Smart.
func TestScenarioModeCorrected(t *testing.T) {
	h := newHarness(t)
	h.sim.Node(1).Mode = ModeNormal
	h.run(2 * Interval)
	if h.sim.Node(1).Mode != ModeSmart {
		t.Errorf("node 1 mode %d, want Smart", h.sim.Node(1).Mode)
	}
}

// Solar export (negative mains) gives more room, never more than the caps.
func TestScenarioExport(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxMains = 16 })
	h.sim.House = [3]float64{-10, -10, -10}
	h.plugIn(1)
	h.plugIn(2)
	h.runUntil(20*time.Second, "both charging", func() bool { return h.charging(1)() && h.charging(2)() })
	h.run(60 * time.Second)
	near(t, "node 1", h.amps(1), 13, 0.5) // 16 - (-10) = 26 A headroom, split
	near(t, "node 2", h.amps(2), 13, 0.5)
}

// The master polls the nodes' EV meters, so the nodes hear the answers: no
// EV_NOCOMM, and the energy each node reports (OCPP) follows its meter.
func TestScenarioEVMeter(t *testing.T) {
	h := newHarness(t)
	h.plugIn(1)
	h.plugIn(2)
	for elapsed := time.Duration(0); elapsed < 10*time.Minute; elapsed += Interval {
		h.run(Interval)
		for id := 1; id <= 2; id++ {
			if h.sim.Node(id).Err&ErrEVNoComm != 0 {
				t.Fatalf("node %d EV_NOCOMM after %v\n%s", id, elapsed, h.dump())
			}
		}
	}
	for id := 1; id <= 2; id++ {
		n := h.sim.Node(id)
		if n.MeterKWh < 1000*float64(id)+0.5 {
			t.Fatalf("node %d meter %.3f kWh: car did not charge", id, n.MeterKWh)
		}
		// polled every 2 nodes × 2 s, a car draws < 3 Wh/s
		near(t, "energy heard by node "+itoa(id), n.HeardKWh, n.MeterKWh, 0.015)

		mi := h.m.Node(id).Info().Meter
		if mi == nil || !mi.Polled || !mi.Online || mi.Type != "Eastron3P" || mi.Address != 16+id {
			t.Fatalf("node %d meter info %+v", id, mi)
		}
		near(t, "energy read by master "+itoa(id), mi.EnergyKWh, n.MeterKWh, 0.015)
		draw := n.EVCurrent()
		near(t, "power "+itoa(id), float64(mi.PowerW), draw*3*230, 1)
		for p := range mi.CurrentsA {
			near(t, "current "+itoa(id), mi.CurrentsA[p], draw, 0.1)
		}
		if mi.Age > 2*Interval.Seconds() {
			t.Errorf("node %d meter reading %.0f s old", id, mi.Age)
		}
	}
}

// An EV meter stops answering: its node stops on EV_NOCOMM after COMM_EVTIMEOUT,
// the other node is not affected. When the meter is back the node resumes.
func TestScenarioEVMeterLost(t *testing.T) {
	h := newHarness(t)
	h.plugIn(1)
	h.plugIn(2)
	h.runUntil(20*time.Second, "both charging", func() bool { return h.charging(1)() && h.charging(2)() })

	h.sim.Node(1).MeterOffline = true
	took := h.runUntil(COMMEVTimeout*time.Second+10*time.Second, "node 1 stopped",
		func() bool { return h.amps(1) == 0 && h.sim.Node(1).Err&ErrEVNoComm != 0 })
	t.Logf("node 1 stopped %v after its meter was lost", took)
	if mi := h.m.Node(1).Info().Meter; mi.Online {
		t.Errorf("master reports lost meter online")
	}
	if !h.charging(2)() || h.sim.Node(2).Err != 0 {
		t.Errorf("node 2 affected: state %s err %d", StateName(h.state(2)), h.sim.Node(2).Err)
	}

	h.sim.Node(1).MeterOffline = false
	h.runUntil(90*time.Second, "node 1 resumed", h.charging(1))
}

// The EV meter setting changes on a node (new address, settings saved): the
// node flags it, the master re-reads it, clears the flag and polls the new address.
func TestScenarioEVMeterReconfigured(t *testing.T) {
	h := newHarness(t)
	h.plugIn(1)
	h.runUntil(20*time.Second, "node 1 charging", h.charging(1))

	n := h.sim.Node(1)
	n.MeterAddr, n.CfgChanged = 30, true
	h.run(3 * Interval)
	if n.CfgChanged {
		t.Errorf("config flag not cleared")
	}
	if mi := h.m.Node(1).Info().Meter; mi.Address != 30 || !mi.Online {
		t.Errorf("meter info %+v, want address 30 online", mi)
	}
	h.run(2 * time.Minute)
	if !h.charging(1)() || n.Err != 0 {
		t.Errorf("node 1 not charging after reconfiguration: state %s err %d", StateName(n.State), n.Err)
	}

	// meter disabled on the node: nothing to poll, the node does not time out
	n.MeterType, n.CfgChanged = MeterDisabled, true
	h.run(2 * time.Minute)
	if mi := h.m.Node(1).Info().Meter; mi != nil {
		t.Errorf("meter info %+v for a node without meter", mi)
	}
	if !h.charging(1)() {
		t.Errorf("node 1 stopped without EV meter")
	}
}
