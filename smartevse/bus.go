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

const (
	charTime = 10 * time.Second / 9600 // one character at 9600 8N1
	minGap   = charTime * 35 / 10      // Modbus RTU silence between frames: 3.5 characters

	// DefaultInterFrameDelay is the default silence between frames (Home
	// Assistant's message_wait_milliseconds for serial Modbus).
	DefaultInterFrameDelay = 30 * time.Millisecond
	// DefaultBroadcastTurnaround is the default extra silence after a broadcast,
	// so the nodes have processed it before the next request.
	DefaultBroadcastTurnaround = 100 * time.Millisecond
)

type busState int

const (
	busNew busState = iota
	busUp
	busDown
)

// Bus is a minimal Modbus RTU master with exactly the functions the
// SmartEVSE protocol needs, including its non-standard broadcast address.
//
// Its request functions are unexported: only the Master's control loop talks
// on the bus, one frame at a time. Every frame follows the same rules: bytes
// left from an earlier exchange are flushed, the silence since the end of the
// last frame on the bus (sent or received) is at least InterFrameDelay (and
// 3.5 characters), plus BroadcastTurnaround after a broadcast, and the
// response timeout counts from the end of transmission.
type Bus struct {
	mu      sync.Mutex
	t       Transport
	Timeout time.Duration // response timeout, firmware uses 150 ms
	// InterFrameDelay is the minimum silence before a frame.
	InterFrameDelay time.Duration
	// BroadcastTurnaround is the extra silence after a broadcast.
	BroadcastTurnaround time.Duration
	// Trace logs every frame at info level (diagnostics).
	Trace bool

	clock   clock
	idle    time.Time // end of the last frame on the bus: the next gap counts from here
	afterBC bool      // the last frame was a broadcast
	prev    frameInfo // the last frame, for the trace and late bytes
	stats   map[byte]*addrStats

	open  func() (Transport, error) // reopens the transport after an I/O error, nil = no reconnect
	name  string                    // port, for logging
	log   *slog.Logger
	state busState
	last  time.Time     // last open attempt or loss
	wait  time.Duration // current reconnect backoff, doubles from retry up to retryMax
	retry time.Duration // first reconnect backoff
}

func newBus(t Transport, c clock) *Bus {
	return &Bus{
		t: t, clock: c, Timeout: 250 * time.Millisecond,
		InterFrameDelay: DefaultInterFrameDelay, BroadcastTurnaround: DefaultBroadcastTurnaround,
		stats: map[byte]*addrStats{},
	}
}

// NewBus wraps a transport without reconnect (tests).
func NewBus(t Transport) *Bus { return newBus(t, realClock{}) }

