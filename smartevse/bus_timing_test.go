package smartevse

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recTransport is the simulator as seen by the bus: it records when each
// frame is sent (bus clock) and fails the test on concurrent use.
type recTransport struct {
	*Sim
	t      *testing.T
	now    func() time.Time
	busy   atomic.Int32
	frames []recFrame
}

type recFrame struct {
	start, end time.Time
	addr, fc   byte
}

func (r *recTransport) enter() {
	if r.busy.Add(1) > 1 {
		r.t.Error("transport used concurrently")
	}
}

func (r *recTransport) leave() { r.busy.Add(-1) }

func (r *recTransport) Write(f []byte) (int, error) {
	r.enter()
	defer r.leave()
	at := r.now()
	r.frames = append(r.frames, recFrame{at, at.Add(frameTime(len(f))), f[0], f[1]})
	return r.Sim.Write(f)
}

func (r *recTransport) Read(p []byte) (int, error) {
	r.enter()
	defer r.leave()
	return r.Sim.Read(p)
}

// newRecHarness is newHarness with the bus on a recTransport.
func newRecHarness(t *testing.T, mutate ...func(*Config)) (*harness, *recTransport) {
	h := newHarness(t, mutate...)
	c := &virtualClock{t: time.Unix(0, 0)}
	rec := &recTransport{Sim: h.sim, t: t, now: c.Now}
	h.sim.busNow = c.Now
	h.m.bus = newBus(rec, c)
	return h, rec
}

// Every frame keeps the inter-frame delay, plus the turnaround after a
// broadcast, and only the control loop uses the transport, also while the
// API reads and writes the master concurrently.
func TestBusGapsAndSingleOwner(t *testing.T) {
	h, rec := newRecHarness(t)
	h.plugIn(1)
	h.plugIn(2)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the HTTP API
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = h.m.Snapshot()
			_ = h.m.Node(1).Info()
			_ = h.m.Node(2).Enable(true)
			_ = h.m.Node(1).MaxCurrentMillis(16)
		}
	}()
	h.run(5 * time.Minute)
	close(stop)
	wg.Wait()

	if len(rec.frames) < 500 {
		t.Fatalf("only %d frames", len(rec.frames))
	}
	for i := 1; i < len(rec.frames); i++ {
		prev, f := rec.frames[i-1], rec.frames[i]
		want := DefaultInterFrameDelay
		if prev.addr == BroadcastAddr {
			want += DefaultBroadcastTurnaround
		}
		if gap := f.start.Sub(prev.end); gap < want {
			t.Fatalf("frame %d to addr %d: gap %v after addr %d, want ≥ %v", i, f.addr, gap, prev.addr, want)
		}
	}
}

// lateDevice answers each request for one register with a counter; the
// answer to the first request arrives only after the master gave up on it.
type lateDevice struct {
	rx, late []byte
	n        int
}

func (d *lateDevice) Write(f []byte) (int, error) {
	d.n++
	ans := appendCRC([]byte{f[0], 0x04, 2, 0, byte(d.n)})
	if d.n == 1 {
		d.late = ans
	} else {
		d.rx = append(d.rx, ans...)
	}
	return len(f), nil
}

func (d *lateDevice) Read(p []byte) (int, error) {
	if len(d.rx) == 0 {
		d.rx, d.late = d.late, nil // arrives after the deadline
		return 0, os.ErrDeadlineExceeded
	}
	n := copy(p, d.rx)
	d.rx = d.rx[n:]
	return n, nil
}

func (d *lateDevice) SetReadDeadline(time.Time) error { return nil }
func (d *lateDevice) Close() error                    { return nil }

// flushingDevice is a lateDevice whose input can be flushed (serial port).
type flushingDevice struct{ lateDevice }

func (d *flushingDevice) flushInput() (int, error) {
	n := len(d.rx)
	d.rx = nil
	return n, nil
}

