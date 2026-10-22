package smartevse

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// NodeConfig describes one SmartEVSE configured as "Node <ID>".
type NodeConfig struct {
	ID   int // 1..7, the Load Balancing setting on the unit (Node 1 = Modbus address 2)
	MaxA int // hard cap in A, applied on top of what the node reports
}

// Config for the master.
type Config struct {
	MaxMains       int           // A per phase
	MaxCircuit     int           // A, 0 = disabled
	MinCurrent     int           // A, default 6
	Nodes          []NodeConfig  //
	Stale          time.Duration // mains data older than this = fault; default 6 s
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
	enabled bool
	limit   int // 0.1 A, from evcc
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
}

// NewMaster validates the config and creates a master.
func NewMaster(bus *Bus, cfg Config, log *slog.Logger) (*Master, error) {
	if cfg.MinCurrent == 0 {
		cfg.MinCurrent = 6
	}
	if cfg.Stale == 0 {
		cfg.Stale = 6 * time.Second
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
			enabled: cfg.DefaultEnabled, limit: nc.MaxA * 10,
		}
	}
	return m, nil
}

// SetClock replaces the clock (for tests and simulation).
func (m *Master) SetClock(now func() time.Time) { m.now = now }

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
	now := m.now()
	m.cycleAt = now

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
		}
		n.online = 5
		n.state, n.err, n.mode, n.maxA = int(data[1]), int(data[3]), int(data[7]), int(data[15])
		m.bal.State[n.slot()] = n.state
		m.bal.Max[n.slot()] = m.effectiveMax(n)
	}

	// 3. handshake
	m.bal.PhasesUpdated = fresh
	m.bal.StartedThisCycle = false
	for _, n := range m.sortedNodes() {
		if n.online == 0 {
			continue
		}
		vals := m.bal.NodeUpdate(n.slot(), n.err, n.mode, imeas, m.allowed(n), m.fault)
		if vals == nil {
			continue
		}
		m.log.Debug("node update", "node", n.id, "state", StateName(n.state), "write", vals)
		if err := m.bus.WriteMultiple(n.addr, regNodeStatus, vals); err != nil {
			m.warn("node write failed", err, "node", n.id)
		}
	}

	// 4. no valid mains: stop broadcasting, nodes stop on CT_NOCOMM (as firmware)
	if m.fault {
		for i := range m.bal.Balanced {
			m.bal.Balanced[i] = 0
		}
		return
	}

	// 5. balance
	m.bal.Calc(imeas, false)
	if m.bal.Shortage() || imeas > m.cfg.MaxMains*20 {
		m.log.Warn("not enough current: stopping all nodes", "imeasured", imeas)
		if err := m.bus.WriteSingle(BroadcastAddr, regNodeError, ErrLess6A); err != nil {
			m.warn("broadcast error failed", err)
		}
	}

	// 6. broadcast currents + mains
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

// warn logs a failed bus request; a lost bus is logged once by the bus itself.
func (m *Master) warn(msg string, err error, args ...any) {
	if errors.Is(err, ErrBusDown) {
		return
	}
	m.log.Warn(msg, append(args, "err", err)...)
}

func (m *Master) allowed(n *node) bool {
	return n.enabled && n.limit >= m.cfg.MinCurrent*10
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

// ---- snapshot -------------------------------------------------------------------

// NodeInfo is the externally visible state of a node.
type NodeInfo struct {
	ID       int     `json:"id"`
	Online   bool    `json:"online"`
	State    string  `json:"state"`
	Status   string  `json:"status"`
	Errors   int     `json:"errors"`
	Enabled  bool    `json:"enabled"`
	LimitA   float64 `json:"limitA"`
	MaxA     float64 `json:"maxA"`     // effective cap
	CurrentA float64 `json:"currentA"` // what is broadcast
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
	return NodeInfo{
		ID: n.id, Online: n.online > 0, State: StateName(n.state), Status: status, Errors: n.err,
		Enabled: n.enabled, LimitA: float64(n.limit) / 10,
		MaxA:     float64(m.effectiveMax(n)) / 10,
		CurrentA: float64(m.bal.Balanced[n.slot()]) / 10,
	}
}
