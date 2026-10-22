package smartevse

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"time"
)

// SniffGap is the silence that ends a frame when sniffing. Larger than the
// Modbus 3.5 character time because USB serial adapters deliver bytes in
// chunks (FTDI latency timer: 16 ms).
const SniffGap = 20 * time.Millisecond

// SniffFrame is a burst of bytes seen on the bus.
type SniffFrame struct {
	At    time.Time
	Data  []byte
	CRCOK bool // a complete Modbus RTU frame (bursts that merged fail this)
}

// Sniff listens on the bus without ever transmitting, until ctx is done.
// Run it with the master stopped: only a master starts traffic on a Modbus
// bus, so anything seen then comes from another master (or noise).
func Sniff(ctx context.Context, uri string, frame func(SniffFrame)) error {
	t, err := OpenURI(uri)
	if err != nil {
		return err
	}
	defer t.Close()

	var (
		buf   []byte
		start time.Time
		tmp   = make([]byte, 256)
	)
	for ctx.Err() == nil {
		wait := SniffGap
		if len(buf) == 0 {
			wait = 200 * time.Millisecond // idle: wake up regularly to check ctx
		}
		if err := t.SetReadDeadline(time.Now().Add(wait)); err != nil {
			return err
		}
		n, err := t.Read(tmp)
		if n > 0 {
			if len(buf) == 0 {
				start = time.Now()
			}
			buf = append(buf, tmp[:n]...)
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) && !isTimeout(err) {
			return err
		}
		for _, f := range splitFrames(buf) {
			f.At = start
			frame(f)
		}
		buf = nil
	}
	return nil
}

// splitFrames splits a burst into Modbus RTU frames: a request and a fast
// answer arrive within SniffGap and are only separable by their CRC. It takes
// the shortest prefix with a valid CRC each time; what is left is one bad frame.
func splitFrames(b []byte) []SniffFrame {
	var out []SniffFrame
next:
	for len(b) > 0 {
		for n := 4; n <= len(b); n++ {
			if crc16(b[:n-2]) == binary.LittleEndian.Uint16(b[n-2:n]) {
				out = append(out, SniffFrame{Data: b[:n], CRCOK: true})
				b = b[n:]
				continue next
			}
		}
		out = append(out, SniffFrame{Data: b})
		break
	}
	return out
}
