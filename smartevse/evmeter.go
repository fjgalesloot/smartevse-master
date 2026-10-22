package smartevse

import (
	"encoding/binary"
	"fmt"
	"math"
)

// EV meter types: the value of the node's "EV Meter" setting (reg 0x0108),
// i.e. the row in the firmware's EMConfig table (meter.cpp, meter.h EM_*).
const (
	MeterDisabled     = 0
	MeterSensorbox    = 1
	MeterPhoenix      = 2
	MeterFinder7E     = 3
	MeterEastron3P    = 4
	MeterEastron3PInv = 5
	MeterABB          = 6
	MeterSolarEdge    = 7
	MeterWAGO         = 8
	MeterAPI          = 9
	MeterEastron1P    = 10
	MeterFinder7M     = 11
	MeterSinotimer    = 12
	MeterHomeWizard   = 13
	MeterSchneider    = 14
	MeterChint3P      = 15
	MeterGavazzi      = 16
	MeterChint1P      = 17
	MeterCustom       = 19
)

// Byte order of 32-bit values (combineBytes in meter.cpp).
const (
	lbfLWF = iota // low byte first, low word first (little endian)
	lbfHWF        // low byte first, high word first
	hbfLWF        // high byte first, low word first
	hbfHWF        // high byte first, high word first (big endian)
)

// Data types (MBDataType in meter.h).
const (
	dtInt32 = iota
	dtFloat32
	dtInt16
)

// meterSpec is one row of EMConfig plus the per-type cases of
// requestCurrentMeasurement / receiveCurrentMeasurement.
type meterSpec struct {
	typ    int // set by lookupMeter
	name   string
	endian int
	fc     byte
	dt     int
	iReg   uint16
	iDiv   int // 10^x
	pReg   uint16
	pDiv   int
	eReg   uint16 // total import energy
	eDiv   int
	iRegs  uint16 // registers read for the current measurement
	pOff   int    // value index of the phase powers in the current response, 0 = none
	single bool   // single-phase meter: one current (and power) only
}

// meterSpecs holds the meters that can serve as EV meter on the bus. Not
// listed: Sensorbox (mains only), SolarEdge (inverter), API and HomeWizard (not
// on the bus) and Custom (its registers are configured on the node itself).
var meterSpecs = map[int]meterSpec{
	MeterPhoenix:      {0, "Phoenix C", hbfLWF, 4, dtInt32, 0x000C, 3, 0x0028, 1, 0x003E, 1, 6, 0, false},
	MeterFinder7E:     {0, "Finder 7E", hbfHWF, 4, dtFloat32, 0x100E, 0, 0x1026, 0, 0x1106, 3, 6, 0, false},
	MeterEastron3P:    {0, "Eastron3P", hbfHWF, 4, dtFloat32, 0x0006, 0, 0x0034, 0, 0x0048, 0, 12, 3, false},
	MeterEastron3PInv: {0, "InvEastrn", hbfHWF, 4, dtFloat32, 0x0006, 0, 0x0034, 0, 0x004A, 0, 12, 3, false}, // mounted reversed: import is the export register
	MeterABB:          {0, "ABB", hbfHWF, 3, dtInt32, 0x5B0C, 2, 0x5B14, 2, 0x5000, 2, 16, 5, false},
	MeterWAGO:         {0, "WAGO", hbfHWF, 3, dtFloat32, 0x500C, 0, 0x5012, -3, 0x600C, 0, 6, 0, false},
	MeterEastron1P:    {0, "Eastron1P", hbfHWF, 4, dtFloat32, 0x0006, 0, 0x000C, 0, 0x0048, 0, 12, 3, false},
	MeterFinder7M:     {0, "Finder 7M", hbfHWF, 4, dtFloat32, 2516, 0, 2536, 0, 2638, 3, 20, 7, false},
	MeterSinotimer:    {0, "Sinotimer", hbfHWF, 4, dtInt16, 0x0003, 2, 0x0008, 0, 0x0027, 2, 3, 0, false},
	MeterSchneider:    {0, "Schneider", hbfHWF, 3, dtFloat32, 0x0BB7, 0, 0x0BF3, -3, 0xB02B, 0, 60, 27, false},
	MeterChint3P:      {0, "Chint 3P", hbfHWF, 3, dtFloat32, 0x200C, 3, 0x2012, 1, 0x101E, 0, 14, 4, false},
	MeterGavazzi:      {0, "C.Gavazzi", hbfLWF, 4, dtInt32, 0x000C, 3, 0x0028, 1, 0x0034, 1, 6, 0, false},
	MeterChint1P:      {0, "Chint 1P", hbfHWF, 3, dtFloat32, 0x2002, 0, 0x2004, -3, 0x4000, 0, 4, 1, true},
}

// lookupMeter returns the spec of a supported EV meter type.
func lookupMeter(t int) (meterSpec, bool) {
	s, ok := meterSpecs[t]
	s.typ = t
	return s, ok
}

// MeterName returns the display name of an EV meter type, as on the SmartEVSE.
func MeterName(t int) string {
	if s, ok := meterSpecs[t]; ok {
		return s.name
	}
	names := map[int]string{
		MeterDisabled: "Disabled", MeterSensorbox: "Sensorbox", MeterSolarEdge: "SolarEdge",
		MeterAPI: "API", MeterHomeWizard: "Homewizrd", MeterCustom: "Custom",
	}
	if n, ok := names[t]; ok {
		return n
	}
	return fmt.Sprintf("type %d", t)
}

