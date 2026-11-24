//go:build !linux

package modem

import (
	"errors"
	"os"
)

// OpenRawSerial is Linux-only (see serial_linux.go); this stub only
// exists so the package still type-checks on a non-Linux dev machine.
func OpenRawSerial(dev string, flag int) (*os.File, error) {
	return nil, errors.New("modem: OpenRawSerial is only supported on Linux")
}
