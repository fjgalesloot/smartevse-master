package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/fjgalesloot/smartevse-master/smartevse"
)

type api struct {
	m   *smartevse.Master
	sim *smartevse.Sim // nil unless -sim
	log *slog.Logger
}

func newAPI(m *smartevse.Master, sim *smartevse.Sim, log *slog.Logger) *api {
	return &api{m: m, sim: sim, log: log}
}

func (a *api) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", a.state)
	mux.HandleFunc("GET /api/nodes/{id}", a.nodeInfo)
	mux.HandleFunc("POST /api/nodes/{id}/enable", a.nodeEnable)
	mux.HandleFunc("POST /api/nodes/{id}/current", a.nodeCurrent)
	mux.HandleFunc("GET /api/nodes/{id}/phases", a.nodePhases)
	mux.HandleFunc("POST /api/nodes/{id}/phases", a.nodeSetPhases)
	mux.HandleFunc("GET /healthz", a.healthz)
	if a.sim != nil {
		mux.HandleFunc("GET /api/sim", a.simState)
		mux.HandleFunc("POST /api/sim/house", a.simHouse)
		mux.HandleFunc("POST /api/sim/sensorbox", a.simSensorbox)
		mux.HandleFunc("POST /api/sim/nodes/{id}", a.simNode)
	}
	return a.logRequests(mux)
}

func (a *api) logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.log.Debug("http", "method", r.Method, "uri", r.URL.RequestURI())
		h.ServeHTTP(w, r)
	})
}

func (a *api) state(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.m.Snapshot())
}

// healthz fails when the control loop is stuck or the mains measurement is lost.
func (a *api) healthz(w http.ResponseWriter, r *http.Request) {
	s := a.m.Snapshot()
	switch {
	case s.CycleAge < 0 || s.CycleAge > 3*smartevse.Interval.Seconds():
		http.Error(w, fmt.Sprintf("control loop stalled (last cycle %.0f s ago)", s.CycleAge), http.StatusServiceUnavailable)
	case s.Fault:
		http.Error(w, "mains measurement lost", http.StatusServiceUnavailable)
	default:
		fmt.Fprintln(w, "ok")
	}
}

func (a *api) node(w http.ResponseWriter, r *http.Request) *smartevse.Node {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid node id", http.StatusBadRequest)
		return nil
	}
	n := a.m.Node(id)
	if n == nil {
		http.Error(w, fmt.Sprintf("node %d not configured", id), http.StatusNotFound)
	}
	return n
}

func (a *api) nodeInfo(w http.ResponseWriter, r *http.Request) {
	if n := a.node(w, r); n != nil {
		writeJSON(w, n.Info())
	}
}

func (a *api) nodeEnable(w http.ResponseWriter, r *http.Request) {
	n := a.node(w, r)
	if n == nil {
		return
	}
	v, err := strconv.ParseBool(r.URL.Query().Get("value"))
	if err != nil {
		http.Error(w, "value: want true or false", http.StatusBadRequest)
		return
	}
	if err := n.Enable(v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.log.Info("enable", "node", r.PathValue("id"), "value", v)
	writeJSON(w, n.Info())
}

func (a *api) nodeCurrent(w http.ResponseWriter, r *http.Request) {
	n := a.node(w, r)
	if n == nil {
		return
	}
	v, err := strconv.ParseFloat(r.URL.Query().Get("value"), 64)
	if err != nil {
		http.Error(w, "value: want current in A", http.StatusBadRequest)
		return
	}
	if err := n.MaxCurrentMillis(v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.log.Info("current", "node", r.PathValue("id"), "value", v)
	writeJSON(w, n.Info())
}

type phasesInfo struct {
	Phases int `json:"phases"`
}

// nodePhases returns the phases of the node's contactor 2 setting (evcc getphases).
func (a *api) nodePhases(w http.ResponseWriter, r *http.Request) {
	n := a.node(w, r)
	if n == nil {
		return
	}
	p, err := n.GetPhases()
	if err != nil {
		http.Error(w, err.Error(), phasesErrCode(err))
		return
	}
	writeJSON(w, phasesInfo{p})
}

// phasesErrCode: a node without host is a configuration error, anything else
// a failure of the node's web server.
func phasesErrCode(err error) int {
	if errors.Is(err, smartevse.ErrNoHost) {
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

// nodeSetPhases switches the node to ?value=1 or 3 phases (evcc phases1p3p).
func (a *api) nodeSetPhases(w http.ResponseWriter, r *http.Request) {
	n := a.node(w, r)
	if n == nil {
		return
	}
	v, err := strconv.Atoi(r.URL.Query().Get("value"))
	if err != nil || (v != 1 && v != 3) {
		http.Error(w, "value: want 1 or 3", http.StatusBadRequest)
		return
	}
	if err := n.Phases1p3p(v); err != nil {
		http.Error(w, err.Error(), phasesErrCode(err))
		return
	}
	writeJSON(w, phasesInfo{v})
}

// ---- simulator control (-sim only) ------------------------------------------

type simNodeInfo struct {
	ID        int     `json:"id"`
	Offline   bool    `json:"offline"`
	Connected bool    `json:"connected"`
	Wants     bool    `json:"wants"`
	CarMaxA   float64 `json:"carMaxA"`
	Phases    int     `json:"phases"`
	State     string  `json:"state"`
	Errors    int     `json:"errors"`
	Access    string  `json:"access"`
	EnableC2  int     `json:"enableC2"`   // contactor 2 setting (1 = 1P, 3 = 3P)
	NrPhases  int     `json:"nrOfPhases"` // contactors as switched on entering C
	PWMA      float64 `json:"pwmA"`
	DrawA     float64 `json:"drawA"`

	MeterAddress int     `json:"meterAddress"`
	MeterOffline bool    `json:"meterOffline"`
	MeterKWh     float64 `json:"meterKWh"`
	HeardKWh     float64 `json:"heardKWh"` // what the node reports over OCPP
}

type simInfo struct {
	MainsA           [3]float64    `json:"mainsA"` // real, incl. EVs
	HouseA           [3]float64    `json:"houseA"`
	SensorboxOffline bool          `json:"sensorboxOffline"`
	Nodes            []simNodeInfo `json:"nodes"`
}

func (a *api) simState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.simInfo())
}

func (a *api) simInfo() simInfo {
	var s simInfo
	a.sim.Do(func() {
		s.HouseA, s.SensorboxOffline = a.sim.House, a.sim.SensorboxOffline
		for id := 1; id <= 7; id++ {
			n := a.sim.Node(id)
			if n == nil {
				continue
			}
			s.Nodes = append(s.Nodes, simNodeInfo{
				ID: id, Offline: n.Offline, Connected: n.Connected, Wants: n.Wants,
				CarMaxA: n.CarMaxA, Phases: n.Phases, State: smartevse.StateName(n.State),
				Errors: n.Err, Access: smartevse.AccessName(n.Access), EnableC2: n.EnableC2, NrPhases: n.NrOfPhases,
				PWMA: float64(n.PWM) / 10, DrawA: n.EVCurrent(),
				MeterAddress: n.MeterAddr, MeterOffline: n.MeterOffline, MeterKWh: n.MeterKWh, HeardKWh: n.HeardKWh,
			})
		}
	})
	s.MainsA = a.sim.Mains()
	return s
}

// simHouse sets the household load: ?value=5,5,5 (A per phase, negative = export).
func (a *api) simHouse(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Query().Get("value"), ",")
	if len(parts) != 3 {
		http.Error(w, "value: want l1,l2,l3 in A", http.StatusBadRequest)
		return
	}
	var house [3]float64
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			http.Error(w, "value: want l1,l2,l3 in A", http.StatusBadRequest)
			return
		}
		house[i] = v
	}
	a.sim.Do(func() { a.sim.House = house })
	writeJSON(w, a.simInfo())
}

