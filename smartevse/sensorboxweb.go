package smartevse

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// The Sensorbox-2 ESP32 also serves its mains currents over WiFi
// (Sensorbox2_ESP main.cpp handle_URI, GET /settings):
//
//	"phase_currents": {"TOTAL": 43, "L1": 12, "L2": 2, "L3": 29, "last_data_update": 1761470000}
//
// L1-L3 are MainsMeterIrms in 0.1 A (P1 if present, else the CTs, as on
// Modbus; positive = import), updated every ~2 s by Timer2S while the
// Sensorbox has valid data. last_data_update is time(NULL) at the last P1
// telegram or CT measurement: it stops changing when the Sensorbox loses its
// meter, while L1-L3 keep their last values. Its clock may not be set (no NTP),
// so only a change of the stamp counts as a new measurement.
//
// The master uses this when the Sensorbox does not answer on the bus, e.g.
// with a broken RS485 transceiver while the ESP32 keeps running.

// sensorboxWebPoll is how often the REST API is read.
const sensorboxWebPoll = time.Second

// sensorboxWebLag is how much older a REST reading can be than the moment its
// stamp was seen to change: Timer2S copies the measurement every 2 s.
const sensorboxWebLag = 2 * time.Second

// sensorboxWeb reads the Sensorbox-2 REST API in the background and keeps the
// last new measurement.
type sensorboxWeb struct {
	url    string
	client *http.Client
	log    *slog.Logger

	mu     sync.Mutex
	stamp  int64 // last_data_update of the last answer
	seen   bool  // stamp is set: the first answer only sets the baseline
	mains  Mains
	at     time.Time // when mains was measured, at the latest; zero = none
	failed bool      // last request failed (logged once)
}

func newSensorboxWeb(host string, log *slog.Logger) *sensorboxWeb {
	return &sensorboxWeb{
		url:    "http://" + host + "/settings",
		client: &http.Client{Timeout: 1500 * time.Millisecond},
		log:    log,
	}
}

// run polls until ctx is done.
func (w *sensorboxWeb) run(ctx context.Context, now func() time.Time) {
	t := time.NewTicker(sensorboxWebPoll)
	defer t.Stop()
	for {
		w.poll(now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// sensorboxSettings is the part of GET /settings used here.
type sensorboxSettings struct {
	PhaseCurrents *struct {
		L1, L2, L3     int
		LastDataUpdate int64 `json:"last_data_update"`
	} `json:"phase_currents"`
}

// poll reads the REST API once; now is the time of the request.
func (w *sensorboxWeb) poll(now time.Time) {
	stamp, mains, err := w.read()

	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		if !w.failed {
			w.log.Warn("sensorbox web api failed", "url", w.url, "err", err)
		}
		w.failed = true
		return
	}
	if w.failed {
		w.log.Info("sensorbox web api answering", "url", w.url)
	}
	w.failed = false
	if w.seen && stamp != w.stamp {
		w.mains, w.at = mains, now.Add(-sensorboxWebLag)
	}
	w.stamp, w.seen = stamp, true
}

func (w *sensorboxWeb) read() (int64, Mains, error) {
	req, err := http.NewRequest(http.MethodGet, w.url, nil)
	if err != nil {
		return 0, Mains{}, err
	}
	req.Close = true // the ESP32 web server has few connections
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, Mains{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, Mains{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, Mains{}, fmt.Errorf("GET %s: %s", w.url, resp.Status)
	}
	return decodeSensorboxSettings(body)
}

func decodeSensorboxSettings(body []byte) (int64, Mains, error) {
	var s sensorboxSettings
	if err := json.Unmarshal(body, &s); err != nil {
		return 0, Mains{}, fmt.Errorf("sensorbox settings: %w", err)
	}
	pc := s.PhaseCurrents
	if pc == nil {
		return 0, Mains{}, fmt.Errorf("sensorbox settings: no phase_currents")
	}
	m := Mains{L: [3]int{pc.L1, pc.L2, pc.L3}, Source: "REST"}
	for i, l := range m.L {
		if l < -2000 || l > 2000 {
			return 0, Mains{}, fmt.Errorf("sensorbox settings: implausible L%d value %.1f A", i+1, float64(l)/10)
		}
		// no voltages over REST
		m.W[i] = l * nominalVolts / 10
	}
	return pc.LastDataUpdate, m, nil
}

// latest returns the last new measurement and when it was taken (at the latest).
func (w *sensorboxWeb) latest() (Mains, time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.mains, w.at, !w.at.IsZero()
}
