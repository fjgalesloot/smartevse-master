package smartevse

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestSplitFrames(t *testing.T) {
	// captured on real hardware: EV meter request + answer in one burst, a
	// request merged with a garbled Sensorbox answer, and an empty burst
	for _, tc := range []struct {
		burst string
		want  []string // "ok:hex" or "bad:hex"
	}{
		{"11 04 00 48 00 02 f3 4d 11 04 04 45 4a e7 cf d5 3b",
			[]string{"ok:110400480002f34d", "ok:11040445 4ae7cfd53b"}},
		{"11 04 00 48 00 02 f3 4d 11 04 04 45 4a e7 cf d5 3b 0a 04 00 00 00 14 f1 7e",
			[]string{"ok:110400480002f34d", "ok:110404454ae7cfd53b", "ok:0a0400000014f17e"}},
		{"0a 04 50 00 00 04 10 24 02 1b",
			[]string{"bad:0a04500000041024021b"}},
		{"", nil},
	} {
		b, err := hex.DecodeString(strings.ReplaceAll(tc.burst, " ", ""))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, f := range splitFrames(b) {
			s := "bad:"
			if f.CRCOK {
				s = "ok:"
			}
			got = append(got, s+hex.EncodeToString(f.Data))
		}
		want := make([]string, len(tc.want))
		for i, w := range tc.want {
			want[i] = strings.ReplaceAll(w, " ", "")
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s:\n got  %v\n want %v", tc.burst, got, want)
		}
	}
}
