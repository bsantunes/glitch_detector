#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/tcp.h>
#include <linux/udp.h>
#include <linux/in.h>
#include <linux/pkt_cls.h>
#include <linux/types.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#define ETH_P_IP 0x0800
#define GLITCH_THRESH_NS 100000000 // 100ms floor - see the idle-vs-fault
                                    // baseline comment on the stall check
                                    // below for how this combines with a
                                    // per-flow EWMA.
#define EVENT_TYPE_STALL_START 1
#define EVENT_TYPE_RETRANS     2
#define EVENT_TYPE_STALL_END   3

// Idle-vs-fault stall baseline: a flow is only flagged as stalled when a
// gap exceeds max(GLITCH_THRESH_NS, STALL_K * avg_gap_ns), where
// avg_gap_ns is that flow's own EWMA of its recent inter-packet gaps -
// this is what lets a flow that normally paces itself slowly (a 1/s or
// 100ms keepalive, even with jitter) go unflagged, while a genuinely
// stalled bulk flow (whose own normal pace is far below 100ms) still
// trips the fixed floor. STALL_K is a load-time constant, not computed
// per-packet. STALL_MIN_PACKETS gates the check off until the EWMA has
// had enough samples to be trustworthy - a brand-new flow's first few
// gaps don't yet represent its real pace.
#define STALL_K 4
#define STALL_MIN_PACKETS 8

// UDP stall detection is opt-in: unlike TCP (gated on real outstanding
// unacked data, see below), a UDP flow has no ACK signal at all to tell
// a genuine stall apart from an application that simply has nothing to
// send right now - so by default no UDP flow is stall-checked. Listing
// a port here (either side of the flow) opts it in. Fixed-size, load-
// time array, not a BPF map - this is operator configuration decided at
// build time, not something that needs changing while the program runs.
#define UDP_MONITOR_PORTS_MAX 8
static const __u16 UDP_MONITOR_PORTS[UDP_MONITOR_PORTS_MAX] = {
    0, 0, 0, 0, 0, 0, 0, 0, // empty by default - add real ports to opt in
};

static __always_inline int udp_port_monitored(__u16 port) {
    if (port == 0) return 0; // 0 is never a real port; never match the padding
#pragma unroll
    for (int i = 0; i < UDP_MONITOR_PORTS_MAX; i++) {
        if (UDP_MONITOR_PORTS[i] == port) return 1;
    }
    return 0;
}

// Key: The 5-Tuple (SrcIP, DstIP, Ports, Proto)
struct flow_key {
    __u32 src_ip;
    __u32 dst_ip;
    __u16 src_port;
    __u16 dst_port;
    __u8  proto;
};

// State we track per flow. Shared between egress_monitor (owns/creates
// entries, tracks pacing + end_seq + stall episodes) and ingress_monitor
// (looks up existing entries only, tracks highest_ack) - see each
// program's own comments.
struct flow_state {
    __u64 last_ts;
    __u64 avg_gap_ns; // EWMA (shift-8, i.e. 1/8 weight on each new sample)
                       // of this flow's inter-packet gap - the per-flow
                       // "normal pace" the stall check judges new gaps
                       // against. Never updated with a gap that itself
                       // was counted as a stall step, so one real stall
                       // doesn't drag the baseline upward right when it
                       // matters.
    __u32 end_seq; // Highest (seq + payload_len) seen so far - the end of
                   // the furthest-reaching segment sent, not just its start.
    __u32 pkt_count; // Total packets seen for this flow so far.
    __u32 highest_ack; // Highest ACK number the remote peer has sent for
                        // this flow, as seen by ingress_monitor - lets
                        // egress_monitor tell "genuinely stuck with data
                        // in flight" apart from "idle, nothing outstanding
                        // to acknowledge" (see the TCP stall gate below).
                        // Seeded from the flow's first packet's own seq
                        // when the entry is created (see the new-flow
                        // branch below), not 0 - with random ISNs,
                        // roughly half of all connections have seq/ack
                        // values at or above 2^31 for their whole
                        // practical lifetime, where a 0 origin makes
                        // every later signed comparison come out wrong.
    __u64 stall_start_ts; // 0 when not currently in a stall episode,
                           // otherwise the timestamp of the last packet
                           // seen *before* the episode began (so the
                           // eventual STALL_END's duration covers the
                           // full gap, not just time since the episode
                           // was first flagged).
    __u32 stall_steps; // Number of qualifying gaps folded into the
                        // current episode so far (1 at STALL_START,
                        // incremented on every subsequent qualifying
                        // gap, reported on STALL_END, then reset).
};

