//go:build linux

package gps

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"dawn-agent/modem"
)

// openPTY returns a pty master (non-blocking) and its slave's path —
// the slave stands in for a modem's NMEA port.
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

const (
	testRMC = "$GNRMC,221923.00,A,4132.882219,N,00824.986564,W,0.0,,241126,5.3,W,A,V*70\r\n"
	testGGA = "$GNGGA,221923.00,4132.882219,N,00824.986564,W,1,06,1.8,197.8,M,54.0,M,,*62\r\n"
)

// feed writes one RMC+GGA pair to the master every 100ms until stop is
// closed.
func feed(t *testing.T, master int, stop <-chan struct{}) {
	t.Helper()
	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				_, _ = unix.Write(master, []byte(testRMC+testGGA))
			}
		}
	}()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestPoller_StaleNMEAFallsBackToAT: while sentences arrive, Fix comes
// from NMEA; once the stream goes quiet for nmeaStaleAfter, Fix polls
// AT instead; when sentences resume, it switches back.
func TestPoller_StaleNMEAFallsBackToAT(t *testing.T) {
	oldStale := nmeaStaleAfter
	nmeaStaleAfter = 400 * time.Millisecond
	t.Cleanup(func() { nmeaStaleAfter = oldStale })

	master, slave := openPTY(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := OpenNMEAReader(ctx, slave)
	defer r.Close()
	var atCalls atomic.Int32
	p := &Poller{
		m:    modem.Modem{Port: "test"},
		nmea: r,
		atFallback: func() (Fix, error) {
			atCalls.Add(1)
			return Fix{Source: "at", Time: time.Now()}, nil
		},
	}

	stop := make(chan struct{})
	feed(t, master, stop)
	waitFor(t, "a valid NMEA fix", func() bool {
		f, _ := p.Fix(ctx)
		return f.Valid && f.Source == "nmea"
	})
	if f, _ := p.Fix(ctx); f.Satellites != 6 || f.HDOP != 1.8 {
		t.Errorf("GGA fields not merged: sats=%d hdop=%v", f.Satellites, f.HDOP)
	}
	if atCalls.Load() != 0 {
		t.Fatalf("AT polled while the NMEA stream was alive (%d calls)", atCalls.Load())
	}

	close(stop) // stream goes quiet
	waitFor(t, "fallback to AT", func() bool {
		f, _ := p.Fix(ctx)
		return f.Source == "at"
	})
	if !p.onAT || atCalls.Load() == 0 {
		t.Fatalf("expected AT fallback (onAT=%v, calls=%d)", p.onAT, atCalls.Load())
	}

	stop2 := make(chan struct{})
	defer close(stop2)
	feed(t, master, stop2) // stream resumes
	waitFor(t, "switch back to NMEA", func() bool {
		f, _ := p.Fix(ctx)
		return f.Valid && f.Source == "nmea"
	})
	if p.onAT {
		t.Error("still flagged as on AT after the stream resumed")
	}
}

// TestPoller_NMEANeverStartsFallsBackToAT: a reader whose port never
// produces anything (stuck setup, wrong port) must not leave Fix
// reporting "no fix" forever — AT takes over once the reader has been
// silent for nmeaStaleAfter since it started.
func TestPoller_NMEANeverStartsFallsBackToAT(t *testing.T) {
	oldStale, oldOpen := nmeaStaleAfter, openNMEAPort
	nmeaStaleAfter = 300 * time.Millisecond
	block := make(chan struct{})
	openNMEAPort = func(string) (*os.File, error) { <-block; return nil, fmt.Errorf("never") }
	t.Cleanup(func() { nmeaStaleAfter, openNMEAPort = oldStale, oldOpen; close(block) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := OpenNMEAReader(ctx, "/dev/stuck")
	defer r.Close()
	p := &Poller{m: modem.Modem{Port: "test"}, nmea: r,
		atFallback: func() (Fix, error) { return Fix{Source: "at"}, nil }}

	waitFor(t, "fallback to AT", func() bool {
		f, _ := p.Fix(ctx)
		return f.Source == "at"
	})
}

// TestNMEAReader_CloseWithStuckSetup: port setup blocks forever (what
// v2.3.1's stty did on aaeon). Close must still return promptly.
func TestNMEAReader_CloseWithStuckSetup(t *testing.T) {
	oldOpen := openNMEAPort
	block := make(chan struct{})
	openNMEAPort = func(string) (*os.File, error) { <-block; return nil, fmt.Errorf("never") }
	t.Cleanup(func() { openNMEAPort = oldOpen; close(block) })

	r := OpenNMEAReader(context.Background(), "/dev/stuck")
	time.Sleep(100 * time.Millisecond) // let setup start and block
	assertClosesQuickly(t, r)
}

// TestNMEAReader_CloseWithStuckRead: the port is open but never sends
// anything, so the reader sits in Read. Close must still return
// promptly.
func TestNMEAReader_CloseWithStuckRead(t *testing.T) {
	_, slave := openPTY(t)
	r := OpenNMEAReader(context.Background(), slave)
	time.Sleep(300 * time.Millisecond) // let it open the port and block in Read
	assertClosesQuickly(t, r)
}

func assertClosesQuickly(t *testing.T, r *NMEAReader) {
	t.Helper()
	done := make(chan struct{})
	start := time.Now()
	go func() { r.Close(); close(done) }()
	select {
	case <-done:
		if d := time.Since(start); d > time.Second {
			t.Errorf("Close took %s", d)
		}
	case <-time.After(nmeaCloseTimeout + time.Second):
		t.Fatal("Close did not return")
	}
}
