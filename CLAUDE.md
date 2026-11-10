# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

DAWN6G glitch detector: started as a single eBPF-powered 5G gateway agent (`dawn-agent`), now split into two shared packages (`modem/`, `twamp/`) plus five consumers — `dawn-agent` (eBPF + TWAMP + modem metrics, Debian/ModemManager gateways only), `cmd/modem-tool`/`cmd/twamp-tool` (standalone diagnostic CLIs), and `cmd/modem-exporter`/`cmd/twamp-exporter` (long-running InfluxDB/Grafana telemetry push). **Builds and runs only on Linux** (eBPF, netlink, mmcli/qmicli/uqmi/ubus), not on macOS.

Two gateway OS targets exist in the fleet:
- **Debian** (e.g. host `aaeon`) — SIMCom modem via ModemManager, runs the full `dawn-agent` (eBPF works here).
- **WaveOS** (OpenWrt-based, x86_64 Supermicro boards, e.g. hosts `6global`/`dawn6g`) — **confirmed to lack `clsact`/TC-BPF kernel support** (kernel 5.15.167, `netlink.QdiscAdd` fails with ENOENT; `modprobe cls_bpf`/`sch_ingress` also fail). `dawn-agent`'s eBPF path cannot run there — only `modem-tool`/`modem-exporter`/`twamp-tool`/`twamp-exporter` are deployed on WaveOS boards.

## Repository Layout

- `main.go` → `dawn-agent` binary — eBPF passive detection + TWAMP + modem status line (Debian gateways only).
- `modem/` — shared package: hybrid modem detection + signal/cell-info reading. Has unit tests (`go test ./modem/...`), several built from real captured `mmcli`/`qmicli`/`uqmi`/`ubus` output.
- `cmd/modem-tool/` → `modem-tool` binary — detect modems, then poll and print signal+cell info forever. No InfluxDB, no eBPF. See `MODEM_TOOL.md`.
- `cmd/modem-exporter/` → `modem-exporter` binary — detect modems, then push signal+cell info to InfluxDB on an interval. Meant to run as a service, not interactively.
- `twamp/` — shared package: native Go TWAMP-Light sender (no external `twampy`/Python dependency — required for WaveOS, which has no Python at all). Has unit tests (`go test ./twamp/...`).
- `cmd/twamp-tool/` → `twamp-tool` binary — probe a TWAMP-Test reflector on an interval and print RTT/jitter/loss forever. No InfluxDB. Mirrors `modem-tool`'s structure.
- `cmd/twamp-exporter/` → `twamp-exporter` binary — probe a TWAMP-Test reflector, then push RTT/jitter/loss to InfluxDB's `network` bucket on an interval. Mirrors `modem-exporter`'s structure; reuses the same per-device token file.
- `openwrt/*.init` — `procd` init scripts for WaveOS (`modem-tool.init`, `modem-exporter.init`, `twamp-tool.init`, `twamp-exporter.init`; the exporter ones need their `DEVICE_ID`/token edited per board before install).
- `*.service` — `systemd` units for Debian gateways (`dawn-agent.service`, `modem-tool.service`, `modem-exporter.service`, `twamp-tool.service`, `twamp-exporter.service`).
- `metrics-endpoint.md` — **the authoritative handoff doc for the InfluxDB/Grafana telemetry side**, written for Claude Code specifically. Read this before touching `modem-exporter`'s schema or the server stack.
- `dawn6g-metrics/` — a local copy of the actual server stack deployed on `wservices.wavesys.pt` (`~/dawn6g-metrics` there, Apache + Docker Compose InfluxDB/Grafana; `add-device.sh`/`revoke-device.sh`/`bootstrap.sh`/`lib.sh`). Treat this copy as reference, not a thing this repo deploys — the real one lives on that server.
- `DAWN6G Metrics Endpoint — Deployment Tutorial.docx` and `dawn6g-metrics/README.md` — **earlier draft schema docs, superseded by `metrics-endpoint.md`** (they describe `vendor`/`dc_mode` tags and no `rssi`/`cell_id` fields — not what's actually implemented or deployed; don't follow their schema example).

## `modem/` package

