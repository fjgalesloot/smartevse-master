package smartevse

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The contactor 2 setting (1P/3P) of a node can only be changed through the
// node's WiFi REST API (esp32.cpp, POST /settings?enable_C2=): Modbus cannot
// reach it, MENU_C2 lies outside the registers mapModbusRegister2ItemID maps.
// The node applies it when it next enters state C (setState STATE_C).

// EnableC2 values (main.h EnableC2_t), in the order of StrEnableC2.
const (
	C2NotPresent = 0
	C2AlwaysOff  = 1 // single phase
	C2SolarOff   = 2 // single phase in Solar mode only; nodes here run Smart
	C2AlwaysOn   = 3 // three phase
	C2Auto       = 4 // not supported on nodes (validate_settings)
)

var c2Names = []string{"Not present", "Always Off", "Solar Off", "Always On", "Auto"}

// c2Phases returns the phases a node in Smart mode charges with.
func c2Phases(c2 int) int {
	if c2 == C2AlwaysOff {
		return 1
	}
	return 3
}

// nodeWeb talks to the WiFi REST API of the nodes.
type nodeWeb struct {
	client *http.Client
	tries  int
	pause  time.Duration
}

func newNodeWeb() *nodeWeb {
	return &nodeWeb{client: &http.Client{Timeout: 3 * time.Second}, tries: 4, pause: time.Second}
}

// errBusy is the node's answer when more than 4 HTTP connections are open
// (MAX_HTTP_CONNECTIONS in network_common.cpp: 503 "Server busy, retry shortly").
var errBusy = errors.New("node web server busy")

// do sends one request and retries while the node is busy. Every request
// closes its connection, so this client never holds one of the node's 4.
func (w *nodeWeb) do(method, url string) ([]byte, error) {
	var err error
	for try := 0; try < w.tries; try++ {
		if try > 0 {
			time.Sleep(w.pause)
		}
		var body []byte
		if body, err = w.once(method, url); !errors.Is(err, errBusy) {
			return body, err
		}
	}
	return nil, err
}

func (w *nodeWeb) once(method, url string) ([]byte, error) {
	// The node's web server (mongoose) needs an empty body on a POST, as evcc's
	// smart-evse driver sends.
	req, err := http.NewRequest(method, url, strings.NewReader(""))
	if err != nil {
		return nil, err
	}
	req.Close = true
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == http.StatusServiceUnavailable:
		return nil, errBusy
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s %s: %s", method, url, resp.Status)
	}
	return body, err
}

// nodeSettings is the part of GET /settings used here.
type nodeSettings struct {
	Settings struct {
		EnableC2 string `json:"enable_C2"`
	} `json:"settings"`
}

func (s nodeSettings) c2() (int, error) {
	for i, name := range c2Names {
		if s.Settings.EnableC2 == name {
			return i, nil
		}
	}
	return 0, fmt.Errorf("unknown enable_C2 %q", s.Settings.EnableC2)
}

// enableC2 reads the contactor 2 setting of the node at host.
func (w *nodeWeb) enableC2(host string) (int, error) {
	body, err := w.do(http.MethodGet, "http://"+host+"/settings")
	if err != nil {
		return 0, err
	}
	var s nodeSettings
	if err := json.Unmarshal(body, &s); err != nil {
		return 0, fmt.Errorf("settings: %w", err)
	}
	return s.c2()
}

// setEnableC2 changes the contactor 2 setting of the node at host. The node
// answers with the new setting.
func (w *nodeWeb) setEnableC2(host string, c2 int) error {
	body, err := w.do(http.MethodPost, fmt.Sprintf("http://%s/settings?enable_C2=%d", host, c2))
	if err != nil {
		return err
	}
	var s nodeSettings
	if err := json.Unmarshal(body, &s); err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	if got, err := s.c2(); err != nil || got != c2 {
		return fmt.Errorf("node answered enable_C2 %q, want %q", s.Settings.EnableC2, c2Names[c2])
	}
	return nil
}
