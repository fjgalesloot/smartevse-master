package smartevse

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestDecodeSensorboxSettings(t *testing.T) {
	// GET /settings of a Sensorbox-2 (docs/REST_API.md), with currents
	body := `{"version":"09:09:41 @Oct  3 2024","serialnr":101110,"smartevse_host":"",` +
		`"settings":{"mains_meter":"Sensorbox"},"evse":{"temp":0},` +
		`"phase_currents":{"TOTAL":21,"L1":12,"L2":-3,"L3":12,"last_data_update":1727961993}}`
	stamp, m, err := decodeSensorboxSettings([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if stamp != 1727961993 || m.L != [3]int{12, -3, 12} || m.W != [3]int{276, -69, 276} || m.Source != "REST" {
		t.Errorf("got stamp %d mains %+v", stamp, m)
	}

	for _, bad := range []string{`{}`, `{"phase_currents":{"L1":2500,"L2":0,"L3":0}}`, `not json`} {
		if _, _, err := decodeSensorboxSettings([]byte(bad)); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}

// Only a changed stamp is a new measurement: the first answer may hold
// values frozen long ago.
func TestSensorboxWebNeedsNewStamp(t *testing.T) {
	sim := NewSim(1)
	sim.House = [3]float64{5, 6, 7}
	sim.Advance(time.Second)
	w := newSensorboxWeb("sensorbox", slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.client.Transport = sim

	w.poll(sim.Now())
	if _, _, ok := w.latest(); ok {
		t.Fatal("first answer used")
	}
	w.poll(sim.Now())
	if _, _, ok := w.latest(); ok {
		t.Fatal("unchanged stamp used")
	}

	sim.Advance(time.Second)
	now := sim.Now()
	w.poll(now)
	m, at, ok := w.latest()
	if !ok || m.L != [3]int{50, 60, 70} || !at.Equal(now.Add(-sensorboxWebLag)) {
		t.Fatalf("got %v at %v (%v), want new measurement", m.L, at, ok)
	}

	sim.SensorboxP1Lost = true
	sim.Advance(5 * time.Second)
	w.poll(sim.Now())
	if _, at2, _ := w.latest(); !at2.Equal(at) {
		t.Errorf("frozen stamp gave a new measurement")
	}
}

func TestSensorboxWebNotWithCT200(t *testing.T) {
	cfg := Config{MaxMains: 25, Nodes: []NodeConfig{{ID: 1}}, CT200A: true, SensorboxHost: "sb"}
	if _, err := NewMaster(NewSimBus(NewSim(1)), cfg, nil); err == nil {
		t.Error("no error for web api with 200A CTs")
	}
}
