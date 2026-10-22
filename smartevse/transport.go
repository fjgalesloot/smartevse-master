package smartevse

import (
	"io"
	"log/slog"
	"net"
	"strings"
	"time"
)

// Transport is a byte stream to the RS485 bus with read deadlines.
type Transport interface {
	io.ReadWriteCloser
	SetReadDeadline(t time.Time) error
}

// NewBusURI returns a bus for a URI. The transport is opened by the first request
// and reopened after I/O errors with backoff (1 s doubling to 30 s), so a missing
// device at startup is handled like one lost while running. Loss and recovery are
// logged once each.
//
//	/dev/ttyUSB0             local serial port, 9600 8N1 (Linux)
//	tcp://192.168.1.50:4001  transparent RS485-to-Ethernet gateway or ser2net raw (RTU frames, NOT Modbus TCP)
func NewBusURI(uri string, log *slog.Logger) *Bus {
	b := &Bus{name: uri, log: log, retry: time.Second, open: func() (Transport, error) { return OpenURI(uri) }}
	if strings.HasPrefix(uri, "tcp://") {
		// network jitter: the gap keeps the next request from merging with a broadcast
		b.Timeout, b.BroadcastGap = 500*time.Millisecond, 50*time.Millisecond
	} else {
		b.Timeout, b.BroadcastGap = 250*time.Millisecond, 20*time.Millisecond
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
