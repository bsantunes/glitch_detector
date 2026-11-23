package core

import (
	"net"
	"testing"
	"time"

	"dawn-agent/glitch"
)

var (
	testUs     = net.IPv4(10, 9, 9, 1)
	testRemote = net.IPv4(10, 9, 9, 2)
)

const (
	testSrcPort = 51000
	testDstPort = 5999
)

func egressPkt(t time.Time, seq uint32, payloadLen uint16) PacketMeta {
	return PacketMeta{
		Time: t, Direction: DirEgress,
		SrcIP: testUs, DstIP: testRemote,
		SrcPort: testSrcPort, DstPort: testDstPort, Proto: protoTCP,
		Seq: seq, PayloadLen: payloadLen,
	}
}

func ingressAckPkt(t time.Time, ack uint32) PacketMeta {
	// On the wire this packet's src is the remote peer, dst is us —
	// real-world shape, not pre-swapped; handleIngress does the swap.
	return PacketMeta{
		Time: t, Direction: DirIngress,
		SrcIP: testRemote, DstIP: testUs,
		SrcPort: testDstPort, DstPort: testSrcPort, Proto: protoTCP,
		Ack: ack,
	}
}

func testFlowKey() flowKey {
	var src, dst [4]byte
	copy(src[:], testUs.To4())
	copy(dst[:], testRemote.To4())
	return flowKey{srcIP: src, dstIP: dst, srcPort: testSrcPort, dstPort: testDstPort, proto: protoTCP}
}

func TestEngine_NewFlow_NoEventAndSeedsHighestAckFromFirstSeq(t *testing.T) {
	e := NewEngine(nil)
	base := time.Now()
	events := e.HandlePacket(egressPkt(base, 1000, 100))
	if len(events) != 0 {
		t.Fatalf("first packet on a new flow must not emit any event, got %+v", events)
	}
	state := e.flows[testFlowKey()]
	if state == nil {
		t.Fatalf("expected a flow entry to have been created")
	}
	// The historical bug this guards against: highestAck seeded at a
	// bare 0 silently disabled stall detection for ~50%% of real
	// connections (random ISN placement) — see CLAUDE.md and
	// tc_glitch.bpf.c's new-flow comment.
	if state.highestAck != 1000 {
		t.Errorf("highestAck = %d, want 1000 (seeded from the first packet's own seq, not 0)", state.highestAck)
	}
	if state.endSeq != 1100 {
		t.Errorf("endSeq = %d, want 1100 (seq + payloadLen)", state.endSeq)
	}
}

// warmUp feeds n packets (default pktCount reaches stallMinPackets at
// n=8), each gap microGap apart, each advancing seq by payloadLen —
// enough to pass the EWMA warm-up and establish a representative
// baseline before a test's real scenario begins.
func warmUp(e *Engine, start time.Time, n int, microGap time.Duration, payloadLen uint16) (lastTime time.Time, nextSeq uint32) {
	seq := uint32(1000)
	tm := start
	for i := 0; i < n; i++ {
		e.HandlePacket(egressPkt(tm, seq, payloadLen))
		seq += uint32(payloadLen)
		tm = tm.Add(microGap)
	}
	return tm, seq
}

func TestEngine_StallDetection_EligibleWhenDataOutstanding(t *testing.T) {
	e := NewEngine(nil)
	base := time.Now()
	// 8 warm-up packets, each carrying payload and never acknowledged
	// (no ingress packet sent in this test) — end_seq keeps growing
	// past highestAck, so this flow has real outstanding data by
	// construction, same as tc_glitch.bpf.c's eligibility gate.
	tm, seq := warmUp(e, base, stallMinPackets, 10*time.Millisecond, 100)

	// A big gap now should trigger a stall.
	tm = tm.Add(500 * time.Millisecond)
	events := e.HandlePacket(egressPkt(tm, seq, 100))
	if len(events) != 1 || events[0].Type != glitch.KindStallStart {
		t.Fatalf("expected one StallStart event, got %+v", events)
	}
	seq += 100

	// Recover with a normal-paced packet — expect StallEnd.
	tm = tm.Add(10 * time.Millisecond)
	events = e.HandlePacket(egressPkt(tm, seq, 100))
	if len(events) != 1 || events[0].Type != glitch.KindStallEnd {
		t.Fatalf("expected one StallEnd event, got %+v", events)
	}
	if events[0].Steps != 1 {
		t.Errorf("Steps = %d, want 1 (one qualifying gap)", events[0].Steps)
	}
}

