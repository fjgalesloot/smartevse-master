package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLogRing(t *testing.T) {
	ring := newLogRing(3)
	var buf strings.Builder
	log := slog.New(teeHandler{
		newLogHandler(&buf, slog.LevelInfo),
		&ringHandler{ring: ring, level: slog.LevelDebug},
	}).With("component", "test")

	log.Debug("first-debug")
	log.Info("two", "node", 2, "err", errors.New("bad thing"))
	log.WithGroup("g").Warn("three", "x", 1.5)
	log.Info("four")

	if strings.Contains(buf.String(), "first-debug") {
		t.Errorf("debug line reached the info-level output: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "two") {
		t.Errorf("info line missing from the output: %s", buf.String())
	}

	got := ring.since(0, 0)
	if len(got) != 3 || got[0].Msg != "two" || got[2].Msg != "four" || got[2].Seq != 4 {
		t.Fatalf("ring keeps the last 3 in order: %+v", got)
	}
	if got[0].Node != "2" || got[0].Level != "INFO" {
		t.Errorf("node/level: %+v", got[0])
	}
	want := [][2]string{{"component", "test"}, {"err", "bad thing"}}
	if len(got[0].Attrs) != 2 || got[0].Attrs[0] != want[0] || got[0].Attrs[1] != want[1] {
		t.Errorf("attrs: %v", got[0].Attrs)
	}
	if a := got[1].Attrs; len(a) != 2 || a[1] != [2]string{"g.x", "1.5"} {
		t.Errorf("group attrs: %v", a)
	}
	if after := ring.since(3, 0); len(after) != 1 || after[0].Msg != "four" {
		t.Errorf("since 3: %+v", after)
	}
	if lim := ring.since(0, 1); len(lim) != 1 || lim[0].Msg != "four" {
		t.Errorf("limit keeps the newest: %+v", lim)
	}
}

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		method, path, kind string
		node               int
	}{
		{"GET", "/api/state", "state", 0},
		{"GET", "/healthz", "health", 0},
		{"GET", "/api/nodes/2", "node", 2},
		{"POST", "/api/nodes/1/enable", "enable", 1},
		{"POST", "/api/nodes/1/current", "current", 1},
		{"GET", "/api/nodes/1/phases", "phasesRead", 1},
		{"POST", "/api/nodes/1/phases", "phases", 1},
		{"POST", "/api/sim/house", "sim", 0},
		{"GET", "/nope", "other", 0},
	} {
		kind, node := classify(c.method, c.path)
		if kind != c.kind || node != c.node {
			t.Errorf("%s %s: got %s/%d, want %s/%d", c.method, c.path, kind, node, c.kind, c.node)
		}
	}
}

func TestUIActivity(t *testing.T) {
	h, _ := newTestAPI(t)
	call := func(method, uri string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, uri, nil))
		return rec
	}
	call("GET", "/api/state")
	call("POST", "/api/nodes/2/enable?value=true")
	call("POST", "/api/nodes/2/current?value=8.5")
	call("POST", "/api/nodes/2/current?value=x")
	call("GET", "/api/nodes/1")

	if rec := call("GET", "/"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "<title>SmartEVSE master</title>") {
		t.Fatalf("index: %d", rec.Code)
	}
	if rec := call("GET", "/nope"); rec.Code != 404 {
		t.Errorf("unknown path: %d", rec.Code)
	}

	rec := call("GET", "/api/ui")
	if rec.Code != 200 {
		t.Fatalf("api/ui: %d %s", rec.Code, rec.Body)
	}
	var info uiInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	e := info.Evcc
	// the UI's own requests are not recorded, the 404 is
	if e.Total != 6 || e.Errors != 2 || len(e.Calls) != 6 || e.Calls[0].URI != "/nope" {
		t.Fatalf("activity: total %d errors %d calls %+v", e.Total, e.Errors, e.Calls)
	}
	if e.StateRead == nil || e.Last == nil || e.Last.Status != 404 {
		t.Errorf("state read / last: %+v %+v", e.StateRead, e.Last)
	}
	n := e.Nodes[2]
	if n == nil || n.Enable == nil || n.Enable.Value != "true" || n.Current == nil || n.Current.Value != "x" ||
		n.Current.Status != http.StatusBadRequest || n.Current.Error == "" {
		t.Fatalf("node 2: %+v", n)
	}
	if e.Nodes[1] == nil || e.Nodes[1].Read == nil || !strings.Contains(e.Calls[1].Response, `"id":1`) {
		t.Errorf("node 1 read: %+v %q", e.Nodes[1], e.Calls[1].Response)
	}
	if !info.Sim || info.SimState == nil || len(info.State.Nodes) != 2 {
		t.Errorf("sim/state: %+v", info)
	}

	rec = call("GET", "/api/ui/logs")
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != 200 || strings.TrimSpace(string(body)) != "[]" {
		t.Errorf("logs without ring: %d %s", rec.Code, body)
	}
}
