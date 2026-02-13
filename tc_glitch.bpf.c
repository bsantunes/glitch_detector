#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/pkt_cls.h>
#include <linux/types.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#define ETH_P_IP 0x0800
#define GLITCH_THRESH_NS 100000000 // 100ms

struct flow_key {
    __u32 src_ip;
    __u32 dst_ip;
    __u16 src_port;
    __u16 dst_port;
    __u8  proto;
};

// 1. NEW: Global Stats for Throughput Monitoring
struct global_metrics {
    __u64 total_bytes;
    __u64 total_pkts;
};

struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1); // Only 1 entry (Global Counter)
    __type(key, __u32);
    __type(value, struct global_metrics);
} global_stats SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 10000);
    __type(key, struct flow_key);
    __type(value, __u64);
} flow_tracker SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 1 << 14); 
} events SEC(".maps");

// 2. ENHANCED: Event struct with more details
struct glitch_event {
    __u32 src_ip;
    __u32 dst_ip;
    __u64 delta_ns;
    __u32 len;    // Packet length
    __u8  proto;  // TCP (6) or UDP (17)
};

SEC("tc")
int egress_monitor(struct __sk_buff *skb) {
    void *data_end = (void *)(long)skb->data_end;
    void *data = (void *)(long)skb->data;
    struct ethhdr *eth = data;

    if ((void *)(eth + 1) > data_end) return TC_ACT_OK;
    if (eth->h_proto != bpf_htons(ETH_P_IP)) return TC_ACT_OK;

    struct iphdr *ip = (void *)(eth + 1);
    if ((void *)(ip + 1) > data_end) return TC_ACT_OK;

    // --- METRIC 1: Update Global Stats ---
    __u32 zero_key = 0;
    struct global_metrics *stats = bpf_map_lookup_elem(&global_stats, &zero_key);
    if (stats) {
        // Atomic add to prevent race conditions
        __sync_fetch_and_add(&stats->total_bytes, skb->len);
        __sync_fetch_and_add(&stats->total_pkts, 1);
    }

    // --- METRIC 2: Glitch Detection ---
    struct flow_key key = {};
    key.src_ip = ip->saddr;
    key.dst_ip = ip->daddr;
    key.proto = ip->protocol;

    __u64 ts = bpf_ktime_get_ns();
    __u64 *last_ts = bpf_map_lookup_elem(&flow_tracker, &key);

    if (last_ts) {
        __u64 delta = ts - *last_ts;

        if (delta > GLITCH_THRESH_NS) {
            struct glitch_event *e;
            e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
            if (e) {
                e->src_ip = key.src_ip;
                e->dst_ip = key.dst_ip;
                e->delta_ns = delta;
                e->len = skb->len;      // Capture size
                e->proto = ip->protocol; // Capture protocol
                bpf_ringbuf_submit(e, 0);
            }
        }
    }

    bpf_map_update_elem(&flow_tracker, &key, &ts, BPF_ANY);
    return TC_ACT_OK;
}

char __license[] SEC("license") = "GPL";
