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
    __u32 max_seq; // Highest TCP Sequence seen
};

// Event data sent to Go
struct glitch_event {
    __u32 src_ip;
    __u32 dst_ip;
    __u64 value;  // Duration (ns) OR Seq Number
    __u32 type;   // 1=Stall, 2=Retrans
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

    // Define Flow Key
    struct flow_key key = {};
    key.src_ip = ip->saddr;
    key.dst_ip = ip->daddr;
    key.proto = ip->protocol;
    
    // Parse TCP if present
    struct tcphdr *tcp = NULL;
    if (ip->protocol == IPPROTO_TCP) {
        tcp = (void *)(ip + 1);
        if ((void *)(tcp + 1) > data_end) tcp = NULL;
    }

    // Get current time in nanoseconds
    __u64 ts = bpf_ktime_get_ns();
    // Look up this flow in our "Memory" Map
    struct flow_state *state = bpf_map_lookup_elem(&flow_tracker, &key);

    if (state) {
        // 1. CHECK FOR STALL (Time Gap)
        __u64 delta = ts - state->last_ts;    // Current Time - Last Time
        if (delta > GLITCH_THRESH_NS) {        // If gap > 100ms
            // Only report stall if NOT a retransmission (avoid double counting)
            struct glitch_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
            if (e) {
                e->src_ip = key.src_ip;
                e->dst_ip = key.dst_ip;
                e->value = delta;
                e->type = EVENT_TYPE_STALL;    // Flag as STALL
                e->proto = ip->protocol;
                bpf_ringbuf_submit(e, 0);
            }
        }

        // 2. CHECK FOR RETRANSMISSION (TCP Only)
        if (tcp) {
            __u32 seq = bpf_ntohl(tcp->seq);    // Get Sequence Number
            
            // If current seq < max_seq, it's an old packet sent again
            if (seq < state->max_seq) {
                 struct glitch_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
                 if (e) {
                    e->src_ip = key.src_ip;
                    e->dst_ip = key.dst_ip;
                    e->value = state->max_seq - seq; // How far back did it jump?
                    // ... It means we already saw this packet! It was RESENT.
                    e->type = EVENT_TYPE_RETRANS;
                    e->proto = ip->protocol;
                    bpf_ringbuf_submit(e, 0);
                 }
            } else {
                state->max_seq = seq; // Update max
            }
        }
        state->last_ts = ts;
    } else {
        // New Flow
        struct flow_state new_state = {};
        new_state.last_ts = ts;
        if (tcp) new_state.max_seq = bpf_ntohl(tcp->seq);
        bpf_map_update_elem(&flow_tracker, &key, &new_state, BPF_ANY);
    }

    // TC_ACT_OK = "Let the packet pass normally"
    return TC_ACT_OK;
}
char __license[] SEC("license") = "GPL";
