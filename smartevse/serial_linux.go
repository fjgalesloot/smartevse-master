//go:build linux

package smartevse

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// ioctl requests missing from package syscall (same on amd64, arm and arm64).
const (
	ioctlTCSBRK = 0x5409 // with arg 1: tcdrain
	ioctlTCFLSH = 0x540B
)

// serialPort is a tty that can drain its output and flush its input.
type serialPort struct {
	*os.File
}

// openSerial opens a tty raw at 9600 8N1, the fixed SmartEVSE bus speed.
// The fd is non-blocking so os.File supports read deadlines via the runtime poller.
func openSerial(path string) (Transport, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	var t syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCGETS, uintptr(unsafe.Pointer(&t))); e != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("tcgets %s: %w", path, e)
	}

	t.Iflag = 0
	t.Oflag = 0
	t.Lflag = 0
	t.Cflag = syscall.CS8 | syscall.CREAD | syscall.CLOCAL | syscall.B9600
	t.Ispeed = syscall.B9600
	t.Ospeed = syscall.B9600
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0

	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCSETS, uintptr(unsafe.Pointer(&t))); e != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("tcsets %s: %w", path, e)
	}

	return serialPort{os.NewFile(uintptr(fd), path)}, nil
}

// control runs f on the fd. Through SyscallConn, because File.Fd would
// switch the fd to blocking mode and break the read deadlines.
func (p serialPort) control(f func(fd uintptr) syscall.Errno) error {
	rc, err := p.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) { errno = f(fd) }); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}

// flushInput discards what the kernel has received but nobody read, and
// returns how many bytes that were.
func (p serialPort) flushInput() (int, error) {
	var n int32
	if err := p.control(func(fd uintptr) syscall.Errno {
		_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCINQ, uintptr(unsafe.Pointer(&n)))
		return e
	}); err != nil {
		n = -1
	}
	return int(n), p.control(func(fd uintptr) syscall.Errno {
		_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlTCFLSH, syscall.TCIFLUSH)
		return e
	})
}

// drainOutput waits until the written bytes have been transmitted (tcdrain).
func (p serialPort) drainOutput() error {
	return p.control(func(fd uintptr) syscall.Errno {
		_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlTCSBRK, 1)
		return e
	})
}
