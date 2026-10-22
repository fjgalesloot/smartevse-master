package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/floris/evse-master/smartevse"
)

func newTestAPI(t *testing.T) (http.Handler, *smartevse.Master) {
	t.Helper()
	sim := smartevse.NewSim(1, 2)
	bus := smartevse.NewBus(sim)
	bus.BroadcastGap = 0
	nodes, err := parseNodes("1:16, 2:10")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := smartevse.NewMaster(bus, smartevse.Config{MaxMains: 25, Nodes: nodes}, log)
	if err != nil {
		t.Fatal(err)
	}
	m.SetClock(sim.Now)
	m.Cycle()
	return newAPI(m, sim, log).routes(), m
}

func do(t *testing.T, h http.Handler, method, uri string) (int, smartevse.NodeInfo) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, uri, nil))
	var info smartevse.NodeInfo
	if rec.Code == http.StatusOK && rec.Header().Get("Content-Type") == "application/json" {
		if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
			t.Fatalf("%s %s: %v", method, uri, err)
		}
	}
	return rec.Code, info
}

// The values below are what evcc's Go templates render for bool, int64 and float64.
func TestAPINodeControl(t *testing.T) {
	h, _ := newTestAPI(t)

	code, info := do(t, h, "GET", "/api/nodes/2")
	if code != 200 || info.Status != "A" || info.Enabled || info.LimitA != 10 {
		t.Fatalf("initial: %d %+v", code, info)
	}
	if code, info = do(t, h, "POST", "/api/nodes/2/enable?value=true"); code != 200 || !info.Enabled {
		t.Fatalf("enable: %d %+v", code, info)
	}
	if code, info = do(t, h, "POST", "/api/nodes/2/current?value=16"); code != 200 || info.LimitA != 16 || info.MaxA != 10 {
		t.Fatalf("current above cap: %d %+v", code, info)
	}
	if code, info = do(t, h, "POST", "/api/nodes/2/current?value=6.5"); code != 200 || info.MaxA != 6.5 {
		t.Fatalf("current millis: %d %+v", code, info)
	}
	if code, info = do(t, h, "POST", "/api/nodes/2/enable?value=false"); code != 200 || info.Enabled || info.MaxA != 0 {
		t.Fatalf("disable: %d %+v", code, info)
	}
}

func TestAPIErrors(t *testing.T) {
	h, _ := newTestAPI(t)
	for _, tc := range []struct {
		method, uri string
		code        int
	}{
		{"GET", "/api/nodes/3", 404},
		{"GET", "/api/nodes/x", 400},
		{"POST", "/api/nodes/1/enable?value=yes", 400},
		{"POST", "/api/nodes/1/current?value=81", 400},
		{"POST", "/api/nodes/1/current", 400},
		{"GET", "/api/nodes/1/enable?value=true", 405},
		{"GET", "/healthz", 200},
	} {
		if code, _ := do(t, h, tc.method, tc.uri); code != tc.code {
			t.Errorf("%s %s: got %d, want %d", tc.method, tc.uri, code, tc.code)
		}
	}
}

func TestParseNodes(t *testing.T) {
	for _, bad := range []string{"1", "a:16", "1:x", "1:16;2:16"} {
		if _, err := parseNodes(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
	nodes, err := parseNodes("1:16,3:32,")
	if err != nil || len(nodes) != 2 || nodes[1] != (smartevse.NodeConfig{ID: 3, MaxA: 32}) {
		t.Fatalf("got %+v, %v", nodes, err)
	}
}