func TestEngine_StallDetection_NotEligibleWhenFullyAcked(t *testing.T) {
	e := NewEngine(nil)
	base := time.Now()
	// Pure keepalives (payloadLen 0 throughout): end_seq never
	// advances past the seeded highestAck, so there is never any
	// outstanding data — a stall must not be flagged no matter how
	// big the gap gets.
	tm, seq := warmUp(e, base, stallMinPackets, 10*time.Millisecond, 0)

	tm = tm.Add(2 * time.Second)
	events := e.HandlePacket(egressPkt(tm, seq, 0))
	if len(events) != 0 {
		t.Fatalf("expected no events for an idle (fully-acked) flow, got %+v", events)
	}
}

func TestEngine_RetransDetection(t *testing.T) {
	e := NewEngine(nil)
	base := time.Now()
	e.HandlePacket(egressPkt(base, 1000, 100)) // end_seq -> 1100

	// Resend starting at 1000 (behind end_seq) with real payload — counted
	// directly (RetransCount), not emitted as an Event at all any more:
	// see RetransCount's own doc comment for why (retransmissions vastly
	// outnumber stall events during a real burst, so keeping them off the
	// Events channel entirely removes their share of channel-depth
	// pressure, rather than just processing them faster once sent).
	events := e.HandlePacket(egressPkt(base.Add(time.Millisecond), 1000, 50))
	if len(events) != 0 {
		t.Fatalf("expected no Events for a retransmission, got %+v", events)
	}
	if got := e.RetransCount(); got != 1 {
		t.Fatalf("RetransCount() = %d, want 1", got)
	}
}

func TestEngine_RetransDetection_PureACKNeverFlagged(t *testing.T) {
	e := NewEngine(nil)
	base := time.Now()
	e.HandlePacket(egressPkt(base, 1000, 100)) // end_seq -> 1100

	// Same seq repeated with payloadLen 0 (a pure ACK/keepalive) must
	// never be flagged — it has no data to have been "resent" at all.
	events := e.HandlePacket(egressPkt(base.Add(time.Millisecond), 1000, 0))
	if len(events) != 0 {
		t.Fatalf("expected no events for a payload-less repeat, got %+v", events)
	}
	if got := e.RetransCount(); got != 0 {
		t.Fatalf("RetransCount() = %d, want 0 (nothing was actually retransmitted)", got)
	}
}

// TestEngine_RetransBurst_NeverProducesEvents is the invariant that
// makes zero user-side channel drops possible under a real burst,
// confirmed live on dawn6g's own link (iperf3_retr~1000-1100 causing
// real UserDropped-equivalent channel drops even after glitch-tool's
// stdout was buffered): however many retransmissions arrive, however
// fast, none of them are ever candidates for a channel send at all — a
// capture layer forwarding HandlePacket's returned Events onto a
// bounded channel (glitch/afpacket.Capture, glitch/glitch.go's eBPF
// readLoop) can never experience a retransmission-caused drop, by
// construction, regardless of that channel's size.
func TestEngine_RetransBurst_NeverProducesEvents(t *testing.T) {
	e := NewEngine(nil)
	base := time.Now()
	e.HandlePacket(egressPkt(base, 1000, 100)) // end_seq -> 1100

	const burst = 5000
	var produced int
	for i := 0; i < burst; i++ {
		// Same already-sent range resent repeatedly, all within 1 real
		// second — "a burst of 5,000 retransmissions in 1s".
		t := base.Add(time.Duration(i) * (time.Second / burst))
		events := e.HandlePacket(egressPkt(t, 1000, 50))
		produced += len(events)
	}
	if produced != 0 {
		t.Fatalf("burst produced %d Events, want 0 — a capture layer forwarding these onto a bounded channel would see exactly this many chances to drop one", produced)
	}
	if got := e.RetransCount(); got != burst {
		t.Fatalf("RetransCount() = %d, want %d", got, burst)
	}
}

