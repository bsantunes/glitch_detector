# DAWN6G Glitch Detector — Architecture

System overview across both endpoints: the gateway (x86 Linux board with a
5G modem) and the server (x86 Linux board hosting the TWAMP reflector and the
telemetry stack).

**The diagram and kernel-space description immediately below are `dawn-agent`'s own pipeline specifically — `dawn-agent` is now superseded** by `glitch-exporter` + `twamp-exporter` + `modem-exporter` running side by side (kept in the tree, not deleted, but no longer built/installed by default — see `CLAUDE.md`'s Project Overview). Its eBPF kernel-space box below is also stale in a second way: `dawn-agent`'s own Go decode logic was never updated alongside the shared BPF program's later changes, so it no longer matches what's actually compiled into `tc_glitch.o` today (wrong struct layout, and it attaches to the wrong clsact hook — see the "Current pipeline" section right after this diagram for what's actually accurate now). Read the current pipeline section for anything you intend to rely on; treat this diagram as a historical/dawn-agent-specific reference only.

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

## Current pipeline (`glitch/` package — supersedes the diagram above)

```
┌──────── GATEWAY, glitch-tool / glitch-exporter (glitch/, glitch/core, glitch/afpacket) ────────┐
│                                                                                       │
│  ═══ KERNEL SPACE — bpf_src/tc_glitch.bpf.c (same source file as the diagram above,  │
│       dawn-agent's own decode logic just never caught up to its later changes) ═══   │
│                                                                                       │
│  ┌─ egress_monitor(skb) ───────────────┐   ┌─ ingress_monitor(skb) ──────────────┐   │
│  │ SEC("tc"), clsact EGRESS hook        │   │ SEC("tc"), clsact INGRESS hook      │   │
│  │ (TC_H_MIN_EGRESS = 0xFFF3)           │   │ (TC_H_MIN_INGRESS = 0xFFF2)         │   │
│  │                                       │   │                                      │   │
│  │ parse raw IP (no Ethernet header —   │   │ parse raw IP, TCP only              │   │
│  │ correct for wwan0)                   │   │                                      │   │
│  │ key = 5-tuple → flow_tracker         │   │ key = 5-tuple, SWAPPED src/dst       │   │
│  │ (creates entry if new; seeds         │   │ (this pkt's src is the remote peer) │   │
│  │  highest_ack = this pkt's own seq —  │   │ → flow_tracker (lookup only, never  │   │
│  │  NOT 0, see "Historical bug" below)  │   │   creates — egress_monitor owns     │   │
│  │                                       │   │   the flow's lifecycle)             │   │
│  │ STALL check, per packet:              │   │ highest_ack = max(highest_ack,      │   │
│  │  baseline = max(100ms,                │   │   this pkt's ack_seq) — signed-diff │   │
│  │              4 × flow's EWMA gap)      │   │   compare, wraparound-safe          │   │
│  │  eligible = pkt_count>=8 AND           │   └──────────────────────────────────────┘   │
│  │    (TCP: end_seq > highest_ack  |          ▲                                        │
│  │     UDP: port in UDP_MONITOR_PORTS)        │ shared flow_tracker map                │
│  │  gap > baseline & eligible?                │ (BPF_MAP_TYPE_LRU_HASH, 10k flows)     │
│  │   ├─ first such gap → STALL_START ─────────┼──────────┐                             │
│  │   ├─ later ones → silently extend          │          │                             │
│  │   │   (step counter, no event)             │          │                             │
│  │   └─ gap normalizes → STALL_END            │          │                             │
│  │       (total duration + step count) ───────┼──────────┤                             │
│  │                                             │          │                             │
│  │ RETRANS check, independent of the above:     │          │                             │
│  │  seq behind end_seq (signed-diff,            │          │                             │
│  │  wraparound-safe) AND payload_len>0?         │          ▼                             │
│  │   └─ RETRANS event ──────────────────────────┼──▶ events (BPF_MAP_TYPE_RINGBUF, 16KiB)│
│  └───────────────────────────────────────────────────────────────────────────────────┘   │
│                                                           │                                │
│  ═══ USER SPACE — two interchangeable capture backends behind glitch.Source ═══         │
│  (Events() / RetransCount() / Dropped() / Err() / Close(); picked with -capture)          │
│                                                                                           │
│  -capture ebpf (default; Debian/aaeon)        -capture afpacket (WaveOS/dawn6g — no       │
│  glitch/glitch.go, Handle                      clsact/TC-BPF in that kernel)              │
│  Open(iface): loads both programs above,       glitch/afpacket, Capture                   │
│  attaches both hooks, reads the ring buffer,   Open(iface): TPACKET_V3 ring + classic     │
│  decodes wireEvent (48 bytes, little-endian).  BPF filter (TCP + opted-in UDP ports),     │
│  RETRANS records are only counted              no eBPF at all. Every packet goes to       │
│  (retransCount) — not forwarded as Events.     glitch/core.Engine: the same algorithm    │
│  Lock: /var/run/glitch-<iface>.lock            as tc_glitch.bpf.c, ported to Go          │
│                                                (stalls, ACK gate, retrans counted        │
│                                                directly; bounded flow table: idle expiry, │
│                                                10k cap, stall_abandoned on eviction).     │
│                                                Lock: /var/run/glitch-afpacket-<iface>.lock│
│                                                (/var/run — WaveOS has no /run)            │
│       │                                        │                                          │
│       └──────────── Event{Time, SrcIP, DstIP, SrcPort, DstPort, Value, Baseline,          │
│                     Type (stall start/end/abandoned), Steps, Proto} ───┐                  │
│                                                                        ▼                  │
│  cmd/glitch-tool                      cmd/glitch-exporter                                 │
│  prints 🟡 START / ✅ END / 💀        aggregates (see Aggregator, cmd/glitch-exporter/     │
│  ABANDONED per episode, plus a 📊     aggregate.go) → glitch_summary (heartbeat, every    │
│  running total every 5 s:             -interval, incl. retrans_count from RetransCount()  │
│  retrans=<cumulative> dropped=<n>     and the kernel/user/write-queue drop counters) +    │
│  (+ kernel/user/untruncated_over_     glitch_event (rate-limited, stall_start/stall_end/  │
│  limit for afpacket). No per-retrans  stall_abandoned) → InfluxDB "network" bucket via a  │
│  lines. No InfluxDB.                  bounded write queue                                 │
└─────────────────────────────────────────────────────────────────────────────────────────┘
```

