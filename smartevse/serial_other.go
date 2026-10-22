//go:build !linux

package smartevse

import "errors"

func openSerial(path string) (Transport, error) {
	return nil, errors.New("serial ports are only supported on Linux; use tcp:// with an RS485 gateway")
}
