//go:build linux

package smartevse

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// openPty returns the master side of a new pty and the path of its slave.
func openPty(t *testing.T) (*os.File, string) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no pty:", err)
	}
	t.Cleanup(func() { m.Close() })
	var unlock, n int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		t.Fatal("unlockpt:", e)
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		t.Fatal("ptsname:", e)
	}
	return m, fmt.Sprintf("/dev/pts/%d", n)
}

// The serial port ioctls work on a tty, also while goroutines are being
// preempted all the time. A pty drains at once, so this does not reproduce
// the EINTR a real UART's tcdrain gets; it guards the ioctl calls themselves.
func TestSerialFlushAndDrain(t *testing.T) {
	m, slave := openPty(t)
	tr, err := openSerial(slave)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	p := tr.(serialPort)

	if _, err := m.Write([]byte{1, 2, 3, 4, 5}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if n, err := p.flushInput(); err != nil || n != 5 {
		t.Errorf("flush: %d bytes, %v; want 5", n, err)
	}
	if n, err := p.flushInput(); err != nil || n != 0 {
		t.Errorf("second flush: %d bytes, %v; want 0", n, err)
	}

	stop := make(chan struct{})
	defer close(stop)
	for i := 0; i < runtime.GOMAXPROCS(0); i++ {
		go func() { // busy goroutines: the scheduler preempts with signals
			for {
				select {
				case <-stop:
					return
				default:
				}
			}
		}()
	}
	go func() { // read what is written, so the pty never fills up
		buf := make([]byte, 4096)
		for {
			if _, err := m.Read(buf); err != nil {
				return
			}
		}
	}()
	frame := make([]byte, 64)
	for i := 0; i < 200; i++ {
		if _, err := p.Write(frame); err != nil {
			t.Fatal(err)
		}
		if err := p.drainOutput(); err != nil {
			t.Fatalf("drain %d: %v", i, err)
		}
	}
}
