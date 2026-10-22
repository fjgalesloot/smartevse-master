package smartevse

import "testing"

// Test vectors taken from the SmartEVSE-3 firmware comments (main.cpp).
func TestCRC(t *testing.T) {
	cases := []struct {
		frame  []byte
		lo, hi byte
	}{
		{[]byte{0x09, 0x10, 0x00, 0x20, 0x00, 0x08, 0x10, 0x00, 0xA0, 0x00, 0x00, 0x00, 0x3C, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, 0x99, 0x24},
		{[]byte{0x03, 0x10, 0x00, 0x00, 0x00, 0x02, 0x04, 0x00, 0x07, 0x00, 0x00}, 0x49, 0xD6},
	}
	for _, c := range cases {
		f := appendCRC(append([]byte{}, c.frame...))
		if f[len(f)-2] != c.lo || f[len(f)-1] != c.hi {
			t.Errorf("crc % x: got %02x %02x, want %02x %02x", c.frame, f[len(f)-2], f[len(f)-1], c.lo, c.hi)
		}
	}
}
