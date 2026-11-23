package main

import (
	"fmt"
	"math"
	"strings"
	"time"

	"dawn-agent/modem"
)

// changeDetectionMaxAge: a signal field repeating the identical value
// for this long is withheld - fixed at 30 minutes regardless of
// -interval, not 3x it: a real, live, stationary reading can plausibly
// hold one integer dBm value for several minutes on its own (confirmed
// live on dawn6g: RSRP held at -79 for ~5 minutes straight on
// 2026-11-19, no freeze involved) - a window tied to a short -interval
// would false-positive on exactly that ordinary case. Used only as the
// fallback for backends with no source timestamp of their own to check
// instead - see staleTracker.Check.
const changeDetectionMaxAge = 30 * time.Minute

// warnEvery bounds how often the stale-metrics log line can repeat for
// the same leg.
const warnEvery = time.Minute

// signalKey identifies one radio leg being tracked for staleness —
// keyed by modem ID and role (anchor/scg), not by technology, since a
// leg's technology can itself only be known once a real reading comes
// in.
type signalKey struct {
	modemID string
	role    string
}

// fieldTracker remembers one signal metric's last value and when it
// last actually changed - not just when it was last fetched, which a
// frozen source updates just as readily as a live one.
type fieldTracker struct {
	last     float64
	changed  time.Time
	hasValue bool
}

// fresh reports whether v should be treated as live right now, per
// change-detection alone: true if it's the first reading, or it
// differs from the last one seen; false (stale) once it has been
// identical to the last value for at least maxAge. NaN is never flagged
// stale on its own - a leg that consistently has no reading for one
// field isn't what this check is for, only a repeating NON-NaN value is
// suspicious.
func (f *fieldTracker) fresh(v float64, now time.Time, maxAge time.Duration) bool {
	if !f.hasValue || !sameFloat(f.last, v) {
		f.last, f.changed, f.hasValue = v, now, true
		return true
	}
	if math.IsNaN(v) {
		return true
	}
	return now.Sub(f.changed) < maxAge
}

func sameFloat(a, b float64) bool {
	return a == b || (math.IsNaN(a) && math.IsNaN(b))
}

type signalState struct {
	rsrp, rsrq, rssi, sinr fieldTracker
	lastWarn               time.Time
}

// staleTracker remembers, per radio leg and per field, the signal
// values seen so far - see Check's own doc comment for the two
// staleness mechanisms it applies (a source's own timestamp where one
// exists, change-detection otherwise).
type staleTracker struct {
	now    func() time.Time
	states map[signalKey]*signalState
}

func newStaleTracker(now func() time.Time) *staleTracker {
	return &staleTracker{now: now, states: make(map[signalKey]*signalState)}
}

// Check returns the Signal to actually write for key (sig, with any
// stale field(s) replaced by NaN - never the whole Signal, since the
// real incident this was built for had SINR still moving while
// RSRP/RSRQ were stuck), how many of its four metric fields were
// withheld this call (0-4, counting only fields that actually held a
// real, non-NaN value before being withheld), and a non-empty log
// message at most once per warnEvery.
//
// Two different staleness mechanisms, chosen per backend by the caller:
//   - hasSourceTimestamp true (ubus only - see UbusMetricsTimestamp):
//     sourceStale is authoritative for all four fields at once, since
//     ubus exposes one timestamp for its whole payload, not one per
//     field - change-detection is not applied at all in this mode.
//   - hasSourceTimestamp false (every other backend, which has no
//     source timestamp to check): each field is judged independently by
//     change-detection against changeDetectionMaxAge.
func (t *staleTracker) Check(key signalKey, sig modem.Signal, hasSourceTimestamp, sourceStale bool) (out modem.Signal, withheldCount int, logMsg string) {
	now := t.now()
	st, ok := t.states[key]
	if !ok {
		st = &signalState{}
		t.states[key] = st
	}

	out = sig
	type field struct {
		name string
		val  *float64
		tr   *fieldTracker
	}
	fields := []field{
		{"rsrp", &out.RSRP, &st.rsrp},
		{"rsrq", &out.RSRQ, &st.rsrq},
		{"rssi", &out.RSSI, &st.rssi},
		{"sinr", &out.SINR, &st.sinr},
	}

	var staleNames []string
	for _, f := range fields {
		hadRealValue := !math.IsNaN(*f.val)
		var stale bool
		if hasSourceTimestamp {
			// Still track the value for continuity in case this leg
			// ever switches to the other mode, but the verdict comes
			// from the source timestamp alone, not from this.
			f.tr.last, f.tr.hasValue = *f.val, true
			stale = sourceStale
		} else {
			stale = !f.tr.fresh(*f.val, now, changeDetectionMaxAge)
		}
		if stale && hadRealValue {
			staleNames = append(staleNames, f.name)
			*f.val = math.NaN()
			withheldCount++
		}
	}

	if len(staleNames) > 0 && now.Sub(st.lastWarn) >= warnEvery {
		st.lastWarn = now
		how := fmt.Sprintf("change-detection, %s", changeDetectionMaxAge)
		if hasSourceTimestamp {
			how = "source timestamp not advancing"
		}
		logMsg = fmt.Sprintf("stale metrics (%s; %s)", strings.Join(staleNames, ", "), how)
	}
	return out, withheldCount, logMsg
}
