# DAWN6G metrics endpoint — context for Claude Code

Handoff from a claude.ai session (2026-10-23). Read this before touching the gateway agent's
telemetry writer, the Grafana dashboards, or the `dawn6g-metrics` server stack.

## Deployed infrastructure (working)

- Host `wservices.wavesys.pt` (Ubuntu, user `idwave`), stack in `~/dawn6g-metrics`.
- **Apache** (host, not Docker) is the only public entry point and also serves **Nagios** — never
  disable it or take port 443 from it. TLS = Let's Encrypt via certbot (`certonly`, renew hook reloads
  Apache; renewal needs inbound TCP 80).
- **InfluxDB 2.7** + **Grafana 12** in Docker Compose, bound to loopback only:
  - Grafana `https://wservices.wavesys.pt/grafana/` → Apache :443 → `127.0.0.1:3000` (sub-path, WebSocket)
  - InfluxDB `https://wservices.wavesys.pt:8086` → Apache vhost :8086 → `127.0.0.1:18086`
- Apache files: `/etc/apache2/dawn6g/grafana.inc` (Included in `default-ssl.conf` 443 vhost),
  `/etc/apache2/sites-available/dawn6g-influxdb.conf`.
- No Traefik (dropped because Apache owns 443).

## InfluxDB layout

- Org `wavecom`. Buckets: `radio` (90d, periodic KPIs), `network` (90d, TWAMP/QoS/nDPI/eBPF),
  `events` (365d, RadioEvent: PLMN/PCI/TAC change, EN-DC add/drop, RRC re-est.), `location` (365d,
  device GPS/GNSS position — see `gps_fix` below; its own bucket since GPS isn't a radio metric
  despite coming from the same modem hardware).
- Tokens: admin (server only), Grafana read-only on all four buckets, **one write-only token per device**
  (`./add-device.sh <id>`, `./revoke-device.sh <id>`; stored in `secrets/devices/<id>.token`).
  On devices the token lives at `/etc/dawn6g/influx.token` (0600). Never commit secrets.
- Scripts: `gen-secrets.sh`, `bootstrap.sh` (idempotent), `add-device.sh`, `revoke-device.sh`, `lib.sh`.

## Target schema (what every device should write)

```
radio_cell,device=<id>,modem_id=<n>,method=<ModemManager|QMI|AT>,rat=<lte|nr>,role=<anchor|scg>[,plmn=<mccmnc>,tac=<tac>] rsrp=<f>,rsrq=<f>,rssi=<f>,sinr=<f>,cell_id="<str>"[,pci=<i>]
modem_state,device=<id>,modem_id=<n> operator_name="<s>",registration_state="<s>",packet_service_state="<s>"[,stale_signal_fields=<i>]
agent_info,device=<id>,component=<modem-exporter|twamp-exporter|gps-exporter|glitch-exporter> version="<str>"
twamp,device=<id>,reflector=<host:port>[,iface=<dev>] rtt_ms=<f>,jitter_ms=<f>,loss_pct=<f>,sent=<i>,received=<i>
```

- Tags = bounded-cardinality grouping keys; fields = measurements. `cell_id` stays a field.
- One point per carrier: under EN-DC the LTE anchor and NR SCG are separate points (never mixed).
- `plmn`/`tac` only on `role=anchor` rows.
- `twamp` (`network` bucket): `iface` only when `twamp-exporter` runs with `-bind-dev` (since
  `v2.3.4`): the probes were pinned to that interface (SO_BINDTODEVICE). On the WaveOS boards
  only `iface=lte1` with `reflector=51.91.57.3:862` measures the cellular path; points to
  `10.30.30.1:862` without `iface` went through WireGuard over the wired link.
- `modem_state.stale_signal_fields` (int, omitted when 0): how many of that modem's `radio_cell` rsrp/rsrq/rssi/sinr fields were withheld this tick because they'd held an identical value too long (30 minutes change-detection, or — for the `ubus` method specifically — its own source timestamp not advancing in 3x the poll interval) — see the `modem-detection` skill. A nonzero run of these over time means `radio_cell`'s numeric fields for that modem are stale, not necessarily missing.
- `agent_info`, one point at startup and again every hour, from every exporter (not `modem_state`/`radio_cell`-specific) — answers "what version is this device actually running" per component, straight from InfluxDB, without needing to SSH in and run each binary with `-version`. Each exporter writes it via its own existing client, so it lands in that exporter's own bucket, not one shared location — `modem-exporter`'s in `radio`, `twamp-exporter`'s and `glitch-exporter`'s in `network`, `gps-exporter`'s in `location`. A dashboard covering all four components needs to query across all three buckets (or `union()` in Flux), not just one.
- Go client `github.com/influxdata/influxdb-client-go/v2`, one `WriteAPI` per bucket, batch 500,
  flush 1000 ms, max retries 10, retry buffer 100000 points (RAM only), precision ms.

