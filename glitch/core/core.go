// Package core is the glitch-detection algorithm itself, ported from
// bpf_src/tc_glitch.bpf.c with identical semantics, decoupled from any
// particular packet-capture method. Phase 1 of the eBPF/AF_PACKET
// unification: glitch/afpacket feeds this Engine on WaveOS (no
// clsact/TC-BPF support there — see CLAUDE.md's glitch/ package
// section); the existing eBPF path (glitch/glitch.go) is untouched
// this phase and still runs its own copy of this logic inside the
// kernel. Phase 4 (later, once this Engine is validated side by side
// against the kernel's own behavior) turns the BPF programs into bare
// header extractors feeding this same Engine, so there is exactly one
// implementation of the algorithm, not two to keep in sync.
//
// Every comment below naming a specific line of reasoning (the
// adaptive baseline, the TCP ACK gate, the wraparound-safe sequence
// compare, the EWMA warm-up) is carried over from tc_glitch.bpf.c's
// own comments, which remain the authoritative explanation — this
// file's comments point back there rather than re-deriving them.
package core

import (
	"net"
	"sync"
	"time"

	"dawn-agent/glitch"
)

// Direction identifies which side of a connection's traffic a packet
// was observed on — Egress for what this host sends, Ingress for what
// it receives. Mirrors tc_glitch.bpf.c's two attached programs
// (egress_monitor does the real work; ingress_monitor only tracks
// highest_ack) exactly: Engine dispatches on this field the same way
// the kernel dispatches by which clsact hook a program sits on.
type Direction int

const (
	DirEgress Direction = iota
	DirIngress
)

const (
	protoTCP = 6  // IPPROTO_TCP
	protoUDP = 17 // IPPROTO_UDP
)

// PacketMeta is the minimal per-packet information Engine needs —
// deliberately capture-method-agnostic, so the identical algorithm
// runs whether packets arrive via glitch/afpacket (this phase) or,
// later, a header-extractor-only eBPF program (Phase 4). Seq/Ack/
// PayloadLen/Flags are meaningful for TCP only (left at their zero
// value for UDP, which the algorithm below never reads them for).
// Flags is carried through but not currently consulted by the
// algorithm — tc_glitch.bpf.c never inspects TCP flags either; kept
// here so a capture layer has somewhere to put it if that changes.
// Frag marks a non-first IP fragment: never a real TCP/UDP header
// (see egress_monitor's own comment), so HandlePacket discards it
// before any flow lookup, exactly like both BPF programs do.
type PacketMeta struct {
	Time       time.Time
	Direction  Direction
	SrcIP      net.IP
	DstIP      net.IP
	SrcPort    uint16
	DstPort    uint16
	Proto      uint8
	Seq        uint32
	Ack        uint32
	PayloadLen uint16
	Flags      uint8
	Frag       bool
}

// Mirrors struct flow_key in tc_glitch.bpf.c field-for-field. A plain
// (not net.IP) fixed-size array so it's directly usable as a Go map
// key — net.IP is a slice, not comparable.
type flowKey struct {
	srcIP   [4]byte
	dstIP   [4]byte
	srcPort uint16
	dstPort uint16
	proto   uint8
}

// Mirrors struct flow_state in tc_glitch.bpf.c field-for-field (field
// names/comments there are the authoritative explanation of each —
// not re-derived here), with Go idioms substituted where natural:
// time.Time instead of a raw monotonic __u64 (stallOpen replaces the
// BPF program's "stallStartTs == 0 means not stalling" sentinel, since
// a zero time.Time is an awkward, easy-to-misread stand-in for that
// here).
type flowState struct {
	lastTs       time.Time
	avgGapNs     int64 // EWMA, shift-8 — see HandlePacket's EWMA update
	endSeq       uint32
	pktCount     uint32
	highestAck   uint32
	stallOpen    bool
	stallStartTs time.Time
	stallSteps   uint32
}

const (
	// stallThreshold mirrors GLITCH_THRESH_NS — the fixed floor a
	// flow's adaptive baseline never drops below.
	stallThreshold = 100 * time.Millisecond
	// stallK mirrors STALL_K — the EWMA multiplier the adaptive
	// baseline uses once a flow's own pace is known.
	stallK = 4
	// stallMinPackets mirrors STALL_MIN_PACKETS — a flow's EWMA isn't
	// trusted as representative until it's seen this many packets.
	stallMinPackets = 8
)

