# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

DAWN6G glitch detector: started as a single eBPF-powered 5G gateway agent (`dawn-agent`), now split into three shared packages (`modem/`, `twamp/`, `gps/`) plus consumers — `dawn-agent` (eBPF + TWAMP + modem metrics, Debian/ModemManager gateways only), `cmd/modem-tool`/`cmd/twamp-tool`/`cmd/gps-tool` (standalone diagnostic CLIs), and `cmd/modem-exporter`/`cmd/twamp-exporter`/`cmd/gps-exporter` (long-running InfluxDB/Grafana telemetry push). **Builds and runs only on Linux** (eBPF, netlink, mmcli/qmicli/uqmi/ubus), not on macOS.

Two gateway OS targets exist in the fleet:
- **Debian** (e.g. host `aaeon`) — SIMCom modem via ModemManager, runs the full `dawn-agent` (eBPF works here).
- **WaveOS** (OpenWrt-based, x86_64 Supermicro boards, e.g. hosts `6global`/`dawn6g`) — **confirmed to lack `clsact`/TC-BPF kernel support** (kernel 5.15.167, `netlink.QdiscAdd` fails with ENOENT; `modprobe cls_bpf`/`sch_ingress` also fail). `dawn-agent`'s eBPF path cannot run there — only `modem-tool`/`modem-exporter`/`twamp-tool`/`twamp-exporter` are deployed on WaveOS boards. (`gps-tool`/`gps-exporter` are **not** deployed there — WaveOS has no ModemManager at all, and the only other GPS backend, raw QMI, is currently disabled — see the `gps/` package section's incident writeup.)

## Repository Layout

