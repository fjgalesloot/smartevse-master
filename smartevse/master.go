package smartevse

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"
)

// NodeConfig describes one SmartEVSE configured as "Node <ID>".
type NodeConfig struct {
	ID   int    // 1..7, the Load Balancing setting on the unit (Node 1 = Modbus address 2)
	Host string // WiFi host name or address of the unit, for 1P/3P switching; "" = none
}

// Config for the master.
type Config struct {
	MaxMains       int           // A per phase
	MaxCircuit     int           // A, 0 = disabled
	MinCurrent     int           // A, default 6
	Nodes          []NodeConfig  //
	Stale          time.Duration // mains data older than this = fault; default 11 s (firmware COMM_TIMEOUT)
	CT200A         bool          // Sensorbox CTs are 200A:50mA
	DefaultEnabled bool          // nodes may charge before evcc sent anything
	SensorboxHost  string        // Sensorbox-2 WiFi REST API, used while it does not answer on the bus; "" = none

	EVMeterEvery      int           // read each EV meter every N cycles, round robin; default 2
	EVMeterTimeout    time.Duration // response timeout of EV meter reads, 0 = from the answer size and measured latency
	EVMeterStaleAfter int           // failed reads in a row before an EV meter is stale; default 3
}

// Interval is the control cycle, same as the firmware.
const Interval = 2 * time.Second

// maxNodeA is the most any node gets (A), the clamp in the firmware's SetCurrent.
const maxNodeA = 80

type node struct {
	id      int
	addr    byte
	online  int // countdown, 5 = just seen
	state   int
	err     int
	mode    int
	maxA    int  // reported by node (reg 0x0007)
	access  int  // reg 0x0005, as read or last written
	off     bool // access OFF, set by the node itself (logged once)
	enabled bool
	limit   int // 0.1 A, from evcc
	logged  int // 0.1 A, current broadcast to the node when last logged

	cfgChanged bool // EV meter setting must be (re)read: at start, back online, or flagged by the node
	meter      evMeter
	meterDue   uint64 // cycle from which the EV meter is due to be read

	host string    // WiFi REST API, for the contactor 2 setting
	c2   int       // contactor 2 setting (C2AlwaysOff ...), -1 = not read yet
	hold time.Time // phase switch: paused until the node has left C; zero = none
}

// holdTimeout ends a phase switch pause that does not complete (C1 lasts at
// most 6 s in the firmware, then the node goes to B1).
const holdTimeout = 30 * time.Second

// evMeter is a node's EV meter as set on the node, with the last readings.
type evMeter struct {
	known     bool // setting read from the node
	typ       int  // EV meter type (reg 0x0108)
	addr      int  // Modbus address (reg 0x0109)
	answered  bool // has answered since the setting was read
	fails     int  // failed reads in a row
	stale     bool // fails reached EVMeterStaleAfter: readings are old
	rejecting bool // an implausible reading has been logged
	currents  [3]int
	power     int       // W
	energy    int       // Wh, total import
	at        time.Time // last successful poll
}

func (n *node) slot() int { return int(n.addr) - 1 }

// Master is an external SmartEVSE load-balancing master.
type Master struct {
	bus *Bus
	cfg Config
	log *slog.Logger
	now func() time.Time

	mu      sync.Mutex
	bal     Balancer
	nodes   map[int]*node
	mains   Mains
	mainsAt time.Time
	fault   bool
	cycles  uint64
	cycleAt time.Time
	busAt   time.Time   // start of the cycle on the bus clock, for the time budget
	meterID int         // node whose EV meter was polled last
	viaWeb  bool        // mains currently from the Sensorbox REST API
	stale   map[int]int // times an EV meter went stale, by address (survives setting changes)

	web   *nodeWeb      // not locked by mu: used outside it
	sbWeb *sensorboxWeb // nil = no REST fallback; has its own lock
}

