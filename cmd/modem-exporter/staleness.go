package main

import (
	"fmt"
	"math"
	"time"

	"dawn-agent/modem"
)

// staleAfter: a signal reading that hasn't changed in this long is
// treated as stale and withheld, rather than re-written as if it were a
// fresh live measurement. 3x the poll interval, per its own doc comment
// below, is set by main() once -interval is known.
//
// Why change-detection, not a source-provided timestamp: confirmed live
// on dawn6g that a frozen QMI-backend read (qmicli --nas-get-signal-info,
// run fresh every poll — see modem/qmi.go) can return bit-for-bit
// identical values for 2.5 days straight with no error at all, almost
// certainly QMI client-side contention with WaveOS's own uqmi-ubus
// daemon rather than a bug in this repo's own code (nothing here caches
// qmicli's output). qmicli's text output carries no timestamp of its
// own to check against. The ubus backend DOES expose one (confirmed
// live: its "qmi" object's network/metrics calls include a "timestamp"
// field, which froze at the same moment this exact kind of staleness
// started) — but relying on a source timestamp only where one happens
// to exist would leave every other backend uncovered, so change-
// detection is the single mechanism applied uniformly to all of them.
// A real value that genuinely hasn't moved for 3 intervals (a modem
// sitting still with a stable link) is rare enough in practice, and the
// cost of a false positive (withholding a few points) is far lower than
// the cost of silently writing 2.5 days of a dead reading as live data.

// signalKey identifies one radio leg being tracked for staleness —
// keyed by modem ID and role (anchor/scg), not by technology, since a
// leg's technology can itself only be known once a real reading comes
// in.
type signalKey struct {
	modemID string
	role    string
}

type signalState struct {
	last      modem.Signal
	changedAt time.Time
	lastWarn  time.Time
}

// staleTracker remembers, per radio leg, the last signal values seen and
// when they last actually changed — not just when they were last
// fetched, which a frozen source updates just as readily as a live one.
type staleTracker struct {
	maxAge   time.Duration
	warnEvery time.Duration
	now      func() time.Time
	states   map[signalKey]*signalState
}

func newStaleTracker(maxAge time.Duration, now func() time.Time) *staleTracker {
	return &staleTracker{
		maxAge:    maxAge,
		warnEvery: time.Minute,
		now:       now,
		states:    make(map[signalKey]*signalState),
	}
}

// signalsEqual compares the four metrics a staleness check cares about,
// treating two NaNs in the same field as equal (a leg that's
// consistently reporting "no data" for one field isn't what this check
// is for — only a repeating NON-NaN value is suspicious).
func signalsEqual(a, b modem.Signal) bool {
	eq := func(x, y float64) bool {
		return x == y || (math.IsNaN(x) && math.IsNaN(y))
	}
	return eq(a.RSRP, b.RSRP) && eq(a.RSRQ, b.RSRQ) && eq(a.RSSI, b.RSSI) && eq(a.SINR, b.SINR)
}

// Check returns the Signal to actually write for key: sig unchanged if
// it's fresh (new, or changed since the last poll), or an all-NaN
// Signal (same Technology/Role, no metrics) if it has been identical for
// at least maxAge. logMsg is non-empty at most once per warnEvery
// window, for the caller to log — staleness is worth knowing about
// continuously, not just the one time it's first detected.
func (t *staleTracker) Check(key signalKey, sig modem.Signal) (out modem.Signal, stale bool, logMsg string) {
	now := t.now()
	st, ok := t.states[key]
	if !ok || !signalsEqual(st.last, sig) {
		t.states[key] = &signalState{last: sig, changedAt: now}
		return sig, false, ""
	}
	st.last = sig // keep Technology/Role current even while stale
	age := now.Sub(st.changedAt)
	if age < t.maxAge {
		return sig, false, ""
	}
	withheld := sig
	withheld.RSRP, withheld.RSRQ, withheld.RSSI, withheld.SINR = math.NaN(), math.NaN(), math.NaN(), math.NaN()
	if now.Sub(st.lastWarn) >= t.warnEvery {
		st.lastWarn = now
		logMsg = fmt.Sprintf("stale metrics (age %s)", age.Round(time.Second))
	}
	return withheld, true, logMsg
}