// A late answer to a timed-out request must not be taken as the answer to the next.
func TestBusFlushesLateReply(t *testing.T) {
	for name, dev := range map[string]Transport{"read drain": &lateDevice{}, "flusher": &flushingDevice{}} {
		t.Run(name, func(t *testing.T) {
			b := newBus(dev, &virtualClock{t: time.Unix(0, 0)})
			if _, err := b.readInput(17, 0, 1); !errors.Is(err, ErrTimeout) {
				t.Fatalf("first read: %v, want timeout", err)
			}
			data, err := b.readInput(17, 0, 1)
			if err != nil {
				t.Fatal(err)
			}
			if data[1] != 2 {
				t.Errorf("second read got the late answer to the first (%d)", data[1])
			}
			st := b.Stats()[0]
			if st.Late != 7 || st.Timeout != 1 || st.OK != 1 || st.TX != 2 {
				t.Errorf("stats %+v", st)
			}
		})
	}
}

// A timeout waits until the deadline on the bus clock: the virtual clock too.
func TestBusTimeoutTakesTime(t *testing.T) {
	c := &virtualClock{t: time.Unix(0, 0)}
	b := newBus(&lateDevice{}, c) // answers the first request too late
	b.Timeout = 200 * time.Millisecond
	start := c.Now()
	b.readInput(17, 0, 1)
	if took := c.Now().Sub(start); took < 200*time.Millisecond+frameTime(8) {
		t.Errorf("timeout took %v", took)
	}
}

// EV meters that never answer, or a timeout as long as the cycle, must not
// delay the broadcast or make a cycle overrun.
func TestEVMeterBudget(t *testing.T) {
	for _, timeout := range []time.Duration{0, 1500 * time.Millisecond} {
		ref, refRec := newRecHarness(t)
		h, rec := newRecHarness(t, func(c *Config) { c.EVMeterTimeout = timeout })
		for _, x := range []*harness{ref, h} {
			x.plugIn(1)
			x.plugIn(2)
		}
		h.sim.Node(1).MeterOffline = true
		h.sim.Node(2).MeterOffline = true

		// shorter than COMM_EVTIMEOUT: the nodes behave the same in both
		for cycle := 0; cycle < 25; cycle++ {
			offsets := [2]time.Duration{}
			for i, x := range []*harness{ref, h} {
				r := []*recTransport{refRec, rec}[i]
				start, from := x.m.bus.now(), len(r.frames)
				x.step()
				if took := x.m.bus.now().Sub(start); took > Interval {
					t.Fatalf("timeout %v cycle %d took %v", timeout, cycle, took)
				}
				for _, f := range r.frames[from:] {
					if f.addr == BroadcastAddr && f.fc == 0x10 {
						offsets[i] = f.start.Sub(start)
					}
				}
			}
			if offsets[0] != offsets[1] {
				t.Fatalf("timeout %v cycle %d: broadcast at %v, %v with meters answering", timeout, cycle, offsets[1], offsets[0])
			}
		}
		for _, st := range h.m.bus.Stats() {
			if st.Addr == 17 && (st.Timeout == 0 || st.OK != 0) {
				t.Errorf("timeout %v: meter stats %+v", timeout, st)
			}
		}
	}
}

func meterStats(t *testing.T, m *Master, addr int) AddrStats {
	t.Helper()
	for _, st := range m.Snapshot().Bus {
		if st.Addr == addr {
			return st
		}
	}
	t.Fatalf("no stats for address %d", addr)
	return AddrStats{}
}