// NewMaster validates the config and creates a master.
func NewMaster(bus *Bus, cfg Config, log *slog.Logger) (*Master, error) {
	if cfg.MinCurrent == 0 {
		cfg.MinCurrent = 6
	}
	if cfg.Stale == 0 {
		cfg.Stale = 11 * time.Second
	}
	if cfg.EVMeterEvery == 0 {
		cfg.EVMeterEvery = 2
	}
	if cfg.EVMeterStaleAfter == 0 {
		cfg.EVMeterStaleAfter = 3
	}
	if cfg.EVMeterEvery < 0 || cfg.EVMeterStaleAfter < 0 || cfg.EVMeterTimeout < 0 {
		return nil, fmt.Errorf("EV meter settings must not be negative")
	}
	if cfg.MaxMains < cfg.MinCurrent {
		return nil, fmt.Errorf("max mains %d A below min current %d A", cfg.MaxMains, cfg.MinCurrent)
	}
	if len(cfg.Nodes) == 0 {
		return nil, fmt.Errorf("no nodes configured")
	}
	if log == nil {
		log = slog.Default()
	}

	m := &Master{
		bus:   bus,
		cfg:   cfg,
		log:   log,
		now:   time.Now,
		nodes: make(map[int]*node),
		bal:   Balancer{MaxMains: cfg.MaxMains, MaxCircuit: cfg.MaxCircuit, MinCurrent: cfg.MinCurrent},
		web:   newNodeWeb(),
		stale: map[int]int{},
	}
	for i, nc := range cfg.Nodes {
		if nc.ID < 1 || nc.ID > 7 {
			return nil, fmt.Errorf("node id %d out of range 1..7", nc.ID)
		}
		if _, dup := m.nodes[nc.ID]; dup {
			return nil, fmt.Errorf("node %d configured twice", nc.ID)
		}
		m.nodes[nc.ID] = &node{
			id: nc.ID, addr: byte(nc.ID + 1),
			enabled: cfg.DefaultEnabled, limit: maxNodeA * 10, cfgChanged: true,
			host: nc.Host, c2: -1,
			meterDue: uint64(1 + i%cfg.EVMeterEvery), // spread the meters over the cycles
		}
	}
	if cfg.SensorboxHost != "" {
		// The REST API does not say whether its currents come from P1 or the
		// CTs, so 200 A CT readings could not be doubled: they would read half.
		if cfg.CT200A {
			return nil, fmt.Errorf("sensorbox web api cannot be used with 200A CTs")
		}
		m.sbWeb = newSensorboxWeb(cfg.SensorboxHost, log)
	}
	return m, nil
}

// SetClock replaces the clock (for tests and simulation).
func (m *Master) SetClock(now func() time.Time) { m.now = now }

// SetHTTPTransport replaces the transport to the nodes' REST API (for tests
// and simulation: Sim implements it). Call before Run.
func (m *Master) SetHTTPTransport(rt http.RoundTripper) {
	m.web.client.Transport = rt
	m.web.pause = 0
	if m.sbWeb != nil {
		m.sbWeb.client.Transport = rt
	}
}

