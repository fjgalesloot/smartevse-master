package smartevse

import (
	"encoding/binary"
	"math"
	"os"
	"sync"
	"time"
)

// Sim is a simulated RS485 bus with a Sensorbox-2 and SmartEVSE nodes.
// It implements Transport, so a Master can run against it unchanged.
//
// Node behaviour follows SmartEVSE-3 v3.11.2 (main.cpp: EVSE state machine in
// Timer10ms, Timer1S error handling, setItemValue STATUS_STATE/STATUS_ERROR,
// modbus.cpp broadcast handling). Time is virtual: call Advance.
type Sim struct {
	mu  sync.Mutex
	rx  []byte
	now time.Time

	House            [3]float64 // household load per phase in A (negative = export)
	SensorboxOffline bool
	Nodes            map[byte]*SimNode // by Modbus address

	// Log records every frame the master sent (for assertions).
	Log []SimFrame
}

// SimFrame is a request seen on the bus.
type SimFrame struct {
	At    time.Time
	Addr  byte
	Func  byte
	Reg   uint16
	Value []uint16
}

// SimNode is a simulated SmartEVSE in "Node n" mode with an EV attached.
type SimNode struct {
	Offline bool // disconnected from the bus (the unit itself keeps running)

	// configuration of the unit
	MaxA int // reported max current (reg 0x0007)
	Mode int

	// the car
	Connected bool    // cable plugged in
	Wants     bool    // car wants to charge (pulls CP to 6V when PWM present)
	CarMaxA   float64 // what the car draws at most
	Phases    int     // 1 or 3

	// EV meter: node setting (reg 0x0108/0x0109) and the meter itself, a
	// separate device on the bus that answers at MeterAddr. Only Eastron3P is
	// simulated, with its registers written out here independent of the master.
	MeterType    int
	MeterAddr    int
	MeterOffline bool    // meter does not answer
	MeterKWh     float64 // meter total
	CfgChanged   bool    // reg 0x0006: settings saved, master clears it

	// firmware state
	State       int
	Err         int
	ChargeDelay int     // seconds
	Timeout     int     // seconds until CT_NOCOMM
	EVTimeout   int     // seconds until EV_NOCOMM
	HeardKWh    float64 // energy the node picked up from its meter's answers (its OCPP meter value)
	PWM         int     // advertised current, 0.1 A

	acc time.Duration
}

// COMMEVTimeout is the firmware's COMM_EVTIMEOUT in seconds (8 × NR_EVSES).
const COMMEVTimeout = 8 * NrEVSEs

// sim Eastron3P registers (SDM630 input registers)
const (
	simEastronCurrent = 0x0006 // L1-L3 current, then L1-L3 power: 12 registers
	simEastronEnergy  = 0x0048 // total import kWh: 2 registers
)

// NewSim creates a simulator with nodes for the given ids (1..7), each with
// a 3-phase car that is not yet connected and an Eastron3P EV meter at
// address 16 + id.
func NewSim(ids ...int) *Sim {
	s := &Sim{now: time.Unix(1_700_000_000, 0), Nodes: map[byte]*SimNode{}}
	for _, id := range ids {
		s.Nodes[byte(id+1)] = &SimNode{
			MaxA: 16, Mode: ModeSmart, CarMaxA: 16, Phases: 3,
			MeterType: MeterEastron3P, MeterAddr: 16 + id, MeterKWh: 1000 * float64(id),
			Timeout: COMMTimeout, EVTimeout: COMMTimeout, PWM: 60,
		}
	}
	return s
}

// COMMTimeout is the firmware's COMM_TIMEOUT in seconds.
const COMMTimeout = 11

// Node returns the simulated node with the given id.
func (s *Sim) Node(id int) *SimNode { return s.Nodes[byte(id+1)] }

// Now is the simulated clock, to be passed to Master.SetClock.
func (s *Sim) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

