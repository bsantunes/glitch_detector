#include <stddef.h>
#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/in.h>
#include <linux/pkt_cls.h>
#include <linux/types.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>
#include <bpf/bpf_tracing.h>

// Some headers miss this definition in older kernels
#ifndef ETH_P_IP
#define ETH_P_IP 0x0800
#endif

#define TC_ACT_OK 0
#define GLITCH_THRESH_NS 100000000 // 100ms threshold for "Stall"

// Key to identify a flow (5-tuple)
struct flow_key {
    __u32 src_ip;
    __u32 dst_ip;
    __u16 src_port;
    __u16 dst_port;
    __u8  proto;
};

// Map to store the LAST timestamp we saw a packet for this flow
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 10000); // Track active flows
    __type(key, struct flow_key);
    __type(value, __u64); // Last Timestamp
} flow_tracker SEC(".maps");

// Ring Buffer to send "Glitch Alerts" to User Space
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 1 << 14); 
} events SEC(".maps");

// The Event Structure sent to Go
struct glitch_event {
    __u32 src_ip;
    __u32 dst_ip;
    __u64 delta_ns;
};

SEC("tc")
int egress_monitor(struct __sk_buff *skb) {
    void *data_end = (void *)(long)skb->data_end;
    void *data = (void *)(long)skb->data;
    struct ethhdr *eth = data;

    // 1. Safety Checks & Parsing
    if ((void *)(eth + 1) > data_end) return TC_ACT_OK;
    if (eth->h_proto != bpf_htons(ETH_P_IP)) return TC_ACT_OK; // IPv4 only for demo

    struct iphdr *ip = (void *)(eth + 1);
    if ((void *)(ip + 1) > data_end) return TC_ACT_OK;

    // 2. Build Flow Key
    struct flow_key key = {};
    key.src_ip = ip->saddr;
    key.dst_ip = ip->daddr;
    key.proto = ip->protocol;
    // (Note: Port extraction omitted for brevity, usually requires parsing TCP/UDP headers)

    // 3. Get Current Time
    __u64 ts = bpf_ktime_get_ns();
    __u64 *last_ts = bpf_map_lookup_elem(&flow_tracker, &key);

    if (last_ts) {
        // 4. Calculate Inter-Departure Time
        __u64 delta = ts - *last_ts;

        // 5. Detect Glitch (If delta > 100ms)
        if (delta > GLITCH_THRESH_NS) {
            struct glitch_event *e;
            e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
            if (e) {
                e->src_ip = key.src_ip;
                e->dst_ip = key.dst_ip;
                e->delta_ns = delta;
                bpf_ringbuf_submit(e, 0);
            }
        }
    }

    // 6. Update Tracker
    bpf_map_update_elem(&flow_tracker, &key, &ts, BPF_ANY);

    return TC_ACT_OK;
}

char __license[] SEC("license") = "GPL";