Hybrid detection cascade, each tried in order until one finds a modem: **ModemManager → `ubus` (WaveOS's own `qmi` object) → QMI (`qmicli`)/MBIM → `uqmi` → AT-over-serial**. `ubus` is tried before raw QMI/`uqmi` specifically to avoid QMI client-ID exhaustion when running alongside WaveOS's own `uqmi-ubus` daemon (see Known Issues).

- `Signal{Technology, Role, RSRP, RSRQ, RSSI, SINR}` — `Role` is `"anchor"` or `"scg"`. `GetSignals()` returns one `Signal` per simultaneously active radio leg (EN-DC: LTE anchor + NR secondary, confirmed live on `aaeon`); `GetSignal()` is a convenience wrapper returning just the anchor leg. Only ModemManager and QMI (`qmicli`) can currently report >1 leg.
- `CellInfo{PLMN, OperatorName, RegistrationState, PacketServiceState, TAC, CellID}` — TAC/CellID normalized to **decimal** across backends, except the `ubus` backend, which currently passes Quectel's raw **hex** TAC/CellID straight through unconverted (known bug, not yet fixed — flagged but deferred).
- **A modem with no SIM can still report real RSRP/RSRQ/RSSI/SINR/PLMN/TAC/CellID** by camping on a cell's broadcast system information (physical-layer, unauthenticated) — it just can never complete registration/attach (no IMSI to authenticate with). Confirmed live on `6global`. Don't assume "no SIM" implies "no data."
- `SupportsCellInfo(method)` — only ModemManager/`ubus`/QMI/`uqmi` implement `GetCellInfo`; MBIM/AT don't yet (no real hardware to validate against).

## `twamp/` package

Native Go sender for "TWAMP Light" (RFC 5357-style, unauthenticated, no TWAMP-Control handshake — direct UDP TWAMP-Test packets). `Measure(ctx, reflectorAddr, count, probeInterval, timeout) (Result, error)` sends a burst of packets and computes `Result{Sent, Received, AvgRTT, Jitter, LossPct}`.

**Packet sizes deliberately diverge from RFC 5357's stated minimums** (`senderPacketLen = 14`, `reflectedHeaderLen = 38`, not the RFC's 41/41) — found via live `strace -f -e trace=network` on `twampy` itself, which showed the real deployed reflector (`10.30.30.1:862`) expects/sends packets smaller than the RFC minimums. This is intentional: it matches the actual wire behavior of this project's reflector, not the spec. If TWAMP is ever pointed at a different reflector and gets 100% loss, check packet sizes via `strace` again before assuming the sizes are wrong.

Confirmed live with 0% loss against `10.30.30.1:862` from `aaeon`, `6global`, and `dawn6g`.

## Build & Run

```bash
make                       # tc_glitch.o + dawn-agent
make modem-tool            # standalone diagnostic CLI (not part of `make`/`make install`)
make modem-exporter        # InfluxDB exporter service (not part of `make`/`make install`)
make twamp-tool            # standalone TWAMP diagnostic CLI (not part of `make`/`make install`)
make twamp-exporter        # InfluxDB TWAMP exporter service (not part of `make`/`make install`)
make clean
sudo make install                  # dawn-agent → /opt/dawn, registers systemd unit
sudo make install-modem-tool       # modem-tool → /opt/dawn, registers systemd unit
sudo make install-modem-exporter   # modem-exporter → /opt/dawn; needs /opt/dawn/modem-exporter.env (INFLUX_TOKEN=...) created manually, see modem-exporter.env.example
sudo make install-twamp-tool       # twamp-tool → /opt/dawn, registers systemd unit
sudo make install-twamp-exporter   # twamp-exporter → /opt/dawn; reuses the same /opt/dawn/modem-exporter.env token as modem-exporter

sudo ./dawn-agent -iface wwan0 -reflector 10.30.30.1:862   # root required to load eBPF
```

On WaveOS: no Go/clang toolchain on-device and storage is tight — cross-compile `CGO_ENABLED=0` elsewhere (static binary, no libc to match) and copy just the binary + an `openwrt/*.init` script over. See `MODEM_TOOL.md`'s WaveOS section for the full walkthrough (`twamp-tool`/`twamp-exporter` follow the identical pattern — same `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build`).

Notes:
- `go.mod`/`go.sum` are gitignored; `make deps` creates them fresh. Don't manually pin dependency versions in a committed `go.mod` — if one goes stale (e.g. a `netlink` version that no longer resolves), delete and regenerate rather than patching by hand.
- `dawn-agent` loads `tc_glitch.o` from the current working directory at runtime.
- `modem/` and `twamp/` both have real unit tests; `dawn-agent`'s eBPF side still has none (`stress_test.sh` / `tc qdisc ... netem` for manual validation).

## Architecture

Full two-endpoint component diagram (gateway + server): see `ARCHITECTURE.md`.

