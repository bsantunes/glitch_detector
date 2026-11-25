//go:build linux

package twamp

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// bindToDeviceControl returns a net.Dialer Control hook that sets
// SO_BINDTODEVICE on the socket before it connects, so it only sends
// and receives through dev. Needs root (or CAP_NET_RAW) on older
// kernels; every exporter here runs as root.
func bindToDeviceControl(dev string) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, dev)
		}); err != nil {
			return err
		}
		if serr != nil {
			return fmt.Errorf("SO_BINDTODEVICE %s: %w", dev, serr)
		}
		return nil
	}
}
