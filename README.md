# 5G-Glitch-Monitor (eBPF + Go)

## DAWN6G Smart Gateway 🛰️

A high-performance, eBPF-powered 5G gateway agent designed for **SIMCom SIM8202G-M2** modules. It provides active/passive network monitoring, 5QI QoS enforcement, and cloud-based telemetry.

## 🌟 Features
- **Passive Monitoring:** eBPF-based detection of TCP retransmissions and micro-stalls (>100ms).
- **Active Auditing:** Multi-profile TWAMP (RFC 5357) to measure 5QI slice performance.
- **Hardware Integration:** Real-time RSRP/RSRQ extraction via ModemManager.
- **QoS Enforcement:** Dynamic traffic shaping (HTB) to protect critical flows (DSCP EF) during congestion.
- **Telemetry:** Asynchronous data push to InfluxDB 2.x with Grafana visualization.
- **Roaming Aware:** Intelligent behavior switching between Private NPN and Public carriers.

## 🛠️ Installation

### 1. Prerequisites (Raspberry Pi CM5)
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

---

## 🛠️ Prerequisites

This guide assumes you are running on a **Raspberry Pi (CM4/CM5)** or similar Debian-based ARM64 system.

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

### 3. Compile the eBPF Kernel Program

We use `clang` with the specific include path for ARM64 headers.

```bash
clang -O2 -g -target bpf \
    -I/usr/include/$(uname -m)-linux-gnu \
    -c bpf_src/tc_glitch.bpf.c -o tc_glitch.o

```

*Output: This generates `tc_glitch.o` in your root directory.*

### 4. Build the User-Space Agent

Initialize the Go module (if not done) and build the binary.

```bash
# Initialize module
go mod init dawn-agent
go get github.com/cilium/ebpf
go get github.com/vishvananda/netlink
go get github.com/influxdata/influxdb-client-go/v2

# Build binary
go build -o dawn-agent

```

---

## 🚦 Usage

### 1. Configure the Interface

By default, the agent attaches to `eth0`. If you are using a cellular modem (e.g., `wwan0` or `usb0`), edit `main.go` before building:

```go
// main.go
const IfaceName = "wwan0" // Change this to your interface

```

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

* **Fix:** Update `IfaceName` in `main.go` to match your actual network interface (check with `ip link`).

---

## 📜 License

This project is licensed under the GPL-2.0 License (required for eBPF kernel compatibility).
