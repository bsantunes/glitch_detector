# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

DAWN6G glitch detector: an eBPF-powered 5G gateway agent (`dawn-agent`) that detects micro-stalls (>100ms inter-packet gaps) and TCP retransmissions on a cellular modem interface, combined with active TWAMP probing and ModemManager radio metrics. Targets an x86 Linux (Debian-based) gateway board with a SIMCom SIM8202G-M2 modem — **it builds and runs only on Linux** (eBPF, netlink, mmcli), not on macOS. The Makefile is architecture-agnostic (`uname -m` resolves the clang include path), so ARM64 boards work too.

## Build & Run

```bash
make               # compiles bpf_src/tc_glitch.bpf.c → tc_glitch.o (clang -target bpf) and builds the Go agent
make clean         # removes tc_glitch.o and dawn-agent
sudo make install  # creates /opt/dawn, deploys agent + BPF object + init script, registers systemd unit

sudo ./dawn-agent -iface wwan0 -reflector 10.30.30.1:862   # root required to load eBPF
```

Notes:
- `go.mod` / `go.sum` are gitignored; the Makefile's `deps` target creates them (`go mod init dawn-agent` + `go get`) on a fresh clone.
- The agent loads `tc_glitch.o` from the current working directory at runtime — the BPF object must be rebuilt after editing the C source and must be alongside the binary.
- There are no automated tests. `stress_test.sh` (TWAMP + iperf3 congestion) and `tc qdisc ... netem` (see README) provide manual validation on the target device.

## Architecture

Full two-endpoint component diagram (gateway + server): see `ARCHITECTURE.md`.

Two components that communicate through a BPF ring buffer:

1. **Kernel space — `bpf_src/tc_glitch.bpf.c`**: TC egress program (`egress_monitor`). Tracks per-flow state in an LRU hash map keyed by 5-tuple; emits a `glitch_event` to the `events` ring buffer when the inter-packet gap exceeds 100ms (type 1 = stall) or a TCP sequence number regresses (type 2 = retransmission). It parses packets as **raw IP with no Ethernet header** — correct for 5G modem interfaces (wwan0), wrong for ethernet interfaces.

2. **User space — `main.go`**: loads `tc_glitch.o` via cilium/ebpf, adds a `clsact` qdisc and attaches the program as an egress BPF filter via vishvananda/netlink (`HANDLE_MINEGRESS = 0xFFF2`). Runs two loops:
   - **Passive**: reads `GlitchEvent`s from the ring buffer and prints them (🔴 loss / ⚠️ stall).
   - **Active** (`runActiveMonitoring`, every 5s): shells out to `mmcli` for RSRP/RSRQ/RSSI (5G with LTE fallback; `-32768` sentinel → NaN) and to `twampy sender` for RTT/jitter/loss.

**Struct layout coupling**: Go's `GlitchEvent` (main.go) must match the C `glitch_event` (tc_glitch.bpf.c) field-for-field — events are decoded with `binary.Read` little-endian. Change both sides together.

## Deployment Context

- `dawn-agent.service`: systemd unit; runs `init_gateway.sh` first, then the agent from `/opt/dawn`.
- `init_gateway.sh`: loads kernel modules, configures the modem's dedicated QoS bearer via AT commands (DSCP EF / cid 2), and sets up HTB traffic shaping with a priority lane for EF-marked traffic.
- `docker-compose.yml`: InfluxDB 2.7 + Grafana stack for telemetry (org `dawn6g`, bucket `gateway_telemetry`).

## License Constraint

GPL-2.0 — required for the eBPF program (`__license` section is "GPL"; kernel helpers demand it).