// NewSimBus returns a bus on the simulator with a virtual clock: gaps, transmit
// times and timeouts take no real time, and the simulator sees when each frame
// starts, so it can ignore a request that follows the previous frame too closely.
func NewSimBus(s *Sim) *Bus {
	c := &virtualClock{t: time.Unix(0, 0)}
	s.Do(func() { s.busNow = c.Now })
	return newBus(s, c)
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

// ErrTimeout is returned when no complete answer arrived in time.
var ErrTimeout = errors.New("timeout")

// ErrFrame is returned for a corrupt answer: CRC, address, function or length.
var ErrFrame = errors.New("bad frame")

// ErrBusDown is returned while the transport is lost and being reopened. The bus
// logs the loss and recovery itself, so callers need not log these errors.
var ErrBusDown = errors.New("bus not connected")

// retryable reports whether an exchange may be repeated: the device may not
// have heard the request, or its answer was damaged. An exception is an answer.
func retryable(err error) bool {
	return errors.Is(err, ErrTimeout) || errors.Is(err, ErrFrame)
}

// readInput reads count input registers (function 0x04) and returns the raw data bytes.
func (b *Bus) readInput(addr byte, reg, count uint16) ([]byte, error) {
	return b.read(addr, 0x04, reg, count)
}

// read reads count holding (fc 0x03) or input (fc 0x04) registers and returns
// the raw data bytes.
func (b *Bus) read(addr, fc byte, reg, count uint16) ([]byte, error) {
	return b.readTimeout(addr, fc, reg, count, b.Timeout)
}

// readTimeout is read with its own response timeout.
func (b *Bus) readTimeout(addr, fc byte, reg, count uint16, timeout time.Duration) ([]byte, error) {
	req := []byte{addr, fc, byte(reg >> 8), byte(reg), byte(count >> 8), byte(count)}
	resp, err := b.exchange(appendCRC(req), 5+int(count)*2, timeout)
	if err != nil {
		return nil, err
	}
	return resp[3 : 3+int(count)*2], nil
}

// writeMultiple writes holding registers (function 0x10). Broadcasts get no answer.
func (b *Bus) writeMultiple(addr byte, reg uint16, values []uint16) error {
	req := []byte{addr, 0x10, byte(reg >> 8), byte(reg), 0, byte(len(values)), byte(len(values) * 2)}
	for _, v := range values {
		req = binary.BigEndian.AppendUint16(req, v)
	}
	_, err := b.exchange(appendCRC(req), 8, b.Timeout)
	return err
}

// writeSingle writes one holding register (function 0x06). Broadcasts get no answer.
func (b *Bus) writeSingle(addr byte, reg, value uint16) error {
	req := appendCRC([]byte{addr, 0x06, byte(reg >> 8), byte(reg), byte(value >> 8), byte(value)})
	_, err := b.exchange(req, 8, b.Timeout)
	return err
}

// frameInfo describes one exchange, for the trace.
type frameInfo struct {
	kind       string // read, write or broadcast
	addr, fc   byte
	reg        uint16
	gap        time.Duration // silence before it, -1 = first frame
	start, end time.Time     // transmission
	first      time.Duration // end of transmission to the first answer byte, -1 = none
	flushed    int           // stale bytes discarded before it
	result     string
}

func (f frameInfo) String() string {
	if f.kind == "" {
		return "none"
	}
	return fmt.Sprintf("%s addr %d fc %02x reg 0x%04x", f.kind, f.addr, f.fc, f.reg)
}

// exchange sends one frame and, unless it is a broadcast, reads the answer
// of expect bytes, waiting at most timeout after the end of transmission.
func (b *Bus) exchange(frame []byte, expect int, timeout time.Duration) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.connect(); err != nil {
		return nil, err
	}
	defer b.checkUp()

	f := frameInfo{addr: frame[0], fc: frame[1], reg: binary.BigEndian.Uint16(frame[2:4]), first: -1}
	broadcast := f.addr == BroadcastAddr
	switch {
	case broadcast:
		f.kind = "broadcast"
	case f.fc == 0x03 || f.fc == 0x04:
		f.kind = "read"
	default:
		f.kind = "write"
	}
	st := b.stat(f.addr)
	st.TX++

	f.flushed = b.flush()
	f.gap = b.waitGap()
	f.start = b.clock.Now()
	if _, err := b.t.Write(frame); err != nil {
		return nil, b.reset(err)
	}
	if d, ok := b.t.(drainer); ok {
		if err := d.drainOutput(); err != nil {
			return nil, b.reset(err)
		}
	}
	// some USB serial drivers return from drain before the bytes are out:
	// the frame cannot have left before its transmit time
	f.end = f.start.Add(frameTime(len(frame)))
	if now := b.clock.Now(); now.After(f.end) {
		f.end = now
	}
	b.clock.Sleep(f.end.Sub(b.clock.Now()))
	b.afterBC = broadcast

	if broadcast { // nobody answers
		b.idle = f.end
		st.OK++
		f.result = "sent"
		b.done(f)
		return nil, nil
	}

	resp, err := b.receive(frame, expect, f.end.Add(timeout), &f)
	b.idle = b.clock.Now()
	switch {
	case err == nil:
		st.OK++
		st.addLatency(f.first)
		f.result = "ok"
	case errors.Is(err, ErrException):
		st.Exception++
		st.addLatency(f.first)
	case errors.Is(err, ErrTimeout):
		st.Timeout++
	case errors.Is(err, ErrFrame):
		st.CRC++
	}
	if err != nil {
		f.result = err.Error()
	}
	b.done(f)
	return resp, err
}