// Event data sent to Go. Ordered large-to-small so the only padding is
// the compiler's usual trailing pad (to the three __u64 members' 8-byte
// struct alignment) after proto - no internal gaps. 45 bytes of real
// fields, padded to 48 (same total size as before adding steps - it
// fills what used to be trailing pad). Keep glitch/glitch.go's wireEvent
// struct (and glitch_test.go's decode fixture) in sync with this exact
// layout.
struct glitch_event {
    __u64 ts;          // bpf_ktime_get_ns() at emission time - CLOCK_MONOTONIC
    __u64 value;       // STALL_START: current gap (ns) that triggered it.
                        // STALL_END: total episode duration (ns), from the
                        // last good packet before the episode to the
                        // packet that closed it. RETRANS: seq delta.
    __u64 baseline_ns; // The stall baseline in effect
                        // (max(GLITCH_THRESH_NS, STALL_K * avg_gap_ns)) -
                        // 0 for retrans events, which have no baseline.
    __u32 src_ip;
    __u32 dst_ip;
    __u32 type;      // 1=StallStart, 2=Retrans, 3=StallEnd
    __u32 steps;     // STALL_END only: qualifying-gap count for the
                      // episode just closed (matches the visible
                      // retry/backoff count). 0 for START and RETRANS.
    __u16 src_port;  // host byte order (bpf_ntohs applied before storing)
    __u16 dst_port;
    __u8  proto;
};

// The "Memory" (BPF Maps) - shared between both attached programs.
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 10000);
    __type(key, struct flow_key);
    __type(value, struct flow_state);
} flow_tracker SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 1 << 14);
} events SEC(".maps");

