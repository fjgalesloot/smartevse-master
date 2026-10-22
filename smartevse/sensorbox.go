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
	Source string // "P1" or "CT"
}

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
//	0x0008..0x000D   P1 currents L1-L3, float32 big-endian, A
//	0x000E..0x0013   CT currents L1-L3, float32 big-endian, A
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
		f := math.Float32frombits(binary.BigEndian.Uint32(data[off+i*4:]))
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) || math.Abs(float64(f)) > 200 {
			return Mains{}, fmt.Errorf("sensorbox: implausible L%d value %v", i+1, f)
		}
		v := int(f * 10)
		if m.Source == "CT" && ct200A {
			v *= 2
		}
		m.L[i] = v
	}
	return m, nil
}
