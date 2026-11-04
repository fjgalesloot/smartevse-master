package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// apiCall is one request to the API (from evcc or anything else but the web UI).
type apiCall struct {
	Seq        uint64    `json:"seq"`
	Time       time.Time `json:"time"`
	Method     string    `json:"method"`
	URI        string    `json:"uri"`
	Remote     string    `json:"remote"`
	Kind       string    `json:"kind"` // state, node, enable, current, phases, health, sim, other
	Node       int       `json:"node,omitempty"`
	Status     int       `json:"status"`
	DurationMs float64   `json:"durationMs"`
	Response   string    `json:"response,omitempty"` // body, compacted, truncated
}

// evccCommand is the last request of one kind for a node.
type evccCommand struct {
	Time   time.Time `json:"time"`
	Value  string    `json:"value,omitempty"`
	Status int       `json:"status"`
	Error  string    `json:"error,omitempty"`
	Remote string    `json:"remote"`
}

// evccNode is what evcc last did with a node.
type evccNode struct {
	Read       *evccCommand `json:"read,omitempty"`    // GET /api/nodes/{id} (status, enabled, meter)
	Enable     *evccCommand `json:"enable,omitempty"`  // POST enable
	Current    *evccCommand `json:"current,omitempty"` // POST current
	Phases     *evccCommand `json:"phases,omitempty"`  // POST phases
	PhasesRead *evccCommand `json:"phasesRead,omitempty"`
}

// activity records the API requests for the web UI.
type activity struct {
	mu        sync.Mutex
	calls     []apiCall // ring
	next      int
	seq       uint64
	counts    map[string]uint64 // per kind
	errors    uint64            // responses >= 400
	nodes     map[int]*evccNode
	stateRead *evccCommand // GET /api/state (evcc grid meter)
	last      *apiCall
}

const (
	activitySize     = 300
	maxResponseBytes = 4096
)

func newActivity() *activity {
	return &activity{
		calls:  make([]apiCall, 0, activitySize),
		counts: map[string]uint64{},
		nodes:  map[int]*evccNode{},
	}
}

// uiPath reports whether a request belongs to the web UI itself, which is not recorded.
func uiPath(p string) bool {
	return p == "/" || p == "/favicon.ico" || strings.HasPrefix(p, "/ui") || strings.HasPrefix(p, "/api/ui")
}

// classify returns the kind of an API request and its node id, if any.
func classify(method, path string) (string, int) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case path == "/api/state":
		return "state", 0
	case path == "/healthz":
		return "health", 0
	case len(parts) >= 2 && parts[0] == "api" && parts[1] == "sim":
		return "sim", 0
	case len(parts) >= 3 && parts[0] == "api" && parts[1] == "nodes":
		id, _ := strconv.Atoi(parts[2])
		if len(parts) == 3 {
			return "node", id
		}
		switch parts[3] {
		case "enable", "current":
			return parts[3], id
		case "phases":
			if method == http.MethodGet {
				return "phasesRead", id
			}
			return "phases", id
		}
		return "other", id
	}
	return "other", 0
}

func (a *activity) record(c apiCall, value, errText string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	c.Seq = a.seq
	a.counts[c.Kind]++
	if c.Status >= 400 {
		a.errors++
	}
	if len(a.calls) < cap(a.calls) {
		a.calls = append(a.calls, c)
	} else {
		a.calls[a.next] = c
		a.next = (a.next + 1) % len(a.calls)
	}
	last := c
	a.last = &last

	cmd := &evccCommand{Time: c.Time, Value: value, Status: c.Status, Error: errText, Remote: c.Remote}
	if c.Kind == "state" {
		a.stateRead = cmd
		return
	}
	if c.Node == 0 {
		return
	}
	n := a.nodes[c.Node]
	if n == nil {
		n = &evccNode{}
		a.nodes[c.Node] = n
	}
	switch c.Kind {
	case "node":
		n.Read = cmd
	case "enable":
		n.Enable = cmd
	case "current":
		n.Current = cmd
	case "phases":
		n.Phases = cmd
	case "phasesRead":
		n.PhasesRead = cmd
	}
}

// activityInfo is the API activity as the web UI gets it.
type activityInfo struct {
	Total     uint64            `json:"total"`
	Errors    uint64            `json:"errors"`
	Counts    map[string]uint64 `json:"counts"`
	Last      *apiCall          `json:"last,omitempty"`
	StateRead *evccCommand      `json:"stateRead,omitempty"`
	Nodes     map[int]*evccNode `json:"nodes"`
	Calls     []apiCall         `json:"calls"` // newest first
}

func (a *activity) info() activityInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := activityInfo{
		Total: a.seq, Errors: a.errors, Counts: map[string]uint64{}, Nodes: map[int]*evccNode{},
		Last: a.last, StateRead: a.stateRead, Calls: make([]apiCall, 0, len(a.calls)),
	}
	for k, v := range a.counts {
		out.Counts[k] = v
	}
	for id, n := range a.nodes {
		c := *n
		out.Nodes[id] = &c
	}
	for i := len(a.calls) - 1; i >= 0; i-- {
		out.Calls = append(out.Calls, a.calls[(a.next+i)%len(a.calls)])
	}
	return out
}

// recorder captures the status and the start of the body of a response.
type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if room := maxResponseBytes + 1 - r.body.Len(); room > 0 {
		r.body.Write(p[:min(len(p), room)])
	}
	return r.ResponseWriter.Write(p)
}

// response returns the body for display: compact JSON or trimmed text.
func (r *recorder) response() string {
	b := r.body.Bytes()
	truncated := len(b) > maxResponseBytes
	if !truncated {
		var c bytes.Buffer
		if json.Compact(&c, b) == nil {
			return c.String()
		}
	}
	s := strings.TrimSpace(string(b[:min(len(b), maxResponseBytes)]))
	if truncated {
		s += " …"
	}
	return s
}

// middleware records every request except the web UI's own.
func (a *activity) middleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if uiPath(r.URL.Path) {
			h.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &recorder{ResponseWriter: w}
		h.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		kind, id := classify(r.Method, r.URL.Path)
		c := apiCall{
			Time: start, Method: r.Method, URI: r.URL.RequestURI(), Remote: remote(r),
			Kind: kind, Node: id, Status: rec.status,
			DurationMs: float64(time.Since(start).Microseconds()) / 1000,
			Response:   rec.response(),
		}
		var errText string
		if rec.status >= 400 {
			errText = strings.TrimSpace(rec.body.String())
		}
		a.record(c, r.URL.Query().Get("value"), errText)
	})
}
