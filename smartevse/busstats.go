package smartevse

import (
	"math"
	"slices"
	"sort"
	"time"
)

// latSamples is the number of recent answer latencies kept per address.
const latSamples = 200

// AddrStats counts the exchanges with one bus address since start.
type AddrStats struct {
	Addr        int     `json:"address"`
	TX          uint64  `json:"tx"`
	OK          uint64  `json:"ok"`
	Timeout     uint64  `json:"timeout"`
	CRC         uint64  `json:"crc"` // corrupt answers: CRC, address, function or length
	Exception   uint64  `json:"exception"`
	RetryOK     uint64  `json:"retryOk"`               // failed reads that succeeded on the retry
	Late        uint64  `json:"lateBytes"`             // bytes still arriving after an exchange with this address
	StaleEvents int     `json:"staleEvents,omitempty"` // EV meter: times it went stale
	P50Ms       float64 `json:"latencyP50Ms"`          // end of transmission to the first answer byte
	P99Ms       float64 `json:"latencyP99Ms"`
	Samples     int     `json:"latencySamples"`
}

type addrStats struct {
	AddrStats
	lat  []time.Duration // ring of the last latSamples latencies
	next int
}

func (b *Bus) stat(addr byte) *addrStats {
	if b.stats == nil {
		b.stats = map[byte]*addrStats{}
	}
	s, ok := b.stats[addr]
	if !ok {
		s = &addrStats{AddrStats: AddrStats{Addr: int(addr)}}
		b.stats[addr] = s
	}
	return s
}

func (s *addrStats) addLatency(d time.Duration) {
	if d < 0 {
		return
	}
	if len(s.lat) < latSamples {
		s.lat = append(s.lat, d)
		return
	}
	s.lat[s.next] = d
	s.next = (s.next + 1) % latSamples
}

// percentile returns the p-th (0..1) latency of the recent samples.
func (s *addrStats) percentile(p float64) (time.Duration, bool) {
	if len(s.lat) == 0 {
		return 0, false
	}
	l := slices.Clone(s.lat)
	slices.Sort(l)
	i := int(math.Ceil(p*float64(len(l)))) - 1
	return l[max(i, 0)], true
}

// countRetryOK counts a read that succeeded on the retry.
func (b *Bus) countRetryOK(addr byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stat(addr).RetryOK++
}

// minLatencySamples is the number of answers before the measured latency
// may raise a response timeout.
const minLatencySamples = 20

// responseTimeout is the response timeout for an answer of n bytes from addr:
// its transmit time plus 100 ms, at least twice the measured p99 latency, and
// never below the bus default.
func (b *Bus) responseTimeout(addr byte, n int) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := frameTime(n) + 100*time.Millisecond
	if s, ok := b.stats[addr]; ok && len(s.lat) >= minLatencySamples {
		p99, _ := s.percentile(0.99)
		t = max(t, 2*p99)
	}
	return max(t, b.Timeout)
}

// Stats returns the counters per address, by address.
func (b *Bus) Stats() []AddrStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]AddrStats, 0, len(b.stats))
	for _, s := range b.stats {
		a := s.AddrStats
		a.Samples = len(s.lat)
		if p, ok := s.percentile(0.5); ok {
			a.P50Ms = ms(p)
		}
		if p, ok := s.percentile(0.99); ok {
			a.P99Ms = ms(p)
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}
