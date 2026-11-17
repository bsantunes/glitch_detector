package main

import (
	"time"

	"dawn-agent/glitch"
)

// flowKey identifies one TCP/UDP flow for matching a STALL_START to its
// eventual STALL_END (or detecting it was abandoned) - Go-side
// bookkeeping only, never written to InfluxDB as a tag. IP addresses and
// ports never become tags anywhere in this package; they're only ever
// carried on glitch_event's fields, keyed per-point, not indexed.
type flowKey struct {
	srcIP, dstIP     string
	srcPort, dstPort uint16
	proto            uint8
}

func keyFor(ev glitch.Event) flowKey {
	return flowKey{ev.SrcIP.String(), ev.DstIP.String(), ev.SrcPort, ev.DstPort, ev.Proto}
}

// abandonedAfter is how long an open episode can go without a STALL_END
// before Aggregator gives up on it and reports it as abandoned - e.g.
// the connection died mid-outage, so the kernel-side episode state (and
// whatever step count it was tracking) was evicted along with the
// flow_tracker entry, and no END will ever arrive to report it normally.
const abandonedAfter = 30 * time.Second

// openEpisode is what Aggregator remembers about a stall episode between
// its STALL_START and whatever closes it (a STALL_END, or a timeout).
// baselineMs is captured at START time specifically so an eventual
// stall_abandoned point can still report it - the schema calls for
// baseline_ms on every glitch_event type, including abandoned ones,
// which have no closing kernel event of their own to read it from.
type openEpisode struct {
	start      time.Time
	baselineMs float64
}

// eventPoint is one glitch_event point to write, already in the shape
// main.go's eventInfluxPoint turns directly into InfluxDB tags/fields -
// see that function and the package doc comment for the schema.
type eventPoint struct {
	Time       time.Time
	Type       string // "stall_start" | "stall_end" | "stall_abandoned"
	Proto      uint8
	SrcPort    uint16
	DstPort    uint16
	DstIP      string
	BaselineMs float64
	DurationMs float64 // only meaningful for stall_end/stall_abandoned
	Steps      uint32  // only meaningful for stall_end; always 0 for
	// stall_abandoned - the kernel only ever reports a step count on
	// STALL_END, which by definition never arrived for an abandoned
	// episode, so the real count is genuinely unknown, not just
	// defaulted to a plausible-looking number.
}

// summaryPoint is one glitch_summary point - see the package doc comment
// for the schema. Always produced once per interval by main.go, even
// when every field is zero (a heartbeat).
type summaryPoint struct {
	StallEpisodes int
	StallMaxMs    float64
	StallSumMs    float64
	StallOpen     int
	RetransCount  int
	DroppedEvents uint64
}

// Aggregator turns a stream of glitch.Events into the points
// glitch-exporter writes to InfluxDB, without ever touching InfluxDB
// itself - kept pure and clock-injectable specifically so it can be unit
// tested with a fake event source and a fake clock, with no root, eBPF,
// or real network involved. main.go is the only thing that wires a real
// glitch.Handle and a real InfluxDB client around it.
type Aggregator struct {
	now             func() time.Time
	maxEventsPerSec int

	open map[flowKey]openEpisode

	// This interval's running counters, reset by Summary. StallOpen is
	// deliberately not one of these - it's a live snapshot of len(open)
	// at Summary time, not something accumulated per-interval.
	stallEpisodes int
	stallMaxMs    float64
	stallSumMs    float64
	retransCount  int

	// Plain per-wall-clock-second rate limiter, shared across every
	// glitch_event point type (stall_start/stall_end/stall_abandoned) -
	// a flat count of points allowed out this second, reset whenever
	// the second rolls over. No new dependency (e.g. golang.org/x/
	// time/rate) needed for this.
	rateSecond int64
	rateCount  int
}

// NewAggregator returns an Aggregator using now for every time-dependent
// decision (rate-limit second boundaries, abandonment timeouts) - pass
// time.Now in production, a fake clock in tests.
func NewAggregator(now func() time.Time, maxEventsPerSec int) *Aggregator {
	return &Aggregator{
		now:             now,
		maxEventsPerSec: maxEventsPerSec,
		open:            make(map[flowKey]openEpisode),
	}
}

