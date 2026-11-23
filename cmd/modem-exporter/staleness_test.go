package main

import (
	"math"
	"strings"
	"testing"
	"time"

	"dawn-agent/modem"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func sig(rsrp, rsrq, rssi, sinr float64) modem.Signal {
	return modem.Signal{Technology: "lte", Role: "anchor", RSRP: rsrp, RSRQ: rsrq, RSSI: rssi, SINR: sinr}
}

// TestStaleTracker_PerFieldNotAllOrNothing reproduces the real incident
// this tracker was built for: SINR kept varying while RSRP/RSRQ froze.
// Only the frozen fields must be withheld - SINR must keep coming
// through fresh throughout.
func TestStaleTracker_PerFieldNotAllOrNothing(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	tr := newStaleTracker(clock.now)
	key := signalKey{modemID: "/dev/cdc-wdm1", role: "anchor"}

	sinr := 10.0
	var out modem.Signal
	var withheld int
	for i := 0; i < 65; i++ { // 65 * 30s = 32.5 minutes, past the 30-min window
		sinr += 1 // SINR genuinely changes every poll; RSRP/RSRQ/RSSI never do
		out, withheld, _ = tr.Check(key, sig(-82, -11, -49, sinr), false, false)
		clock.advance(30 * time.Second)
	}
	if withheld != 3 {
		t.Fatalf("withheldCount = %d, want 3 (rsrp+rsrq+rssi, all frozen)", withheld)
	}
	if !math.IsNaN(out.RSRP) || !math.IsNaN(out.RSRQ) || !math.IsNaN(out.RSSI) {
		t.Fatalf("a frozen field was not withheld: %+v", out)
	}
	if math.IsNaN(out.SINR) || out.SINR != sinr {
		t.Fatalf("SINR (the one field that keeps changing) withheld or wrong: %+v, want %v", out, sinr)
	}
}

func TestStaleTracker_30MinuteWindow_NotTiedToPollInterval(t *testing.T) {
	// A real, live, stationary RSRP can hold one integer value for
	// several minutes on its own (confirmed live: -79 dBm for ~5 min on
	// 2026-11-19) - must not be withheld well before the 30-minute mark
	// just because it hasn't changed yet.
	clock := &fakeClock{t: time.Unix(2000, 0)}
	tr := newStaleTracker(clock.now)
	key := signalKey{modemID: "/dev/cdc-wdm1", role: "anchor"}

	tr.Check(key, sig(-79, -11, -49, 10), false, false)
	clock.advance(29 * time.Minute)
	_, withheld, _ := tr.Check(key, sig(-79, -11, -49, 10), false, false)
	if withheld != 0 {
		t.Fatalf("withheld=%d at 29 minutes unchanged, want 0 (still within the normal stationary-signal range)", withheld)
	}

	clock.advance(2 * time.Minute) // total 31 min
	_, withheld, msg := tr.Check(key, sig(-79, -11, -49, 10), false, false)
	if withheld != 4 {
		t.Fatalf("withheld=%d at 31 minutes unchanged, want 4", withheld)
	}
	if msg == "" {
		t.Fatalf("expected a stale-metrics log message, got none")
	}
}

func TestStaleTracker_SourceTimestampMode_AllOrNothingPerPoll(t *testing.T) {
	// ubus mode: a stuck source timestamp withholds everything
	// immediately, regardless of per-field change-detection state (even
	// a field that happens to still be changing is withheld, since the
	// whole payload is suspect) - and recovers immediately once the
	// source timestamp starts advancing again.
	clock := &fakeClock{t: time.Unix(3000, 0)}
	tr := newStaleTracker(clock.now)
	key := signalKey{modemID: "lte1", role: "anchor"}

	_, withheld, _ := tr.Check(key, sig(-82, -11, -49, 14), true, true)
	if withheld != 4 {
		t.Fatalf("withheld=%d with sourceStale=true, want 4 (all fields, immediately)", withheld)
	}

	out, withheld, _ := tr.Check(key, sig(-82, -11, -49, 14), true, false)
	if withheld != 0 {
		t.Fatalf("withheld=%d with sourceStale=false, want 0 (recovers immediately, no change-detection delay)", withheld)
	}
	if out.RSRP != -82 {
		t.Fatalf("RSRP = %v, want -82 once fresh again", out.RSRP)
	}
}

func TestStaleTracker_WarnRateLimited(t *testing.T) {
	clock := &fakeClock{t: time.Unix(4000, 0)}
	tr := newStaleTracker(clock.now)
	key := signalKey{modemID: "lte1", role: "anchor"}

	_, _, msg1 := tr.Check(key, sig(-82, -11, -49, 14), true, true)
	if msg1 == "" {
		t.Fatalf("expected a log message on first staleness detection")
	}
	clock.advance(10 * time.Second)
	_, _, msg2 := tr.Check(key, sig(-82, -11, -49, 14), true, true)
	if msg2 != "" {
		t.Fatalf("expected no log message within the 1-minute rate limit, got %q", msg2)
	}
	clock.advance(51 * time.Second) // total 61s since msg1
	_, _, msg3 := tr.Check(key, sig(-82, -11, -49, 14), true, true)
	if msg3 == "" || !strings.Contains(msg3, "source timestamp") {
		t.Fatalf("expected a repeat log message past the 1-minute window, got %q", msg3)
	}
}
