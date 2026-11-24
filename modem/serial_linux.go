//go:build linux

package modem

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// OpenRawSerial opens a modem serial port (AT or NMEA) and puts it in
// raw mode with echo off, without running any external command.
//
// Why not `stty -F dev raw -echo` (what every AT/NMEA path used up to
// v2.3.1): GNU stty applies settings with TCSADRAIN, which waits for
// the tty's output queue to drain first. Confirmed live on aaeon's
// SIM8202G NMEA port (2026-11-24): the port comes up after boot with
// echo on, so every incoming NMEA byte is echoed back toward the modem,
// whose NMEA interface never consumes it — the output queue never
// drains, and stty blocks in the kernel's set_termios forever. The
// NMEA reader then never opened the port and silently reported "no
// fix" on every poll.
//
// The sequence here is: open with O_NONBLOCK|O_NOCTTY (O_NONBLOCK so
// the open itself can't block on modem-control lines, O_NOCTTY so the
// port never becomes our controlling terminal), set raw/no-echo with
// TCSETS (= TCSANOW — never a drain variant), then TCIOFLUSH to throw
// away anything already queued in either direction (including echoed
// bytes stuck in the output queue and stale input from before this
// open). No baud rate is set: these are virtual USB-serial ports and
// the kernel driver rejects an explicit speed on them.
//
// The fd is deliberately left O_NONBLOCK: os.NewFile then registers it
// with Go's runtime poller, which is what makes SetReadDeadline work
// (every caller relies on bounded reads) and what lets Close unblock a
// pending Read. From the caller's side reads still behave as blocking
// reads with optional deadlines.
//
// flag is the access mode (os.O_RDONLY or os.O_RDWR).
func OpenRawSerial(dev string, flag int) (*os.File, error) {
	fd, err := unix.Open(dev, flag|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: dev, Err: err}
	}
	if err := setRawNoEcho(fd); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("serial setup %s: %w", dev, err)
	}
	if err := unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIOFLUSH); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("serial flush %s: %w", dev, err)
	}
	return os.NewFile(uintptr(fd), dev), nil
}

// setRawNoEcho is cfmakeraw(3) applied with TCSETS (immediately, no
// drain). The speed bits are left untouched.
func setRawNoEcho(fd int) error {
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return err
	}
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	return unix.IoctlSetTermios(fd, unix.TCSETS, t)
}
