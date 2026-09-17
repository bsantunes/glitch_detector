# 5G-Glitch-Monitor (eBPF + Go)

## DAWN6G Smart Gateway 🛰️

A high-performance, eBPF-powered 5G gateway agent designed for **SIMCom SIM8202G-M2** modules. It provides active/passive network monitoring, 5QI QoS enforcement, and cloud-based telemetry.

## 🌟 Features
- **Passive Monitoring:** eBPF-based detection of TCP retransmissions and micro-stalls (>100ms).
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

**A high-precision network diagnostic tool for 5G Gateways and Embedded Linux.**

## 📖 Overview

Standard network tools like `ping` or `iperf` measure averages. They often miss the **micro-stalls** (100ms - 500ms gaps) that ruin real-time applications like teleoperation, video streaming, or VoIP.

**5G-Glitch-Monitor** attaches directly to the Linux Kernel via **eBPF** (Traffic Control hook) to observe every single packet leaving the interface. It detects:

1. **Micro-Stalls (Jitter):** When the modem stops sending data for >100ms despite active traffic.
2. **Packet Loss (Retransmissions):** By tracking TCP sequence numbers, it definitively identifies when a packet was lost and resent.
3. **Idle vs. Fault:** Intelligently distinguishes between a broken network and a sleeping application.

---

## 🚀 Architecture

The project consists of two components:

1. **Kernel Space (C):** A lightweight eBPF program (`tc_glitch.bpf.c`) that attaches to the egress path. It calculates Inter-Packet Arrival Time (IPAT) and tracks TCP flows.
2. **User Space (Go):** A high-performance agent (`dawn-agent`) that loads the BPF program, reads events from a Ring Buffer, and prints human-readable diagnostics.

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

| Icon | Event Type | Meaning | Action Required |
| --- | --- | --- | --- |
| 💤 | **Idle Stall** | Gap > 1000ms. App is likely sleeping. | None (Normal). |
| ⚠️ | **Micro-Stall** | Gap 100ms - 1000ms during active flow. | Monitor. Indicates jitter/bufferbloat. |
| 🔴 | **LOSS** | TCP Retransmission detected. | **Critical.** Radio link quality is poor. |

### 4. Modem Detection

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

## 🧪 Testing

To verify the tool is working, you can simulate network faults.

**1. Simulate a Stall (Jitter)**
In a separate terminal, use `netem` to delay packets on the interface:

```bash
# Add 200ms delay to all packets
sudo tc qdisc add dev eth0 root netem delay 200ms

# Watch the agent output "⚠️ [STALL] ~200ms"
# Remove the delay
sudo tc qdisc del dev eth0 root netem

```

**2. Simulate Idle Traffic**
Simply run a slow ping:

```bash
ping -i 1.5 8.8.8.8
# Watch the agent output "💤 [STALL] ~1500ms"

```

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