// Engine tracks per-flow state and applies tc_glitch.bpf.c's stall/
// retransmission logic to a stream of packets from any capture
// source. Safe for concurrent use; construct with NewEngine.
//
// Deliberately does not replicate the kernel's flow_tracker being a
// size-10,000 LRU hash map — flows here live in a plain Go map with
// no eviction at all. Acceptable for this phase (validation, run
// alongside the real eBPF path, not replacing it yet — see Phase 2),
// but a real limitation to close before Phase 4 retires the kernel
// map as the thing actually bounding memory use in production.
type Engine struct {
	mu              sync.Mutex
	flows           map[flowKey]*flowState
	udpMonitorPorts map[uint16]bool
}

// NewEngine constructs an Engine. udpMonitorPorts mirrors
// UDP_MONITOR_PORTS — a port (checked against either side of a UDP
// flow) opts it into stall detection, empty by default (see
// udpPortMonitored). Unlike the BPF program's own fixed-size, load-
// time array, this is an ordinary runtime constructor argument — Go
// has no equivalent reason to fix its size at compile time.
func NewEngine(udpMonitorPorts []uint16) *Engine {
	ports := make(map[uint16]bool, len(udpMonitorPorts))
	for _, p := range udpMonitorPorts {
		ports[p] = true
	}
	return &Engine{
		flows:           make(map[flowKey]*flowState),
		udpMonitorPorts: ports,
	}
}

// udpPortMonitored mirrors udp_port_monitored — 0 is never a real
// port, so it never matches regardless of what's configured.
func (e *Engine) udpPortMonitored(port uint16) bool {
	return port != 0 && e.udpMonitorPorts[port]
}

// HandlePacket feeds one packet into the engine and returns zero or
// more Events it produced. nil/empty SrcIP or DstIP, or one that
// isn't representable as IPv4, is silently dropped — tc_glitch.bpf.c
// only ever handles IPv4 (`if (ip->version != 4) return TC_ACT_OK`),
// so there is no IPv6 behavior to port here either.
func (e *Engine) HandlePacket(pm PacketMeta) []glitch.Event {
	if pm.Frag {
		return nil
	}
	src := pm.SrcIP.To4()
	dst := pm.DstIP.To4()
	if src == nil || dst == nil {
		return nil
	}
	var srcKey, dstKey [4]byte
	copy(srcKey[:], src)
	copy(dstKey[:], dst)

	e.mu.Lock()
	defer e.mu.Unlock()
	switch pm.Direction {
	case DirEgress:
		return e.handleEgress(pm, srcKey, dstKey)
	case DirIngress:
		return e.handleIngress(pm, srcKey, dstKey)
	default:
		return nil
	}
}

