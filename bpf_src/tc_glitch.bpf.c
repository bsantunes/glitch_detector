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
#define GLITCH_THRESH_NS 100000000 // 100ms
// TODO: this threshold fires identically whether the flow was actually
// stalled while trying to send, or just idle (e.g. an application with
// nothing to send right now) - the two currently look the same to the
// BPF program. Not addressed in this phase; left for whenever idle-vs-
// fault detection (the README's "Idle vs. Fault" claim isn't actually
// implemented yet) gets tackled.
#define EVENT_TYPE_STALL 1
#define EVENT_TYPE_RETRANS 2

// Key: The 5-Tuple (SrcIP, DstIP, Ports, Proto)
struct flow_key {
    __u32 src_ip;
    __u32 dst_ip;
    __u16 src_port;
    __u16 dst_port;
    __u8  proto;
};

// State we track per flow
struct flow_state {
    __u64 last_ts;
    __u32 end_seq; // Highest (seq + payload_len) seen so far - the end of
                   // the furthest-reaching segment sent, not just its start.
                   // Tracking only the start (as a prior version of this
                   // program did) under-counts how far a segment actually
                   // reached, which matters once a retransmission is only
                   // flagged for segments that carry real payload (see
                   // below) - a resend of the *most recent* segment must
                   // still compare behind its true end, not just its start.
};

// Event data sent to Go. Ordered large-to-small so the only padding is
// the compiler's usual trailing pad (to the two __u64 members' 8-byte
// struct alignment) after proto - no internal gaps. 33 bytes of real
// fields, padded to 40. Keep glitch/glitch.go's wireEvent struct (and
// glitch_test.go's decode fixture) in sync with this exact layout.
struct glitch_event {
    __u64 ts;        // bpf_ktime_get_ns() at emission time - CLOCK_MONOTONIC
    __u64 value;     // Duration (ns) for a stall, OR seq delta for a retrans
    __u32 src_ip;
    __u32 dst_ip;
    __u32 type;      // 1=Stall, 2=Retrans
    __u16 src_port;  // host byte order (bpf_ntohs applied before storing)
    __u16 dst_port;
    __u8  proto;
};

// The "Memory" (BPF Maps)
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
    // struct ethhdr *eth = data;

    // Safety Check 1: Is the Ethernet header fully inside the buffer?
    if ((void *)(ip + 1) > data_end) return TC_ACT_OK;
    // Logic Check: Is this an IP packet? (Ignore ARP, IPv6, etc for now)
    if (ip->version != 4) return TC_ACT_OK;

    //struct iphdr *ip = (void *)(eth + 1);
    // Safety Check 2: Is the IP header valid?
    //if ((void *)(ip + 1) > data_end) return TC_ACT_OK;

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
        // 1. CHECK FOR STALL (Time Gap)
        __u64 delta = ts - state->last_ts;    // Current Time - Last Time
        if (delta > GLITCH_THRESH_NS) {        // If gap > 100ms
            struct glitch_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
            if (e) {
                e->ts = ts;
                e->value = delta;
                e->src_ip = key.src_ip;
                e->dst_ip = key.dst_ip;
                e->type = EVENT_TYPE_STALL;    // Flag as STALL
                e->src_port = key.src_port;
                e->dst_port = key.dst_port;
                e->proto = ip->protocol;
                bpf_ringbuf_submit(e, 0);
            }
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
                    e->src_ip = key.src_ip;
                    e->dst_ip = key.dst_ip;
                    e->type = EVENT_TYPE_RETRANS;
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
        if (tcp) {
            __u16 tcp_hlen = tcp->doff * 4;
            __u16 l4_hlen = ihl * 4 + tcp_hlen;
            __u16 ip_total_len = bpf_ntohs(ip->tot_len);
            __u16 payload_len = (ip_total_len > l4_hlen) ? (ip_total_len - l4_hlen) : 0;
            new_state.end_seq = bpf_ntohl(tcp->seq) + payload_len;
        }
        bpf_map_update_elem(&flow_tracker, &key, &new_state, BPF_ANY);
    }

    // TC_ACT_OK = "Let the packet pass normally"
    return TC_ACT_OK;
}
char __license[] SEC("license") = "GPL";
