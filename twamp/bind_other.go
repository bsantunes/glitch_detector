//go:build !linux

package twamp

import (
	"errors"
	"syscall"
)

// bindToDeviceControl is Linux-only (see bind_linux.go); this stub only
// exists so the package still type-checks on a non-Linux dev machine.
func bindToDeviceControl(dev string) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		return errors.New("bind-dev is only supported on Linux")
	}
}
