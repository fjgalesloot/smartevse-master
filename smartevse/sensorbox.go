package smartevse

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Mains holds per-phase mains currents in 0.1 A (positive = import).
type Mains struct {
	L      [3]int
	W      [3]int     // power per phase in W (positive = import)
	V      [3]float64 // P1 voltage per phase, 0 = not available
	Source string     // "P1" or "CT"
}

// PowerW is the total mains power in W (positive = import).
func (m Mains) PowerW() int {
	return m.W[0] + m.W[1] + m.W[2]
}

// nominalVolts converts currents to power when the Sensorbox has no
// plausible voltage (CT mode, meter without voltage per phase).
const nominalVolts = 230

// Imeasured is the highest phase current, as the firmware uses for MaxMains.
func (m Mains) Imeasured() int {
	max := m.L[0]
	for _, v := range m.L[1:] {
		if v > max {
			max = v
		}
	}
	return max
}

// ReadSensorbox reads the mains currents from a Sensorbox-2 (address 0x0A).
//
// Register map (Sensorbox-2 firmware, main.cpp header):
//
//	0x0001 low byte: 0x80 = P1 data present, 0x03 = CTs used
//	0x0002..0x0007   P1 voltages L1-L3, float32 big-endian, V
//	0x0008..0x000D   P1 currents L1-L3, float32 big-endian, A
//	0x000E..0x0013   CT currents L1-L3, float32 big-endian, A
//
// The Sensorbox has no power registers. Its P1 current is the meter's net
// power per phase divided by the meter's voltage (21.7.0 - 22.7.0) / 32.7.0,
// so voltage × current gives that power back exactly. CT currents are real
// power divided by a fixed nominal voltage, so × 230 V is their real power.
//
// ct200A doubles CT readings for 200A:50mA clamps, as the firmware does when MaxMains > 100.
func ReadSensorbox(b *Bus, ct200A bool) (Mains, error) {
	data, err := b.ReadInput(SensorboxAddr, 0x0000, 20)
	if err != nil {
		return Mains{}, err
	}
	return decodeSensorbox(data, ct200A)
}

func decodeSensorbox(data []byte, ct200A bool) (Mains, error) {
	if len(data) < 40 {
		return Mains{}, fmt.Errorf("sensorbox: short response (%d bytes)", len(data))
	}
	flags := data[3]
	if flags == 0 {
		return Mains{}, errors.New("sensorbox: no P1 or CT measurement available")
	}

	m := Mains{Source: "CT"}
	off := 28 // register 0x000E
	if flags&0x80 != 0 {
		m.Source = "P1"
		off = 16 // register 0x0008
	}

	for i := 0; i < 3; i++ {
		f := float32At(data, off+i*4)
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) || math.Abs(float64(f)) > 200 {
			return Mains{}, fmt.Errorf("sensorbox: implausible L%d value %v", i+1, f)
		}
		if m.Source == "CT" && ct200A {
			f *= 2
		}
		m.L[i] = int(f * 10)

		volts := float64(nominalVolts)
		if m.Source == "P1" {
			// The firmware reports 1 V for a phase the meter sends no voltage for.
			if v := float64(float32At(data, 4+i*4)); v >= 180 && v <= 270 {
				m.V[i] = math.Round(v*10) / 10
				volts = v
			}
		}
		m.W[i] = int(math.Round(float64(f) * volts))
	}
	return m, nil
}

func float32At(data []byte, off int) float32 {
	return math.Float32frombits(binary.BigEndian.Uint32(data[off:]))
}
