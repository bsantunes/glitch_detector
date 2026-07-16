# DAWN6G Glitch Detector — Architecture

System overview across both endpoints: the gateway (x86 Linux board with a
SIMCom SIM8202G-M2 5G modem) and the server (x86 Linux board hosting the TWAMP
reflector and the telemetry stack).

```
┌─────────────────────────────────── GATEWAY (x86 Linux board) ────────────────────────────────────┐
│                                                                                                   │
│  BOOT / SETUP                                                                                     │
│  ┌─────────────────────────────┐      ┌──────────────────────────────────────────────┐           │
│  │ dawn-agent.service (systemd)│─────▶│ init_gateway.sh (ExecStartPre)               │           │
│  │  Restart=always, logs to    │      │  1. modprobe cls_bpf, sch_ingress, sch_htb   │           │
│  │  /var/log/dawn-agent.log    │      │  2. AT cmds via mmcli → modem QoS bearer     │           │
│  └──────────────┬──────────────┘      │     (cid 2 = DSCP EF / critical traffic)     │           │
│                 │ ExecStart           │  3. tc HTB queues: 1:10 priority (EF),       │           │
│                 ▼                     │     1:20 default — shapes egress             │           │
│  ═══════════ USER SPACE ═══════       └──────────────────────────────────────────────┘           │
│                                                                                                   │
│  ┌─ dawn-agent (Go, main.go) ─────────────────────────────────────────────────────┐              │
│  │                                                                                │              │
│  │  main()                                                                        │              │
│  │   ├─ ebpf.LoadCollectionSpec("tc_glitch.o")  ← loads BPF object from CWD       │              │
│  │   ├─ netlink: add clsact qdisc + attach egress_monitor as TC filter            │              │
│  │   │           (parent = HANDLE_MINEGRESS 0xFFF2 → egress hook)                 │              │
│  │   ├─ mmcli --signal-setup=1  ← tells ModemManager to start polling radio       │              │
│  │   │                                                                            │              │
│  │   ├─ PASSIVE LOOP ◀──────────────── ring buffer ──────────────┐                │              │
│  │   │   reads GlitchEvent{SrcIP,DstIP,Value,Type,Proto}          │               │              │
│  │   │   Type 1 → "⚠️ Stall: X ms"   Type 2 → "🔴 TCP Loss"      │               │              │
│  │   │   (struct must match C glitch_event byte-for-byte,         │               │              │
│  │   │    decoded little-endian via binary.Read)                  │               │              │
│  │   │                                                            │               │              │
│  │   └─ ACTIVE LOOP (goroutine, every 5s)                         │               │              │
│  │       ├─ getRadioMetrics(): exec `mmcli --signal-get`          │               │              │
│  │       │    parses key:value → RSRP/RSRQ/RSSI (5G, LTE          │               │              │
│  │       │    fallback; -32768 sentinel → NaN)                    │               │              │
│  │       └─ getTwampMetrics(): exec `twampy sender <reflector>`   │               │              │
│  │            parses "Roundtrip:" line → RTT / Jitter / Loss %  ──┼── UDP ──▶ (to server :862)   │
│  └────────────────────────────────────────────────────────────────┼───────────────┘              │
│                                                                    │                              │
│  ═══════════ KERNEL SPACE ═════════════════════════════════════════════════════════              │
│                                                                    │                              │
│  ┌─ tc_glitch.o  (from bpf_src/tc_glitch.bpf.c) ──────────────────┼─────────────┐                │
│  │                                                                 │             │                │
│  │  SEC("tc") egress_monitor(skb)     runs on EVERY egress packet │             │                │
│  │   ├─ parse pkt as RAW IP (no Ethernet header — correct for     │             │                │
│  │   │  wwan modem interfaces; wrong for plain eth NICs)          │             │                │
│  │   ├─ key = 5-tuple → lookup in flow_tracker                    │             │                │
│  │   │                                                            │             │                │
│  │   ├─ STALL check:  now - last_ts > 100ms?  ──▶ event ─────────▶│             │                │
│  │   ├─ RETRANS check: tcp.seq < max_seq seen? ──▶ event ─────────┘             │                │
│  │   └─ update flow state, return TC_ACT_OK (never drops packets)               │                │
│  │                                                                              │                │
│  │  MAPS:                                                                       │                │
│  │   • flow_tracker  BPF_MAP_TYPE_LRU_HASH, 10k flows                           │                │
│  │       key: {src_ip, dst_ip, ports, proto}  val: {last_ts, max_seq}           │                │
│  │   • events        BPF_MAP_TYPE_RINGBUF, 16 KiB  → kernel→user channel        │                │
│  └──────────────────────────────────────────────────────────────────────────────┘                │
│                                             │                                                     │
│                                       wwan0 (egress, HTB-shaped, raw IP)                          │
└─────────────────────────────────────────────┼─────────────────────────────────────────────────────┘
                                              │
                                   5G modem (SIM8202G-M2)
                        │ USB/PCIe, MBIM — AT commands / signal polling
                        │ via ModemManager (mmcli)
                                              │
                                     5G / IP network
                       (all user traffic + TWAMP probes + iperf3 flood)
                                              │
┌─────────────────────────────────────────────┼──────────────── SERVER (x86 Linux board) ──────────┐
│                                             ▼                                                     │
│  ┌─ twampy reflector (UDP :862) ──────────────────────────────┐                                   │
│  │  RFC 5357 responder: timestamps each probe and echoes it   │                                   │
│  │  back → lets the gateway compute RTT / jitter / loss        │                                   │
│  │  per QoS class (DSCP EF vs Best Effort)                     │                                   │
│  └─────────────────────────────────────────────────────────────┘                                   │
│                                                                                                    │
│  ┌─ iperf3 server ────────────────────────────────────────────┐                                   │
│  │  target of stress_test.sh: 50 Mbps UDP flood (Best Effort) │                                   │
│  │  used to prove the EF slice stays low-jitter under load    │                                   │
│  └─────────────────────────────────────────────────────────────┘                                   │
│                                                                                                    │
│  ┌─ docker-compose.yml ───────────────────────────────────────┐                                   │
│  │  InfluxDB 2.7 (:8086)  org=dawn6g bucket=gateway_telemetry │                                   │
│  │       ▲  (telemetry push planned — client dep is in the    │                                   │
│  │       │   Makefile, not yet wired into main.go)            │                                   │
│  │  Grafana (:3000) ── reads InfluxDB → dashboards            │                                   │
│  └─────────────────────────────────────────────────────────────┘                                   │
└────────────────────────────────────────────────────────────────────────────────────────────────────┘
```