- `main.go` → `dawn-agent` binary — eBPF passive detection + TWAMP + modem status line (Debian gateways only).
- `modem/` — shared package: hybrid modem detection + signal/cell-info reading. Has unit tests (`go test ./modem/...`), several built from real captured `mmcli`/`qmicli`/`uqmi`/`ubus` output.
- `cmd/modem-tool/` → `modem-tool` binary — detect modems, then poll and print signal+cell info forever. No InfluxDB, no eBPF. See `MODEM_TOOL.md`.
- `cmd/modem-exporter/` → `modem-exporter` binary — detect modems, then push signal+cell info to InfluxDB on an interval. Meant to run as a service, not interactively.
- `twamp/` — shared package: native Go TWAMP-Light sender (no external `twampy`/Python dependency — required for WaveOS, which has no Python at all). Has unit tests (`go test ./twamp/...`).
- `cmd/twamp-tool/` → `twamp-tool` binary — probe a TWAMP-Test reflector on an interval and print RTT/jitter/loss forever. No InfluxDB. Mirrors `modem-tool`'s structure.
- `cmd/twamp-exporter/` → `twamp-exporter` binary — probe a TWAMP-Test reflector, then push RTT/jitter/loss to InfluxDB's `network` bucket on an interval. Mirrors `modem-exporter`'s structure; reuses the same per-device token file.
- `gps/` — shared package: GPS/GNSS fix reading via ModemManager's `gps-raw` location source or raw QMI's LOC service. Deliberately its own package, not folded into `modem/` — see the `gps/` package section below for why. Has unit tests (`go test ./gps/...`).
- `cmd/gps-tool/` → `gps-tool` binary — probe for a GPS fix on an interval and print it forever. No InfluxDB. Mirrors `modem-tool`/`twamp-tool`'s structure.
- `cmd/gps-exporter/` → `gps-exporter` binary — probe for a GPS fix, then push it to InfluxDB's `location` bucket on an interval (only when a fix is actually valid). Mirrors `modem-exporter`/`twamp-exporter`'s structure; reuses the same per-device token file. Only usable where `gps.Supports` is true (ModemManager only right now — see `gps/` package section).
- `openwrt/*.init` — `procd` init scripts for WaveOS (`modem-tool.init`, `modem-exporter.init`, `twamp-tool.init`, `twamp-exporter.init`, `gps-tool.init`, `gps-exporter.init`; the exporter ones need their `DEVICE_ID`/token edited per board before install — though `gps-*` aren't actually deployed on WaveOS today, see above).
- `*.service` — `systemd` units for Debian gateways (`dawn-agent.service`, `modem-tool.service`, `modem-exporter.service`, `twamp-tool.service`, `twamp-exporter.service`, `gps-tool.service`, `gps-exporter.service`).
- `metrics-endpoint.md` — **the authoritative handoff doc for the InfluxDB/Grafana telemetry side**, written for Claude Code specifically. Read this before touching `modem-exporter`'s schema or the server stack.
- `DEPLOYMENT.md` — end-to-end runbook for bringing a new gateway board onto the fleet: provisioning its InfluxDB token (`add-device.sh`), installing the right binaries for Debian (systemd) vs WaveOS (procd, cross-compiled), verifying data lands in InfluxDB, and why Grafana needs no manual "add device" step (its `device` variable is query-driven).
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

## `gps/` package

Reads GPS/GNSS fixes from a modem's own GNSS receiver. Deliberately a standalone package (own process, `gps-tool`/`gps-exporter`) — not wired into `dawn-agent`'s active-monitoring loop or folded into `modem/`'s stateless per-call backend shape, because the QMI backend genuinely needs a different shape (see below).

`Supports(method)` — **only ModemManager is currently enabled**:
- **ModemManager**: `gps-raw` location source reports already-parsed decimal `modem.location.gps.{latitude,longitude,altitude}` directly — no NMEA parsing needed. Enabled via `--location-enable-gps-raw` (root/PolicyKit-gated, retried the same way `modem/mmcli.go`'s `runMmcliSetup` does). **Not yet validated live** — `aaeon`'s modem has confirmed capability (`gps-raw`/`gps-nmea`/`gps-unmanaged`/`agps-msa`/`agps-msb`, plus a dedicated `ttyUSB1 (gps)` port) but hasn't actually been enabled/tested with a real fix yet.
- **Raw QMI (`qmicli`) is implemented but deliberately disabled** (`Supports`/`NewPoller` both refuse it — see ⚠️ below). The code (`QMISession`) is real and was confirmed working against a SIMCom modem (real NMEA sentences streaming, no fix — no antenna), but is gated off after repeatedly corrupting production QMI client state — see the incident writeup below before ever re-enabling it.
- `ubus`/`uqmi`/AT are **not implemented**: `ubus`'s `"location"` method only returns cell-ID/LAC-CI data, never real GPS coordinates, despite the name (would need a patch to the vendor's `uqmi-ubus.lua`); `uqmi` lists `pds` as a service name but exposes no actual PDS/location CLI action; AT hasn't been tested live.

**`gps.Detect`/`gps-tool`/`gps-exporter` do NOT reuse `modem.Detect()`'s cascade result** — that cascade prioritizes `ubus` over raw QMI specifically to avoid QMI client contention, but `ubus` has no GPS support at all, so naively using its pick means they'd find nothing on any WaveOS board. Instead `gps.Detect` tries `modem.DetectModemManager` first, then independently falls back to `modem.DetectQMIMBIM` — GPS detection is its own concern, decoupled from the main cascade's `ubus`-priority tradeoff. (With QMI currently disabled via `Supports`, that fallback only matters again once QMI is re-enabled.)

**⚠️ Raw QMI backend disabled after two separate production incidents this session — read before touching `Supports`/`NewQMISession`:**
1. **First incident**: manually killing `qmicli --loc-follow-nmea` with `timeout` (SIGTERM, but `qmicli` doesn't release its QMI client cleanly on it) against `6global`'s Quectel (`/dev/cdc-wdm1`) left its QMI control endpoint hung (`CID allocation failed... endpoint hangup`) for *every* process on that device node, not just the one killed — this also broke the production `uqmi-ubus` daemon's own access to that modem, taking down `modem-exporter`'s telemetry for it. Restarting `qmi-proxy`/`uqmi-ubus` did **not** recover it; only a board reboot did.
2. **`QMISession.Close()` was fixed** to call `--loc-stop` first (letting `--loc-follow-nmea` exit on its own) and only force-kill as a timeout fallback, specifically to prevent a repeat.
3. **Second incident, despite that fix**: a single, clean `gps-exporter` test run against `6global` (SIMCom, `/dev/cdc-wdm0`, no abrupt kill involved — session ended via the new graceful `Close()` path) still left that device's raw QMI client state corrupted afterward (`CID allocation failed... Service mismatch`), confirmed moments later and still broken after a short wait (not a transient race). Production (`ubus`-based `modem-exporter`) was **not** affected this second time, but direct `qmicli` access to that device was.

**Root cause not understood.** Best working theory: some fragility in `qmi-proxy` (the shared QMI client arbitration daemon both `uqmi-ubus` and raw `qmicli` route through) under repeated LOC session churn — possibly aggravated by the unusually high volume of connect/disconnect cycles this one development session put through it — but this is unverified. **There is no non-production QMI hardware available to investigate further without risking a live board again.** Decision: leave the QMI backend implemented (so the groundwork isn't lost) but disabled via `Supports`/`NewPoller`, ship GPS as ModemManager-only, and revisit if/when dedicated test hardware becomes available or the `qmi-proxy` behavior is better understood. **Do not re-enable `Supports(modem.MethodQMI)` or call `NewQMISession` against `6global`/`dawn6g` without a real plan for this risk.**

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
make gps-tool                      # standalone GPS diagnostic CLI (not part of `make`/`make install`)
sudo make install-gps-tool         # gps-tool → /opt/dawn, registers systemd unit
make gps-exporter                  # InfluxDB GPS exporter service (not part of `make`/`make install`)
sudo make install-gps-exporter     # gps-exporter → /opt/dawn; reuses the same /opt/dawn/modem-exporter.env token

sudo ./dawn-agent -iface wwan0 -reflector 10.30.30.1:862   # root required to load eBPF
```

On WaveOS: no Go/clang toolchain on-device and storage is tight — cross-compile `CGO_ENABLED=0` elsewhere (static binary, no libc to match) and copy just the binary + an `openwrt/*.init` script over. See `MODEM_TOOL.md`'s WaveOS section for the full walkthrough (`twamp-tool`/`twamp-exporter` follow the identical pattern — same `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build`).

Notes:
- `go.mod`/`go.sum` are gitignored; `make deps` creates them fresh. Don't manually pin dependency versions in a committed `go.mod` — if one goes stale (e.g. a `netlink` version that no longer resolves), delete and regenerate rather than patching by hand.
- `dawn-agent` loads `tc_glitch.o` from the current working directory at runtime.
- `modem/`, `twamp/`, and `gps/` all have real unit tests; `dawn-agent`'s eBPF side still has none (`stress_test.sh` / `tc qdisc ... netem` for manual validation).

## Architecture

Full two-endpoint component diagram (gateway + server): see `ARCHITECTURE.md`.

1. **Kernel space — `bpf_src/tc_glitch.bpf.c`** (Debian gateways only): TC egress program (`egress_monitor`). Tracks per-flow state in an LRU hash map keyed by 5-tuple; emits a `glitch_event` to the `events` ring buffer when the inter-packet gap exceeds 100ms (type 1 = stall) or a TCP sequence number regresses (type 2 = retransmission). Parses packets as **raw IP with no Ethernet header** — correct for 5G modem interfaces (wwan0), wrong for ethernet interfaces.

2. **User space — `main.go`**: loads `tc_glitch.o` via cilium/ebpf, adds a `clsact` qdisc and attaches the program as an egress BPF filter via vishvananda/netlink (`HANDLE_MINEGRESS = 0xFFF2`). Runs two loops:
   - **Passive**: reads `GlitchEvent`s from the ring buffer and prints them (🔴 loss / ⚠️ stall).
   - **Active** (`runActiveMonitoring`, every 5s): calls `modem.Detect`/`modem.GetSignal`/`modem.GetCellInfo` for radio status, and `twamp.Measure` (native Go sender, `twamp/` package — no longer shells out to `twampy`/Python) for RTT/jitter/loss.

**Struct layout coupling**: Go's `GlitchEvent` (main.go) must match the C `glitch_event` (tc_glitch.bpf.c) field-for-field — events are decoded with `binary.Read` little-endian.

## Telemetry (InfluxDB/Grafana)

**Read `metrics-endpoint.md` first** — it's the authoritative handoff doc, written for Claude Code specifically, and its target schema matches what's actually implemented (unlike the `.docx`/`dawn6g-metrics/README.md`, which describe an earlier, superseded draft — see Repository Layout).

The real server stack is **`~/dawn6g-metrics` on `wservices.wavesys.pt`** (Apache on the host, not Docker, is the sole public entry point and also serves Nagios — never take port 443 from it; InfluxDB/Grafana are Docker Compose, bound to loopback only). This repo's own `docker-compose.yml` is stale/unrelated (wrong org/bucket names, no TLS) — ignore it. Org `wavecom`; buckets `radio` (90d, modem signal/cell telemetry), `network` (90d, now live — TWAMP RTT/jitter/loss, see below; still earmarked for future QoS/nDPI/eBPF data too), `events` (365d, unused so far — future event-driven RadioEvent points), **`location`** (365d, added this session — device GPS/GNSS position, see below; deliberately its own bucket rather than folded into `radio`, since GPS isn't a radio metric despite coming from the same modem hardware).

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

**What `gps-exporter` writes** (built this session, **not yet validated live** — `gps.Supports` is ModemManager-only right now and `aaeon`'s GPS hasn't been enabled/tested yet, see `gps/` package section): measurement `gps_fix` in the `location` bucket. Tags `device`/`modem_id`/`method`; fields `latitude`/`longitude`/`altitude` (floats). **Only written when `Fix.Valid == true`** — skip the point entirely otherwise, same "return nil, don't write" pattern `radioCellPoint`/`modemStatePoint` already use, but for a different reason: `(0, 0)` is a real coordinate (Null Island), so unlike signal metrics there's no safe NaN-style sentinel for "no fix yet." Deliberately a **separate bucket**, not folded into `radio` — plan is to **join at query time** (Flux `join()` on the `device` tag against the latest `gps_fix` point) for map panels combining radio/network metrics with position, rather than denormalizing lat/lon onto every `radio_cell`/`twamp` point.

**Per-device tokens**: `./add-device.sh <id>` in `dawn6g-metrics/` on `wservices` — writes `secrets/devices/<id>.token` **and** `secrets/devices/<id>.authid` (the InfluxDB auth entry's own ID; `revoke-device.sh` needs this file and can't revoke by token value alone). `aaeon`/`6global`/`dawn6g` all have proper `.authid` files as of this session (`6global`/`dawn6g`'s earlier manually-created tokens, which lacked one, were rotated out — see below). **No in-place way to add a permission to an existing token** — this InfluxDB CLI's `auth` command only supports `create`/`delete`/`active`/`inactive`, no `update`; expanding a token's bucket scope means issuing a new one and rotating it in (create new → deploy → verify → revoke old — never revoke-then-recreate, to avoid a write-gap window).
On each gateway, the token is wired in as `INFLUX_TOKEN` — `/opt/dawn/modem-exporter.env` (mode 600) for systemd, baked into the `procd` init script for WaveOS. This diverges from `metrics-endpoint.md`'s documented on-device convention (`/etc/dawn6g/influx.token`, read directly as a file by the Go agent) — functionally fine since our services resolve the env var themselves, but worth knowing if anything else ever expects that exact path.
**All three device tokens, plus Grafana's read-only token, were rotated this session** to add `location`-bucket access (`add-device.sh`/`bootstrap.sh` now provision/scope all four buckets for new tokens automatically). One gotcha hit during the Grafana token rotation: a Docker Compose file-based secret is mounted into the container with the **exact host file permissions/ownership**, not normalized — `chmod 600`-ing the secret file (seemed like better hygiene) broke Grafana's ability to read it (its container runs as a different UID than the host file owner), surfacing as `provisioning: open /run/secrets/...: permission denied`. Match the original file's mode (`644`) when touching these secret files, don't tighten them.
**Known cleanup opportunity, not yet done**: a stale, pre-`add-device.sh` token literally named `dawn6g` (not `device:dawn6g`), write-scoped to `radio` only, is still sitting unrevoked in InfluxDB (`influx auth list` will show it) — it predates this repo's current token conventions and isn't used by anything live, but hasn't been cleaned up.

**Known deployed devices**: `aaeon` (Debian, ModemManager, SIMCom, real MEO registration), `6global` (WaveOS Supermicro, reachable at `10.10.10.184`, SIMCom + Quectel), `dawn6g` (WaveOS Supermicro, `10.10.10.201`, SIMCom + Quectel). Neither WaveOS board has an antenna connected. `6global` has no SIM; its Quectel has nonetheless shown real RSRP/PLMN/TAC (see the no-SIM note above) by camping on a nearby cell through incidental RF leakage on the bare antenna connector — not reproducible/meaningful until a real antenna is attached. **`dawn6g` got a real SIM installed this session (2026-11-10)** — its Quectel (`lte1`) registered immediately with operator NOS over `ubus`, confirming the SIM/registration path works end-to-end; still no antenna attached.

## Known Issues

- **Startup detection race, no retry on the `ubus`/QMI cascade.** `modem.Detect()` runs once per process lifetime. If WaveOS's `uqmi-ubus` daemon (or ModemManager) isn't fully ready yet right after a reboot, detection falls through to a worse/contention-prone method (raw `qmicli`/`uqmi`) and never re-evaluates — this has caused real QMI client-ID exhaustion in production (`6global`, twice). A retry-with-backoff exists for ModemManager's `--signal-setup` call specifically (`modem/mmcli.go`, `runMmcliSetup`) but **not** for the overall cascade. If telemetry goes quiet after a reboot, check whether the process is stuck on `qmicli`/`uqmi` (log lines say `via: QMI`/`via: uqmi` instead of `via: ubus`/`via: ModemManager`) and just restart the service — a fresh detection pass usually fixes it immediately.
- **`uqmi-ubus.lua` discards PCI/EARFCN/bandwidth.** It computes them via `AT+QENG` (Quectel) but explicitly nils them before exposing anything over `ubus` (`/lib/wavesys/lib/lua/uqmi/uqmi-ubus.lua`). Not fixable from this repo — would need a patch to that vendor script.
- **`ubus` backend doesn't normalize TAC/CellID to decimal** (see above) — cosmetic inconsistency vs. other backends, not yet fixed.
- **Raw QMI sessions (e.g. `qmicli`'s LOC service) can corrupt a device's QMI client state even with a graceful stop, for reasons not yet understood** — caused two separate production incidents on `6global` this session (one from an abrupt `timeout`/SIGKILL test, one from a single clean session with no kill involved at all). This is why `gps/`'s raw-QMI backend is currently disabled — see that package's section for the full writeup before considering raw QMI session work on `6global`/`dawn6g` again.

## Deployment Context

- `dawn-agent.service`: systemd unit; runs `init_gateway.sh` first, then the agent from `/opt/dawn`.
- `init_gateway.sh`: loads kernel modules, configures the modem's dedicated QoS bearer via AT commands (DSCP EF / cid 2), and sets up HTB traffic shaping with a priority lane for EF-marked traffic.
- `modem-tool.service`/`modem-exporter.service`/`twamp-tool.service`/`twamp-exporter.service`: systemd units for the Debian gateway; `openwrt/*.init` for WaveOS (`procd`). All four are deployed and running on `aaeon`/`6global`/`dawn6g` as of this session.
- `gps-tool.service`/`gps-exporter.service`/`openwrt/gps-tool.init`/`openwrt/gps-exporter.init`: built and unit-tested this session, **not yet deployed as a running service anywhere** — only ad hoc live testing via manually-copied binaries so far (see `gps/` package section). Don't install the `openwrt/gps-*.init` scripts on WaveOS boards until GPS actually works there (it doesn't today — see Known Issues/the `gps/` package section).

## License Constraint

GPL-2.0 — required for the eBPF program (`__license` section is "GPL"; kernel helpers demand it). Only applies to the `dawn-agent`/`bpf_src` side; `modem/`, `modem-tool`, `modem-exporter`, `twamp/`, `twamp-tool`, `twamp-exporter`, `gps/`, `gps-tool`, `gps-exporter` have no such constraint.
