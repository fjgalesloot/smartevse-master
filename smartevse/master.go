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
	MaxA int    // hard cap in A, applied on top of what the node reports
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
}

// Interval is the control cycle, same as the firmware.
const Interval = 2 * time.Second

type node struct {
	id      int
	addr    byte
	capA    int
	online  int // countdown, 5 = just seen
	state   int
	err     int
	mode    int
	maxA    int // reported by node (reg 0x0007)
	access  int // reg 0x0005, as read or last written
	enabled bool
	limit   int // 0.1 A, from evcc

	cfgChanged bool // EV meter setting must be (re)read: at start, back online, or flagged by the node
	meter      evMeter

	host string    // WiFi REST API, for the contactor 2 setting
	c2   int       // contactor 2 setting (C2AlwaysOff ...), -1 = not read yet
	hold time.Time // phase switch: paused until the node has left C; zero = none
}

// holdTimeout ends a phase switch pause that does not complete (C1 lasts at
// most 6 s in the firmware, then the node goes to B1).
const holdTimeout = 30 * time.Second

// evMeter is a node's EV meter as set on the node, with the last readings.
type evMeter struct {
	known    bool // setting read from the node
	typ      int  // EV meter type (reg 0x0108)
	addr     int  // Modbus address (reg 0x0109)
	ok       bool // last poll succeeded
	logged   bool // ok has been logged
	currents [3]int
	power    int       // W
	energy   int       // Wh, total import
	at       time.Time // last successful poll
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
	meterID int // node whose EV meter was polled last

	web *nodeWeb // not locked by mu: used outside it
}