1. **Kernel space — `bpf_src/tc_glitch.bpf.c`** (Debian gateways only): TC egress program (`egress_monitor`). Tracks per-flow state in an LRU hash map keyed by 5-tuple; emits a `glitch_event` to the `events` ring buffer when the inter-packet gap exceeds 100ms (type 1 = stall) or a TCP sequence number regresses (type 2 = retransmission). Parses packets as **raw IP with no Ethernet header** — correct for 5G modem interfaces (wwan0), wrong for ethernet interfaces.

2. **User space — `main.go`**: loads `tc_glitch.o` via cilium/ebpf, adds a `clsact` qdisc and attaches the program as an egress BPF filter via vishvananda/netlink (`HANDLE_MINEGRESS = 0xFFF2`). Runs two loops:
   - **Passive**: reads `GlitchEvent`s from the ring buffer and prints them (🔴 loss / ⚠️ stall).
   - **Active** (`runActiveMonitoring`, every 5s): calls `modem.Detect`/`modem.GetSignal`/`modem.GetCellInfo` for radio status, and `twamp.Measure` (native Go sender, `twamp/` package — no longer shells out to `twampy`/Python) for RTT/jitter/loss.

**Struct layout coupling**: Go's `GlitchEvent` (main.go) must match the C `glitch_event` (tc_glitch.bpf.c) field-for-field — events are decoded with `binary.Read` little-endian.

## Telemetry (InfluxDB/Grafana)

**Read `metrics-endpoint.md` first** — it's the authoritative handoff doc, written for Claude Code specifically, and its target schema matches what's actually implemented (unlike the `.docx`/`dawn6g-metrics/README.md`, which describe an earlier, superseded draft — see Repository Layout).

The real server stack is **`~/dawn6g-metrics` on `wservices.wavesys.pt`** (Apache on the host, not Docker, is the sole public entry point and also serves Nagios — never take port 443 from it; InfluxDB/Grafana are Docker Compose, bound to loopback only). This repo's own `docker-compose.yml` is stale/unrelated (wrong org/bucket names, no TLS) — ignore it. Org `wavecom`; buckets `radio` (90d, modem signal/cell telemetry), `network` (90d, now live — TWAMP RTT/jitter/loss, see below; still earmarked for future QoS/nDPI/eBPF data too), `events` (365d, unused so far — future event-driven RadioEvent points).

**What `modem-exporter` writes today** (confirmed live on `aaeon`/`6global`/`dawn6g`, to `radio` only) — this matches `metrics-endpoint.md`'s target schema exactly, except `pci` (optional there, not implemented — see Known Issues):
- `radio_cell`: tags `device`/`modem_id`/`method`/`rat`/`role` (+`plmn`/`tac` on anchor rows only); fields `rsrp`/`rsrq`/`rssi`/`sinr` (NaN-omitted) + `cell_id` (anchor rows only).
- `modem_state`: tags `device`/`modem_id`; fields `operator_name`/`registration_state`/`packet_service_state`.
- Uses `WriteAPIBlocking` + log-and-drop on failure (deliberate: avoids unbounded in-RAM buffering on constrained devices if InfluxDB is unreachable). Both `metrics-endpoint.md` and the `.docx`/README recommend the retry-buffered async `WriteAPI` (batch 500, flush 1000ms, 10 retries, 100k-point RAM buffer) instead — a real, repeatedly-suggested tradeoff worth reconsidering, not just a one-off idea.
- `metrics-endpoint.md`'s "TODO 1" (migrate off the old `modem_telemetry`/`device_id` schema) is **done** as of this session. Its "TODO 2" (provision the Grafana dashboard as JSON under `dawn6g-metrics/grafana/provisioning/dashboards/`) is **still open** — a dashboard exists and works in Grafana's live DB, but isn't captured as repo-tracked JSON yet (now also true of a TWAMP panel/dashboard, not yet added — see below), so none of it survives the Grafana container's volume being lost/recreated. Datasource UID is `influxdb-flux` (Flux query language, default bucket `radio`).

