package smartevse

import (
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

// Transport is a byte stream to the RS485 bus with read deadlines.
type Transport interface {
	io.ReadWriteCloser
	SetReadDeadline(t time.Time) error
}

// flusher is a transport that can discard received bytes not yet read
// (serial: TCFLSH). n is the number of bytes dropped, -1 if unknown.
type flusher interface {
	flushInput() (n int, err error)
}

// drainer is a transport that can wait until written bytes have left the
// UART (serial: tcdrain). Without it the bus assumes they leave at line speed.
type drainer interface {
	drainOutput() error
}

// clock is the time base of the bus: gaps, transmit ends and timeouts.
type clock interface {
	Now() time.Time
	Sleep(time.Duration)
}

type realClock struct{}

func (realClock) Now() time.Time        { return time.Now() }
func (realClock) Sleep(d time.Duration) { time.Sleep(d) }

// virtualClock only moves when the bus waits, so the simulator and tests run
// at full speed while gaps, transmit times and timeouts are still accounted for.
type virtualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *virtualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *virtualClock) Sleep(d time.Duration) {
	if d <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// NewBusURI returns a bus for a URI. The transport is opened by the first request
// and reopened after I/O errors with backoff (1 s doubling to 30 s), so a missing
// device at startup is handled like one lost while running. Loss and recovery are
// logged once each.
//
//	/dev/ttyUSB0             local serial port, 9600 8N1 (Linux)
//	tcp://192.168.1.50:4001  transparent RS485-to-Ethernet gateway or ser2net raw (RTU frames, NOT Modbus TCP)
func NewBusURI(uri string, log *slog.Logger) *Bus {
	b := newBus(nil, realClock{})
	b.name, b.log, b.retry = uri, log, time.Second
	b.open = func() (Transport, error) { return OpenURI(uri) }
	if strings.HasPrefix(uri, "tcp://") {
		b.Timeout = 500 * time.Millisecond // network jitter
	}
	return b
}

// OpenURI opens the transport for a URI once, without reconnect (see NewBusURI).
func OpenURI(uri string) (Transport, error) {
	if addr, ok := strings.CutPrefix(uri, "tcp://"); ok {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			return nil, err
		}
		return conn.(*net.TCPConn), nil
	}
	return openSerial(uri)
}
