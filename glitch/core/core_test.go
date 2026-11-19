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

	// Resend starting at 1000 (behind end_seq) with real payload.
	events := e.HandlePacket(egressPkt(base.Add(time.Millisecond), 1000, 50))
	if len(events) != 1 || events[0].Type != glitch.KindRetrans {
		t.Fatalf("expected one Retrans event, got %+v", events)
	}
	if events[0].Value != 100 {
		t.Errorf("Value (seq delta) = %d, want 100", events[0].Value)
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