**What `twamp-exporter` writes** (confirmed live on `aaeon`/`6global`/`dawn6g`, to `network`) — schema is new, not specified by `metrics-endpoint.md` (which only earmarks the bucket, not a schema):
- `twamp`: tags `device`/`reflector`; fields `rtt_ms`/`jitter_ms`/`loss_pct` (floats) + `sent`/`received` (ints), one point per probe burst (default interval 30s).
- Same `WriteAPIBlocking` + log-and-drop pattern as `modem-exporter`, same reasoning.
- **A Grafana panel for this now exists** (built live in Grafana's UI this session — not yet repo-tracked JSON, same open "TODO 2" gap as the `radio` dashboard). Query:
  ```flux
  from(bucket: "network")
    |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
    |> filter(fn: (r) => r._measurement == "twamp")
    |> filter(fn: (r) => r._field == "rtt_ms" or r._field == "jitter_ms" or r._field == "loss_pct")
    |> filter(fn: (r) => r.device =~ /^${device:regex}$/)
  ```
  Reuses the dashboard's existing `device` template variable (query: `tagValues(bucket: "network", tag: "device")`, multi-value on) — `:regex` formatting is what makes multi-select/"All" work with `=~`, since plain `==` only matches one value. Same `influxdb-flux` datasource as the `radio` panels, just pointed at the `network` bucket.

**Per-device tokens**: `./add-device.sh <id>` in `dawn6g-metrics/` on `wservices` — writes `secrets/devices/<id>.token` **and** `secrets/devices/<id>.authid` (the InfluxDB auth entry's own ID; `revoke-device.sh` needs this file and can't revoke by token value alone). **`6global` and `dawn6g`'s tokens were created manually** (not via `add-device.sh`), so neither has an `.authid` file — `revoke-device.sh` won't work for them until one is created (look up the auth ID via `influx auth list` and write it to `secrets/devices/<id>.authid` by hand, or recreate the token properly through the script).
On each gateway, the token is wired in as `INFLUX_TOKEN` — `/opt/dawn/modem-exporter.env` (mode 600) for systemd, baked into the `procd` init script for WaveOS. This diverges from `metrics-endpoint.md`'s documented on-device convention (`/etc/dawn6g/influx.token`, read directly as a file by the Go agent) — functionally fine since our services resolve the env var themselves, but worth knowing if anything else ever expects that exact path.

**Known deployed devices**: `aaeon` (Debian, ModemManager, SIMCom, real MEO registration), `6global` (WaveOS Supermicro, reachable at `10.10.10.184`, SIMCom + Quectel), `dawn6g` (WaveOS Supermicro, `10.10.10.201`, SIMCom + Quectel). Neither WaveOS board has a SIM or an antenna connected. `6global`'s Quectel has nonetheless shown real RSRP/PLMN/TAC (see the no-SIM note above) by camping on a nearby cell through incidental RF leakage on the bare antenna connector — not reproducible/meaningful until real antennas are attached; `dawn6g` hasn't picked up anything at all, which is plausible RF randomness given neither board has an antenna, not evidence of a fault.

## Known Issues

- **Startup detection race, no retry on the `ubus`/QMI cascade.** `modem.Detect()` runs once per process lifetime. If WaveOS's `uqmi-ubus` daemon (or ModemManager) isn't fully ready yet right after a reboot, detection falls through to a worse/contention-prone method (raw `qmicli`/`uqmi`) and never re-evaluates — this has caused real QMI client-ID exhaustion in production (`6global`, twice). A retry-with-backoff exists for ModemManager's `--signal-setup` call specifically (`modem/mmcli.go`, `runMmcliSetup`) but **not** for the overall cascade. If telemetry goes quiet after a reboot, check whether the process is stuck on `qmicli`/`uqmi` (log lines say `via: QMI`/`via: uqmi` instead of `via: ubus`/`via: ModemManager`) and just restart the service — a fresh detection pass usually fixes it immediately.
- **`uqmi-ubus.lua` discards PCI/EARFCN/bandwidth.** It computes them via `AT+QENG` (Quectel) but explicitly nils them before exposing anything over `ubus` (`/lib/wavesys/lib/lua/uqmi/uqmi-ubus.lua`). Not fixable from this repo — would need a patch to that vendor script.
- **`ubus` backend doesn't normalize TAC/CellID to decimal** (see above) — cosmetic inconsistency vs. other backends, not yet fixed.

## Deployment Context

- `dawn-agent.service`: systemd unit; runs `init_gateway.sh` first, then the agent from `/opt/dawn`.
- `init_gateway.sh`: loads kernel modules, configures the modem's dedicated QoS bearer via AT commands (DSCP EF / cid 2), and sets up HTB traffic shaping with a priority lane for EF-marked traffic.
- `modem-tool.service`/`modem-exporter.service`/`twamp-tool.service`/`twamp-exporter.service`: systemd units for the Debian gateway; `openwrt/*.init` for WaveOS (`procd`). All four are deployed and running on `aaeon`/`6global`/`dawn6g` as of this session.

## License Constraint

GPL-2.0 — required for the eBPF program (`__license` section is "GPL"; kernel helpers demand it). Only applies to the `dawn-agent`/`bpf_src` side; `modem/`, `modem-tool`, `modem-exporter`, `twamp/`, `twamp-tool`, `twamp-exporter` have no such constraint.
