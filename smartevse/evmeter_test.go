package smartevse

import (
	"encoding/hex"
	"strings"
	"testing"
)

func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func spec(t *testing.T, typ int) meterSpec {
	t.Helper()
	s, ok := lookupMeter(typ)
	if !ok {
		t.Fatalf("type %d not supported", typ)
	}
	return s
}

// The requests must be exactly what the firmware master sends: a node
// recognises the answer by the register of the request it saw.
func TestMeterRequests(t *testing.T) {
	for _, tc := range []struct {
		typ                    int
		current, energy, power meterRead
		hasPower               bool
	}{
		{MeterEastron3P, meterRead{4, 0x0006, 12}, meterRead{4, 0x0048, 2}, meterRead{}, false},
		{MeterEastron3PInv, meterRead{4, 0x0006, 12}, meterRead{4, 0x004A, 2}, meterRead{}, false},
		{MeterEastron1P, meterRead{4, 0x0006, 12}, meterRead{4, 0x0048, 2}, meterRead{}, false},
		{MeterABB, meterRead{3, 0x5B0C, 16}, meterRead{3, 0x5000, 4}, meterRead{}, false},
		{MeterFinder7M, meterRead{4, 2516, 20}, meterRead{4, 2638, 2}, meterRead{}, false},
		{MeterSchneider, meterRead{3, 0x0BB7, 60}, meterRead{3, 0xB02B, 2}, meterRead{}, false},
		{MeterChint3P, meterRead{3, 0x200C, 14}, meterRead{3, 0x101E, 2}, meterRead{}, false},
		{MeterChint1P, meterRead{3, 0x2002, 4}, meterRead{3, 0x4000, 2}, meterRead{}, false},
		{MeterPhoenix, meterRead{4, 0x000C, 6}, meterRead{4, 0x003E, 2}, meterRead{4, 0x0028, 2}, true},
		{MeterFinder7E, meterRead{4, 0x100E, 6}, meterRead{4, 0x1106, 2}, meterRead{4, 0x1026, 2}, true},
		{MeterWAGO, meterRead{3, 0x500C, 6}, meterRead{3, 0x600C, 2}, meterRead{3, 0x5012, 2}, true},
		{MeterSinotimer, meterRead{4, 0x0003, 3}, meterRead{4, 0x0027, 2}, meterRead{4, 0x0008, 3}, true},
		{MeterGavazzi, meterRead{4, 0x000C, 6}, meterRead{4, 0x0034, 2}, meterRead{4, 0x0028, 2}, true},
	} {
		s := spec(t, tc.typ)
		p, ok := s.powerRead()
		if s.currentRead() != tc.current || s.energyRead() != tc.energy || p != tc.power || ok != tc.hasPower {
			t.Errorf("%s: current %+v energy %+v power %+v %v", s.name, s.currentRead(), s.energyRead(), p, ok)
		}
	}
	for _, typ := range []int{MeterSensorbox, MeterSolarEdge, MeterAPI, MeterHomeWizard, MeterCustom} {
		if _, ok := lookupMeter(typ); ok {
			t.Errorf("%s must not be polled as EV meter", MeterName(typ))
		}
	}
}

func TestMeterDecodeEnergy(t *testing.T) {
	for _, tc := range []struct {
		typ  int
		data string
		want int // Wh
	}{
		{MeterEastron3P, "45 4a e7 cf", 3246488},       // captured on the real bus: 3246.488 kWh
		{MeterABB, "00 00 00 00 00 01 86 a0", 1000000}, // uint64, 0.01 kWh
		{MeterPhoenix, "86 a0 00 01", 10000000},        // low word first, 0.1 kWh
		{MeterSinotimer, "00 01 86 a0", 1000000},       // 32 bit despite int16 registers, 0.01 kWh
		{MeterFinder7E, "47 c3 50 00", 100000},         // float Wh
	} {
		got, err := spec(t, tc.typ).decodeEnergy(hexBytes(t, tc.data))
		if err != nil || got != tc.want {
			t.Errorf("%s: %d Wh (%v), want %d", MeterName(tc.typ), got, err, tc.want)
		}
	}
}

func TestMeterDecodeCurrents(t *testing.T) {
	// Eastron: 16 A, 10 A, 0.5 A, phase powers 3680, -2300, 115 W
	eastron := "41 80 00 00  41 20 00 00  3f 00 00 00  45 66 00 00  c5 0f c0 00  42 e6 00 00"
	for _, tc := range []struct {
		typ      int
		data     string
		currents [3]int
		power    int
		hasPower bool
	}{
		{MeterEastron3P, eastron, [3]int{160, -100, 5}, 3680 - 2300 + 115, true},
		{MeterEastron3PInv, eastron, [3]int{-160, 100, -5}, -(3680 - 2300 + 115), true},
		// Chint 1P: 7.5 A, -1.725 kW
		{MeterChint1P, "40 f0 00 00  bf dc cc cd", [3]int{-75, 0, 0}, -1725, true},
		// Sinotimer: int16 0.01 A: 16.00, 0.10, 0.00 A
		{MeterSinotimer, "06 40 00 0a 00 00", [3]int{160, 1, 0}, 0, false},
		// Phoenix: int32 mA, low word first: 16000, 6000, 0 mA
		{MeterPhoenix, "3e 80 00 00  17 70 00 00  00 00 00 00", [3]int{160, 60, 0}, 0, false},
	} {
		c, p, hasP, err := spec(t, tc.typ).decodeCurrents(hexBytes(t, tc.data))
		if err != nil || c != tc.currents || p != tc.power || hasP != tc.hasPower {
			t.Errorf("%s: currents %v power %d %v (%v), want %v %d %v",
				MeterName(tc.typ), c, p, hasP, err, tc.currents, tc.power, tc.hasPower)
		}
	}
	if _, _, _, err := spec(t, MeterEastron3P).decodeCurrents(hexBytes(t, "41 80 00 00")); err == nil {
		t.Errorf("short response accepted")
	}
}

func TestMeterDecodePower(t *testing.T) {
	for _, tc := range []struct {
		typ  int
		data string
		want int
	}{
		{MeterSinotimer, "0e 60 0e 60 ff 9c", 3680 + 3680 - 100},
		{MeterWAGO, "41 30 00 00", 11000},    // 11 kW
		{MeterGavazzi, "ad b0 00 01", 11000}, // 0.1 W, low word first
	} {
		got, err := spec(t, tc.typ).decodePower(hexBytes(t, tc.data))
		if err != nil || got != tc.want {
			t.Errorf("%s: %d W (%v), want %d", MeterName(tc.typ), got, err, tc.want)
		}
	}
}