// handleEgress mirrors egress_monitor field-for-field; see that
// function's own comments in tc_glitch.bpf.c for the reasoning behind
// each step; this is deliberately a direct translation, not a
// reinterpretation.
func (e *Engine) handleEgress(pm PacketMeta, srcKey, dstKey [4]byte) []glitch.Event {
	key := flowKey{srcIP: srcKey, dstIP: dstKey, srcPort: pm.SrcPort, dstPort: pm.DstPort, proto: pm.Proto}
	isTCP := pm.Proto == protoTCP
	isUDP := pm.Proto == protoUDP

	state, ok := e.flows[key]
	if !ok {
		// New flow — see tc_glitch.bpf.c's new-flow branch for why
		// highestAck is seeded from this packet's own seq, not 0.
		state = &flowState{lastTs: pm.Time, pktCount: 1}
		if isTCP {
			state.endSeq = pm.Seq + uint32(pm.PayloadLen)
			state.highestAck = pm.Seq
		}
		e.flows[key] = state
		return nil
	}

	var events []glitch.Event
	state.pktCount++ // this packet counts as seen before the check below

	// 1. Stall check (idle-vs-fault aware) — see STALL_K/
	// STALL_MIN_PACKETS comment in tc_glitch.bpf.c.
	delta := pm.Time.Sub(state.lastTs)
	eligible := false
	if state.pktCount >= stallMinPackets {
		switch {
		case isTCP:
			eligible = int32(state.endSeq-state.highestAck) > 0
		case isUDP:
			eligible = e.udpPortMonitored(pm.SrcPort) || e.udpPortMonitored(pm.DstPort)
		}
	}

	baseline := time.Duration(state.avgGapNs * stallK)
	if baseline < stallThreshold {
		baseline = stallThreshold
	}

	if eligible && delta > baseline {
		if !state.stallOpen {
			state.stallOpen = true
			state.stallStartTs = state.lastTs // episode began at the last good packet
			state.stallSteps = 1
			events = append(events, glitch.Event{
				Time: pm.Time, SrcIP: pm.SrcIP, DstIP: pm.DstIP,
				SrcPort: pm.SrcPort, DstPort: pm.DstPort,
				Value:    uint64(delta.Nanoseconds()),
				Baseline: baseline,
				Type:     glitch.KindStallStart,
				Proto:    pm.Proto,
			})
		} else {
			state.stallSteps++
		}
		// Not folded into the EWMA — see tc_glitch.bpf.c: it's the
		// anomaly just counted, not the flow's normal pace.
	} else {
		if state.stallOpen {
			duration := pm.Time.Sub(state.stallStartTs)
			events = append(events, glitch.Event{
				Time: pm.Time, SrcIP: pm.SrcIP, DstIP: pm.DstIP,
				SrcPort: pm.SrcPort, DstPort: pm.DstPort,
				Value:    uint64(duration.Nanoseconds()),
				Baseline: baseline,
				Type:     glitch.KindStallEnd,
				Steps:    state.stallSteps,
				Proto:    pm.Proto,
			})
			state.stallOpen = false
			state.stallSteps = 0
		}
		// EWMA update, signed shift-8 — matches tc_glitch.bpf.c's
		// (s64) arithmetic exactly; Go's >> on a signed integer is
		// arithmetic (sign-extending), same as the BPF compiler emits.
		state.avgGapNs += (delta.Nanoseconds() - state.avgGapNs) >> 3
	}

	// 2. Retransmission check (TCP only), independent of the stall
	// logic above — wraparound-safe signed-difference compare against
	// end_seq, only for a segment that actually carries payload (a
	// pure ACK/keepalive can never be flagged, same as the kernel).
	if isTCP {
		diff := int32(pm.Seq - state.endSeq)
		switch {
		case diff < 0 && pm.PayloadLen > 0:
			events = append(events, glitch.Event{
				Time: pm.Time, SrcIP: pm.SrcIP, DstIP: pm.DstIP,
				SrcPort: pm.SrcPort, DstPort: pm.DstPort,
				Value: uint64(-diff),
				Type:  glitch.KindRetrans,
				Proto: pm.Proto,
			})
		case diff >= 0 && pm.PayloadLen > 0:
			state.endSeq = pm.Seq + uint32(pm.PayloadLen)
		}
	}

	state.lastTs = pm.Time
	return events
}

// handleIngress mirrors ingress_monitor: looks up an existing flow
// only (never creates one — handleEgress is the sole owner of a
// flow's lifecycle) and tracks the highest ACK the remote peer has
// sent, so handleEgress can tell "stuck with data in flight" apart
// from "idle, nothing outstanding."
func (e *Engine) handleIngress(pm PacketMeta, srcKey, dstKey [4]byte) []glitch.Event {
	if pm.Proto != protoTCP {
		return nil // only TCP has ACKs to track
	}
	// This packet's src is the remote peer, dst is us — reconstruct
	// the key handleEgress would have built for the forward (our-
	// upload) direction of the same connection, same swap
	// ingress_monitor itself does.
	key := flowKey{srcIP: dstKey, dstIP: srcKey, srcPort: pm.DstPort, dstPort: pm.SrcPort, proto: protoTCP}
	state, ok := e.flows[key]
	if !ok {
		return nil // no matching egress flow tracked (yet) — nothing to update
	}
	if diff := int32(pm.Ack - state.highestAck); diff > 0 {
		state.highestAck = pm.Ack
	}
	return nil
}
