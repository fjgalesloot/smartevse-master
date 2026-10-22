package smartevse

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Bus is a minimal Modbus RTU master with exactly the functions the
// SmartEVSE protocol needs, including its non-standard broadcast address.
type Bus struct {
	mu      sync.Mutex
	t       Transport
	Timeout time.Duration // response timeout, firmware uses 150 ms
	// BroadcastGap is the pause after a broadcast so nodes can process it.
	BroadcastGap time.Duration
}

// NewBus wraps a transport.
func NewBus(t Transport) *Bus {
	return &Bus{t: t, Timeout: 250 * time.Millisecond, BroadcastGap: 20 * time.Millisecond}
}

// Close closes the underlying transport.
func (b *Bus) Close() error { return b.t.Close() }

// ErrException is returned when a device answers with a Modbus exception.
var ErrException = errors.New("modbus exception")

// ReadInput reads count input registers (function 0x04) and returns the raw data bytes.
func (b *Bus) ReadInput(addr byte, reg, count uint16) ([]byte, error) {
	req := []byte{addr, 0x04, byte(reg >> 8), byte(reg), byte(count >> 8), byte(count)}
	resp, err := b.transact(appendCRC(req), 5+int(count)*2)
	if err != nil {
		return nil, err
	}
	if resp[1] != 0x04 || int(resp[2]) != int(count)*2 {
		return nil, fmt.Errorf("addr %d: unexpected response % x", addr, resp)
	}
	return resp[3 : 3+int(count)*2], nil
}

// WriteMultiple writes holding registers (function 0x10). Broadcasts get no answer.
func (b *Bus) WriteMultiple(addr byte, reg uint16, values []uint16) error {
	req := []byte{addr, 0x10, byte(reg >> 8), byte(reg), 0, byte(len(values)), byte(len(values) * 2)}
	for _, v := range values {
		req = binary.BigEndian.AppendUint16(req, v)
	}
	if addr == BroadcastAddr {
		return b.send(appendCRC(req))
	}
	_, err := b.transact(appendCRC(req), 8)
	return err
}

// WriteSingle writes one holding register (function 0x06). Broadcasts get no answer.
func (b *Bus) WriteSingle(addr byte, reg, value uint16) error {
	req := appendCRC([]byte{addr, 0x06, byte(reg >> 8), byte(reg), byte(value >> 8), byte(value)})
	if addr == BroadcastAddr {
		return b.send(req)
	}
	_, err := b.transact(req, 8)
	return err
}

func (b *Bus) send(frame []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.drain()
	_, err := b.t.Write(frame)
	// nobody answers a broadcast: wait for the frame to leave the bus plus a gap
	if b.BroadcastGap > 0 {
		time.Sleep(frameTime(len(frame)) + b.BroadcastGap)
	}
	return err
}

func (b *Bus) transact(frame []byte, expect int) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.drain()
	if _, err := b.t.Write(frame); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(frameTime(len(frame)) + b.Timeout)
	buf := make([]byte, 0, expect)
	tmp := make([]byte, 256)
	for len(buf) < expect {
		if err := b.t.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		n, err := b.t.Read(tmp)
		buf = append(buf, tmp[:n]...)
		// exception response is 5 bytes: addr, fc|0x80, code, crc
		if len(buf) >= 5 && buf[1]&0x80 != 0 {
			if crc16(buf[:3]) != binary.LittleEndian.Uint16(buf[3:5]) {
				return nil, fmt.Errorf("addr %d: crc error in exception", frame[0])
			}
			return nil, fmt.Errorf("addr %d: %w %d", frame[0], ErrException, buf[2])
		}
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) || isTimeout(err) {
				return nil, fmt.Errorf("addr %d: timeout (%d/%d bytes)", frame[0], len(buf), expect)
			}
			return nil, err
		}
	}
	buf = buf[:expect]
	if buf[0] != frame[0] {
		return nil, fmt.Errorf("addr %d: answer from %d", frame[0], buf[0])
	}
	if crc16(buf[:expect-2]) != binary.LittleEndian.Uint16(buf[expect-2:]) {
		return nil, fmt.Errorf("addr %d: crc error", frame[0])
	}
	return buf, nil
}

// drain discards stale bytes left on the bus.
func (b *Bus) drain() {
	tmp := make([]byte, 256)
	for {
		_ = b.t.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
		n, err := b.t.Read(tmp)
		if n == 0 || err != nil {
			return
		}
	}
}

// frameTime is the transmit time of n bytes at 9600 baud (10 bits per byte).
func frameTime(n int) time.Duration {
	return time.Duration(n*10) * time.Second / 9600
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}
