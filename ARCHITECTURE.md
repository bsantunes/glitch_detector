# DAWN6G Glitch Detector — Architecture

System overview across both endpoints: the gateway (x86 Linux board with a
SIMCom SIM8202G-M2 5G modem) and the server (x86 Linux board hosting the TWAMP
reflector and the telemetry stack).

```
┌─────────────────────────────────── GATEWAY (x86 Linux board) ────────────────────────────────────┐
│                                                                                                  │
│  BOOT / SETUP                                                                                    │
│  ┌─────────────────────────────┐      ┌──────────────────────────────────────────────┐           │
│  │ dawn-agent.service (systemd)│─────▶│ init_gateway.sh (ExecStartPre)               │           │
│  │  Restart=always, logs to    │      │  1. modprobe cls_bpf, sch_ingress, sch_htb   │           │
│  │  /var/log/dawn-agent.log    │      │  2. AT cmds via mmcli → modem QoS bearer     │           │
│  └──────────────┬──────────────┘      │     (cid 2 = DSCP EF / critical traffic)     │           │
│                 │ ExecStart           │  3. tc HTB queues: 1:10 priority (EF),       │           │
│                 ▼                     │     1:20 default — shapes egress             │           │
│  ═══════════ USER SPACE ═══════       └──────────────────────────────────────────────┘           │
│                                                                                                  │
│  ┌─ dawn-agent (Go, main.go) ─────────────────────────────────────────────────────┐              │
│  │                                                                                │              │
│  │  main()                                                                        │              │
│  │   ├─ ebpf.LoadCollectionSpec("tc_glitch.o")  ← loads BPF object from CWD       │              │
│  │   ├─ netlink: add clsact qdisc + attach egress_monitor as TC filter            │              │
│  │   │           (parent = HANDLE_MINEGRESS 0xFFF2 → egress hook)                 │              │
│  │   ├─ modem.Detect(ctx)  ← hybrid modem detection, see below                    │              │
│  │   │                                                                            │              │
│  │   ├─ PASSIVE LOOP ◀──────────────── ring buffer ──────────────┐                │              │
│  │   │   reads GlitchEvent{SrcIP,DstIP,Value,Type,Proto}         │                │              │
│  │   │   Type 1 → "⚠️ Stall: X ms"   Type 2 → "🔴 TCP Loss"      │               │              │
│  │   │   (struct must match C glitch_event byte-for-byte,         │               │              │
│  │   │    decoded little-endian via binary.Read)                  │               │              │
│  │   │                                                            │               │              │
│  │   └─ ACTIVE LOOP (goroutine, every 5s, per detected modem)      │               │              │
│  │       ├─ modem.GetSignal(): RSRP/RSRQ/RSSI/SINR, technology-   │               │              │
│  │       │    agnostic (5g/lte/umts/gsm), NaN for unavailable      │               │              │
│  │       └─ getTwampMetrics(): exec `twampy sender <reflector>`   │               │              │
│  │            parses "Roundtrip:" line → RTT / Jitter / Loss %  ──┼── UDP ──▶ (to server :862)   │
│  └────────────────────────────────────────────────────────────────┼───────────────┘              │
│                                                                   │                              │
│  ═══════════ KERNEL SPACE ═════════════════════════════════════════════════════════              │
│                                                                   │                              │
│  ┌─ tc_glitch.o  (from bpf_src/tc_glitch.bpf.c) ──────────────────┼─────────────┐                │
│  │                                                                │             │                │
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
│                                             │                                                    │
│                                       wwan0 (egress, HTB-shaped, raw IP)                         │
└─────────────────────────────────────────────┼────────────────────────────────────────────────────┘
                                              │
                                   5G modem (SIM8202G-M2)
                        │ USB/PCIe, MBIM — AT commands / signal polling
                        │ via ModemManager (mmcli)
                                              │
                                     5G / IP network
                       (all user traffic + TWAMP probes + iperf3 flood)
                                              │
┌─────────────────────────────────────────────┼──────────────── SERVER (x86 Linux board) ──────────┐
│                                             ▼                                                    │
│  ┌─ twampy reflector (UDP :862) ──────────────────────────────┐                                  │
│  │  RFC 5357 responder: timestamps each probe and echoes it   │                                  │
│  │  back → lets the gateway compute RTT / jitter / loss       │                                  │
│  │  per QoS class (DSCP EF vs Best Effort)                    │                                  │
│  └────────────────────────────────────────────────────────────┘                                  │
│                                                                                                  │
│  ┌─ iperf3 server ────────────────────────────────────────────┐                                  │
│  │  target of stress_test.sh: 50 Mbps UDP flood (Best Effort) │                                  │
│  │  used to prove the EF slice stays low-jitter under load    │                                  │
│  └────────────────────────────────────────────────────────────┘                                  │
│                                                                                                  │
│  ┌─ docker-compose.yml ───────────────────────────────────────┐                                  │
│  │  InfluxDB 2.7 (:8086)  org=dawn6g bucket=gateway_telemetry │                                  │
│  │       ▲  (telemetry push planned — client dep is in the    │                                  │
│  │       │   Makefile, not yet wired into main.go)            │                                  │
│  │  Grafana (:3000) ── reads InfluxDB → dashboards            │                                  │
│  └────────────────────────────────────────────────────────────┘                                  │
└──────────────────────────────────────────────────────────────────────────────────────────────────┘
```