**Historical bug, fixed, relevant to interpreting old data:** the constant `dawn-agent`'s diagram above calls "egress" (`HANDLE_MINEGRESS = 0xFFF2`) is actually `TC_H_MIN_INGRESS` in the real kernel ABI — `dawn-agent` itself still has this bug (frozen/superseded, never fixed there). `glitch/glitch.go` fixed it (commit `023a517`): `handleMinEgress` is now `0xFFF3`, and a new `handleMinIngress = 0xFFF2` constant is used for `ingress_monitor`'s own attachment. Every result measured against the old code — `dawn-agent`'s own output and the original pre-refactor stress test included — was observing ingress (downlink) traffic, not egress (uplink), regardless of its label at the time.

**`highest_ack`'s seed value, fixed, the other historical bug worth knowing about:** seeding it from a bare `0` (rather than the flow's own first packet's seq) made the TCP stall gate permanently non-functional for roughly half of all connections — see `CLAUDE.md`'s `glitch/` package section for the full signed-arithmetic explanation and `bpf_src/verify_highest_ack_wraparound.sh` for the regression test. Fixed in commit `e1b0945`.

**Known limitations of this design** (see `CLAUDE.md`'s `glitch/` package section for the full writeup): stalls are only known once the silence ends, not while it lasts; an outage shorter than about one RTT can leave no egress gap at all if the sender's own congestion window keeps it transmitting throughout, surfacing only as a retransmission burst; a TCP stall episode's reported duration includes RTO backoff time, so it can run roughly 2× the underlying outage; UDP stall detection is opt-in per port, compiled in, empty by default.

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
  ├─ 2. ubus (modem/ubus.go) — WaveOS only: reads the uqmi-ubus daemon's
  │     `qmi` object (`ubus call qmi metrics`). That daemon already owns
  │     one serialized QMI/AT session per modem, so reading its results
  │     avoids opening a competing qmicli/uqmi client (which exhausted
  │     the modem's QMI client-ID pool when run alongside it). Preferred
  │     over raw QMI for exactly that reason.
  │
  ├─ 3. QMI / MBIM (modem/qmi.go) — used when ModemManager isn't managing
  │     the modem (not installed, or the modem isn't supported). Probes
  │     /dev/cdc-wdm* with qmicli first (`--dms-get-manufacturer` /
  │     `--dms-get-model` — must be separate calls, qmicli rejects
  │     combining DMS actions), then mbimcli. Signal reads
  │     `--nas-get-signal-info` (preferred) or the older, differently
  │     structured `--nas-get-signal-strength`; MBIM only exposes a coarse
  │     RSSI via `--query-signal-state`.
  │
  ├─ 4. uqmi (modem/uqmi.go) — OpenWrt's lightweight QMI client, for
  │     systems (like WaveOS) that have uqmi but not qmicli/mbimcli.
  │
  └─ 5. Raw AT over serial (modem/at.go) — last resort. Scans
        /dev/ttyUSB*/ttyACM*, identifies via `ATI`, reads signal via the
        standard 3GPP `AT+CESQ` command (works across vendors; richer
        vendor-specific commands like Quectel's AT+QCSQ are a possible
        future addition once needed).
```

`modem-exporter` calls `modem.DetectValidated`, which runs the same cascade
in the same order but only accepts a backend whose signal read returns real
(non-N/A) values — `ubus` can exist moments before its daemon has anything
to report.

**Periodic re-detection (`modem-exporter`, since `v2.3.1`).** Detection is
no longer a one-shot at startup: every 5 minutes `modem-exporter` re-runs
the validated cascade and switches method if (a) a better-ranked method has
become available — e.g. after a reboot, when `uqmi-ubus` wasn't ready yet
and detection settled on raw QMI — or (b) the current method has gone bad:
its data is stale (per-field 30-minute change detection, and `ubus`'s own
source timestamp not advancing) or `GetSignals` fails outright. In case (b)
it falls back even to a worse-ranked method (`DetectValidatedExcluding`
skips the stale one so it isn't re-selected), and moves back once the
better method recovers. Every switch is logged with its reason; all modems
switch together, never a partial swap. `modem-tool` still detects once.

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

## GPS detection (`gps/` package)

```
gps.Detect(ctx)
  │
  ├─ 1. ModemManager (gps/gps.go, enableModemManager)
  │     `--location-enable-gps-raw`, then reads already-parsed decimal
  │     modem.location.gps.{latitude,longitude,altitude} — no NMEA
  │     parsing needed. Disabled on aaeon specifically by policy (never
  │     call mmcli --location-* there, never restart ModemManager —
  │     see CLAUDE.md's gps/ package section for why), independent of
  │     whether it would otherwise work.
  │
  ├─ 2. Raw QMI (gps/qmi.go, QMISession) — implemented, but Supports()
  │     deliberately refuses it. A persistent background NMEA session
  │     (qmicli has no one-shot "get current fix" command), gated off
  │     after repeatedly corrupting production QMI client state on
  │     6global — see CLAUDE.md's incident writeup before ever
  │     re-enabling it.
  │
  └─ 3. AT-over-serial (gps/at.go) — the backend actually deployed.
        SIMCom (AT+CGPS=1,1 / AT+CGPSINFO) or Quectel (AT+QGPS=1 /
        AT+QGPSLOC=2), chosen via detectATGPSVendor. AT always starts
        the GNSS engine and XTRA assistance data (AT+CGPSXE=1 on
        SIMCom). modem.PortInUse skips any serial port another process
        already has open. Ports are set up with modem.OpenRawSerial
        (direct termios: raw, no echo, TCSANOW, then flush — no
        external stty since v2.3.2).

Fix source, per poll (gps.Poller.Fix), once the AT poller exists:

  -nmea-port set? ── no ──▶ AT position query every poll
        │                   (AT+CGPSINFO / AT+QGPSLOC=2)
       yes
        ▼
  NMEAReader (gps/nmea.go): background reader on the modem's NMEA
  port (interface 01), keeps the latest RMC (validity, lat/lon,
  speed, course) + GGA (satellites, HDOP, altitude).
        │
  stream alive? (an RMC/GGA sentence within the last 5 s)
        ├─ yes ─▶ NMEA fix (source="nmea"; RMC "A"/"V" decides Valid)
        └─ no ──▶ AT fallback for this poll (each switch logged once,
                   back to NMEA as soon as sentences resume) — v2.3.2+

Which board uses which:
  aaeon  — SIMCom SIM8202G: -nmea-port usb:1e0e:9001:01. Its AT
           position commands (AT+CGPSINFO, AT+CGNSSINFO) report no
           fix even while the NMEA port has one.
  dawn6g — Quectel RM520N-GL: AT only (-port usb:2c7c:0801:03); its
           AT fixes arrive, and its NMEA streaming is unresolved.
```

`gps.Detect` does **not** reuse `modem.Detect()`'s own cascade result or
stopping rule — it tries ModemManager, then raw QMI/MBIM, then AT, but
stops at the first step `Supports` will actually use (`anySupported`), not
the first step that finds any modem at all. That distinction matters on
WaveOS: raw `qmicli` reliably answers a modem there, but it's unusable for
GPS (disabled), so without `anySupported` AT would never be reached. See
CLAUDE.md's `gps/` package section for the full backend/incident history,
including why `aaeon`'s ModemManager `GeneralError` is not a missing-antenna
issue despite earlier documentation having theorized that it was.

**`cmd/gps-tool`/`cmd/gps-exporter`** mirror `modem-tool`/`modem-exporter`'s
shape: detect, then poll on an interval (`gps-tool` prints forever, no
InfluxDB; `gps-exporter` pushes `gps_fix` points to InfluxDB's `location`
bucket, only when a fix is valid — see `metrics-endpoint.md`). `gps-exporter`
runs on `dawn6g` (AT) and `aaeon` (NMEA, `v2.3.2`); `6global` is switched
off. `gps-tool` isn't installed anywhere as a running service.

## How the two measurement paths complement each other

|                  | **Passive path** (eBPF)                          | **Active path** (TWAMP + modem/)                       |
|------------------|--------------------------------------------------|--------------------------------------------------------|
| **What**         | Observes *real user traffic* leaving the NIC     | Injects *synthetic probes* + reads modem radio stats   |
| **Detects**      | Stall episodes (adaptive per-flow baseline, TCP ACK-gated — see Current pipeline above), TCP retransmissions | RTT, jitter, loss per QoS class; RSRP/RSRQ/RSSI/SINR |
| **Cost**         | Near-zero (runs in kernel, per-packet)           | One 5-probe TWAMP burst + one signal read per modem, every 5s |
| **Needs server?**| No — fully local                                 | Yes — the twampy reflector on the server               |

## Notes

- **Raw-IP assumption**: `tc_glitch.bpf.c` maps the packet start directly to
  `struct iphdr` because wwan modem interfaces carry raw IP with no Ethernet
  header. If the program is ever attached to a regular ethernet NIC (e.g. for
  the netem tests in the README), the parse offset is wrong and the
  `ip->version != 4` check silently discards nearly everything — the
  commented-out `ethhdr` path in the C source is the ethernet variant.
- **Struct coupling**: a Go decoder and the C `glitch_event` (tc_glitch.bpf.c)
  must stay byte-compatible; events cross the ring buffer as raw little-endian
  bytes. `glitch/glitch.go`'s `wireEvent` is the current, correct decoder (48
  bytes, kept in sync with the C struct and covered by
  `glitch/glitch_test.go`'s `TestWireEventDecode`); `main.go`'s own
  `GlitchEvent` (24 bytes) is the pre-refactor layout and no longer matches
  what's actually compiled into `tc_glitch.o` — see the superseded note at the
  top of this document.
- **Modem detection is best-effort past ModemManager**: the QMI/MBIM/AT
  fallbacks in `modem/` were written against documented `qmicli`/`mbimcli`
  text formats and 3GPP AT command semantics, then corrected against real
  captured output where the gateway's hardware could exercise them (QMI, and
  AT via manual test). Text formats can still drift across `libqmi`/`libmbim`
  versions — if a different modem model takes one of these fallback paths and
  behaves unexpectedly, capture its raw `qmicli`/`mbimcli`/AT output and check
  it against the parsers in `modem/qmi.go` / `modem/at.go` first.