// meterRead is one Modbus read of an EV meter.
type meterRead struct {
	fc    byte
	reg   uint16
	count uint16
}

// currentRead is requestCurrentMeasurement: the register a node recognises the
// answer by is iReg, so this must match the firmware exactly.
func (s meterSpec) currentRead() meterRead { return meterRead{s.fc, s.iReg, s.iRegs} }

// energyRead is requestEnergyMeasurement(Export = false). ABB stores energy as
// 64 bit, Sinotimer as 32 bit despite its 16-bit registers.
func (s meterSpec) energyRead() meterRead {
	if s.typ == MeterABB {
		return meterRead{s.fc, s.eReg, 4}
	}
	return meterRead{s.fc, s.eReg, 2}
}

// powerRead is requestPowerMeasurement; ok is false for meters whose current
// response already holds the phase powers (the node takes power from there).
func (s meterSpec) powerRead() (r meterRead, ok bool) {
	if s.pOff > 0 {
		return meterRead{}, false
	}
	if s.dt == dtInt16 { // Sinotimer: three phase powers, no total
		return meterRead{s.fc, s.pReg, 3}, true
	}
	return meterRead{s.fc, s.pReg, 2}, true
}

// value decodes the idx-th value of size dt and divides it by 10^div
// (decodeMeasurement in meter.cpp, including float32 arithmetic and truncation).
func (s meterSpec) value(buf []byte, idx, dt, div int) (int, error) {
	size := 4
	if dt == dtInt16 {
		size = 2
	}
	off := idx * size
	if off+size > len(buf) {
		return 0, fmt.Errorf("short meter response (%d bytes, need %d)", len(buf), off+size)
	}
	b := buf[off : off+size]

	var w uint32
	switch {
	case size == 2 && (s.endian == hbfLWF || s.endian == hbfHWF):
		w = uint32(binary.BigEndian.Uint16(b))
	case size == 2:
		w = uint32(binary.LittleEndian.Uint16(b))
	case s.endian == lbfLWF:
		w = binary.LittleEndian.Uint32(b)
	case s.endian == lbfHWF:
		w = uint32(b[2]) | uint32(b[3])<<8 | uint32(b[0])<<16 | uint32(b[1])<<24
	case s.endian == hbfLWF:
		w = uint32(b[1]) | uint32(b[0])<<8 | uint32(b[3])<<16 | uint32(b[2])<<24
	default:
		w = binary.BigEndian.Uint32(b)
	}

	pow := int32(math.Pow10(abs(div)))
	if dt == dtFloat32 {
		f := math.Float32frombits(w)
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return 0, fmt.Errorf("meter value is %v", f)
		}
		if div >= 0 {
			return int(f / float32(pow)), nil
		}
		return int(f * float32(pow)), nil
	}
	v := int32(w)
	if dt == dtInt16 {
		v = int32(int16(w))
	}
	if div >= 0 {
		return int(v / pow), nil
	}
	return int(v * pow), nil
}

// decodeCurrents is receiveCurrentMeasurement for an EV meter: currents in
// 0.1 A, negative when the phase power is negative, and the total power in W
// when the response holds the phase powers (hasPower).
func (s meterSpec) decodeCurrents(buf []byte) (currents [3]int, power int, hasPower bool, err error) {
	phases := 3
	if s.single {
		phases = 1
	}
	var ma [3]int
	for x := 0; x < phases; x++ {
		if ma[x], err = s.value(buf, x, s.dt, s.iDiv-3); err != nil {
			return
		}
	}
	if s.pOff > 0 {
		hasPower = true
		for x := 0; x < phases; x++ {
			var p int
			if p, err = s.value(buf, x+s.pOff, s.dt, s.pDiv); err != nil {
				return
			}
			if s.typ == MeterEastron3PInv {
				p = -p
			}
			power += p
			if p < 0 {
				ma[x] = -ma[x]
			}
		}
	}
	for x := range ma {
		currents[x] = ma[x] / 100 // mA to 0.1 A
	}
	return
}

// decodeEnergy is receiveEnergyMeasurement: Wh.
func (s meterSpec) decodeEnergy(buf []byte) (int, error) {
	switch {
	case s.typ == MeterABB: // uint64: skip the high word
		return s.value(buf, 1, dtInt32, s.eDiv-3)
	case s.dt == dtInt16: // Sinotimer: 32 bit
		return s.value(buf, 0, dtInt32, s.eDiv-3)
	default:
		return s.value(buf, 0, s.dt, s.eDiv-3)
	}
}

// decodePower is receivePowerMeasurement: W.
func (s meterSpec) decodePower(buf []byte) (int, error) {
	if s.dt != dtInt16 {
		p, err := s.value(buf, 0, s.dt, s.pDiv)
		if s.typ == MeterEastron3PInv {
			p = -p
		}
		return p, err
	}
	sum := 0 // Sinotimer: sum of the phase powers
	for x := 0; x < 3; x++ {
		p, err := s.value(buf, x, s.dt, s.pDiv)
		if err != nil {
			return 0, err
		}
		sum += p
	}
	return sum, nil
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
