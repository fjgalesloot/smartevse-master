package smartevse

import (
	"encoding/binary"
	"math"
	"testing"
)

// sensorboxData builds a 20-register Sensorbox answer.
func sensorboxData(flags byte, volts, p1, ct [3]float32) []byte {
	data := make([]byte, 40)
	data[1] = 0x14
	data[2] = 50 // DSMR 5.0
	data[3] = flags
	for i := 0; i < 3; i++ {
		binary.BigEndian.PutUint32(data[4+i*4:], math.Float32bits(volts[i]))
		binary.BigEndian.PutUint32(data[16+i*4:], math.Float32bits(p1[i]))
		binary.BigEndian.PutUint32(data[28+i*4:], math.Float32bits(ct[i]))
	}
	return data
}

func TestDecodeSensorbox(t *testing.T) {
	// P1 currents as the Sensorbox computes them: net phase power / voltage.
	p1 := [3]float32{1234.0 / 231.4, -2000.0 / 229.8, 0}
	ct := [3]float32{3.5, -1.2, 0.4}

	for _, tc := range []struct {
		name   string
		data   []byte
		ct200A bool
		want   Mains
	}{
		{
			name: "P1 with voltages",
			data: sensorboxData(0x80, [3]float32{231.4, 229.8, 232.06}, p1, ct),
			want: Mains{L: [3]int{53, -87, 0}, W: [3]int{1234, -2000, 0}, V: [3]float64{231.4, 229.8, 232.1}, Source: "P1"},
		},
		{
			// The firmware reports 1 V for phases the meter sends no voltage for.
			name: "P1 without voltages",
			data: sensorboxData(0x80, [3]float32{1, 1, 1}, [3]float32{5, -2, 0}, ct),
			want: Mains{L: [3]int{50, -20, 0}, W: [3]int{1150, -460, 0}, Source: "P1"},
		},
		{
			name: "CT ignores voltages",
			data: sensorboxData(0x03, [3]float32{231.4, 229.8, 232.1}, p1, ct),
			want: Mains{L: [3]int{35, -12, 4}, W: [3]int{805, -276, 92}, Source: "CT"},
		},
		{
			name:   "CT 200A",
			data:   sensorboxData(0x03, [3]float32{}, p1, ct),
			ct200A: true,
			want:   Mains{L: [3]int{70, -24, 8}, W: [3]int{1610, -552, 184}, Source: "CT"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeSensorbox(tc.data, tc.ct200A)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDecodeSensorboxErrors(t *testing.T) {
	v := [3]float32{230, 230, 230}
	for name, data := range map[string][]byte{
		"short":       make([]byte, 38),
		"no data":     sensorboxData(0, v, [3]float32{}, [3]float32{}),
		"implausible": sensorboxData(0x80, v, [3]float32{float32(math.NaN()), 0, 0}, [3]float32{}),
	} {
		if _, err := decodeSensorbox(data, false); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
