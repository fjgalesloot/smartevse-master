package smartevse

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// retryMax caps the reconnect backoff.
const retryMax = 30 * time.Second

type busState int

const (
	busNew busState = iota
	busUp
	busDown
)

// Bus is a minimal Modbus RTU master with exactly the functions the
// SmartEVSE protocol needs, including its non-standard broadcast address.
type Bus struct {
	mu      sync.Mutex
	t       Transport
	Timeout time.Duration // response timeout, firmware uses 150 ms
	// BroadcastGap is the pause after a broadcast so nodes can process it.
	BroadcastGap time.Duration

	open  func() (Transport, error) // reopens the transport after an I/O error, nil = no reconnect
	name  string                    // port, for logging
	log   *slog.Logger
	state busState
	last  time.Time     // last open attempt or loss
	wait  time.Duration // current reconnect backoff, doubles from retry up to retryMax
	retry time.Duration // first reconnect backoff
}

// NewBus wraps a transport without reconnect (simulator, tests).
func NewBus(t Transport) *Bus {
	return &Bus{t: t, Timeout: 250 * time.Millisecond, BroadcastGap: 20 * time.Millisecond}
}

// Close closes the underlying transport.
func (b *Bus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.open = nil
	if b.t == nil {
		return nil
	}
	return b.t.Close()
}

// ErrException is returned when a device answers with a Modbus exception.
var ErrException = errors.New("modbus exception")

// ErrBusDown is returned while the transport is lost and being reopened. The bus
// logs the loss and recovery itself, so callers need not log these errors.
var ErrBusDown = errors.New("bus not connected")

// ReadInput reads count input registers (function 0x04) and returns the raw data bytes.
func (b *Bus) ReadInput(addr byte, reg, count uint16) ([]byte, error) {
	return b.Read(addr, 0x04, reg, count)
}

// Read reads count holding (fc 0x03) or input (fc 0x04) registers and returns
// the raw data bytes.
func (b *Bus) Read(addr, fc byte, reg, count uint16) ([]byte, error) {
	req := []byte{addr, fc, byte(reg >> 8), byte(reg), byte(count >> 8), byte(count)}
	resp, err := b.transact(appendCRC(req), 5+int(count)*2)
	if err != nil {
		return nil, err
	}
	if resp[1] != fc || int(resp[2]) != int(count)*2 {
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
	if err := b.connect(); err != nil {
		return err
	}
	defer b.checkUp()
	b.drain()
	if _, err := b.t.Write(frame); err != nil {
		return b.reset(err)
	}
	// nobody answers a broadcast: wait for the frame to leave the bus plus a gap
	if b.BroadcastGap > 0 {
		time.Sleep(frameTime(len(frame)) + b.BroadcastGap)
	}
	return nil
}

func (b *Bus) transact(frame []byte, expect int) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.connect(); err != nil {
		return nil, err
	}
	defer b.checkUp()

	b.drain()
	if _, err := b.t.Write(frame); err != nil {
		return nil, b.reset(err)
	}

	sent := time.Now()
	deadline := sent.Add(frameTime(len(frame)) + b.Timeout)
	buf := make([]byte, 0, expect)
	tmp := make([]byte, 256)
	var first time.Duration // until the first answer byte, for diagnostics
	for len(buf) < expect {
		if err := b.t.SetReadDeadline(deadline); err != nil {
			return nil, b.reset(err)
		}
		n, err := b.t.Read(tmp)
		if n > 0 && len(buf) == 0 {
			first = time.Since(sent)
		}
		buf = append(buf, tmp[:n]...)
		// exception response is 5 bytes: addr, fc|0x80, code, crc
		if len(buf) >= 5 && buf[1]&0x80 != 0 {
			if crc16(buf[:3]) != binary.LittleEndian.Uint16(buf[3:5]) {
				return nil, fmt.Errorf("addr %d: crc error in exception%s", frame[0], rxInfo(buf, first))
			}
			return nil, fmt.Errorf("addr %d: %w %d", frame[0], ErrException, buf[2])
		}
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) || isTimeout(err) {
				return nil, fmt.Errorf("addr %d: timeout (%d/%d bytes)%s", frame[0], len(buf), expect, rxInfo(buf, first))
			}
			return nil, b.reset(err)
		}
	}
	if buf[0] != frame[0] {
		return nil, fmt.Errorf("addr %d: answer from %d%s", frame[0], buf[0], rxInfo(buf, first))
	}
	if crc16(buf[:expect-2]) != binary.LittleEndian.Uint16(buf[expect-2:expect]) {
		return nil, fmt.Errorf("addr %d: crc error%s", frame[0], rxInfo(buf, first))
	}
	return buf[:expect], nil
}

// rxInfo describes what arrived for a failed request: latency and raw bytes.
func rxInfo(buf []byte, first time.Duration) string {
	if len(buf) == 0 {
		return ""
	}
	return fmt.Sprintf(" [first byte after %d ms, rx % x]", first.Milliseconds(), buf)
}

// connect (re)opens the transport, with exponential backoff between attempts.
func (b *Bus) connect() error {
	if b.t != nil {
		return nil
	}
	if b.open == nil {
		return fmt.Errorf("%w: closed", ErrBusDown)
	}
	if time.Since(b.last) < b.wait {
		return ErrBusDown
	}
	b.last = time.Now()
	t, err := b.open()
	if err != nil {
		b.backoff()
		if b.state != busDown {
			b.state = busDown
			b.log.Error("bus not available, retrying", "port", b.name, "err", err)
		} else {
			b.log.Debug("bus reconnect failed", "port", b.name, "err", err, "next", b.wait)
		}
		return fmt.Errorf("%w: %w", ErrBusDown, err)
	}
	b.t = t
	return nil
}

// reset drops a broken transport so a later request reopens it, and returns err
// marked as ErrBusDown. Without reconnect it returns err unchanged.
func (b *Bus) reset(err error) error {
	if b.open == nil || b.t == nil {
		return err
	}
	_ = b.t.Close()
	b.t = nil
	b.last = time.Now()
	b.backoff()
	if b.state == busUp {
		b.log.Error("bus lost, reconnecting", "port", b.name, "err", err)
	} else {
		b.log.Debug("bus lost again", "port", b.name, "err", err)
	}
	b.state = busDown
	return fmt.Errorf("%w: %w", ErrBusDown, err)
}

// checkUp marks the bus working once a request completed without dropping the
// transport; a device that does not answer is not a bus problem.
func (b *Bus) checkUp() {
	if b.open == nil || b.t == nil || b.state == busUp {
		return
	}
	b.state, b.wait = busUp, 0
	b.log.Info("bus connected", "port", b.name, "timeout", b.Timeout)
}

func (b *Bus) backoff() {
	b.wait = min(max(2*b.wait, b.retry), retryMax)
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