// simSensorbox takes the Sensorbox off the bus: ?offline=true.
func (a *api) simSensorbox(w http.ResponseWriter, r *http.Request) {
	v, err := strconv.ParseBool(r.URL.Query().Get("offline"))
	if err != nil {
		http.Error(w, "offline: want true or false", http.StatusBadRequest)
		return
	}
	a.sim.Do(func() { a.sim.SensorboxOffline = v })
	writeJSON(w, a.simInfo())
}

// simNode changes the car or bus connection of a simulated node. All
// parameters are optional: connected, wants, offline, meteroffline (bool),
// carmax (A), phases, access (on, pause or off: as if changed on the node).
func (a *api) simNode(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || a.sim.Node(id) == nil {
		http.Error(w, "unknown node", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	var (
		bools  = map[string]*bool{}
		carMax *float64
		phases *int
		access *int
	)
	for _, k := range []string{"connected", "wants", "offline", "meteroffline"} {
		if !q.Has(k) {
			continue
		}
		v, err := strconv.ParseBool(q.Get(k))
		if err != nil {
			http.Error(w, k+": want true or false", http.StatusBadRequest)
			return
		}
		bools[k] = &v
	}
	if q.Has("carmax") {
		v, err := strconv.ParseFloat(q.Get("carmax"), 64)
		if err != nil || v < 0 {
			http.Error(w, "carmax: want current in A", http.StatusBadRequest)
			return
		}
		carMax = &v
	}
	if q.Has("phases") {
		v, err := strconv.Atoi(q.Get("phases"))
		if err != nil || (v != 1 && v != 3) {
			http.Error(w, "phases: want 1 or 3", http.StatusBadRequest)
			return
		}
		phases = &v
	}
	if q.Has("access") {
		v := -1
		for _, a := range []int{smartevse.AccessOff, smartevse.AccessOn, smartevse.AccessPause} {
			if q.Get("access") == smartevse.AccessName(a) {
				v = a
			}
		}
		if v < 0 {
			http.Error(w, "access: want on, pause or off", http.StatusBadRequest)
			return
		}
		access = &v
	}

	a.sim.Do(func() {
		n := a.sim.Node(id)
		if v, ok := bools["connected"]; ok {
			n.Connected = *v
		}
		if v, ok := bools["wants"]; ok {
			n.Wants = *v
		}
		if v, ok := bools["offline"]; ok {
			n.Offline = *v
		}
		if v, ok := bools["meteroffline"]; ok {
			n.MeterOffline = *v
		}
		if carMax != nil {
			n.CarMaxA = *carMax
		}
		if phases != nil {
			n.Phases = *phases
		}
		if access != nil {
			n.SetAccess(*access)
		}
	})
	writeJSON(w, a.simInfo())
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