// Advance moves virtual time forward, running node state machines once per second.
func (s *Sim) Advance(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for step := time.Second; d > 0; d -= step {
		if d < step {
			step = d
		}
		s.now = s.now.Add(step)
		for _, n := range s.Nodes {
			n.MeterKWh += n.EVCurrent() * float64(n.Phases) * 230 * step.Hours() / 1000
			n.acc += step
			for n.acc >= time.Second {
				n.acc -= time.Second
				n.tick1s()
			}
		}
	}
}

// Do runs f with the simulator locked, so House and Nodes can be changed
// safely while a Master is running against it in another goroutine.
func (s *Sim) Do(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
}

// EVCurrent is what the car actually draws per phase, in A.
func (n *SimNode) EVCurrent() float64 {
	if n.State != StateC {
		return 0
	}
	return math.Min(float64(n.PWM)/10, n.CarMaxA)
}

// Mains returns the real mains currents (house + all EVs), in A.
func (s *Sim) Mains() [3]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mainsLocked()
}

func (s *Sim) mainsLocked() [3]float64 {
	m := s.House
	for _, n := range s.Nodes {
		i := n.EVCurrent()
		for p := 0; p < n.Phases && p < 3; p++ {
			m[p] += i
		}
	}
	return m
}

// MaxPhase returns the highest real mains phase current in A.
func (s *Sim) MaxPhase() float64 {
	m := s.Mains()
	return math.Max(m[0], math.Max(m[1], m[2]))
}

// ---- node firmware model ------------------------------------------------------

type pilot int

const (
	pilot12V pilot = iota // no car
	pilot9V               // car connected, not requesting (or no PWM)
	pilot6V               // car requests charging
)

func (n *SimNode) pilot() pilot {
	if !n.Connected {
		return pilot12V
	}
	pwmOn := n.State == StateB || n.State == StateCommC || n.State == StateC
	if n.Wants && pwmOn {
		return pilot6V
	}
	return pilot9V
}

// setStatePowerUnavailable from main.cpp.
func (n *SimNode) powerUnavailable() {
	switch n.State {
	case StateA:
	case StateC:
		n.State = StateC1
	case StateC1, StateB1:
	default:
		n.State = StateB1
	}
}

// tick1s runs the 1 s timer work and the EVSE state machine.
func (n *SimNode) tick1s() {
	// Timer1S: node timeout on master communication
	if n.Timeout == 0 && n.Err&ErrCTNoComm == 0 {
		n.Err |= ErrCTNoComm
		n.powerUnavailable()
	} else if n.Timeout > 0 {
		n.Timeout--
	}
	if n.MeterType != 0 {
		if n.EVTimeout == 0 && n.Err&ErrEVNoComm == 0 && n.Mode != ModeNormal {
			n.Err |= ErrEVNoComm
			n.powerUnavailable()
		} else if n.EVTimeout > 0 {
			n.EVTimeout--
		}
	} else {
		n.EVTimeout = COMMEVTimeout
	}
	if n.Err&ErrCTNoComm != 0 && n.Timeout > 0 {
		n.Err &^= ErrCTNoComm
	}
	if n.Err&ErrEVNoComm != 0 && n.EVTimeout > 0 {
		n.Err &^= ErrEVNoComm
	}
	if n.Err&ErrLess6A != 0 {
		n.powerUnavailable()
		n.ChargeDelay = 60 // CHARGEDELAY
	}
	if n.ChargeDelay > 0 {
		n.ChargeDelay--
	}
	n.stateMachine()
}

