package smartevse

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeNode answers every 8-byte FC04 request on conn with register value 7.
func fakeNode(conn net.Conn) {
	defer conn.Close()
	req := make([]byte, 8)
	for {
		if _, err := io.ReadFull(conn, req); err != nil {
			return
		}
		if _, err := conn.Write(appendCRC([]byte{req[0], 0x04, 2, 0, 7})); err != nil {
			return
		}
	}
}

func nodeTransport() (Transport, error) {
	c, s := net.Pipe()
	go fakeNode(s)
	return c.(Transport), nil
}

// reconnectBus returns a bus without backoff that logs into buf.
func reconnectBus(buf *bytes.Buffer, open func() (Transport, error)) *Bus {
	b := NewBus(nil)
	b.open, b.Timeout, b.log = open, time.Second, slog.New(slog.NewTextHandler(buf, nil))
	return b
}

func TestBusReconnects(t *testing.T) {
	var log bytes.Buffer
	opens := 0
	b := reconnectBus(&log, func() (Transport, error) {
		opens++
		if opens == 2 {
			c, s := net.Pipe()
			s.Close() // gateway restarted, adapter unplugged
			return c.(Transport), nil
		}
		return nodeTransport()
	})

	if _, err := b.readInput(2, 0, 1); err != nil {
		t.Fatal(err)
	}
	b.t.Close()
	b.t, _ = b.open()
	if _, err := b.readInput(2, 0, 1); !errors.Is(err, ErrBusDown) {
		t.Fatalf("read on closed connection: got %v, want ErrBusDown", err)
	}
	data, err := b.readInput(2, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if opens != 3 || data[1] != 7 {
		t.Errorf("opens %d, data % x", opens, data)
	}
	for _, msg := range []string{"bus connected", "bus lost", "bus connected"} {
		i := strings.Index(log.String(), msg)
		if i < 0 {
			t.Fatalf("log misses %q:\n%s", msg, log.String())
		}
		log.Next(i + len(msg))
	}
}

func TestBusMissingAtStartLogsOnce(t *testing.T) {
	var log bytes.Buffer
	present := false
	b := reconnectBus(&log, func() (Transport, error) {
		if !present {
			return nil, io.ErrUnexpectedEOF
		}
		return nodeTransport()
	})

	for i := 0; i < 5; i++ {
		if _, err := b.readInput(2, 0, 1); !errors.Is(err, ErrBusDown) {
			t.Fatalf("read without device: got %v, want ErrBusDown", err)
		}
	}
	present = true
	for i := 0; i < 3; i++ {
		if _, err := b.readInput(2, 0, 1); err != nil {
			t.Fatal(err)
		}
	}
	s := log.String()
	if strings.Count(s, "bus not available") != 1 || strings.Count(s, "bus connected") != 1 {
		t.Errorf("want one loss and one recovery line:\n%s", s)
	}
}

func TestBusBackoff(t *testing.T) {
	var log bytes.Buffer
	opens := 0
	b := reconnectBus(&log, func() (Transport, error) {
		opens++
		return nil, io.ErrUnexpectedEOF
	})
	b.retry = time.Second

	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i, w := range want {
		b.readInput(2, 0, 1)
		b.readInput(2, 0, 1) // within the backoff: no attempt
		if b.wait != w*time.Second {
			t.Errorf("attempt %d: wait %v, want %v", i+1, b.wait, w*time.Second)
		}
		b.last = time.Time{} // backoff expired
	}
	if opens != len(want) {
		t.Errorf("opens %d, want %d", opens, len(want))
	}
}
