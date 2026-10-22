package smartevse

import (
	"math/rand"
	"testing"
)

// TestSoak throws random events at the master for two simulated hours per seed
// and checks the invariants every cycle (see harness.check):
//   - no node ever advertises more than min(reported max, configured cap, evcc limit)
//   - an overload of the real mains never lasts longer than maxOverload
//
// Household load alone stays below MaxMains, so every overload is the
// controller's responsibility.
func TestSoak(t *testing.T) {
	// Hard shortage stops all EVSEs after NoCurrent > 2, i.e. the third cycle
	// after detection; one more cycle for the measurement to show it.
	const maxOverload = 4 * Interval

	for seed := int64(1); seed <= 20; seed++ {
		h := newHarness(t)
		r := rand.New(rand.NewSource(seed))
		events := map[string]int{}

		for step := 0; step < 3600; step++ { // 3600 × 2 s = 2 h
			if r.Intn(10) == 0 {
				id := 1 + r.Intn(2)
				n := h.sim.Node(id)
				switch e := r.Intn(9); e {
				case 0:
					n.Connected = !n.Connected
					n.Wants = n.Connected
					events["plug"]++
				case 1:
					n.Wants = !n.Wants
					events["car pause"]++
				case 2, 3:
					p := r.Intn(3)
					h.sim.House[p] = float64(r.Intn(24)) // up to 23 A < MaxMains
					events["house load"]++
				case 4:
					h.m.Node(id).MaxCurrent(int64(4 + r.Intn(14)))
					events["evcc limit"]++
				case 5:
					h.m.Node(id).Enable(r.Intn(3) > 0)
					events["evcc enable"]++
				case 6:
					n.Offline = r.Intn(4) == 0
					events["bus fault"]++
				case 7:
					h.sim.SensorboxOffline = r.Intn(5) == 0
					events["sensorbox"]++
				case 8:
					n.MaxA = 10 + r.Intn(7)
					events["node max"]++
				}
			}
			h.run(Interval)
			if h.longest > maxOverload {
				t.Fatalf("seed %d step %d: overload of %.1f A lasted %v\n%s",
					seed, step, h.peakOver, h.longest, h.dump())
			}
		}
		t.Logf("seed %2d: peak overload %.1f A, longest %v, events %v", seed, h.peakOver, h.longest, events)
	}
}