func (n *SimNode) stateMachine() {
	p := n.pilot()
	switch n.State {
	case StateA, StateCommB, StateB1:
		if p == pilot12V {
			n.State = StateA
			n.ChargeDelay = 0
		} else if p == pilot9V && n.Err == 0 && n.ChargeDelay == 0 && n.State != StateCommB {
			n.State = StateCommB // ask master
		} else if p == pilot9V && n.State != StateB1 && n.State != StateCommB {
			n.State = StateB1
		}
	case StateCommBOK:
		n.State = StateB
	case StateB, StateCommC:
		if p == pilot12V {
			n.State = StateA
		} else if p == pilot6V && n.Err == 0 && n.ChargeDelay == 0 {
			n.State = StateCommC // ask master
		}
	case StateC1:
		if p == pilot12V {
			n.State = StateA
		} else if p == pilot9V {
			n.State = StateB1
		}
	case StateCommCOK:
		n.State = StateC
	case StateC:
		if p == pilot12V {
			n.State = StateA
		} else if p == pilot9V {
			n.State = StateB
		}
	}
}

// setCurrent mirrors SetCurrent(): invalid values fall back to 6 A.
func (n *SimNode) setCurrent(v int) {
	if v < 60 || v > 800 {
		v = 60
	}
	n.PWM = v
}

// writeError mirrors setItemValue(STATUS_ERROR).
func (n *SimNode) writeError(v int) {
	n.Err = v
	if v != 0 {
		if v&ErrCTNoComm != 0 {
			n.Timeout = 0
		}
		n.powerUnavailable()
		n.ChargeDelay = 60
	}
}

// ---- Transport --------------------------------------------------------------------

// Write receives a request frame from the master and queues the answer.
func (s *Sim) Write(f []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(f) < 4 || crc16(f[:len(f)-2]) != binary.LittleEndian.Uint16(f[len(f)-2:]) {
		return len(f), nil // garbage: nobody answers
	}
	addr, fc := f[0], f[1]
	reg := binary.BigEndian.Uint16(f[2:4])
	frame := SimFrame{At: s.now, Addr: addr, Func: fc, Reg: reg}

	switch fc {
	case 0x04:
		count := binary.BigEndian.Uint16(f[4:6])
		if addr == SensorboxAddr {
			if !s.SensorboxOffline && reg == 0 {
				s.reply(addr, fc, s.sensorboxRegs(count))
			}
		} else if n, ok := s.Nodes[addr]; ok && !n.Offline && reg == regNodeStatus {
			cfg := uint16(0)
			if n.CfgChanged {
				cfg = 1
			}
			s.reply(addr, fc, []uint16{
				uint16(n.State), uint16(n.Err), uint16(n.PWM), uint16(n.Mode),
				0, 1, cfg, uint16(n.MaxA),
			}[:count])
		} else if ok && !n.Offline && reg == regNodeEVMeter && count == 2 {
			s.reply(addr, fc, []uint16{uint16(n.MeterType), uint16(n.MeterAddr)})
		} else if m := s.meterAt(addr); m != nil {
			s.meterRequest(m, reg, count)
		}
	case 0x10:
		count := int(binary.BigEndian.Uint16(f[4:6]))
		vals := make([]uint16, count)
		for i := range vals {
			vals[i] = binary.BigEndian.Uint16(f[7+i*2:])
		}
		frame.Value = vals
		if addr == BroadcastAddr && reg == regBroadcastLoad {
			for a, n := range s.Nodes {
				if n.Offline {
					continue
				}
				cur := int(vals[a-1])
				if cur == 0 && n.State == StateC {
					n.State = StateC1
				} else if n.State == StateB || n.State == StateC {
					n.setCurrent(cur)
				}
				n.Timeout = COMMTimeout
			}
		} else if n, ok := s.Nodes[addr]; ok && !n.Offline && reg == regNodeStatus {
			for i, v := range vals {
				switch i {
				case 0:
					if int(v) != n.State {
						n.State = int(v)
					}
				case 1:
					n.writeError(int(v))
				case 3:
					if int(v) != n.Mode {
						n.Mode = int(v)
						n.ChargeDelay = 0 // setMode clears ChargeDelay
					}
				}
			}
			n.stateMachine() // firmware processes the new state within 10 ms
			s.rx = append(s.rx, appendCRC(append([]byte{}, f[:6]...))...)
		}
	case 0x06:
		val := binary.BigEndian.Uint16(f[4:6])
		frame.Value = []uint16{val}
		if addr == BroadcastAddr && reg == regNodeError {
			for _, n := range s.Nodes {
				if !n.Offline {
					n.writeError(int(val))
				}
			}
		} else if n, ok := s.Nodes[addr]; ok && !n.Offline && reg == regNodeCfgChanged {
			n.CfgChanged = val != 0
			s.rx = append(s.rx, f...)
		}
	}
	s.Log = append(s.Log, frame)
	return len(f), nil
}

