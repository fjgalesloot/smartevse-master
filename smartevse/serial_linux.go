//go:build linux

package smartevse

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

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

	return os.NewFile(uintptr(fd), path), nil
}
