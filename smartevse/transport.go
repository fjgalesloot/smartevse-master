package smartevse

import (
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Transport is a byte stream to the RS485 bus with read deadlines.
type Transport interface {
	io.ReadWriteCloser
	SetReadDeadline(t time.Time) error
}

// Open opens a transport from a URI:
//
//	/dev/ttyUSB0            local serial port, 9600 8N1 (Linux)
//	tcp://192.168.1.50:502  transparent RS485-to-Ethernet gateway (raw RTU frames, NOT Modbus TCP)
func Open(uri string) (Transport, error) {
	if strings.HasPrefix(uri, "tcp://") {
		conn, err := net.DialTimeout("tcp", strings.TrimPrefix(uri, "tcp://"), 5*time.Second)
		if err != nil {
			return nil, fmt.Errorf("dial %s: %w", uri, err)
		}
		return conn.(*net.TCPConn), nil
	}
	return openSerial(uri)
}