## Modem detection (`modem/` package)

Radio signal quality no longer assumes a single ModemManager modem at index 0.
`modem.Detect(ctx)` runs a hybrid cascade, trying each backend in order and
stopping at the first one that finds a modem — so it works across modem
models/vendors, not just the gateway's SIMCom SIM8202G-M2:

```
modem.Detect(ctx)
  │
  ├─ 1. ModemManager (modem/mmcli.go)
  │     `mmcli -L` lists modems → per modem, `mmcli -m <n> --output-keyvalue`
  │     for identity (manufacturer/model/port) and `--signal-get` for signal.
  │     Tries 5g → lte → umts → gsm sections in priority order (umts maps
  │     rscp/ecio onto RSRP/RSRQ; gsm only ever has rssi). Also issues
  │     `--signal-setup=5` once per modem so ModemManager actually polls
  │     the radio (silently a no-op without root/PolicyKit — falls through
  │     to the other backends only if `mmcli -L` itself fails or finds
  │     nothing, not if signal-setup fails).
  │
  ├─ 2. QMI / MBIM (modem/qmi.go) — used when ModemManager isn't managing
  │     the modem (not installed, or the modem isn't supported). Probes
  │     /dev/cdc-wdm* with qmicli first (`--dms-get-manufacturer` /
  │     `--dms-get-model` — must be separate calls, qmicli rejects
  │     combining DMS actions), then mbimcli. Signal reads
  │     `--nas-get-signal-info` (preferred) or the older, differently
  │     structured `--nas-get-signal-strength`; MBIM only exposes a coarse
  │     RSSI via `--query-signal-state`.
  │
  └─ 3. Raw AT over serial (modem/at.go) — last resort. Scans
        /dev/ttyUSB*/ttyACM*, identifies via `ATI`, reads signal via the
        standard 3GPP `AT+CESQ` command (works across vendors; richer
        vendor-specific commands like Quectel's AT+QCSQ are a possible
        future addition once needed).
```

`modem.GetSignal(ctx, m)` then dispatches back to whichever backend found
the modem. Every backend returns the same technology-agnostic `Signal{
Technology, RSRP, RSRQ, RSSI, SINR}` (NaN for anything unavailable), so
`main.go` and `cmd/modem-tool` don't need to care which backend answered.

**`cmd/modem-tool`** is a standalone CLI built on the same package
(`make modem-tool`, `sudo ./modem-tool`) for developing/testing modem
support independently of the eBPF/TWAMP agent — no clsact qdisc, no root
required beyond what the backend itself needs. Its detection phase lists
every modem found (id, manufacturer, model, port, backend used) before
polling signal quality on an interval.

**Validated on the gateway's SIM8202G-M2** (real hardware, over SSH): the
ModemManager and QMI paths both detect the modem and read live, changing
LTE RSRP/RSRQ/RSSI/SINR correctly. MBIM isn't reachable on this modem (it's
QMI-only), and the AT path — while its `ATI`/`AT+CESQ` parsing is exercised
by unit tests and was confirmed to work via a manual capture — hasn't been
observed end-to-end through `modem.Detect`, since QMI always answers first
in the cascade on this hardware. All pure-parsing logic (`modem/*_test.go`)
is covered by unit tests, several built directly from real captured
`mmcli`/`qmicli` output.

## How the two measurement paths complement each other

|                  | **Passive path** (eBPF)                          | **Active path** (TWAMP + modem/)                       |
|------------------|--------------------------------------------------|--------------------------------------------------------|
| **What**         | Observes *real user traffic* leaving the NIC     | Injects *synthetic probes* + reads modem radio stats   |
| **Detects**      | Micro-stalls (>100ms gaps mid-flow), TCP retransmissions | RTT, jitter, loss per QoS class; RSRP/RSRQ/RSSI/SINR |
| **Cost**         | Near-zero (runs in kernel, per-packet)           | One 5-probe TWAMP burst + one signal read per modem, every 5s |
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
- **Modem detection is best-effort past ModemManager**: the QMI/MBIM/AT
  fallbacks in `modem/` were written against documented `qmicli`/`mbimcli`
  text formats and 3GPP AT command semantics, then corrected against real
  captured output where the gateway's hardware could exercise them (QMI, and
  AT via manual test). Text formats can still drift across `libqmi`/`libmbim`
  versions — if a different modem model takes one of these fallback paths and
  behaves unexpectedly, capture its raw `qmicli`/`mbimcli`/AT output and check
  it against the parsers in `modem/qmi.go` / `modem/at.go` first.