func TestEngine_HighestAckWraparound(t *testing.T) {
	// Ported from bpf_src/verify_highest_ack_wraparound.sh: a
	// synthetic flow entirely inside the >2^31 sequence-number region
	// (TCP ISNs are randomized across the full 32-bit range, so ~50%
	// of real connections live here for their whole lifetime). Before
	// the fix (commit e1b0945), a 0-seeded highestAck made every
	// later signed comparison come out wrong for exactly this region,
	// permanently disabling stall detection. This proves the Go port
	// didn't reintroduce it.
	const baseSeq = uint32(3_000_000_000) // > 2^31 = 2147483648

	e := NewEngine(nil)
	// warmUp starts at seq 1000, which isn't in the region this test
	// needs to exercise, so the warm-up is built directly here instead.
	tm := time.Now()
	seq := baseSeq
	for i := 0; i < stallMinPackets; i++ {
		e.HandlePacket(egressPkt(tm, seq, 100))
		seq += 100
		tm = tm.Add(time.Millisecond)
	}
	// One ingress ACK, also > 2^31, acknowledging some but not all of
	// the warm-up data.
	e.HandlePacket(ingressAckPkt(tm, baseSeq+400))

	// Final packet after a real gap — seq equals the current end_seq
	// (no new data), so eligibility rests entirely on end_seq (now
	// baseSeq+800) being strictly ahead of the just-updated highestAck
	// (baseSeq+400).
	tm = tm.Add(time.Second)
	events := e.HandlePacket(egressPkt(tm, seq, 0))
	if len(events) != 1 || events[0].Type != glitch.KindStallStart {
		t.Fatalf("expected a StallStart in the >2^31 region (highestAck wraparound regression), got %+v", events)
	}
}

func TestEngine_UDP_OnlyMonitoredPortsStall(t *testing.T) {
	udpPkt := func(t time.Time, port uint16) PacketMeta {
		return PacketMeta{
			Time: t, Direction: DirEgress,
			SrcIP: testUs, DstIP: testRemote,
			SrcPort: port, DstPort: 9999, Proto: protoUDP,
		}
	}

	t.Run("unmonitored port never stalls", func(t *testing.T) {
		e := NewEngine(nil)
		tm := time.Now()
		for i := 0; i < stallMinPackets; i++ {
			e.HandlePacket(udpPkt(tm, 4000))
			tm = tm.Add(10 * time.Millisecond)
		}
		tm = tm.Add(2 * time.Second)
		if events := e.HandlePacket(udpPkt(tm, 4000)); len(events) != 0 {
			t.Errorf("expected no events for an unmonitored UDP port, got %+v", events)
		}
	})

	t.Run("monitored port can stall", func(t *testing.T) {
		e := NewEngine([]uint16{4000})
		tm := time.Now()
		for i := 0; i < stallMinPackets; i++ {
			e.HandlePacket(udpPkt(tm, 4000))
			tm = tm.Add(10 * time.Millisecond)
		}
		tm = tm.Add(2 * time.Second)
		events := e.HandlePacket(udpPkt(tm, 4000))
		if len(events) != 1 || events[0].Type != glitch.KindStallStart {
			t.Fatalf("expected a StallStart for a monitored UDP port, got %+v", events)
		}
	})
}

func TestEngine_NonFirstFragment_Skipped(t *testing.T) {
	e := NewEngine(nil)
	pkt := egressPkt(time.Now(), 1000, 100)
	pkt.Frag = true
	if events := e.HandlePacket(pkt); len(events) != 0 {
		t.Errorf("expected no events for a non-first fragment, got %+v", events)
	}
	if len(e.flows) != 0 {
		t.Errorf("expected no flow entry created from a non-first fragment, got %d", len(e.flows))
	}
}

