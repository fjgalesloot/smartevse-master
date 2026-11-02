package main

import (
	_ "embed"
	"net/http"
	"strconv"
	"time"

	"github.com/fjgalesloot/smartevse-master/smartevse"
)

// The web UI is one static page that polls /api/ui and /api/ui/logs.
//
//go:embed ui/index.html
var uiPage []byte

func (a *api) uiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", a.uiIndex)
	mux.HandleFunc("GET /ui", a.uiIndex)
	mux.HandleFunc("GET /api/ui", a.uiState)
	mux.HandleFunc("GET /api/ui/logs", a.uiLogs)
}

func (a *api) uiIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(uiPage)
}

// uiInfo is everything the web UI shows except the log.
type uiInfo struct {
	Version   string             `json:"version"`
	Now       time.Time          `json:"now"`
	Started   time.Time          `json:"started"`
	Uptime    float64            `json:"uptimeSeconds"`
	Sim       bool               `json:"sim"`
	Healthy   bool               `json:"healthy"`
	Health    string             `json:"health"`
	Interval  float64            `json:"intervalSeconds"`
	Settings  [][2]string        `json:"settings"`
	State     smartevse.Snapshot `json:"state"`
	Evcc      activityInfo       `json:"evcc"`
	SimState  *simInfo           `json:"simState,omitempty"`
	LogSeq    uint64             `json:"logSeq"` // newest log entry
	LogBuffer int                `json:"logBuffer"`
}

func (a *api) uiState(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	s := a.m.Snapshot()
	info := uiInfo{
		Version: buildVersion(), Now: now, Started: a.started, Uptime: now.Sub(a.started).Seconds(),
		Sim: a.sim != nil, Interval: smartevse.Interval.Seconds(), Settings: a.settings,
		State: s, Evcc: a.act.info(),
	}
	info.Health = unhealthy(s)
	info.Healthy = info.Health == ""
	if info.Healthy {
		info.Health = "ok"
	}
	if a.sim != nil {
		si := a.simInfo()
		info.SimState = &si
	}
	if a.logs != nil {
		info.LogSeq, info.LogBuffer = a.logs.last()
	}
	writeJSON(w, info)
}

// uiLogs returns log entries newer than ?after= (sequence number), at most ?limit=.
func (a *api) uiLogs(w http.ResponseWriter, r *http.Request) {
	entries := []logEntry{}
	if a.logs != nil {
		after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || limit <= 0 {
			limit = 1000
		}
		entries = a.logs.since(after, limit)
	}
	writeJSON(w, entries)
}
