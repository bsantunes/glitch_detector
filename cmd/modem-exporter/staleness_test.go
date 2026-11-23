package main

import (
	"math"
	"testing"
	"time"

	"dawn-agent/modem"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func sig(rsrp float64) modem.Signal {
	return modem.Signal{Technology: "lte", Role: "anchor", RSRP: rsrp, RSRQ: -11, RSSI: -49, SINR: 10}
}

func TestStaleTracker_FrozenSourceWithheldAfterMaxAge(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	tr := newStaleTracker(90*time.Second, clock.now) // 3x a 30s interval
	key := signalKey{modemID: "/dev/cdc-wdm1", role: "anchor"}

	// First read: always fresh, never withheld.
	out, stale, _ := tr.Check(key, sig(-82))
	if stale || math.IsNaN(out.RSRP) {
		t.Fatalf("first read: stale=%v out=%+v, want fresh with real RSRP", stale, out)
	}

	// Same value repeated, but not yet 3 intervals since it last changed
	// (a frozen source, this early, looks identical to a real value that
	// just hasn't moved yet — not stale on its own).
	clock.advance(60 * time.Second)
	out, stale, _ = tr.Check(key, sig(-82))
	if stale || math.IsNaN(out.RSRP) {
		t.Fatalf("at 60s unchanged: stale=%v out=%+v, want still fresh", stale, out)
	}

	// Past maxAge with the identical value: now withheld.
	clock.advance(31 * time.Second) // age = 91s > 90s
	out, stale, msg := tr.Check(key, sig(-82))
	if !stale {
		t.Fatalf("at 91s unchanged: stale=false, want true")
	}
	if !math.IsNaN(out.RSRP) || !math.IsNaN(out.RSRQ) || !math.IsNaN(out.RSSI) || !math.IsNaN(out.SINR) {
		t.Fatalf("stale signal fields not all withheld: %+v", out)
	}
	if out.Technology != "lte" || out.Role != "anchor" {
		t.Fatalf("stale signal lost its Technology/Role: %+v", out)
	}
	if msg == "" {
		t.Fatalf("expected a stale-metrics log message on first detection, got none")
	}

	// A second poll one second later, still frozen: no repeat log yet
	// (rate-limited to once per minute).
	clock.advance(1 * time.Second)
	_, stale, msg = tr.Check(key, sig(-82))
	if !stale {
		t.Fatalf("still frozen: stale=false, want true")
	}
	if msg != "" {
		t.Fatalf("expected no log message within the 1-minute rate limit, got %q", msg)
	}

	// A genuinely new value arrives: fresh again immediately, no NaN.
	out, stale, _ = tr.Check(key, sig(-81))
	if stale || math.IsNaN(out.RSRP) || out.RSRP != -81 {
		t.Fatalf("after a real change: stale=%v out=%+v, want fresh RSRP=-81", stale, out)
	}
}

func TestStaleTracker_RepeatedNaNFieldsAreNotFlaggedStale(t *testing.T) {
	// A leg that consistently has no SINR (NaN) shouldn't trip staleness
	// just because that one field never changes — only a repeating
	// REAL value is suspicious. Here RSRP does change every poll, so the
	// leg should never be withheld even though SINR stays NaN throughout.
	clock := &fakeClock{t: time.Unix(2000, 0)}
	tr := newStaleTracker(90*time.Second, clock.now)
	key := signalKey{modemID: "/dev/ttyUSB0", role: "anchor"}

	rsrp := -90.0
	for i := 0; i < 6; i++ {
		s := modem.Signal{Technology: "lte", Role: "anchor", RSRP: rsrp, RSRQ: math.NaN(), RSSI: math.NaN(), SINR: math.NaN()}
		_, stale, _ := tr.Check(key, s)
		if stale {
			t.Fatalf("iteration %d: flagged stale, want fresh (RSRP keeps changing)", i)
		}
		rsrp -= 1
		clock.advance(30 * time.Second)
	}
}