// Run executes the control cycle every Interval until ctx is done. This is
// the only goroutine that uses the bus. A cycle that overruns the interval
// is logged and the next one is skipped, not caught up.
func (m *Master) Run(ctx context.Context) {
	if m.sbWeb != nil {
		go m.sbWeb.run(ctx, m.now) // HTTP only, never the bus
	}
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		start := time.Now()
		m.Cycle()
		if took := time.Since(start); took > Interval {
			m.log.Warn("cycle overrun: skipping a cycle", "took", took)
			select {
			case <-t.C: // the tick that passed during the cycle
			default:
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Cycle runs one control cycle, mirroring the firmware's ModbusRequestLoop.
func (m *Master) Cycle() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cycles++
	m.cycleAt = m.now()
	m.busAt = m.bus.now()
	m.control()
	m.pollEVMeters() // after the broadcast, in the time left: meters never delay the control loop
}

// control reads mains and nodes, answers node requests and broadcasts the currents.
func (m *Master) control() {
	now := m.cycleAt

	// 1. mains
	mains, err := readSensorbox(m.bus, m.cfg.CT200A)
	fresh := err == nil
	if fresh {
		m.mains, m.mainsAt = mains, now
		if m.viaWeb {
			m.log.Info("mains from the sensorbox on the bus again")
			m.viaWeb = false
		}
	} else {
		fresh = m.mainsFromWeb()
		if m.viaWeb {
			m.log.Debug("sensorbox read failed", "err", err) // expected while on the web api
		} else {
			m.warn("sensorbox read failed", err)
		}
	}
	wasFault := m.fault
	m.fault = m.mainsAt.IsZero() || now.Sub(m.mainsAt) > m.cfg.Stale
	if m.fault && !wasFault {
		m.viaWeb = false // web api stale too: log bus failures again
		m.log.Error("mains measurement lost: stopping all nodes", "last", m.mainsAt)
	} else if !m.fault && wasFault {
		m.log.Info("mains measurement restored")
	}
	imeas := m.mains.Imeasured()

	// 2. poll nodes
	for _, n := range m.sortedNodes() {
		data, err := m.bus.readInput(n.addr, regNodeStatus, nodeStatusRegs)
		m.bal.Answered[n.slot()] = err == nil
		if err != nil {
			if n.online > 0 {
				n.online--
				if n.online == 0 {
					m.log.Warn("node offline", "node", n.id)
					m.bal.State[n.slot()] = StateA
					m.bal.Balanced[n.slot()] = 0
				}
			}
			continue
		}
		if n.online == 0 {
			m.log.Info("node online", "node", n.id)
			n.cfgChanged = true // may have restarted with other settings
			n.c2 = -1
		}
		n.online = 5
		n.state, n.err, n.mode, n.access, n.maxA = int(data[1]), int(data[3]), int(data[7]), int(data[11]), int(data[15])
		if data[13] != 0 {
			n.cfgChanged = true
		}
		m.bal.State[n.slot()] = n.state
		m.bal.Max[n.slot()] = m.effectiveMax(n)
	}

	// 3. pause or resume nodes for evcc
	m.syncAccess()

	// 4. handshake
	m.bal.PhasesUpdated = fresh
	m.bal.StartedThisCycle = false
	for _, n := range m.sortedNodes() {
		if n.online == 0 {
			continue
		}
		// a node that went OFF while waiting for an answer is sent to B1 like a paused one
		vals := m.bal.NodeUpdate(n.slot(), n.err, n.mode, imeas, m.allowed(n) && !n.off, n.access != AccessOn, m.fault)
		if vals == nil {
			continue
		}
		args := []any{"node", n.id, "from", StateName(n.state), "to", StateName(int(vals[0])), "errors", vals[1]}
		if len(vals) > 3 {
			args = append(args, "mode", vals[3])
		}
		m.log.Info("node state written", args...)
		if err := m.bus.writeMultiple(n.addr, regNodeStatus, vals); err != nil {
			m.warn("node write failed", err, "node", n.id)
		}
	}

	// 5. no valid mains: stop broadcasting, nodes stop on CT_NOCOMM (as firmware)
	if m.fault {
		for i := range m.bal.Balanced {
			m.bal.Balanced[i] = 0
		}
		return
	}

	// 6. balance
	m.bal.Calc(imeas, false)
	if m.bal.Shortage() || imeas > m.cfg.MaxMains*20 {
		m.log.Warn("not enough current: stopping all nodes", "imeasured", imeas)
		if err := m.bus.writeSingle(BroadcastAddr, regNodeError, ErrLess6A); err != nil {
			m.warn("broadcast error failed", err)
		}
	}

	// 7. broadcast currents + mains
	if m.log.Enabled(context.Background(), slog.LevelDebug) {
		m.logCycle(imeas, fresh)
	}
	vals := make([]uint16, 0, broadcastLoadRegs)
	for _, b := range m.bal.Balanced {
		vals = append(vals, uint16(b))
	}
	for _, l := range m.mains.L {
		vals = append(vals, uint16(int16(l)))
	}
	if err := m.bus.writeMultiple(BroadcastAddr, regBroadcastLoad, vals); err != nil {
		m.warn("broadcast failed", err)
	} else {
		m.logCurrents()
	}
}

// logStep is the change since the last log (0.1 A) above which a current set
// by the balancer is logged; it moves the current nearly every cycle.
const logStep = 5

// logCurrents logs the nodes whose broadcast current (A) changed, with the reason.
func (m *Master) logCurrents() {
	for _, n := range m.sortedNodes() {
		b := m.bal.Balanced[n.slot()]
		if b == n.logged {
			continue
		}
		reason := m.currentReason(n, b)
		if reason == "balancer" && abs(b-n.logged) <= logStep {
			continue
		}
		m.log.Info("node current", "node", n.id, "from", float64(n.logged)/10, "to", float64(b)/10, "reason", reason)
		n.logged = b
	}
}

// currentReason tells why node n gets current b (0.1 A).
func (m *Master) currentReason(n *node, b int) string {
	switch {
	case n.online == 0:
		return "offline"
	case n.off:
		return "node off"
	case !n.enabled:
		return "evcc disabled"
	case !n.hold.IsZero():
		return "phase switch"
	case n.limit < m.cfg.MinCurrent*10:
		return "evcc limit below min current"
	case b == 0:
		return "not charging"
	case n.logged == 0:
		return "start"
	case b == n.limit:
		return "evcc limit"
	case b == n.maxA*10:
		return "node max"
	}
	return "balancer"
}

// logCycle logs the mains measurement (A) and the current (A) broadcast to each node.
func (m *Master) logCycle(imeas int, fresh bool) {
	args := []any{
		"source", m.mains.Source, "fresh", fresh,
		"l1", float64(m.mains.L[0]) / 10, "l2", float64(m.mains.L[1]) / 10, "l3", float64(m.mains.L[2]) / 10,
		"imeasured", float64(imeas) / 10, "powerW", m.mains.PowerW(),
	}
	for _, n := range m.sortedNodes() {
		args = append(args, fmt.Sprintf("node%d", n.id), float64(m.bal.Balanced[n.slot()])/10)
	}
	m.log.Debug("cycle", args...)
}

// mainsFromWeb takes a new measurement from the Sensorbox REST API, if there
// is one newer than the last measurement used.
func (m *Master) mainsFromWeb() bool {
	if m.sbWeb == nil {
		return false
	}
	mains, at, ok := m.sbWeb.latest()
	if !ok || !at.After(m.mainsAt) {
		return false
	}
	m.mains, m.mainsAt = mains, at
	if !m.viaWeb {
		m.log.Warn("sensorbox not answering on the bus: mains from its web api")
		m.viaWeb = true
	}
	return true
}

// warn logs a failed bus request; a lost bus is logged once by the bus itself.
func (m *Master) warn(msg string, err error, args ...any) {
	if errors.Is(err, ErrBusDown) {
		return
	}
	m.log.Warn(msg, append(args, "err", err)...)
}

// syncAccess pauses a node evcc does not allow to charge through the node's
// own access register: the node stops a charging car by itself, does not ask
// to charge again, and its display, web UI and MQTT show "Pause". evcc wins
// over a pause or resume made on the node (button, web UI, MQTT): it is undone
// in the next cycle.
//
// OFF is left alone: it is the node's own "may not charge" (OCPP without a
// transaction, RFID lock, delayed start, mode Off). With the RFID reader
// disabled or in OCPP mode, OCPP sets OFF again on every loop while no
// transaction permits charging, so writing ON would only make the node flap
// between B and B1. The node sets ON itself once it may charge again (OCPP
// auto-auth starts a transaction when a car is plugged in).
//
// A paused node without a car (state A) is set to ON: a paused node in A
// ignores a car being plugged in, so evcc would never see it connect. Plugging
// in then makes it ask to charge (COMM_B), and it is paused in the same cycle.
//
// A phase switch holds the node paused the same way until it reports that it
// has left C (B1 or B), so it restarts and switches its contactors.
func (m *Master) syncAccess() {
	for _, n := range m.sortedNodes() {
		if !n.hold.IsZero() {
			left := n.access != AccessOn && n.state != StateC && n.state != StateD &&
				n.state != StateC1 && n.state != StateCommCOK
			if timeout := m.cycleAt.Sub(n.hold) > holdTimeout; left || timeout || n.online == 0 {
				if timeout {
					m.log.Warn("phase switch: node did not stop charging, resuming", "node", n.id)
				}
				n.hold = time.Time{}
			}
		}
		if n.online == 0 {
			continue
		}
		if off := n.access == AccessOff; off != n.off {
			n.off = off
			if off {
				m.log.Info("node is off (OCPP, RFID or on the node): it may not charge until it switches on", "node", n.id)
			} else {
				m.log.Info("node switched on", "node", n.id, "access", AccessName(n.access))
			}
		}
		if n.off {
			continue
		}
		want := AccessOn
		if !m.allowed(n) && n.state != StateA {
			want = AccessPause
		}
		if n.access == want {
			continue
		}
		// the node answers after it has applied the new value
		if err := m.bus.writeSingle(n.addr, regNodeAccess, uint16(want)); err != nil {
			m.warn("node access write failed", err, "node", n.id)
			continue
		}
		m.log.Info("node access set", "node", n.id, "from", AccessName(n.access), "to", AccessName(want))
		n.access = want
	}
}

// meterBudget is the part of the cycle that must be left to start reading an
// EV meter, so meter reads and their retries never delay the next cycle.
const meterBudget = 600 * time.Millisecond

// Plausibility limits for EV meter readings.
const (
	maxMeterCurrent = 1000 // 0.1 A
	maxPhaseVolts   = 250
	energySlackWh   = 50 // meter resolution and timing
)

// errNoBudget: an EV meter read was not started because the cycle is nearly over.
var errNoBudget = errors.New("cycle time budget used")

// errImplausible marks an EV meter reading that was rejected.
var errImplausible = errors.New("implausible")

// pollEVMeters reads the EV meters that are due, in the time left after the
// broadcast. Each meter is due every EVMeterEvery cycles (the firmware master
// reads one per cycle: ModbusRequestLoop states 3-5 and 20), round robin from
// the node after the one read last. A meter that does not fit in the cycle
// stays due and goes first next cycle.
//
// A node does not read its own meter: it listens to the answers on the bus.
// The current reading keeps its EV meter timeout (64 s, then EV_NOCOMM stops
// charging in Smart mode) alive, and the energy reading is what it reports
// over OCPP.
func (m *Master) pollEVMeters() {
	ns := m.sortedNodes()
	start := 0
	for i, n := range ns {
		if n.id == m.meterID {
			start = i + 1
		}
	}
	for k := range ns {
		n := ns[(start+k)%len(ns)]
		if n.online == 0 || n.meterDue > m.cycles {
			continue
		}
		if !m.budget(meterBudget) || !m.pollEVMeter(n) {
			m.log.Debug("EV meter read postponed: cycle time used", "node", n.id)
			return
		}
		n.meterDue = m.cycles + uint64(m.cfg.EVMeterEvery)
		m.meterID = n.id
	}
}

// pollEVMeter reads one node's EV meter; false if it ran out of cycle time.
func (m *Master) pollEVMeter(n *node) bool {
	if n.cfgChanged && !m.readMeterSetting(n) {
		return true
	}
	spec, ok := lookupMeter(n.meter.typ)
	if !ok || n.meter.addr <= SensorboxAddr {
		return true
	}
	mt := &n.meter
	addr := byte(mt.addr)

	var (
		currents [3]int
		power    int
		energy   int
		hasPower bool
	)
	// current first: it is what the node's timeout depends on
	data, err := m.meterRead(addr, spec.currentRead())
	if err == nil {
		currents, power, hasPower, err = spec.decodeCurrents(data)
	}
	if err == nil {
		if data, err = m.meterRead(addr, spec.energyRead()); err == nil {
			energy, err = spec.decodeEnergy(data)
		}
	}
	if r, ok := spec.powerRead(); ok && err == nil {
		if data, err = m.meterRead(addr, r); err == nil {
			power, err = spec.decodePower(data)
			hasPower = true
		}
	}
	if errors.Is(err, errNoBudget) {
		return false
	}
	if err == nil {
		err = m.plausible(n, currents, energy)
	}
	if err != nil {
		m.meterFailed(n, err)
		return true
	}

	mt.currents, mt.energy, mt.at = currents, energy, m.cycleAt
	if hasPower {
		mt.power = power
	}
	m.log.Debug("EV meter", "node", n.id,
		"l1", float64(mt.currents[0])/10, "l2", float64(mt.currents[1])/10, "l3", float64(mt.currents[2])/10,
		"powerW", mt.power, "energyKWh", float64(mt.energy)/1000)
	switch {
	case mt.stale:
		m.log.Info("EV meter answering again", "node", n.id, "meter", MeterName(mt.typ), "address", mt.addr,
			"missed", mt.fails, "energyKWh", float64(mt.energy)/1000)
	case !mt.answered:
		m.log.Info("EV meter answering", "node", n.id, "meter", MeterName(mt.typ), "address", mt.addr,
			"energyKWh", float64(mt.energy)/1000)
	}
	mt.answered, mt.stale, mt.fails, mt.rejecting = true, false, 0, false
	return true
}

// meterFailed counts a failed EV meter read. Single failures are normal on
// a busy bus (debug); the meter is stale after EVMeterStaleAfter in a row.
func (m *Master) meterFailed(n *node, err error) {
	mt := &n.meter
	mt.fails++
	if errors.Is(err, errImplausible) && !mt.rejecting {
		mt.rejecting = true
		m.log.Warn("EV meter reading rejected", "node", n.id, "address", mt.addr, "err", err)
	}
	m.log.Debug("EV meter read failed", "node", n.id, "address", mt.addr, "missed", mt.fails, "err", err)
	if mt.fails == m.cfg.EVMeterStaleAfter {
		mt.stale = true
		m.stale[mt.addr]++
		m.warn("EV meter not answering: readings are stale", err, "node", n.id, "meter", MeterName(mt.typ),
			"address", mt.addr, "missed", mt.fails)
	}
}

// plausible rejects readings that cannot be right, so that a damaged answer
// with a valid CRC never reaches the energy total: a current above 100 A, an
// energy total that went down, or rose faster than the node could charge.
func (m *Master) plausible(n *node, currents [3]int, energy int) error {
	for _, c := range currents {
		if abs(c) > maxMeterCurrent {
			return fmt.Errorf("%w: current %.1f A", errImplausible, float64(c)/10)
		}
	}
	mt := &n.meter
	if mt.at.IsZero() {
		return nil
	}
	if energy < mt.energy {
		return fmt.Errorf("%w: energy went down from %.3f to %.3f kWh", errImplausible,
			float64(mt.energy)/1000, float64(energy)/1000)
	}
	dt := m.cycleAt.Sub(mt.at)
	capA := n.maxA
	if capA <= 0 || capA > maxNodeA {
		capA = maxNodeA
	}
	maxWh := float64(capA*3*maxPhaseVolts)*dt.Hours()*1.5 + energySlackWh
	if rise := energy - mt.energy; float64(rise) > maxWh {
		return fmt.Errorf("%w: energy rose %d Wh in %v, at most %.0f Wh possible", errImplausible, rise, dt, maxWh)
	}
	return nil
}

// budget reports whether at least d of the cycle is left.
func (m *Master) budget(d time.Duration) bool {
	return Interval-m.bus.now().Sub(m.busAt) >= d
}

// meterRead reads an EV meter, with one retry for a request that may not
// have been heard or an answer that was damaged. Each attempt must fit in
// the cycle, else errNoBudget.
func (m *Master) meterRead(addr byte, r meterRead) ([]byte, error) {
	timeout := m.cfg.EVMeterTimeout
	if timeout == 0 {
		timeout = m.bus.responseTimeout(addr, 5+int(r.count)*2)
	}
	need := timeout + 100*time.Millisecond // plus gap and transmit times
	if !m.budget(need) {
		return nil, errNoBudget
	}
	data, err := m.bus.readTimeout(addr, r.fc, r.reg, r.count, timeout)
	if !retryable(err) {
		return data, err
	}
	if !m.budget(need) {
		return nil, err
	}
	m.log.Debug("EV meter read failed, retrying", "address", addr, "err", err)
	if data, err = m.bus.readTimeout(addr, r.fc, r.reg, r.count, timeout); err == nil {
		m.bus.countRetryOK(addr)
	}
	return data, err
}

// readMeterSetting reads the node's EV meter type and address and clears the
// node's config-changed flag (requestNodeConfig / receiveNodeConfig).
func (m *Master) readMeterSetting(n *node) bool {
	data, err := m.bus.readInput(n.addr, regNodeEVMeter, 2)
	if err != nil {
		m.warn("reading EV meter setting failed", err, "node", n.id)
		return false
	}
	n.cfgChanged = false
	typ, addr := int(data[1]), int(data[3])
	if !n.meter.known || typ != n.meter.typ || addr != n.meter.addr {
		n.meter = evMeter{known: true, typ: typ, addr: addr}
		_, supported := lookupMeter(typ)
		switch {
		case typ == MeterDisabled:
			m.log.Info("node has no EV meter", "node", n.id)
		case typ == MeterAPI || typ == MeterHomeWizard:
			m.log.Info("node EV meter is not on the bus", "node", n.id, "meter", MeterName(typ))
		case !supported:
			m.log.Warn("node EV meter type not supported, not polling it", "node", n.id, "meter", MeterName(typ))
		case addr <= SensorboxAddr:
			m.log.Warn("node EV meter address collides with SmartEVSE addresses, not polling it", "node", n.id, "address", addr)
		default:
			m.log.Info("node EV meter", "node", n.id, "meter", MeterName(typ), "address", addr)
		}
	}
	if err := m.bus.writeSingle(n.addr, regNodeCfgChanged, 0); err != nil {
		m.warn("clearing node config flag failed", err, "node", n.id)
	}
	return true
}

// allowed reports whether a node may charge: evcc enabled it with a usable
// limit, and no phase switch is waiting for it to stop.
func (m *Master) allowed(n *node) bool {
	return n.enabled && n.limit >= m.cfg.MinCurrent*10 && n.hold.IsZero()
}

// effectiveMax is the most a node may ever get: the lower of its own max
// setting (reg 0x0007) and the evcc limit. Nodes do not enforce their own
// maximum when driven by a master, so this is the safety cap. Until the node
// has reported its setting it gets nothing.
func (m *Master) effectiveMax(n *node) int {
	if !m.allowed(n) || n.maxA <= 0 {
		return 0
	}
	return min(min(n.maxA, maxNodeA)*10, n.limit)
}

func (m *Master) sortedNodes() []*node {
	ns := make([]*node, 0, len(m.nodes))
	for _, n := range m.nodes {
		ns = append(ns, n)
	}
	sort.Slice(ns, func(i, j int) bool { return ns[i].id < ns[j].id })
	return ns
}

// ---- evcc-shaped node handle -------------------------------------------------

// Node is a handle with the same method set evcc's api.Charger uses.
type Node struct {
	m  *Master
	id int
}

// Node returns the handle for node id, or nil.
func (m *Master) Node(id int) *Node {
	if _, ok := m.nodes[id]; !ok {
		return nil
	}
	return &Node{m: m, id: id}
}

// Status returns the IEC 61851 state letter A, B or C.
func (h *Node) Status() (string, error) {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	n := h.m.nodes[h.id]
	if n.online == 0 {
		return "", fmt.Errorf("node %d offline", h.id)
	}
	switch n.state {
	case StateA:
		return "A", nil
	case StateC, StateD, StateCommCOK:
		return "C", nil
	default: // B, COMM_B, COMM_B_OK, COMM_C, ACTIVATION, B1, C1
		return "B", nil
	}
}

// Enabled reports whether evcc allows charging.
func (h *Node) Enabled() (bool, error) {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.m.nodes[h.id].enabled, nil
}

// Enable allows or stops charging. Takes effect in the next cycle.
func (h *Node) Enable(enable bool) error {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	h.m.nodes[h.id].enabled = enable
	return nil
}

// MaxCurrent sets the evcc limit in A.
func (h *Node) MaxCurrent(current int64) error {
	return h.MaxCurrentMillis(float64(current))
}

// MaxCurrentMillis sets the evcc limit in A with 0.1 A resolution.
func (h *Node) MaxCurrentMillis(current float64) error {
	if current < 0 || current > maxNodeA {
		return fmt.Errorf("current %.1f A out of range", current)
	}
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	h.m.nodes[h.id].limit = int(current * 10)
	return nil
}

// ErrNoHost is returned for 1P/3P switching on a node without a WiFi host.
var ErrNoHost = errors.New("no host configured for this node (-nodes id:host)")

// Phases1p3p switches the node between 1 and 3 phases through contactor 2
// (evcc api.PhaseSwitcher). The setting goes to the node's WiFi REST API;
// the node applies it when it next enters C. A charging node is paused until
// it has stopped, then resumed, as evcc's smart-evse driver does (whenDisabled).
func (h *Node) Phases1p3p(phases int) error {
	var c2 int
	switch phases {
	case 1:
		c2 = C2AlwaysOff
	case 3:
		c2 = C2AlwaysOn
	default:
		return fmt.Errorf("invalid phases: %d", phases)
	}
	m := h.m
	n := m.nodes[h.id] // host is fixed after NewMaster
	if n.host == "" {
		return ErrNoHost
	}
	if err := m.web.setEnableC2(n.host, c2); err != nil {
		return fmt.Errorf("node %d: %w", h.id, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	charging := n.state == StateC || n.state == StateD || n.state == StateCommCOK
	restart := charging && n.c2 != c2
	n.c2 = c2
	if restart {
		n.hold = m.now()
	}
	m.log.Info("node phases set", "node", h.id, "phases", phases, "restart", restart)
	return nil
}

// GetPhases returns the phases the node's contactor 2 setting gives
// (evcc api.PhaseGetter). Read from the node's WiFi REST API the first time.
func (h *Node) GetPhases() (int, error) {
	m := h.m
	n := m.nodes[h.id]
	if n.host == "" {
		return 0, ErrNoHost
	}
	m.mu.Lock()
	c2 := n.c2
	m.mu.Unlock()
	if c2 < 0 {
		var err error
		if c2, err = m.web.enableC2(n.host); err != nil {
			return 0, fmt.Errorf("node %d: %w", h.id, err)
		}
		m.mu.Lock()
		if n.c2 < 0 {
			n.c2 = c2
		}
		c2 = n.c2
		m.mu.Unlock()
	}
	return c2Phases(c2), nil
}

// ---- snapshot -------------------------------------------------------------------

// NodeInfo is the externally visible state of a node.
type NodeInfo struct {
	ID       int        `json:"id"`
	Online   bool       `json:"online"`
	State    string     `json:"state"`
	Status   string     `json:"status"`
	Errors   int        `json:"errors"`
	Access   string     `json:"access"`           // on, pause or off (node access register)
	Phases   int        `json:"phases,omitempty"` // contactor 2 setting, once known (GetPhases)
	Switch   bool       `json:"phaseSwitch"`      // paused for a phase switch
	Enabled  bool       `json:"enabled"`
	LimitA   float64    `json:"limitA"`
	MaxA     float64    `json:"maxA"`     // effective cap
	CurrentA float64    `json:"currentA"` // what is broadcast
	Meter    *MeterInfo `json:"meter,omitempty"`
}

// MeterInfo is a node's EV meter as polled by the master.
type MeterInfo struct {
	Type      string     `json:"type"`
	Address   int        `json:"address"`
	Polled    bool       `json:"polled"` // false: type not supported or not on the bus
	Online    bool       `json:"online"` // last poll succeeded
	Stale     bool       `json:"stale"`  // failed EVMeterStaleAfter times in a row: values are the last good ones (see ageSeconds)
	Missed    int        `json:"missed"` // failed reads in a row
	CurrentsA [3]float64 `json:"currentsA"`
	PowerW    int        `json:"powerW"`
	EnergyKWh float64    `json:"energyKWh"`  // meter total
	Age       float64    `json:"ageSeconds"` // since the last successful poll, -1 = none yet
}

// Snapshot is the externally visible state of the master.
type Snapshot struct {
	MainsA    [3]float64  `json:"mainsA"`
	MainsV    [3]float64  `json:"mainsV"` // P1 voltage, 0 = not available (power uses 230 V)
	MainsW    [3]int      `json:"mainsW"`
	PowerW    int         `json:"powerW"` // total mains power, positive = import
	MainsAge  float64     `json:"mainsAgeSeconds"`
	Source    string      `json:"source"`
	Fault     bool        `json:"fault"`
	MaxMainsA int         `json:"maxMainsA"`
	Cycles    uint64      `json:"cycles"`
	CycleAge  float64     `json:"cycleAgeSeconds"` // since the last cycle started, -1 = none yet
	Nodes     []NodeInfo  `json:"nodes"`
	Bus       []AddrStats `json:"bus"` // counters per bus address
}

// Snapshot returns the current state.
func (m *Master) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{
		MainsV: m.mains.V, MainsW: m.mains.W, PowerW: m.mains.PowerW(),
		Source: m.mains.Source, Fault: m.fault, MaxMainsA: m.cfg.MaxMains, Cycles: m.cycles, MainsAge: -1, CycleAge: -1,
	}
	for i, l := range m.mains.L {
		s.MainsA[i] = float64(l) / 10
	}
	now := m.now()
	if !m.mainsAt.IsZero() {
		s.MainsAge = now.Sub(m.mainsAt).Seconds()
	}
	if !m.cycleAt.IsZero() {
		s.CycleAge = now.Sub(m.cycleAt).Seconds()
	}
	for _, n := range m.sortedNodes() {
		s.Nodes = append(s.Nodes, m.nodeInfo(n))
	}
	s.Bus = m.bus.Stats()
	for i := range s.Bus {
		s.Bus[i].StaleEvents = m.stale[s.Bus[i].Addr]
	}
	return s
}

// NodeInfo returns the state of one node.
func (h *Node) Info() NodeInfo {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.m.nodeInfo(h.m.nodes[h.id])
}

func (m *Master) nodeInfo(n *node) NodeInfo {
	var status string
	switch {
	case n.online == 0:
		status = "offline"
	case n.state == StateA:
		status = "A"
	case n.state == StateC || n.state == StateD || n.state == StateCommCOK:
		status = "C"
	default:
		status = "B"
	}
	var phases int
	if n.c2 >= 0 {
		phases = c2Phases(n.c2)
	}
	return NodeInfo{
		ID: n.id, Online: n.online > 0, State: StateName(n.state), Status: status, Errors: n.err,
		Access: AccessName(n.access), Phases: phases, Switch: !n.hold.IsZero(), Enabled: n.enabled, LimitA: float64(n.limit) / 10,
		MaxA:     float64(m.effectiveMax(n)) / 10,
		CurrentA: float64(m.bal.Balanced[n.slot()]) / 10,
		Meter:    m.meterInfo(n),
	}
}

func (m *Master) meterInfo(n *node) *MeterInfo {
	mt := n.meter
	if !mt.known || mt.typ == MeterDisabled {
		return nil
	}
	_, supported := lookupMeter(mt.typ)
	mi := &MeterInfo{
		Type: MeterName(mt.typ), Address: mt.addr, Polled: supported && mt.addr > SensorboxAddr,
		Online: mt.answered && mt.fails == 0, Stale: mt.stale, Missed: mt.fails,
		PowerW: mt.power, EnergyKWh: float64(mt.energy) / 1000, Age: -1,
	}
	for i, c := range mt.currents {
		mi.CurrentsA[i] = float64(c) / 10
	}
	if !mt.at.IsZero() {
		mi.Age = m.now().Sub(mt.at).Seconds()
	}
	return mi
}