## How the two measurement paths complement each other

|                  | **Passive path** (eBPF)                          | **Active path** (TWAMP + mmcli)                        |
|------------------|--------------------------------------------------|--------------------------------------------------------|
| **What**         | Observes *real user traffic* leaving the NIC     | Injects *synthetic probes* + reads modem radio stats   |
| **Detects**      | Micro-stalls (>100ms gaps mid-flow), TCP retransmissions | RTT, jitter, loss per QoS class; RSRP/RSRQ/RSSI |
| **Cost**         | Near-zero (runs in kernel, per-packet)           | One 5-probe TWAMP burst + one mmcli call every 5s      |
| **Needs server?**| No — fully local                                 | Yes — the twampy reflector on the server               |

## Notes

- **Raw-IP assumption**: `tc_glitch.bpf.c` maps the packet start directly to
  `struct iphdr` because wwan modem interfaces carry raw IP with no Ethernet
  header. If the program is ever attached to a regular ethernet NIC (e.g. for
  the netem tests in the README), the parse offset is wrong and the
  `ip->version != 4` check silently discards nearly everything — the
  commented-out `ethhdr` path in the C source is the ethernet variant.
- **Struct coupling**: the Go `GlitchEvent` (main.go) and C `glitch_event`
  (tc_glitch.bpf.c) must stay byte-compatible; events cross the ring buffer as
  raw little-endian bytes.