func egressPktPort(t time.Time, srcPort uint16, seq uint32, payloadLen uint16) PacketMeta {
	pm := egressPkt(t, seq, payloadLen)
	pm.SrcPort = srcPort
	return pm
}

func flowKeyForPort(srcPort uint16) flowKey {
	key := testFlowKey()
	key.srcPort = srcPort
	return key
}

func TestEngine_Sweep_IdleFlowEvicted(t *testing.T) {
	e := NewEngine(nil)
	base := time.Now()
	e.HandlePacket(egressPkt(base, 1000, 100))
	if len(e.flows) != 1 {
		t.Fatalf("expected one flow, got %d", len(e.flows))
	}

	// Not idle long enough yet — must survive.
	if events := e.Sweep(base.Add(idleTimeout - time.Second)); len(events) != 0 {
		t.Errorf("expected no events from a sweep before idleTimeout, got %+v", events)
	}
	if len(e.flows) != 1 {
		t.Fatalf("flow evicted too early, len(e.flows)=%d", len(e.flows))
	}

	// Past idleTimeout, and the flow was never in a stall — evicted
	// silently, no event.
	events := e.Sweep(base.Add(idleTimeout + time.Second))
	if len(events) != 0 {
		t.Errorf("expected no events for a non-stalling idle flow, got %+v", events)
	}
	if len(e.flows) != 0 {
		t.Errorf("expected the idle flow to be evicted, len(e.flows)=%d", len(e.flows))
	}
	if got := e.Evictions(); got != 1 {
		t.Errorf("Evictions() = %d, want 1", got)
	}
}

func TestEngine_Sweep_AbandonsOpenEpisode(t *testing.T) {
	e := NewEngine(nil)
	base := time.Now()
	tm, seq := warmUp(e, base, stallMinPackets, 10*time.Millisecond, 100)

	// Trigger a stall — then the connection goes silent forever (no
	// more packets), the real-world shape an abandoned episode has.
	tm = tm.Add(500 * time.Millisecond)
	events := e.HandlePacket(egressPkt(tm, seq, 100))
	if len(events) != 1 || events[0].Type != glitch.KindStallStart {
		t.Fatalf("expected a StallStart to set up this test, got %+v", events)
	}

	events = e.Sweep(tm.Add(idleTimeout + time.Second))
	if len(events) != 1 || events[0].Type != glitch.KindStallAbandoned {
		t.Fatalf("expected one StallAbandoned event, got %+v", events)
	}
	if events[0].Value == 0 {
		t.Errorf("expected a non-zero abandoned-episode duration, got 0")
	}
	if len(e.flows) != 0 {
		t.Errorf("expected the abandoned flow to be removed, len(e.flows)=%d", len(e.flows))
	}
}

func TestEngine_CapEviction_EvictsLeastRecentlySeen(t *testing.T) {
	e := NewEngine(nil)
	base := time.Now()
	for i := 0; i < maxFlows; i++ {
		e.HandlePacket(egressPktPort(base.Add(time.Duration(i)*time.Millisecond), uint16(i), 1000, 100))
	}
	if len(e.flows) != maxFlows {
		t.Fatalf("expected %d flows after warm-up, got %d", maxFlows, len(e.flows))
	}

	// One more flow, past the cap — must evict exactly the oldest
	// (port 0, inserted first) to make room.
	e.HandlePacket(egressPktPort(base.Add(maxFlows*time.Millisecond), uint16(maxFlows), 1000, 100))

	if len(e.flows) != maxFlows {
		t.Fatalf("expected the table to stay at the cap (%d), got %d", maxFlows, len(e.flows))
	}
	if _, stillThere := e.flows[flowKeyForPort(0)]; stillThere {
		t.Errorf("expected the oldest flow (port 0) to have been evicted")
	}
	if _, present := e.flows[flowKeyForPort(1)]; !present {
		t.Errorf("expected the second-oldest flow (port 1) to still be present")
	}
	if _, present := e.flows[flowKeyForPort(maxFlows)]; !present {
		t.Errorf("expected the newly-inserted flow to be present")
	}
	if got := e.Evictions(); got != 1 {
		t.Errorf("Evictions() = %d, want 1", got)
	}
}