// Tells the kernel to attach this to the Traffic Control (TC) layer
SEC("tc")
// skb: The "Socket Buffer." Linux Kernel's handle for a single network packet
int egress_monitor(struct __sk_buff *skb) {
    // data points to the start of the packet. data_end points to the end
    void *data_end = (void *)(long)skb->data_end;
    void *data = (void *)(long)skb->data;
    // 5G Modems are RAW IP. There is no Ethernet header!
    // Map the start of the data directly to the IP header.
    struct iphdr *ip = data;

    // Safety Check 1: Is the IP header fully inside the buffer?
    if ((void *)(ip + 1) > data_end) return TC_ACT_OK;
    // Logic Check: Is this an IP packet? (Ignore ARP, IPv6, etc for now)
    if (ip->version != 4) return TC_ACT_OK;

    // IHL is in 32-bit words; the minimum valid value is 5 (20 bytes,
    // no options). Using the real header length here instead of
    // blindly assuming 20 bytes (`ip + 1`) matters whenever IP options
    // are present - the previous fixed offset silently misread
    // whatever came after a longer header as the L4 header.
    __u8 ihl = ip->ihl;
    if (ihl < 5) return TC_ACT_OK;
    void *l4 = (void *)ip + (ihl * 4);

    // Skip non-first fragments: the fragment offset (low 13 bits of
    // frag_off) is non-zero for every fragment after the first, and
    // those never carry a TCP/UDP header at all - what sits right after
    // the IP header in a non-first fragment is raw payload, not a
    // tcphdr/udphdr, so parsing it as one would misread arbitrary bytes
    // as ports/sequence numbers. The first fragment (offset 0, possibly
    // with the more-fragments bit set) still carries the real L4 header
    // and is processed normally below.
    if (bpf_ntohs(ip->frag_off) & 0x1FFF) return TC_ACT_OK;

    // Define Flow Key
    struct flow_key key = {};
    key.src_ip = ip->saddr;
    key.dst_ip = ip->daddr;
    key.proto = ip->protocol;

    // Parse TCP or UDP if present, filling src_port/dst_port into the
    // flow key - previously declared but never set, so every flow
    // between the same two hosts collided into one shared state
    // regardless of port.
    struct tcphdr *tcp = NULL;
    struct udphdr *udp = NULL;
    if (ip->protocol == IPPROTO_TCP) {
        tcp = l4;
        if ((void *)(tcp + 1) > data_end) {
            tcp = NULL;
        } else {
            key.src_port = bpf_ntohs(tcp->source);
            key.dst_port = bpf_ntohs(tcp->dest);
        }
    } else if (ip->protocol == IPPROTO_UDP) {
        udp = l4;
        if ((void *)(udp + 1) > data_end) {
            udp = NULL;
        } else {
            key.src_port = bpf_ntohs(udp->source);
            key.dst_port = bpf_ntohs(udp->dest);
        }
    }

    // Get current time in nanoseconds
    __u64 ts = bpf_ktime_get_ns();
    // Look up this flow in our "Memory" Map
    struct flow_state *state = bpf_map_lookup_elem(&flow_tracker, &key);

    if (state) {
        state->pkt_count++; // this packet counts as seen before the check below

        // 1. CHECK FOR STALL (Time Gap), idle-vs-fault aware: a flow
        // that normally paces itself slowly (a periodic keepalive, with
        // or without jitter) must not be flagged just for behaving
        // normally, while a genuinely stalled bulk flow - whose own
        // normal pace is far below the 100ms floor - still must be.
        // Comparing against a per-flow baseline (not a single fixed
        // threshold) is what makes both of those true at once; see the
        // STALL_K/STALL_MIN_PACKETS comment above.
        //
        // Eligibility, on top of the baseline compare:
        //  - TCP: only eligible once this flow has real outstanding
        //    (sent but not yet acknowledged) data - end_seq strictly
        //    ahead of highest_ack. Without this, a TCP connection that
        //    is simply idle (everything it sent has been ACKed, nothing
        //    queued to send) looks identical to one that is genuinely
        //    stuck waiting on the network - both produce a long gap
        //    between egress packets, but only the second is a real
        //    glitch. highest_ack is maintained by ingress_monitor.
        //  - UDP: only eligible if this port pair opted in via
        //    UDP_MONITOR_PORTS - there's no ACK signal to tell a real
        //    stall apart from an application with nothing queued, so
        //    the safe default is not to guess.
        __u64 delta = ts - state->last_ts;    // Current Time - Last Time
        int eligible = 0;
        if (state->pkt_count >= STALL_MIN_PACKETS) {
            if (tcp) {
                eligible = (__s32)(state->end_seq - state->highest_ack) > 0;
            } else if (udp) {
                eligible = udp_port_monitored(key.src_port) || udp_port_monitored(key.dst_port);
            }
        }

        __u64 baseline = state->avg_gap_ns * STALL_K;
        if (baseline < GLITCH_THRESH_NS) baseline = GLITCH_THRESH_NS;

        if (eligible && delta > baseline) {
            // A qualifying gap - either the start of a new stall episode
            // or another step extending one already in progress.
            if (state->stall_start_ts == 0) {
                state->stall_start_ts = state->last_ts; // episode began at the last good packet
                state->stall_steps = 1;
                struct glitch_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
                if (e) {
                    e->ts = ts;
                    e->value = delta;
                    e->baseline_ns = baseline;
                    e->src_ip = key.src_ip;
                    e->dst_ip = key.dst_ip;
                    e->type = EVENT_TYPE_STALL_START;
                    e->steps = 0; // meaningful only on STALL_END
                    e->src_port = key.src_port;
                    e->dst_port = key.dst_port;
                    e->proto = ip->protocol;
                    bpf_ringbuf_submit(e, 0);
                }
            } else {
                state->stall_steps++;
            }
            // Don't fold this gap into the EWMA - it's the anomaly we
            // just counted, not the flow's normal pace, and folding it
            // in would drag the baseline upward right when it most
            // needs to stay representative.
        } else {
            if (state->stall_start_ts != 0) {
                // Recovered: this gap no longer qualifies, so the
                // episode that was open is now over.
                __u64 duration = ts - state->stall_start_ts;
                struct glitch_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
                if (e) {
                    e->ts = ts;
                    e->value = duration;
                    e->baseline_ns = baseline;
                    e->src_ip = key.src_ip;
                    e->dst_ip = key.dst_ip;
                    e->type = EVENT_TYPE_STALL_END;
                    e->steps = state->stall_steps;
                    e->src_port = key.src_port;
                    e->dst_port = key.dst_port;
                    e->proto = ip->protocol;
                    bpf_ringbuf_submit(e, 0);
                }
                state->stall_start_ts = 0;
                state->stall_steps = 0;
            }
            // EWMA update, signed arithmetic required: avg_gap_ns
            // can legitimately decrease (a flow speeding up), and
            // unsigned subtraction would wrap on that instead of
            // going negative, corrupting the average. Folded in during
            // warm-up too, so avg_gap_ns is actually representative by
            // the time pkt_count reaches STALL_MIN_PACKETS.
            state->avg_gap_ns = (__u64)((__s64)state->avg_gap_ns +
                (((__s64)delta - (__s64)state->avg_gap_ns) >> 3));
        }

        // 2. CHECK FOR RETRANSMISSION (TCP Only), wraparound-safe: a
        // plain `seq < end_seq` compare breaks every time the 32-bit
        // sequence number wraps (roughly every 4 GiB of one flow's
        // traffic), since a freshly-wrapped small seq would otherwise
        // always look "behind" a large pre-wrap end_seq. The signed-
        // difference idiom (same one the Linux kernel's own TCP stack
        // uses) stays correct across the wrap.
        //
        // Compared against end_seq (seq + payload length), not just the
        // segment's start - and only when this packet actually carries
        // payload. A pure ACK/keepalive (payload_len == 0) repeats the
        // same seq every time by design (it has nothing new to send),
        // which would otherwise look identical to a real resend; it
        // carries no data to have been "resent" at all, so it can never
        // be flagged and never advances end_seq either.
        if (tcp) {
            __u32 seq = bpf_ntohl(tcp->seq);
            __u16 tcp_hlen = tcp->doff * 4;
            __u16 l4_hlen = ihl * 4 + tcp_hlen;
            __u16 ip_total_len = bpf_ntohs(ip->tot_len);
            __u16 payload_len = (ip_total_len > l4_hlen) ? (ip_total_len - l4_hlen) : 0;

            __s32 diff = (__s32)(seq - state->end_seq);
            if (diff < 0 && payload_len > 0) {
                 // seq is behind end_seq and this segment carries real
                 // data - an old byte range sent again.
                 struct glitch_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
                 if (e) {
                    e->ts = ts;
                    e->value = (__u64)(-diff); // How far back did it jump?
                    // ... It means we already saw this data! It was RESENT.
                    e->baseline_ns = 0; // no baseline concept for retrans - bpf_ringbuf_reserve does not zero memory, so this must be explicit
                    e->src_ip = key.src_ip;
                    e->dst_ip = key.dst_ip;
                    e->type = EVENT_TYPE_RETRANS;
                    e->steps = 0;
                    e->src_port = key.src_port;
                    e->dst_port = key.dst_port;
                    e->proto = ip->protocol;
                    bpf_ringbuf_submit(e, 0);
                 }
            } else if (diff >= 0 && payload_len > 0) {
                state->end_seq = seq + payload_len; // Update furthest-reaching end
            }
        }
        state->last_ts = ts;
    } else {
        // New Flow
        struct flow_state new_state = {};
        new_state.last_ts = ts;
        new_state.pkt_count = 1; // this first packet counts as seen
        // avg_gap_ns, stall_start_ts, stall_steps all start at 0 via the
        // zero-initializer above - no prior gap or episode exists yet.
        if (tcp) {
            __u32 seq = bpf_ntohl(tcp->seq);
            __u16 tcp_hlen = tcp->doff * 4;
            __u16 l4_hlen = ihl * 4 + tcp_hlen;
            __u16 ip_total_len = bpf_ntohs(ip->tot_len);
            __u16 payload_len = (ip_total_len > l4_hlen) ? (ip_total_len - l4_hlen) : 0;
            new_state.end_seq = seq + payload_len;
            // highest_ack must start relative to this connection's own
            // sequence space, not a bare 0: TCP ISNs are randomized
            // across the full 32-bit range, so roughly half of all
            // connections have seq/ack values that sit at or above
            // 2^31 for their entire practical lifetime (real transfer
            // sizes never approach the 4 GiB wrap). For those
            // connections, (s32)(real_ack - 0) is always negative, so
            // ingress_monitor's "did the ack advance" check never sees
            // it as an advance and highest_ack would stay stuck at 0
            // forever - and independently, egress_monitor's own
            // eligibility check (s32)(end_seq - highest_ack) also comes
            // out negative purely because end_seq itself is >= 2^31,
            // permanently disabling stall detection for exactly that
            // ~50% of connections. Seeding highest_ack from this first
            // packet's own seq keeps every later signed comparison
            // anchored to the connection's real sequence space instead
            // of an arbitrary origin that may be on the wrong side of
            // the 32-bit signed boundary.
            new_state.highest_ack = seq;
        }
        bpf_map_update_elem(&flow_tracker, &key, &new_state, BPF_ANY);
    }

    // TC_ACT_OK = "Let the packet pass normally"
    return TC_ACT_OK;
}