// receive reads the answer to req: complete at expect bytes, or at 5 bytes
// for an exception. The end is never guessed from a pause between bytes:
// USB serial adapters deliver in chunks.
func (b *Bus) receive(req []byte, expect int, deadline time.Time, f *frameInfo) ([]byte, error) {
	addr := req[0]
	buf := make([]byte, 0, expect)
	tmp := make([]byte, 256)
	for len(buf) < expect {
		if err := b.t.SetReadDeadline(deadline); err != nil {
			return nil, b.reset(err)
		}
		n, err := b.t.Read(tmp)
		if n > 0 && len(buf) == 0 {
			f.first = b.clock.Now().Sub(f.end)
		}
		buf = append(buf, tmp[:n]...)
		if len(buf) > 0 && buf[0] != addr {
			// a late answer to an earlier request, or noise: drop the rest too
			b.flush()
			return nil, fmt.Errorf("addr %d: %w: answer from %d%s", addr, ErrFrame, buf[0], rxInfo(buf, f.first))
		}
		// exception response is 5 bytes: addr, fc|0x80, code, crc
		if len(buf) >= 5 && buf[1]&0x80 != 0 {
			if buf[1]&0x7f != req[1] || crc16(buf[:3]) != binary.LittleEndian.Uint16(buf[3:5]) {
				return nil, fmt.Errorf("addr %d: %w: crc error in exception%s", addr, ErrFrame, rxInfo(buf, f.first))
			}
			return nil, fmt.Errorf("addr %d: %w %d", addr, ErrException, buf[2])
		}
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) || isTimeout(err) {
				b.clock.Sleep(deadline.Sub(b.clock.Now())) // the virtual clock waits too
				return nil, fmt.Errorf("addr %d: %w (%d/%d bytes)%s", addr, ErrTimeout, len(buf), expect, rxInfo(buf, f.first))
			}
			return nil, b.reset(err)
		}
	}
	if crc16(buf[:expect-2]) != binary.LittleEndian.Uint16(buf[expect-2:expect]) {
		return nil, fmt.Errorf("addr %d: %w: crc error%s", addr, ErrFrame, rxInfo(buf, f.first))
	}
	if buf[1] != req[1] || (req[1] == 0x03 || req[1] == 0x04) && int(buf[2]) != expect-5 {
		return nil, fmt.Errorf("addr %d: %w: unexpected response % x", addr, ErrFrame, buf[:expect])
	}
	return buf[:expect], nil
}

// done finishes an exchange: trace and remember it.
func (b *Bus) done(f frameInfo) {
	if b.Trace {
		first := -1.0
		if f.first >= 0 {
			first = ms(f.first)
		}
		gap := -1.0
		if f.gap >= 0 {
			gap = ms(f.gap)
		}
		b.logger().Info("bus frame", "kind", f.kind, "addr", f.addr, "fc", fmt.Sprintf("%02x", f.fc),
			"reg", fmt.Sprintf("0x%04x", f.reg), "gapMs", gap, "txStart", f.start.Format("15:04:05.000"),
			"txMs", ms(f.end.Sub(f.start)), "firstByteMs", first, "flushed", f.flushed, "result", f.result)
	}
	b.prev = f
}

// flush discards bytes left from an earlier exchange (a late answer, noise)
// and returns how many there were (-1 = some, count unknown).
func (b *Bus) flush() int {
	n := 0
	if fl, ok := b.t.(flusher); ok {
		var err error
		if n, err = fl.flushInput(); err != nil {
			b.logger().Debug("bus flush failed", "err", err)
		}
	} else {
		tmp := make([]byte, 256)
		for {
			_ = b.t.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
			r, err := b.t.Read(tmp)
			n += r
			if r == 0 || err != nil {
				break
			}
		}
	}
	if n != 0 {
		if n > 0 && b.prev.kind != "" {
			b.stat(b.prev.addr).Late += uint64(n)
		}
		b.logger().Debug("bus: discarded late bytes", "bytes", n, "after", b.prev.String())
	}
	return n
}

// waitGap keeps the bus silent for the inter-frame delay after the last
// frame, longer after a broadcast, and returns the silence (-1 = first frame).
func (b *Bus) waitGap() time.Duration {
	if b.idle.IsZero() {
		return -1
	}
	need := max(minGap, b.InterFrameDelay)
	if b.afterBC {
		need += b.BroadcastTurnaround
	}
	b.clock.Sleep(b.idle.Add(need).Sub(b.clock.Now()))
	return b.clock.Now().Sub(b.idle)
}

// now is the bus clock (virtual on the simulator).
func (b *Bus) now() time.Time { return b.clock.Now() }

// rxInfo describes what arrived for a failed request: latency and raw bytes.
func rxInfo(buf []byte, first time.Duration) string {
	if len(buf) == 0 {
		return ""
	}
	return fmt.Sprintf(" [first byte after %d ms, rx % x]", first.Milliseconds(), buf)
}

func (b *Bus) logger() *slog.Logger {
	if b.log == nil {
		return slog.Default()
	}
	return b.log
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
	b.log.Info("bus connected", "port", b.name, "timeout", b.Timeout,
		"interFrameDelay", b.InterFrameDelay, "broadcastTurnaround", b.BroadcastTurnaround)
}

func (b *Bus) backoff() {
	b.wait = min(max(2*b.wait, b.retry), retryMax)
}

// frameTime is the transmit time of n bytes at 9600 baud (10 bits per byte).
func frameTime(n int) time.Duration {
	return time.Duration(n) * charTime
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}