## `network` bucket: `glitch_summary` / `glitch_event` (stall/retransmission monitor)

Written by `cmd/glitch-exporter` (the `glitch/` package's InfluxDB exporter —
see `CLAUDE.md`'s `glitch/` package section for the detection logic this
schema reports on). Confirmed live on `aaeon`: a forced-outage test's points
matched the kernel's own reported duration/step count exactly, and 10+
minutes of background-only traffic kept ticking `glitch_summary` with
`stall_episodes=0` throughout.

**`-capture` (`ebpf`, default, or `afpacket`) selects which capture backend
produces the events both measurements below report on — added this session
as part of unifying the detection algorithm (`glitch/core`, ported from
`bpf_src/tc_glitch.bpf.c`) behind either capture method, for boards with no
clsact/TC-BPF support at all (confirmed on WaveOS). Both measurements carry
an additive `capture` tag recording which one produced each point. Results
from the two methods are not directly comparable: AF_PACKET observes egress
traffic after the qdisc and after GSO segmentation; eBPF observes it before
— a side-by-side validation of the two is tracked as separate follow-up
work, not done yet.**

**`glitch_summary`** — one point per `-interval` (default 10s), **always
written even when every field is zero** (a heartbeat — unlike `radio_cell`/
`twamp`, which skip a point when there's nothing to report).

| Field | Type | Unit | Meaning |
|---|---|---|---|
| `stall_episodes` | int | count | `STALL_END`s seen this interval. |
| `stall_max_ms` | float | ms | Largest single episode duration this interval (0 if none). |
| `stall_sum_ms` | float | ms | Sum of episode durations this interval (0 if none). |
| `stall_open` | int | count | Episodes started but not yet ended **as of this point** — a live snapshot of in-progress stalls, not accumulated over the interval like the fields above. |
| `retrans_count` | int | count | Retransmissions seen this interval. |
| `dropped_events` | uint | count | Delta of `glitch.Handle.Dropped()` since the last tick — kernel ring-buffer events the Go side's internal channel couldn't keep up with. Should stay 0 under normal load. |
| `kernel_dropped_events` | uint | count | `afpacket.Capture.KernelDropped()` delta — the kernel's own `PACKET_STATISTICS` couldn't queue a packet into the ring at all. Always 0 for `-capture ebpf` (no equivalent exists there). |
| `user_dropped_events` | uint | count | `afpacket.Capture.UserDropped()` delta — this process's own `Events()` channel wasn't drained fast enough. Always 0 for `-capture ebpf`. |
| `untruncated_over_limit_events` | uint | count | `afpacket.Capture.UntruncatedOverLimit()` delta — ring packets the classic BPF filter's truncation didn't actually apply to. Expected to stay 0; a correctness check on the capture filter, not a capacity one. Always 0 for `-capture ebpf`. |
| `write_queue_dropped_events` | uint | count | Points dropped by the bounded queue in front of the InfluxDB write client, when it was full — i.e. this device's own write path couldn't keep up (e.g. its uplink was down at the same time as a stall burst). Should stay 0; a sustained nonzero run here means points are being lost outright, not just delayed. |

Tags: `device`, `iface`, `capture` (`ebpf`/`afpacket` — additive, see above). **Never an IP address or port** — see `glitch_event` below for where those actually live.

**⚠️ `glitch_summary` mixes signed (`int`) and unsigned (`uint`) integer fields, and always has — not something introduced by adding the four drop-breakdown fields above.** `dropped_events` has been `uint64` in Go (InfluxDB line-protocol `u` suffix) since this measurement's very first write; `stall_episodes`/`stall_open`/`retrans_count` have always been plain `int` (signed, no suffix). **Never change either side's type to match the other** — InfluxDB enforces a field's type per measurement for its retention period once established, so retyping a field that's already been written (e.g. `dropped_events` → signed, to "simplify" things) doesn't migrate existing data, it makes every future write of that field get rejected in any shard where the old type was written first. This is exactly why the four new drop-breakdown fields were kept `uint64` too, matching `dropped_events` (whose breakdown they are), not the signed fields. **A Flux query that tries to `sum()`/`group()` across both signed and unsigned fields in one ungrouped stream fails with `schema collision: cannot group unsigned and integer types`** — confirmed live. Fix the query, not the schema: either filter to one type family at a time, or `pivot(rowKey: ["_time"], columnKey: ["_field"], valueColumn: "_value")` first (reshapes into one row per timestamp with a column per field, sidestepping the type-grouping issue entirely), or cast explicitly with `int()`/`uint()` before grouping.

**`glitch_event`** — one point per `stall_start`/`stall_end`/`stall_abandoned`.
**No point for a retransmission** — those are counted only, via
`glitch_summary.retrans_count`. Timestamped with the kernel event's own time
(or, for `stall_abandoned`, the moment the 30s no-`STALL_END` timeout was
detected — there is no kernel event to read a time from for that case).
Rate-limited to `-max-events-per-sec` (default 20) points/second, shared
across all three types; anything the limit drops still counts in
`glitch_summary` above.

| Field | Type | Unit | Meaning |
|---|---|---|---|
| `proto` | int | — | IP protocol number (6=TCP, 17=UDP). |
| `src_port` | int | — | Local port. |
| `dst_port` | int | — | Remote port. |
| `dst_ip` | string | — | Remote IP address. **A field, not a tag** (see below). |
| `baseline_ms` | float | ms | The adaptive stall baseline this flow was judged against when the episode started (`stall_start`) or, for `stall_end`/`stall_abandoned`, the one in effect when it closed/timed out. |
| `duration_ms` | float | ms | **`stall_end`/`stall_abandoned` only.** Total episode duration — from the last normal-paced packet before the episode to whatever closed it. Can run roughly 2× the underlying network outage, since it includes TCP retransmission-timer backoff time, not just the outage itself. |
| `steps` | int | count | **`stall_end`/`stall_abandoned` only.** Number of qualifying gaps folded into the episode (matches the visible retry/backoff count). Always 0 for `stall_abandoned` — the kernel only ever reports a step count on `STALL_END`, which by definition never arrived, so the real count is genuinely unknown, not a plausible-looking default. |

Tags: `device`, `iface`, `type` (`"stall_start"` / `"stall_end"` /
`"stall_abandoned"`), `capture` (`ebpf`/`afpacket` — additive, see above).

**Deliberately never a tag on an IP address or port anywhere in this
schema** — unlike `radio_cell`'s `device`/`modem_id` or `twamp`'s
`reflector`, a 5-tuple is high-cardinality and per-flow, not a stable
grouping key across a device's lifetime.

**Known limitations** (full explanation in `CLAUDE.md`'s `glitch/` package
section — these are inherent to the design, not bugs):
- Stalls are detected when the silence ends, not while it lasts — there is
  no mid-outage "still stalling" signal, only the eventual `stall_end` (or,
  after 30s, `stall_abandoned`).
- An outage shorter than about one RTT may leave no egress gap at all, if
  the sender's own congestion window keeps it transmitting throughout —
  that case shows up only as a retransmission burst, with no stall episode.
- `duration_ms` includes RTO backoff time, so it isn't a 1:1 measurement of
  the underlying outage length.
- UDP stall detection is opt-in per port, fixed at compile time
  (`UDP_MONITOR_PORTS` in `bpf_src/tc_glitch.bpf.c`), empty by default.
- On `aaeon`, `glitch-exporter`'s own InfluxDB write traffic goes out over
  the same `wwan0` interface it monitors, so it's counted in its own stats.

Panels: `glitch_summary`/`glitch_event` are on both provisioned dashboards
(`dawn6g-metrics/grafana/dashboards/`, see "Grafana dashboards" below).

## `location` bucket: `gps_fix`

Written by `cmd/gps-exporter` (the `gps/` package's InfluxDB exporter — see
`CLAUDE.md`'s `gps/` package section for the detection/backend logic this
schema reports on). Running on `dawn6g`/`6global` via the AT-over-serial
backend (`aaeon` wrote NMEA-sourced points until it became build-only on
2026-11-25).

**`gps_fix`** — one point per poll interval (default 30s), **only written
when the fix is actually valid** — unlike `radio_cell`/`twamp`, there is no
safe "skip/NaN" sentinel for "no fix yet," since `(0, 0)` is a real
coordinate (Null Island); the point is skipped entirely rather than written
with meaningless zeros.

| Field | Type | Unit | Meaning |
|---|---|---|---|
| `latitude` | float | degrees | Signed decimal degrees. |
| `longitude` | float | degrees | Signed decimal degrees. |
| `altitude` | float | meters | — |
| `speed_knots` | float | knots | Additive, NMEA-reader-only (see below) — NaN-omitted. |
| `course_deg` | float | degrees | Additive, NMEA-reader-only — NaN-omitted. |
| `hdop` | float | — | Additive, NMEA-reader-only — NaN-omitted. A GGA HDOP of `500` (the receiver's "unknown") is omitted since `v2.3.3`; **data caveat:** `aaeon`'s first NMEA point (2026-11-24, `v2.3.2`) has `hdop=500` — ignore `hdop >= 500` in queries over older data. |
| `satellites` | int | count | Additive, NMEA-reader-only. |

Tags: `device`, `modem_id`, `method` (`"ModemManager"` or `"AT"` — raw QMI
is implemented but deliberately disabled, see `CLAUDE.md`), and `source`
(`"nmea"`, additive — only present on an NMEA-reader-sourced point).

**Additive NMEA reader** (`gps/nmea.go`, `-nmea-port`): `AT+QGPSLOC=2` can
intermittently report "no fix" even with a continuous lock (confirmed live
on `dawn6g`'s Quectel), so a background reader on the modem's dedicated NMEA
port (interface 01) can be used instead for reading fixes — AT still owns
engine start/stop and assistance data regardless. Opt-in only (empty
`-nmea-port` = unchanged AT-only behavior, still the default everywhere as
of this writing). **Live status, not yet resolved**: confirmed the port
resolves correctly and the modem's own `AT+QGPSCFG` reports NMEA output
enabled and routed there, but no NMEA data was actually observed arriving
during a live check on `dawn6g` — see `CLAUDE.md`'s `gps/` package section.
Not deployed to any running `gps-exporter` yet.

**Real fixes confirmed landing from `dawn6g`** (its Quectel, via the plain
AT path) and from manual testing on `aaeon`'s SIMCom (`gps-exporter` there
now also running, via AT, on its reserved port — see `CLAUDE.md`'s Phase
C/D); `6global` has no GPS antenna on either modem, so it stays at "engine
enabled, polling, no fix."

Deliberately its own bucket, not folded into `radio` — the plan is to
**join at query time** (Flux `join()` on the `device` tag against the
latest `gps_fix` point) for map panels combining radio/network metrics with
position, rather than denormalizing lat/lon onto every `radio_cell`/`twamp`/
`glitch_summary` point.

No Grafana panel for this yet either.

## Current state / open work

- `gw01`: only a manual curl test point (`radio_cell`, tag `device`).
- `aaeon` (amd64, ModemManager, MEO 26806, LTE) is writing every 30 s with the **old schema**:
  measurement `modem_telemetry`, tag `device_id`, tags `method`, `modem_id`;
  `technology`, `plmn`, `tac`, `cell_id`, `operator_name`, `registration_state`,
  `packet_service_state` as string **fields**; `rsrp/rsrq/rssi/sinr` floats.
  It is invisible to the dashboard, which uses `radio_cell` + `device`.
- **TODO 1:** migrate the Go agent writer to the target schema above
  (`device_id`→`device`, `technology` field→`rat` tag, add `role`, `plmn`/`tac`→tags,
  status strings→`modem_state`). Old `modem_telemetry` data just ages out.
- **TODO 2 — done (2026-11-26):** dashboards are provisioned from repo JSON — see "Grafana
  dashboards" below. Still open from the original idea: annotations from `events`.

## Grafana dashboards (provisioned)

- `dawn6g-metrics/grafana/dashboards/*.json`, bind-mounted read-only at `/etc/grafana/dashboards`
  and loaded by `grafana/provisioning/dashboards/dawn6g.yaml` into the **DAWN6G** folder
  (re-read every 30 s; `allowUiUpdates: false`, `disableDeletion: true`). All queries use the
  `influxdb-flux` datasource UID.
- **`gateway-health.json`** (`uid: dawn6g-gateway-health`): `glitch_summary` drop counters
  (should be 0), `modem_state.stale_signal_fields` (should be 0; absent when 0), `agent_info`
  versions per device/component against the **`expected_version`** textbox variable (status `OK` /
  `MISMATCH` / `NOT REPORTING` after 65 min without a point), and the `radio_cell` detection method
  over time. **`expected_version` is bumped to the new tag at each release.**
- **`radio-network.json`** (`uid: 990a32ca-18bf-4bcd-8f97-0d137fcc5c29-v2`): radio KPIs per
  leg, TWAMP RTT/jitter/loss **filtered to `exists r.iface`** (cellular path only — older wired-path
  points are deliberately hidden), glitch stalls/retransmissions/events, latest modem state and
  serving cell, and a GPS map joined with anchor RSRP/SINR.
- `device` variable (both): devices with a `radio_cell` `rsrp` point in the last 2 days.

## Verifying writes on the server

```bash
cd ~/dawn6g-metrics
ix() { docker compose exec -T influxdb sh -c 'influx query -o wavecom -t "$(cat /run/secrets/influxdb_admin_token)" "$1"' _ "$1"; }
ix 'import "influxdata/influxdb/schema" schema.tagValues(bucket: "radio", tag: "device", start: -1h)'
sudo grep "/api/v2/write" /var/log/apache2/dawn6g-influxdb-access.log | tail
```

Note: don't `group()` across all fields in `radio` — string and float fields collide; filter by
`_measurement`/`_field` first.