// Attached to clsact ingress. Tracks the highest TCP ACK number the
// remote peer has sent back for each flow this host is uploading to,
// so egress_monitor can tell "stuck with real unacked data in flight"
// apart from "idle, everything already acknowledged" (see its stall
// gate above). Looks up existing flow_tracker entries only - never
// creates one - since egress_monitor is the sole owner of a flow's
// lifecycle; an ACK for a flow we have no record of sending data on
// has nothing to update.
SEC("tc")
int ingress_monitor(struct __sk_buff *skb) {
    void *data_end = (void *)(long)skb->data_end;
    void *data = (void *)(long)skb->data;
    struct iphdr *ip = data;
    if ((void *)(ip + 1) > data_end) return TC_ACT_OK;
    if (ip->version != 4) return TC_ACT_OK;
    if (ip->protocol != IPPROTO_TCP) return TC_ACT_OK; // only TCP has ACKs to track

    __u8 ihl = ip->ihl;
    if (ihl < 5) return TC_ACT_OK;
    if (bpf_ntohs(ip->frag_off) & 0x1FFF) return TC_ACT_OK; // non-first fragment, see egress_monitor

    struct tcphdr *tcp = (void *)ip + (ihl * 4);
    if ((void *)(tcp + 1) > data_end) return TC_ACT_OK;

    // This packet's src is the remote peer, dst is us - build the key
    // the way egress_monitor would have for the forward (our-upload)
    // direction of the same connection, so both programs' packets for
    // one logical flow land on the same flow_tracker entry.
    struct flow_key key = {};
    key.src_ip = ip->daddr;
    key.dst_ip = ip->saddr;
    key.src_port = bpf_ntohs(tcp->dest);
    key.dst_port = bpf_ntohs(tcp->source);
    key.proto = IPPROTO_TCP;

    struct flow_state *state = bpf_map_lookup_elem(&flow_tracker, &key);
    if (!state) return TC_ACT_OK; // no matching egress flow tracked (yet) - nothing to update

    __u32 ack = bpf_ntohl(tcp->ack_seq);
    __s32 diff = (__s32)(ack - state->highest_ack);
    if (diff > 0) state->highest_ack = ack;

    return TC_ACT_OK;
}
char __license[] SEC("license") = "GPL";