// NewMaster validates the config and creates a master.
func NewMaster(bus *Bus, cfg Config, log *slog.Logger) (*Master, error) {
	if cfg.MinCurrent == 0 {
		cfg.MinCurrent = 6
	}
	if cfg.Stale == 0 {
		cfg.Stale = 11 * time.Second
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
	}
	for _, nc := range cfg.Nodes {
		if nc.ID < 1 || nc.ID > 7 {
			return nil, fmt.Errorf("node id %d out of range 1..7", nc.ID)
		}
		if nc.MaxA < cfg.MinCurrent || nc.MaxA > 80 {
			return nil, fmt.Errorf("node %d: max %d A out of range %d..80", nc.ID, nc.MaxA, cfg.MinCurrent)
		}
		if _, dup := m.nodes[nc.ID]; dup {
			return nil, fmt.Errorf("node %d configured twice", nc.ID)
		}
		m.nodes[nc.ID] = &node{
			id: nc.ID, addr: byte(nc.ID + 1), capA: nc.MaxA,
			enabled: cfg.DefaultEnabled, limit: nc.MaxA * 10, cfgChanged: true,
			host: nc.Host, c2: -1,
		}
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
}

// Run executes the control cycle every Interval until ctx is done.
func (m *Master) Run(ctx context.Context) {
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		m.Cycle()
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
	m.control()
	m.pollEVMeter() // after the broadcast: meters do not delay the control loop
}

// control reads mains and nodes, answers node requests and broadcasts the currents.
func (m *Master) control() {
	now := m.cycleAt

	// 1. mains
	mains, err := ReadSensorbox(m.bus, m.cfg.CT200A)
	fresh := err == nil
	if fresh {
		m.mains, m.mainsAt = mains, now
	} else {
		m.warn("sensorbox read failed", err)
	}
	wasFault := m.fault
	m.fault = m.mainsAt.IsZero() || now.Sub(m.mainsAt) > m.cfg.Stale
	if m.fault && !wasFault {
		m.log.Error("mains measurement lost: stopping all nodes", "last", m.mainsAt)
	} else if !m.fault && wasFault {
		m.log.Info("mains measurement restored")
	}
	imeas := m.mains.Imeasured()

	// 2. poll nodes
	for _, n := range m.sortedNodes() {
		data, err := m.bus.ReadInput(n.addr, regNodeStatus, nodeStatusRegs)
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
		vals := m.bal.NodeUpdate(n.slot(), n.err, n.mode, imeas, m.allowed(n), n.access == AccessPause, m.fault)
		if vals == nil {
			continue
		}
		m.log.Debug("node update", "node", n.id, "state", StateName(n.state), "write", vals)
		if err := m.bus.WriteMultiple(n.addr, regNodeStatus, vals); err != nil {
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
		if err := m.bus.WriteSingle(BroadcastAddr, regNodeError, ErrLess6A); err != nil {
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
	if err := m.bus.WriteMultiple(BroadcastAddr, regBroadcastLoad, vals); err != nil {
		m.warn("broadcast failed", err)
	}
}

// logCycle logs the mains measurement (A) and the current (A) broadcast to each node.
func (m *Master) logCycle(imeas int, fresh bool) {
	args := []any{
		"source", m.mains.Source, "fresh", fresh,
		"l1", float64(m.mains.L[0]) / 10, "l2", float64(m.mains.L[1]) / 10, "l3", float64(m.mains.L[2]) / 10,
		"imeasured", float64(imeas) / 10,
	}
	for _, n := range m.sortedNodes() {
		args = append(args, fmt.Sprintf("node%d", n.id), float64(m.bal.Balanced[n.slot()])/10)
	}
	m.log.Debug("cycle", args...)
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
// to charge again, and its display, web UI and MQTT show "Pause". evcc wins: a
// change made on the node (button, web UI, MQTT) is undone in the next cycle.
//
// A node without a car (state A) is set to ON: a paused node in A ignores a car
// being plugged in, so evcc would never see it connect. Plugging in then makes
// it ask to charge (COMM_B), and it is paused in the same cycle.
//
// A phase switch holds the node paused the same way until it reports that it
// has left C (B1 or B), so it restarts and switches its contactors.
func (m *Master) syncAccess() {
	for _, n := range m.sortedNodes() {
		if !n.hold.IsZero() {
			left := n.access == AccessPause && n.state != StateC && n.state != StateD &&
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
		want := AccessOn
		if !m.allowed(n) && n.state != StateA {
			want = AccessPause
		}
		if n.access == want {
			continue
		}
		// the node answers after it has applied the new value
		if err := m.bus.WriteSingle(n.addr, regNodeAccess, uint16(want)); err != nil {
			m.warn("node access write failed", err, "node", n.id)
			continue
		}
		m.log.Info("node access set", "node", n.id, "from", AccessName(n.access), "to", AccessName(want))
		n.access = want
	}
}

// pollEVMeter reads the EV meter of one online node per cycle, round robin, as
// the firmware master does (ModbusRequestLoop states 3-5 and 20). A node does
// not read its own meter: it listens to the answers on the bus. The current
// reading keeps its EV meter timeout (64 s, then EV_NOCOMM stops charging in
// Smart mode) alive, and the energy reading is what it reports over OCPP.
func (m *Master) pollEVMeter() {
	n := m.nextMeterNode()
	if n == nil {
		return
	}
	if n.cfgChanged && !m.readMeterSetting(n) {
		return
	}
	spec, ok := lookupMeter(n.meter.typ)
	if !ok || n.meter.addr <= SensorboxAddr {
		return
	}
	mt := &n.meter
	addr := byte(mt.addr)

	// current first: it is what the node's timeout depends on
	data, err := m.meterRead(addr, spec.currentRead())
	if err == nil {
		var power int
		var hasPower bool
		if mt.currents, power, hasPower, err = spec.decodeCurrents(data); err == nil && hasPower {
			mt.power = power
		}
	}
	if err == nil {
		if data, err = m.meterRead(addr, spec.energyRead()); err == nil {
			mt.energy, err = spec.decodeEnergy(data)
		}
	}
	if r, ok := spec.powerRead(); ok && err == nil {
		if data, err = m.meterRead(addr, r); err == nil {
			mt.power, err = spec.decodePower(data)
		}
	}
	if err == nil {
		mt.at = m.cycleAt
		m.log.Debug("EV meter", "node", n.id,
			"l1", float64(mt.currents[0])/10, "l2", float64(mt.currents[1])/10, "l3", float64(mt.currents[2])/10,
			"powerW", mt.power, "energyKWh", float64(mt.energy)/1000)
	}

	if ok := err == nil; ok != mt.ok || !mt.logged {
		mt.ok, mt.logged = ok, true
		if ok {
			m.log.Info("EV meter answering", "node", n.id, "meter", MeterName(mt.typ), "address", mt.addr,
				"energyKWh", float64(mt.energy)/1000)
		} else {
			m.warn("EV meter read failed", err, "node", n.id, "meter", MeterName(mt.typ), "address", mt.addr)
		}
	} else if !ok {
		m.log.Debug("EV meter read failed", "node", n.id, "err", err)
	}
}

func (m *Master) meterRead(addr byte, r meterRead) ([]byte, error) {
	return m.bus.Read(addr, r.fc, r.reg, r.count)
}

// nextMeterNode returns the next online node after the one polled last.
func (m *Master) nextMeterNode() *node {
	var first, next *node
	for _, n := range m.sortedNodes() {
		if n.online == 0 {
			continue
		}
		if first == nil {
			first = n
		}
		if next == nil && n.id > m.meterID {
			next = n
		}
	}
	if next == nil {
		next = first
	}
	if next != nil {
		m.meterID = next.id
	}
	return next
}

// readMeterSetting reads the node's EV meter type and address and clears the
// node's config-changed flag (requestNodeConfig / receiveNodeConfig).
func (m *Master) readMeterSetting(n *node) bool {
	data, err := m.bus.ReadInput(n.addr, regNodeEVMeter, 2)
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
	if err := m.bus.WriteSingle(n.addr, regNodeCfgChanged, 0); err != nil {
		m.warn("clearing node config flag failed", err, "node", n.id)
	}
	return true
}

// allowed reports whether a node may charge: evcc enabled it with a usable
// limit, and no phase switch is waiting for it to stop.
func (m *Master) allowed(n *node) bool {
	return n.enabled && n.limit >= m.cfg.MinCurrent*10 && n.hold.IsZero()
}

// effectiveMax is the most a node may ever get: the lowest of what the node
// reports, the configured cap and the evcc limit. Nodes do not enforce their
// own maximum when driven by a master, so this is the safety cap.
func (m *Master) effectiveMax(n *node) int {
	if !m.allowed(n) {
		return 0
	}
	max := n.capA * 10
	if n.maxA > 0 {
		max = min(max, n.maxA*10)
	}
	return min(max, n.limit)
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
	if current < 0 || current > 80 {
		return fmt.Errorf("current %.1f A out of range", current)
	}
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	h.m.nodes[h.id].limit = int(current * 10)
	return nil
}

// ErrNoHost is returned for 1P/3P switching on a node without a WiFi host.
var ErrNoHost = errors.New("no host configured for this node (-nodes id:maxA:host)")

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
	m.log.Info("phases set", "node", h.id, "phases", phases, "restart", restart)
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
	CurrentsA [3]float64 `json:"currentsA"`
	PowerW    int        `json:"powerW"`
	EnergyKWh float64    `json:"energyKWh"`  // meter total
	Age       float64    `json:"ageSeconds"` // since the last successful poll, -1 = none yet
}

// Snapshot is the externally visible state of the master.
type Snapshot struct {
	MainsA    [3]float64 `json:"mainsA"`
	MainsAge  float64    `json:"mainsAgeSeconds"`
	Source    string     `json:"source"`
	Fault     bool       `json:"fault"`
	MaxMainsA int        `json:"maxMainsA"`
	Cycles    uint64     `json:"cycles"`
	CycleAge  float64    `json:"cycleAgeSeconds"` // since the last cycle started, -1 = none yet
	Nodes     []NodeInfo `json:"nodes"`
}

// Snapshot returns the current state.
func (m *Master) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{Source: m.mains.Source, Fault: m.fault, MaxMainsA: m.cfg.MaxMains, Cycles: m.cycles, MainsAge: -1, CycleAge: -1}
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
		Type: MeterName(mt.typ), Address: mt.addr, Polled: supported && mt.addr > SensorboxAddr, Online: mt.ok,
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
