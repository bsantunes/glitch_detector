package main

import (
	"net"
	"testing"
	"time"

	"dawn-agent/glitch"
)

// fakeClock lets tests control exactly what Aggregator sees as "now,"
// instead of depending on real wall-clock timing - which would make
// rate-limit-second-boundary and abandonment-timeout tests flaky.
type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time         { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func mustIP(s string) net.IP {
	ip := net.ParseIP(s)
	if ip == nil {
		panic("bad IP in test: " + s)
	}
	return ip
}

func TestSummaryAggregation(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	agg := NewAggregator(clock.now, 100)

	// Zero interval: Summary must still return a valid (all-zero) point
	// without any prior event - this is what lets glitch_summary act as
	// a heartbeat, written every tick regardless of activity.
	if s := agg.Summary(0, DropCounts{}); s != (summaryPoint{}) {
		t.Fatalf("zero-interval Summary = %+v, want all zero", s)
	}

	srcIP, dstIP := mustIP("10.0.0.1"), mustIP("10.0.0.2")
	ev := func(kind glitch.Kind, valueNs uint64, steps uint32) glitch.Event {
		return glitch.Event{
			Time: clock.now(), SrcIP: srcIP, DstIP: dstIP,
			SrcPort: 1000, DstPort: 2000, Proto: 6,
			Type: kind, Value: valueNs, Steps: steps,
			Baseline: 100 * time.Millisecond,
		}
	}

	// One flow: START then END, 500ms duration, steps=3.
	agg.HandleEvent(ev(glitch.KindStallStart, 0, 0))
	if s := agg.Summary(0, DropCounts{}); s.StallOpen != 1 {
		t.Fatalf("after START, StallOpen = %d, want 1", s.StallOpen)
	}
	agg.HandleEvent(ev(glitch.KindStallEnd, uint64(500*time.Millisecond), 3))

	// A second, independent flow, with a bigger duration (900ms) so
	// max/sum both have something to distinguish from stallEpisodes*X.
	ev2 := glitch.Event{
		Time: clock.now(), SrcIP: srcIP, DstIP: mustIP("10.0.0.3"),
		SrcPort: 1001, DstPort: 2001, Proto: 6,
		Type: glitch.KindStallStart, Baseline: 100 * time.Millisecond,
	}
	agg.HandleEvent(ev2)
	ev2.Type = glitch.KindStallEnd
	ev2.Value = uint64(900 * time.Millisecond)
	ev2.Steps = 5
	agg.HandleEvent(ev2)

	// Retransmissions are never seen via HandleEvent at all any more -
	// both capture backends count them directly instead of emitting them
	// as Events (see glitch.Source.RetransCount) - so a KindRetrans event
	// reaching HandleEvent regardless falls through every case and
	// returns nil, same as any other event type this aggregator doesn't
	// recognize.
	if pts := agg.HandleEvent(ev(glitch.KindRetrans, 1234, 0)); pts != nil {
		t.Fatalf("an unrecognized/retrans event returned points %+v, want nil", pts)
	}

	// retransCount is threaded straight through from the caller (main.go
	// computes it as a delta of Source.RetransCount()), not accumulated
	// from events - 11 here is an arbitrary caller-supplied value.
	s := agg.Summary(11, DropCounts{Total: 7}) // dropped_events delta passed in by the caller
	want := summaryPoint{StallEpisodes: 2, StallMaxMs: 900, StallSumMs: 1400, StallOpen: 0, RetransCount: 11, DroppedEvents: 7}
	if s != want {
		t.Fatalf("Summary = %+v, want %+v", s, want)
	}

	// Summary resets the per-interval counters for the next one.
	if s := agg.Summary(0, DropCounts{}); s != (summaryPoint{}) {
		t.Fatalf("Summary after reset = %+v, want all-zero", s)
	}
}

func TestRateLimiting(t *testing.T) {
	clock := &fakeClock{t: time.Unix(2000, 0)}
	const limit = 3
	agg := NewAggregator(clock.now, limit)

	srcIP, dstIP := mustIP("10.0.0.1"), mustIP("10.0.0.2")
	newStart := func(port uint16) glitch.Event {
		return glitch.Event{
			Time: clock.now(), SrcIP: srcIP, DstIP: dstIP,
			SrcPort: port, DstPort: 2000, Proto: 6,
			Type: glitch.KindStallStart, Baseline: 100 * time.Millisecond,
		}
	}

	// 10 distinct flows' STALL_STARTs, all within the same fake second -
	// only `limit` of them should produce a point.
	written := 0
	for i := uint16(0); i < 10; i++ {
		written += len(agg.HandleEvent(newStart(i)))
	}
	if written != limit {
		t.Fatalf("wrote %d points in one second, want exactly %d (the limit)", written, limit)
	}

	// All 10 are still tracked as open regardless of the limit - it only
	// governs whether a point gets written, never whether the
	// underlying state (and hence glitch_summary's own counts) updates.
	if s := agg.Summary(0, DropCounts{}); s.StallOpen != 10 {
		t.Fatalf("StallOpen = %d, want 10 (rate limit must not drop tracking)", s.StallOpen)
	}

	// Advancing to the next wall-clock second resets the budget.
	clock.advance(time.Second)
	if pts := agg.HandleEvent(newStart(100)); len(pts) != 1 {
		t.Fatalf("first event of a new second returned %d points, want 1", len(pts))
	}
}

func TestAbandonedEpisodeTimeout(t *testing.T) {
	clock := &fakeClock{t: time.Unix(3000, 0)}
	agg := NewAggregator(clock.now, 100)

	srcIP, dstIP := mustIP("10.0.0.1"), mustIP("10.0.0.2")
	start := glitch.Event{
		Time: clock.now(), SrcIP: srcIP, DstIP: dstIP,
		SrcPort: 1000, DstPort: 2000, Proto: 6,
		Type: glitch.KindStallStart, Baseline: 250 * time.Millisecond,
	}
	agg.HandleEvent(start)

	// Well under the timeout: nothing abandoned yet.
	clock.advance(10 * time.Second)
	if pts := agg.CheckAbandoned(); len(pts) != 0 {
		t.Fatalf("CheckAbandoned at +10s returned %d points, want 0", len(pts))
	}
	if s := agg.Summary(0, DropCounts{}); s.StallOpen != 1 {
		t.Fatalf("StallOpen at +10s = %d, want 1 (still open)", s.StallOpen)
	}

	// Past the 30s timeout: exactly one stall_abandoned point, carrying
	// the duration-so-far and the flow's own fields - but steps=0,
	// genuinely unknown rather than a fabricated count.
	clock.advance(25 * time.Second) // total elapsed: 35s
	pts := agg.CheckAbandoned()
	if len(pts) != 1 {
		t.Fatalf("CheckAbandoned at +35s returned %d points, want 1", len(pts))
	}
	p := pts[0]
	if p.Type != "stall_abandoned" {
		t.Errorf("Type = %q, want stall_abandoned", p.Type)
	}
	if p.DstIP != dstIP.String() || p.SrcPort != 1000 || p.DstPort != 2000 || p.Proto != 6 {
		t.Errorf("abandoned point lost the flow's own fields: %+v", p)
	}
	if p.BaselineMs != 250 {
		t.Errorf("BaselineMs = %v, want 250 (captured at START)", p.BaselineMs)
	}
	if p.DurationMs < 35000 {
		t.Errorf("DurationMs = %v, want >= 35000 (35s elapsed)", p.DurationMs)
	}
	if p.Steps != 0 {
		t.Errorf("Steps = %d, want 0 (unknown for an abandoned episode)", p.Steps)
	}

	// It's gone from the open set now, and reporting it once is enough -
	// a later check must not re-report the same episode.
	if s := agg.Summary(0, DropCounts{}); s.StallOpen != 0 {
		t.Fatalf("StallOpen after abandonment = %d, want 0", s.StallOpen)
	}
	clock.advance(time.Minute)
	if pts := agg.CheckAbandoned(); len(pts) != 0 {
		t.Fatalf("CheckAbandoned re-reported an already-abandoned episode: %+v", pts)
	}

	// Abandonment must never count toward glitch_summary's own
	// stall_episodes/stall_max_ms/stall_sum_ms - those are STALL_ENDs
	// only, by the schema's own definition.
	if s := agg.Summary(0, DropCounts{}); s.StallEpisodes != 0 || s.StallMaxMs != 0 || s.StallSumMs != 0 {
		t.Fatalf("abandonment leaked into Summary: %+v", s)
	}
}