// allow reports whether another glitch_event point may be written this
// wall-clock second, and books it against the limit if so. Call exactly
// once per point actually about to be written - never speculatively,
// since a true result consumes one slot of the budget.
func (a *Aggregator) allow() bool {
	sec := a.now().Unix()
	if sec != a.rateSecond {
		a.rateSecond = sec
		a.rateCount = 0
	}
	if a.rateCount >= a.maxEventsPerSec {
		return false
	}
	a.rateCount++
	return true
}

// HandleEvent processes one decoded glitch.Event, unconditionally
// updating internal counters and open-episode tracking, and returns the
// glitch_event point(s) to actually write - nil if this event never gets
// its own point (a retransmission never does - see glitch_summary's
// retrans_count instead) or if -max-events-per-sec was already hit this
// second (the event is still counted either way; only the point itself
// is dropped).
func (a *Aggregator) HandleEvent(ev glitch.Event) []eventPoint {
	switch ev.Type {
	case glitch.KindRetrans:
		a.retransCount++
		return nil

	case glitch.KindStallStart:
		baselineMs := ev.Baseline.Seconds() * 1000
		a.open[keyFor(ev)] = openEpisode{start: ev.Time, baselineMs: baselineMs}
		if !a.allow() {
			return nil
		}
		return []eventPoint{{
			Time:       ev.Time,
			Type:       "stall_start",
			Proto:      ev.Proto,
			SrcPort:    ev.SrcPort,
			DstPort:    ev.DstPort,
			DstIP:      ev.DstIP.String(),
			BaselineMs: baselineMs,
		}}

	case glitch.KindStallEnd:
		delete(a.open, keyFor(ev))
		durationMs := float64(ev.Value) / 1e6
		a.stallEpisodes++
		a.stallSumMs += durationMs
		if durationMs > a.stallMaxMs {
			a.stallMaxMs = durationMs
		}
		if !a.allow() {
			return nil
		}
		return []eventPoint{{
			Time:       ev.Time,
			Type:       "stall_end",
			Proto:      ev.Proto,
			SrcPort:    ev.SrcPort,
			DstPort:    ev.DstPort,
			DstIP:      ev.DstIP.String(),
			BaselineMs: ev.Baseline.Seconds() * 1000,
			DurationMs: durationMs,
			Steps:      ev.Steps,
		}}
	}
	return nil
}

// CheckAbandoned scans open episodes for any with no STALL_END after
// abandonedAfter, removes them from the open set, and returns a
// stall_abandoned point for each - call periodically (shorter than
// abandonedAfter, e.g. every few seconds) for reasonably prompt
// detection rather than only at the next glitch_summary tick, which
// could be minutes away. Abandoned episodes do NOT count toward
// glitch_summary's stall_episodes/stall_max_ms/stall_sum_ms (those are
// literal STALL_ENDs only, by the schema's own definition) - only their
// own glitch_event point reports them at all.
func (a *Aggregator) CheckAbandoned() []eventPoint {
	now := a.now()
	var points []eventPoint
	for key, ep := range a.open {
		if now.Sub(ep.start) < abandonedAfter {
			continue
		}
		delete(a.open, key)
		if !a.allow() {
			continue
		}
		points = append(points, eventPoint{
			Time:       now,
			Type:       "stall_abandoned",
			Proto:      key.proto,
			SrcPort:    key.srcPort,
			DstPort:    key.dstPort,
			DstIP:      key.dstIP,
			BaselineMs: ep.baselineMs,
			DurationMs: float64(now.Sub(ep.start).Milliseconds()),
			Steps:      0,
		})
	}
	return points
}

// Summary builds this interval's glitch_summary point from the running
// counters, then resets them for the next interval - StallOpen (a live
// snapshot of len(open)) is the one field that is not itself
// accumulated/reset, since it describes "right now," not "during this
// interval." droppedEvents is the caller's own responsibility to
// compute (the delta of Handle.Dropped() since the last tick) and pass
// in, since Dropped() belongs to the glitch.Handle, not this aggregator.
func (a *Aggregator) Summary(droppedEvents uint64) summaryPoint {
	s := summaryPoint{
		StallEpisodes: a.stallEpisodes,
		StallMaxMs:    a.stallMaxMs,
		StallSumMs:    a.stallSumMs,
		StallOpen:     len(a.open),
		RetransCount:  a.retransCount,
		DroppedEvents: droppedEvents,
	}
	a.stallEpisodes = 0
	a.stallMaxMs = 0
	a.stallSumMs = 0
	a.retransCount = 0
	return s
}