func TestEngine_CapEviction_AbandonsOpenEpisodeOnEvict(t *testing.T) {
	e := NewEngine(nil)
	base := time.Now()

	// Flow 0 (will be the oldest) gets warmed up and driven into an
	// open stall, then goes silent — exactly like
	// TestEngine_Sweep_AbandonsOpenEpisode, just evicted by the cap
	// instead of Sweep's idle timeout.
	tm, seq := warmUp(e, base, stallMinPackets, 10*time.Millisecond, 100)
	tm = tm.Add(500 * time.Millisecond)
	events := e.HandlePacket(egressPkt(tm, seq, 100))
	if len(events) != 1 || events[0].Type != glitch.KindStallStart {
		t.Fatalf("expected a StallStart to set up this test, got %+v", events)
	}

	// Fill the rest of the table with ordinary flows, all newer than
	// flow 0's own last packet.
	fillBase := tm.Add(time.Second)
	for i := 1; i < maxFlows; i++ {
		e.HandlePacket(egressPktPort(fillBase.Add(time.Duration(i)*time.Millisecond), uint16(i), 1000, 100))
	}
	if len(e.flows) != maxFlows {
		t.Fatalf("expected %d flows after filling to the cap, got %d", maxFlows, len(e.flows))
	}

	// One more flow past the cap: flow 0 is still the oldest (it went
	// silent right after its StallStart, every other flow is newer)
	// and must be evicted, producing a StallAbandoned event for it.
	events = e.HandlePacket(egressPktPort(fillBase.Add(maxFlows*time.Millisecond), uint16(maxFlows), 1000, 100))
	if len(events) != 1 || events[0].Type != glitch.KindStallAbandoned {
		t.Fatalf("expected one StallAbandoned event from the cap-triggered eviction, got %+v", events)
	}
	if _, stillThere := e.flows[testFlowKey()]; stillThere { // flow 0's key (default srcPort)
		t.Errorf("expected flow 0 (the stalling one) to have been evicted")
	}
}

// TestEngine_BurstWindow_SameSendDoesNotSplitEpisode replicates the
// exact sequence seen live in the Phase 2 simultaneous eBPF/AF_PACKET
// comparison on aaeon: a stall-triggering packet, a second segment of
// the same GSO-coalesced send 47us later (which must not close the
// episode it just opened), then the real recovery (two retransmission
// retries, then a normal-paced packet). Before burstWindow, the 47us
// segment satisfied "not stalling" and closed the episode immediately,
// fragmenting what eBPF (seeing only the one pre-GSO-split packet)
// reports as a single continuous episode into two.
func TestEngine_BurstWindow_SameSendDoesNotSplitEpisode(t *testing.T) {
	e := NewEngine(nil)
	base := time.Now()
	tm, seq := warmUp(e, base, stallMinPackets, 10*time.Millisecond, 100)
	s := seq // first byte of the stalled send, kept for the retries below

	// P1: the stall-triggering packet — genuinely new data, not itself
	// a retransmission.
	tm = tm.Add(137 * time.Millisecond)
	events := e.HandlePacket(egressPkt(tm, s, 100))
	if len(events) != 1 || events[0].Type != glitch.KindStallStart {
		t.Fatalf("P1: expected one StallStart, got %+v", events)
	}

	// P2: the same send's second GSO segment, 47us later — same flow,
	// continuing data (not a retransmission), within burstWindow.
	tm = tm.Add(47 * time.Microsecond)
	events = e.HandlePacket(egressPkt(tm, s+100, 50))
	if len(events) != 0 {
		t.Fatalf("P2 (same-burst segment): expected no events at all, got %+v", events)
	}

	// P3: first retry — a retransmission, well past burstWindow. Must
	// not reopen (already open) or close (still genuinely stalled).
	tm = tm.Add(320 * time.Millisecond)
	events = e.HandlePacket(egressPkt(tm, s, 100)) // resend from s
	if len(events) != 1 || events[0].Type != glitch.KindRetrans {
		t.Fatalf("P3 (first retry): expected one Retrans, got %+v", events)
	}

	// P4: second retry — another retransmission.
	tm = tm.Add(270 * time.Millisecond)
	events = e.HandlePacket(egressPkt(tm, s+50, 50)) // resend from s+50
	if len(events) != 1 || events[0].Type != glitch.KindRetrans {
		t.Fatalf("P4 (second retry): expected one Retrans, got %+v", events)
	}

	// P5: recovery — normal-paced, genuinely new data closes the
	// episode. Exactly one episode end to end, with all three
	// qualifying gaps (P1, P3, P4) folded into it — the real outage
	// reported once, not fragmented by P2.
	tm = tm.Add(10 * time.Millisecond)
	events = e.HandlePacket(egressPkt(tm, s+150, 100))
	if len(events) != 1 || events[0].Type != glitch.KindStallEnd {
		t.Fatalf("P5 (recovery): expected one StallEnd, got %+v", events)
	}
	if events[0].Steps != 3 {
		t.Errorf("Steps = %d, want 3 (P1 opens + P3 + P4 each extend)", events[0].Steps)
	}
}