// A single lost answer is caught by the retry; a meter that keeps failing
// goes stale (last values kept) and recovers.
func TestEVMeterRetryAndStale(t *testing.T) {
	h := newHarness(t)
	h.plugIn(1)
	h.run(30 * time.Second)

	h.sim.Node(1).MeterDrop = 1
	h.run(2 * Interval * time.Duration(h.m.cfg.EVMeterEvery))
	if st := meterStats(t, h.m, 17); st.RetryOK != 1 || st.Timeout != 1 || st.StaleEvents != 0 {
		t.Errorf("after one lost answer: %+v", st)
	}
	if mi := h.m.Node(1).Info().Meter; !mi.Online || mi.Stale || mi.Missed != 0 {
		t.Errorf("meter info %+v", mi)
	}

	before := h.m.Node(1).Info().Meter.EnergyKWh
	h.sim.Node(1).MeterOffline = true
	h.run(3 * 2 * Interval)
	mi := h.m.Node(1).Info().Meter
	if !mi.Stale || mi.Online || mi.Missed != 3 || mi.EnergyKWh != before || mi.Age < 4 {
		t.Errorf("offline meter info %+v", mi)
	}
	if st := meterStats(t, h.m, 17); st.StaleEvents != 1 {
		t.Errorf("stats %+v", st)
	}

	h.sim.Node(1).MeterOffline = false
	h.run(2 * Interval)
	if mi := h.m.Node(1).Info().Meter; mi.Stale || !mi.Online || mi.Missed != 0 {
		t.Errorf("recovered meter info %+v", mi)
	}
}

// A meter total that goes down is rejected: the last plausible value stays.
func TestEVMeterImplausible(t *testing.T) {
	h := newHarness(t)
	h.plugIn(1)
	h.run(30 * time.Second)
	good := h.m.Node(1).Info().Meter.EnergyKWh

	h.sim.Node(1).MeterKWh -= 5
	h.run(4 * 2 * Interval)
	mi := h.m.Node(1).Info().Meter
	if mi.EnergyKWh != good || !mi.Stale {
		t.Errorf("energy %.3f kWh (was %.3f), meter info %+v", mi.EnergyKWh, good, mi)
	}

	n := h.m.nodes[1]
	if err := h.m.plausible(n, [3]int{1200, 0, 0}, n.meter.energy); !errors.Is(err, errImplausible) {
		t.Errorf("120 A accepted: %v", err)
	}
	if err := h.m.plausible(n, [3]int{160, 160, 160}, n.meter.energy+1_000_000); !errors.Is(err, errImplausible) {
		t.Errorf("1 MWh jump accepted: %v", err)
	}
	if err := h.m.plausible(n, [3]int{160, 160, 160}, n.meter.energy+20); err != nil {
		t.Errorf("normal reading rejected: %v", err)
	}
}

// The simulator ignores a request that starts within 3.5 characters of the
// previous frame, as the real devices do (merged frame, bad CRC): the old
// bug can be reproduced, and the scenarios check it never happens.
func TestSimMergedFrame(t *testing.T) {
	s := NewSim(1)
	c := &virtualClock{t: time.Unix(0, 0)}
	s.busNow = c.Now
	req := appendCRC([]byte{2, 0x04, 0, 0, 0, 8})

	s.Write(req)
	c.Sleep(frameTime(len(req)) + minGap)
	s.Write(req)
	if s.Merged != 0 || len(s.rx) != 2*21 {
		t.Fatalf("separate frames: merged %d, %d answer bytes", s.Merged, len(s.rx))
	}
	c.Sleep(frameTime(len(req)) + time.Millisecond) // < 3.5 characters after it
	s.Write(req)
	if s.Merged != 1 || len(s.rx) != 2*21 {
		t.Errorf("merged frame: merged %d, %d answer bytes", s.Merged, len(s.rx))
	}
}

func TestResponseTimeout(t *testing.T) {
	b := NewBus(nil)
	if got := b.responseTimeout(17, 29); got != b.Timeout {
		t.Errorf("without samples: %v, want the bus default %v", got, b.Timeout)
	}
	st := b.stat(17)
	for i := 0; i < minLatencySamples; i++ {
		st.addLatency(time.Duration(10+i*10) * time.Millisecond)
	}
	if got := b.responseTimeout(17, 29); got != 400*time.Millisecond { // 2 × p99 (200 ms)
		t.Errorf("with samples: %v, want 400ms", got)
	}
}
