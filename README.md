# 5G-Glitch-Monitor (eBPF + Go)

## DAWN6G Smart Gateway 🛰️

A high-performance, eBPF-powered 5G gateway agent designed for **SIMCom SIM8202G-M2** modules. It provides active/passive network monitoring, 5QI QoS enforcement, and cloud-based telemetry.

## 🌟 Features
- **Passive Monitoring:** eBPF-based detection of TCP retransmissions and stall episodes, judged against each flow's own adaptive pacing baseline (a fixed 100ms floor for flows with no established pace yet) rather than a single fixed threshold — see [glitch-tool's output](#4-glitch-tool-output) below.
- **Active Auditing:** Multi-profile TWAMP (RFC 5357) to measure 5QI slice performance.
- **Hardware Integration:** Real-time RSRP/RSRQ/RSSI/SINR extraction with automatic modem detection — tries ModemManager, then QMI/MBIM, then raw AT, so it isn't locked to one modem vendor.
- **QoS Enforcement:** Dynamic traffic shaping (HTB) to protect critical flows (DSCP EF) during congestion.
- **Telemetry:** Asynchronous data push to InfluxDB 2.x with Grafana visualization.
- **Roaming Aware:** Intelligent behavior switching between Private NPN and Public carriers.

## 🛠️ Installation

### 1. Prerequisites (x86 Linux gateway)
```bash
sudo apt update
sudo apt install clang llvm libbpf-dev golang modemmanager python3-pip -y
pip3 install twampy --break-system-packages
```

**A high-precision network diagnostic tool for 5G Gateways and Embedded Linux.**

## 📖 Overview

Standard network tools like `ping` or `iperf` measure averages. They often miss the **micro-stalls** (100ms - 500ms gaps) that ruin real-time applications like teleoperation, video streaming, or VoIP.

**5G-Glitch-Monitor** attaches directly to the Linux Kernel via **eBPF** (Traffic Control hook) to observe every single packet leaving the interface. It detects:

1. **Stalls, as episodes:** When a flow's own egress pacing goes quiet for longer than its adaptive baseline, that's reported as one episode (start → end, with a step count), not one event per lagging retry.
2. **Packet Loss (Retransmissions):** By tracking TCP sequence numbers, it definitively identifies when a packet was lost and resent.
3. **Idle vs. Fault:** For TCP, a stall only counts if the flow actually has unacknowledged data in flight — an idle connection with nothing queued to send is never flagged just for going quiet. UDP has no ACK signal to make that same distinction, so it's stall-checked only on ports explicitly opted in (none, by default).

---

## 🚀 Architecture

The project consists of two components:

1. **Kernel Space (C):** `tc_glitch.bpf.c` defines two eBPF programs sharing one flow-state map — `egress_monitor` (attached to the egress hook) does the actual work: inter-packet-gap pacing, stall episodes, and TCP sequence tracking for retransmission detection; `ingress_monitor` (attached to the ingress hook) only tracks the highest TCP ACK seen per flow, so `egress_monitor` can tell a genuinely stuck flow (data in flight, nothing coming back) apart from one that's simply idle. Both hooks live on the same `clsact` qdisc — see [glitch-tool's output](#4-glitch-tool-output) below for exactly what each detects.
2. **User Space (Go):** A high-performance agent (`dawn-agent`, superseded — see below) or the standalone `glitch-tool`/`glitch-exporter`, all built on the shared `glitch/` package, which loads both BPF programs, reads events from a Ring Buffer, and prints/exports human-readable diagnostics.

See [ARCHITECTURE.md](ARCHITECTURE.md) for a full component diagram covering both the gateway and the server side.

---

## 🛠️ Prerequisites

This guide assumes you are running on an **x86 gateway board** (or similar Debian-based Linux system) with a 5G modem attached. ARM64 boards (e.g. Raspberry Pi CM4/CM5) also work — the Makefile resolves the architecture automatically.

### 1. System Dependencies

Install the required compilers and BPF libraries:

```bash
sudo apt update
sudo apt install clang llvm libbpf-dev golang git make

```

### 2. Kernel Header Check

Ensure your include paths are correct for your architecture (fixes common `asm/types.h` errors):

```bash
# Verify this directory exists
ls -d /usr/include/$(uname -m)-linux-gnu

```

---

## 📦 Installation & Compilation

### 1. Clone the Repository

```bash
git clone git@github.com:bsantunes/glitch_detector.git
cd glitch-detector

```

### 2. Directory Structure Setup

Ensure your C source code is in a subfolder to prevent Go build errors.

```bash
mkdir -p bpf_src
# Move your C file here if it isn't already
# mv tc_glitch.bpf.c bpf_src/

```

### 3. Build Everything with Make

A `Makefile` is provided so you don’t need to manually compile the C program or
invoke the Go toolchain. From the project root:

```bash
# compile the eBPF object and build the agent
make

# clean build artifacts if needed
make clean
```

If you're running `go vet ./...`/`go test ./...` directly instead of through
`make`, run `make embed` first — the `glitch` package embeds the compiled BPF
object via `go:embed`, so that file has to exist before the Go toolchain can
even compile the package, not just before running the final binary.

**`dawn-agent` itself (`main.go`) is superseded** by `glitch-exporter` +
`twamp-exporter` + `modem-exporter` running side by side — the eBPF passive
detection, TWAMP, and modem telemetry it used to do in one process now each
live in their own package/tool/exporter (`glitch/`, `twamp/`, `modem/`), with
events actually reaching InfluxDB instead of only being printed. `dawn-agent`
is kept in the tree for now but is no longer built or installed by `make`/
`make install` — use `make dawn-agent`/`sudo make install-dawn-agent`
explicitly if you still need it. Note that it loads `tc_glitch.o` itself and
still decodes the old, smaller `glitch_event` layout, which no longer matches
what the shared BPF object now emits — don't run it alongside `glitch-tool`/
`glitch-exporter` on the same interface, and don't trust its stall/retrans
output as-is.

**A clsact handle bug meant every result from `dawn-agent` (and any build
before the `glitch/` package's egress/ingress handle fix) was actually
measuring ingress (downlink) traffic, not egress** — the constant used for
"egress" was `0xFFF2`, which is `TC_H_MIN_INGRESS` in the real kernel ABI;
`TC_H_MIN_EGRESS` is `0xFFF3`. If you have old captured output or test
results from before this fix, treat them as describing the downlink, not
the uplink, regardless of what they were labeled as at the time.

### 4. Install as a Service (optional)

To deploy the agent permanently on the gateway, install it into `/opt/dawn`
(the directory is created if it doesn't exist) and register the systemd unit:

```bash
sudo make install
sudo systemctl enable --now dawn-agent
```

This copies `dawn-agent`, `tc_glitch.o` and `init_gateway.sh` into `/opt/dawn`
and installs `dawn-agent.service` into `/etc/systemd/system/`. Logs go to
`/var/log/dawn-agent.log`.

---

## 🚦 Usage

### 1. Configure and Run the Agent

The agent defaults to the `wwan0` interface (appropriate for most cellular modems)
and the TWAMP reflector is set to `dawn6g.duckdns.org`. You can override both
parameters at runtime with command-line flags:

```bash
sudo ./dawn-agent -iface wwan0 -reflector my.reflector.example.com
```

There’s no longer a need to edit source code for these values; the flags make the
behavior flexible. If you prefer different compile‑time defaults, modify the
constants in `main.go` and rebuild using `make`.  

(Flags are parsed by Go's `flag` package; run `./dawn-agent -h` for help.)

### 2. Run the Monitor

**Root privileges are required** to load eBPF programs.

```bash
sudo ./dawn-agent

```

### 3. Interpret the Output

This table describes `dawn-agent`'s own output specifically — it's unchanged,
frozen legacy code (see the superseded note above) still using a single fixed
gap threshold, not the adaptive model below.

| Icon | Event Type | Meaning | Action Required |
| --- | --- | --- | --- |
| 💤 | **Idle Stall** | Gap > 1000ms. App is likely sleeping. | None (Normal). |
| ⚠️ | **Micro-Stall** | Gap 100ms - 1000ms during active flow. | Monitor. Indicates jitter/bufferbloat. |
| 🔴 | **LOSS** | TCP Retransmission detected. | **Critical.** Radio link quality is poor. |

### 4. glitch-tool Output

`glitch-tool`/`glitch-exporter` (and `dawn-agent`'s own original pre-Phase-2
code, conceptually) use a different, more refined model than the fixed-
threshold table above. Run it the same way, just with its own binary:

```bash
make glitch-tool
sudo ./glitch-tool -iface wwan0
```

| Icon | Event Type | Meaning |
| --- | --- | --- |
| 🔴 | **Retransmission** | A TCP sequence number went backwards with real payload attached — the kernel resent data it already sent once. Independent of the stall logic below; fires regardless of pacing. |
| 🟡 | **Stall START** | The first gap in this flow's egress pacing that exceeded its adaptive baseline. Emitted immediately (not buffered until the episode ends), so a consumer reacting in a closed loop (e.g. QoS re-routing) doesn't have to wait for recovery to find out something is wrong. |
| ✅ | **Stall END** | The episode closed: a normal-paced packet arrived again. Carries the **total duration** (from the last good packet before the episode to the one that closed it) and the **step count** (how many qualifying gaps were folded into the episode — e.g. a TCP RTO backoff retrying 4 times shows up as one episode with `steps=4`, not four separate events). |

**Stalls are reported as episodes, not one event per lagging packet.** Every
qualifying gap after the first silently extends the open episode (tracked
internally as a step count) instead of emitting its own event; only the
first gap (START) and the eventual recovery (END) are ever printed. This
matters specifically for TCP retransmission backoff, where a single real
outage produces several retries at growing intervals — without episode
tracking, that reads as a burst of unrelated stall events instead of the one
underlying problem it actually is.

**A gap only counts toward a stall at all if it's eligible:**
- **TCP:** only once the flow has genuine outstanding data — bytes already
  sent (`end_seq`) but not yet acknowledged by the remote peer. A second,
  separate eBPF program (`ingress_monitor`, attached to the same interface's
  ingress hook) tracks the highest ACK number returned for each flow
  specifically so this comparison is possible. An idle TCP connection with
  nothing queued to send is never flagged just for going quiet — only a
  connection that is demonstrably waiting on data it already sent is.
- **UDP:** never eligible by default — there's no ACK signal for UDP, so
  there's no way to tell "stuck" apart from "nothing queued" the way TCP
  allows. A port (checked against either side of the flow) can opt in via
  the `UDP_MONITOR_PORTS` load-time constant array in `bpf_src/tc_glitch.bpf.c`
  (empty by default — rebuild after editing it to take effect).

Either way, a gap is also never eligible until the flow has seen at least 8
packets (`STALL_MIN_PACKETS`) — a brand-new flow's first few gaps don't yet
represent its real pace, so the adaptive baseline isn't trustworthy until
then.

### 5. Modem Detection

On startup, `dawn-agent` auto-detects whatever modem is present instead of
assuming a fixed ModemManager index. It tries, in order, **ModemManager**
(`mmcli`), then **QMI/MBIM** (`qmicli`/`mbimcli`), then raw **AT** commands
over serial — stopping at the first backend that finds a modem. You'll see
what it found logged at startup:

```
📶 Modem detected: [0] QUALCOMM INCORPORATED 0 (via ModemManager)
📊 [STATUS] Modem[0/lte] Radio: -86.0 RSRP / -10.0 RSRQ / -55.0 RSSI | TWAMP: ...
```

(The model showing as `0` above is real, captured output — the SIM8202G-M2's
ModemManager plugin genuinely reports `"0"` as `modem.generic.model`, not a
parsing bug.)

If no modem is found at all, the agent logs a warning and keeps running —
the eBPF/TWAMP paths don't depend on radio metrics being available.

To detect and poll modem signal quality on its own, without the eBPF/TWAMP
side (e.g. while bringing up support for a new modem model), use the
standalone CLI built from the same code:

```bash
make modem-tool
sudo ./modem-tool                # detect, then poll every 5s
sudo ./modem-tool -interval 10s  # custom poll interval
```

See [ARCHITECTURE.md](ARCHITECTURE.md#modem-detection-modem-package) for how
the detection cascade and each backend work.

---

## 🧰 Other Standalone Tools & Exporters

`modem/`, `twamp/`, and `gps/` are shared packages, each with its own pair of
binaries: a `*-tool` for ad hoc/interactive inspection (prints to stdout
forever, no InfluxDB, no root required), and a `*-exporter` meant to run as
a continuous background service, pushing to InfluxDB on an interval. None
of the three needs the eBPF/TC side at all — they're independent of
`dawn-agent`/`glitch-tool`/`glitch-exporter`.

Every exporter needs an InfluxDB token, either via `-influx-token` or the
`INFLUX_TOKEN` environment variable — when installed as a service
(`sudo make install-*-exporter`), they all read it from the same
`/opt/dawn/modem-exporter.env` file (copy `modem-exporter.env.example` there
first and fill in a real token from `add-device.sh`; `twamp-exporter`'s and
`gps-exporter`'s own install targets reuse that file rather than needing
their own).

### modem-tool / modem-exporter

Detects modems (ModemManager → QMI/MBIM → `uqmi` → AT) and reads
RSRP/RSRQ/RSSI/SINR + cell/registration info — see
[§5 Modem Detection](#5-modem-detection) above for `modem-tool`'s usage and
output.

```bash
make modem-exporter
sudo ./modem-exporter -influx-url http://10.30.30.1:8086 -influx-token "$INFLUX_TOKEN"
# optional: -interval 30s (default) -influx-org wavecom -influx-bucket radio -device-id <name>
```

Writes `radio_cell` (one point per active radio leg — two under EN-DC) and
`modem_state` to InfluxDB. Install as a service with
`sudo make install-modem-exporter` (see `modem-exporter.service`).

### twamp-tool / twamp-exporter

Sends TWAMP-Test ("TWAMP Light", unauthenticated, no TWAMP-Control
handshake) probe bursts to a reflector that's already listening, and
reports RTT/jitter/loss:

```bash
make twamp-tool
sudo ./twamp-tool -reflector 10.30.30.1:862
# optional: -interval 5s (default) -count 5 -probe-interval 100ms -timeout 2s
```

`twamp-exporter` runs the same probe on an interval and pushes a `twamp`
point (`rtt_ms`/`jitter_ms`/`loss_pct`/`sent`/`received`) to InfluxDB's
`network` bucket each burst:

```bash
make twamp-exporter
sudo ./twamp-exporter -reflector 10.30.30.1:862 -influx-url https://wservices.wavesys.pt:8086 -influx-token "$INFLUX_TOKEN"
# optional: -interval 30s (default) -influx-org wavecom -influx-bucket network -device-id <name>
```

Install as a service with `sudo make install-twamp-tool` /
`sudo make install-twamp-exporter`.

### gps-tool / gps-exporter

Detects GPS/GNSS-capable modems (ModemManager's `gps-raw` source, or
AT-over-serial as a fallback — raw QMI is implemented but deliberately
disabled, see `CLAUDE.md`'s `gps/` package section) and polls for a fix:

```bash
make gps-tool
sudo ./gps-tool
# optional: -interval 10s (default)
```

Until a GPS antenna is actually connected, expect `no GPS fix yet` forever
— that's the hardware, not the tool; the detection/polling path itself
works correctly without one. `gps-exporter` polls the same way and writes
a `gps_fix` point (`latitude`/`longitude`/`altitude`) to InfluxDB's
`location` bucket, but **only once a fix is valid** — no point is written
at all while there's no fix, rather than writing meaningless zeros (unlike
signal metrics, `(0, 0)` is a real coordinate):

```bash
make gps-exporter
sudo ./gps-exporter -influx-url https://wservices.wavesys.pt:8086 -influx-token "$INFLUX_TOKEN"
# optional: -interval 30s (default) -influx-org wavecom -influx-bucket location -device-id <name>
```

Install as a service with `sudo make install-gps-tool` /
`sudo make install-gps-exporter`.

---

## 🧪 Testing

To verify the tool is working, you can simulate network faults. The two
approaches below test different code paths — `dawn-agent`'s own fixed-
threshold logic vs. `glitch-tool`'s adaptive, ACK-gated one — and aren't
interchangeable.

**1. `dawn-agent` (fixed threshold): simulate a stall with `netem` delay**
In a separate terminal, use `netem` to delay packets on the interface:

```bash
# Add 200ms delay to all packets
sudo tc qdisc add dev eth0 root netem delay 200ms

# Watch the agent output "⚠️ [STALL] ~200ms"
# Remove the delay
sudo tc qdisc del dev eth0 root netem

```

**2. `dawn-agent` (fixed threshold): simulate idle traffic**
Simply run a slow ping:

```bash
ping -i 1.5 8.8.8.8
# Watch the agent output "💤 [STALL] ~1500ms"

```

**3. `glitch-tool`: real TCP retransmissions on the monitored interface**
Ping alone won't exercise the retransmission path at all (no TCP, no real
loss) — drive real traffic through real loss instead. The monitored
interface has to be the one actually carrying the traffic: if the box has
more than one route to the target (e.g. a wired `enp1s0` default route
alongside `wwan0`), force egress onto the one you're testing with
`iperf3 --bind-dev`, and sanity-check with `ip route get <target> oif wwan0`
first — a quiet result otherwise just means the traffic never crossed the
monitored interface at all, not that nothing went wrong.

```bash
sudo ./glitch-tool -iface wwan0 &
sudo tc qdisc add dev wwan0 root netem loss 10%
iperf3 -c <some-iperf3-server> --bind-dev wwan0 -t 10
sudo tc qdisc del dev wwan0 root netem
# Watch glitch-tool output a 🔴 line per real retransmission — compare
# the count against iperf3's own reported "Retr" column.
```

**4. `glitch-tool`: force a stall episode**
A forced outage on a *low-throughput* flow (small TCP congestion window,
so the sender genuinely exhausts what it can send and goes quiet) reliably
shows one 🟡/✅ episode pair:

```bash
sudo ./glitch-tool -iface wwan0 &
iperf3 -c <some-iperf3-server> --bind-dev wwan0 -t 10 -b 500K &
sleep 3
sudo tc qdisc add dev wwan0 root netem loss 100%
sleep 1
sudo tc qdisc del dev wwan0 root netem
# Watch for one 🟡 Stall START followed by one ✅ Stall END with
# duration >= 1s once the outage clears.
```

A *high-throughput, large-cwnd* bulk flow (no `-b` limit) forced through
the same 1s total-loss outage is a harder case: the sender may have enough
already-queued congestion window to keep transmitting new data throughout
the outage without ever actually going quiet on egress, so no stall episode
fires even though the retransmission burst afterward is very real — this
is an open area of investigation for the episode/ACK-gating logic, not a
confirmed intended behavior.

---

## 🤝 Troubleshooting

**Error: `fatal error: 'asm/types.h' file not found**`

* **Fix:** You missed the `-I/usr/include/$(uname -m)-linux-gnu` flag in the clang command. See step 3.

**Error: `Failed to attach TC filter: invalid argument**`

* **Fix:** Ensure you are using `sudo`. If it persists, run `sudo modprobe cls_bpf` and `sudo modprobe sch_ingress`.

**Error: `Link not found**`

* **Fix:** Run the agent with `-iface <name>` to match your actual network interface (check with `ip link`).

**Warning: `no modems detected (tried ModemManager, QMI/MBIM, AT)`**

* **Fix:** Confirm the modem shows up at all — `mmcli -L`, or `ls /dev/cdc-wdm* /dev/ttyUSB*` if ModemManager isn't managing it. Radio metrics are simply unavailable until a modem is found; the rest of the agent keeps running.

**Radio values stay `N/A` even though a modem was detected (ModemManager path)**

* **Fix:** `mmcli --signal-setup` (which starts ModemManager's radio polling) needs root/PolicyKit authorization — run `dawn-agent`/`modem-tool` with `sudo`, not as an unprivileged user.

---

## 📜 License

This project is licensed under the GPL-2.0 License (required for eBPF kernel compatibility).