// TestEngine_BurstWindow_OrdinaryBulkTransfer_NoFalseStall confirms the
// burstWindow skip doesn't itself introduce a false stall during
// perfectly normal traffic: a steady bulk transfer where every "send"
// happens to arrive as two GSO-split segments a few microseconds
// apart, same as real post-split AF_PACKET capture of ordinary
// traffic, not just the stall scenario above.
func TestEngine_BurstWindow_OrdinaryBulkTransfer_NoFalseStall(t *testing.T) {
	e := NewEngine(nil)
	base := time.Now()
	tm := base
	seq := uint32(1000)
	for i := 0; i < stallMinPackets+20; i++ {
		e.HandlePacket(egressPkt(tm, seq, 100))
		seq += 100
		tm = tm.Add(50 * time.Microsecond) // same-burst second segment
		events := e.HandlePacket(egressPkt(tm, seq, 50))
		seq += 50
		if len(events) != 0 {
			t.Fatalf("iteration %d: same-burst segment produced events during ordinary traffic: %+v", i, events)
		}
		tm = tm.Add(10 * time.Millisecond) // normal pace to the next send
	}
	if state := e.flows[testFlowKey()]; state.stallOpen {
		t.Errorf("expected no stall open after an uninterrupted bulk transfer")
	}
}

func TestEngine_EWMA_NotPollutedByStallGap(t *testing.T) {
	e := NewEngine(nil)
	base := time.Now()
	tm, seq := warmUp(e, base, stallMinPackets, 10*time.Millisecond, 100)
	key := testFlowKey()
	baselineAvg := e.flows[key].avgGapNs
	if baselineAvg <= 0 {
		t.Fatalf("expected a positive warmed-up avgGapNs, got %d", baselineAvg)
	}

	// A qualifying stall gap must not be folded into the EWMA.
	tm = tm.Add(500 * time.Millisecond)
	e.HandlePacket(egressPkt(tm, seq, 100))
	seq += 100
	if got := e.flows[key].avgGapNs; got != baselineAvg {
		t.Errorf("avgGapNs changed from %d to %d after a stalling gap — it must be left untouched", baselineAvg, got)
	}

	// Recovering with another normal-paced gap folds in as usual and
	// should land close to the original baseline, not anywhere near
	// the 500ms stall gap.
	tm = tm.Add(10 * time.Millisecond)
	e.HandlePacket(egressPkt(tm, seq, 100))
	if got := e.flows[key].avgGapNs; got > int64(50*time.Millisecond) {
		t.Errorf("avgGapNs = %dns after recovery, want it to stay near the ~10ms baseline, not reflect the stall gap", got)
	}
}