// meterAt returns the node whose EV meter answers at addr.
func (s *Sim) meterAt(addr byte) *SimNode {
	for _, n := range s.Nodes {
		if n.MeterType == MeterEastron3P && n.MeterAddr == int(addr) && !n.MeterOffline {
			return n
		}
	}
	return nil
}

// meterRequest answers a read of an Eastron3P meter. Nodes listening for that
// address take the answer as the firmware does (MBEVMeterResponse): the
// register comes from the request they saw just before.
func (s *Sim) meterRequest(m *SimNode, reg, count uint16) {
	var vals []float64
	switch {
	case reg == simEastronCurrent && count == 12:
		i := m.EVCurrent()
		for p := 0; p < 3; p++ {
			if p >= m.Phases {
				i = 0
			}
			vals = append(vals, i)
		}
		for p := 0; p < 3; p++ {
			vals = append(vals, vals[p]*230)
		}
	case reg == simEastronEnergy && count == 2:
		vals = []float64{m.MeterKWh}
	default:
		return // the meter answers with an exception; the master gets a timeout here
	}
	regs := make([]uint16, 0, len(vals)*2)
	for _, v := range vals {
		bits := math.Float32bits(float32(v))
		regs = append(regs, uint16(bits>>16), uint16(bits))
	}
	s.reply(byte(m.MeterAddr), 0x04, regs)

	for _, n := range s.Nodes {
		if n.Offline || n.MeterType != MeterEastron3P || n.MeterAddr != m.MeterAddr {
			continue
		}
		switch reg {
		case simEastronCurrent:
			n.EVTimeout = COMMEVTimeout
		case simEastronEnergy:
			n.HeardKWh = float64(float32(m.MeterKWh))
		}
	}
}

func (s *Sim) reply(addr, fc byte, regs []uint16) {
	out := []byte{addr, fc, byte(len(regs) * 2)}
	for _, r := range regs {
		out = binary.BigEndian.AppendUint16(out, r)
	}
	s.rx = append(s.rx, appendCRC(out)...)
}

func (s *Sim) sensorboxRegs(count uint16) []uint16 {
	regs := make([]uint16, 20)
	regs[0] = 0x0014
	regs[1] = 0x3280 // DSMR 5.0, P1 present
	m := s.mainsLocked()
	for i := 0; i < 3; i++ {
		bits := math.Float32bits(float32(m[i]))
		regs[8+i*2] = uint16(bits >> 16)
		regs[9+i*2] = uint16(bits)
	}
	if int(count) < len(regs) {
		return regs[:count]
	}
	return regs
}

// Read returns queued answer bytes, or a timeout when there are none.
func (s *Sim) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rx) == 0 {
		return 0, os.ErrDeadlineExceeded
	}
	n := copy(p, s.rx)
	s.rx = s.rx[n:]
	return n, nil
}

// SetReadDeadline is a no-op: the simulator answers instantly.
func (s *Sim) SetReadDeadline(time.Time) error { return nil }

// Close is a no-op.
func (s *Sim) Close() error { return nil }

// Broadcasts returns the current broadcasts sent since t.
func (s *Sim) Broadcasts(since time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := 0
	for _, f := range s.Log {
		if f.Addr == BroadcastAddr && f.Reg == regBroadcastLoad && !f.At.Before(since) {
			c++
		}
	}
	return c
}
