//go:build linux

package modem

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// openPTY returns a pty master (non-blocking) and its slave's path.
func openPTY(t *testing.T) (master int, slave string) {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatalf("unlock pty: %v", err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatalf("pty number: %v", err)
	}
	return fd, fmt.Sprintf("/dev/pts/%d", n)
}

// TestOpenRawSerial_EchoingTTYDoesNotBlock recreates what aaeon's
// SIM8202G NMEA port looked like after boot: a tty still in its default
// cooked mode with echo on, a stream of incoming lines, and nobody
// draining the echoed output. Port setup must return promptly, leave
// echo off, and discard the backlog.
//
// Limitation, checked on aaeon: a pty does NOT reproduce the v2.3.1
// hang itself — `stty raw -echo` (TCSADRAIN) returns at once on this
// exact setup, because the pty driver reports no pending output to
// wait for, unlike the SIM8202G's USB-serial driver. So this test pins
// down the new setup's behaviour (no blocking, echo off, flushed); the
// regression check for the original hang is the hardware run on aaeon
// (port left in its post-boot echo state, no manual flush).
func TestOpenRawSerial_EchoingTTYDoesNotBlock(t *testing.T) {
	master, slave := openPTY(t)

	// Hold the slave open in its default (echo, icanon) mode, the
	// state the port is in before anything configures it.
	holder, err := os.OpenFile(slave, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open slave: %v", err)
	}
	defer holder.Close()
	if tio, err := unix.IoctlGetTermios(int(holder.Fd()), unix.TCGETS); err != nil || tio.Lflag&unix.ECHO == 0 {
		t.Fatalf("expected the fresh slave to have echo on (err=%v)", err)
	}

	// Feed NMEA-like lines until the pty pushes back, never reading the
	// master — so the echoed copies pile up with nowhere to go.
	line := []byte("$GNRMC,221923.00,A,4132.882219,N,00824.986564,W,0.0,,241126,5.3,W,A,V*70\r\n")
	for i := 0; i < 10000; i++ {
		if _, err := unix.Write(master, line); err != nil {
			if errors.Is(err, unix.EAGAIN) {
				break
			}
			t.Fatalf("write master: %v", err)
		}
	}

	type result struct {
		f   *os.File
		err error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := OpenRawSerial(slave, os.O_RDWR)
		ch <- result{f, err}
	}()
	var f *os.File
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("OpenRawSerial: %v", r.err)
		}
		f = r.f
	case <-time.After(2 * time.Second):
		t.Fatal("OpenRawSerial blocked on an echoing tty with an undrained backlog")
	}
	defer f.Close()

	tio, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatalf("TCGETS: %v", err)
	}
	if tio.Lflag&(unix.ECHO|unix.ICANON) != 0 {
		t.Errorf("echo/icanon still set after setup: lflag=%#x", tio.Lflag)
	}

	// The backlog was flushed: nothing stale is readable right away.
	_ = f.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 4096)
	if n, _ := f.Read(buf); n > 0 {
		t.Errorf("stale input survived setup: %q", buf[:n])
	}

	// Fresh data arrives raw and without being echoed back.
	drainMaster(master)
	if _, err := unix.Write(master, []byte("$GNGGA,fresh\r\n")); err != nil {
		t.Fatalf("write master: %v", err)
	}
	_ = f.SetReadDeadline(time.Now().Add(time.Second))
	n, err := f.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), "$GNGGA,fresh") {
		t.Errorf("fresh line not read raw: n=%d err=%v got=%q", n, err, buf[:n])
	}
	time.Sleep(100 * time.Millisecond)
	if echoed := drainMaster(master); strings.Contains(echoed, "fresh") {
		t.Errorf("input was echoed back after setup: %q", echoed)
	}
}

// drainMaster reads and returns whatever is currently readable on the
// non-blocking pty master.
func drainMaster(master int) string {
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := unix.Read(master, buf)
		if n <= 0 || err != nil {
			return sb.String()
		}
		sb.Write(buf[:n])
	}
}
