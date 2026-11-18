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
modem_state,device=<id>,modem_id=<n> operator_name="<s>",registration_state="<s>",packet_service_state="<s>"
```

- Tags = bounded-cardinality grouping keys; fields = measurements. `cell_id` stays a field.
- One point per carrier: under EN-DC the LTE anchor and NR SCG are separate points (never mixed).
- `plmn`/`tac` only on `role=anchor` rows.
- Go client `github.com/influxdata/influxdb-client-go/v2`, one `WriteAPI` per bucket, batch 500,
  flush 1000 ms, max retries 10, retry buffer 100000 points (RAM only), precision ms.

## `network` bucket: `glitch_summary` / `glitch_event` (eBPF stall/retransmission monitor)

Written by `cmd/glitch-exporter` (the `glitch/` package's InfluxDB exporter —
see `CLAUDE.md`'s `glitch/` package section for the detection logic this
schema reports on). Confirmed live on `aaeon`: a forced-outage test's points
matched the kernel's own reported duration/step count exactly, and 10+
minutes of background-only traffic kept ticking `glitch_summary` with
`stall_episodes=0` throughout.

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

Tags: `device`, `iface`. **Never an IP address or port** — see `glitch_event` below for where those actually live.

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
`"stall_abandoned"`).

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

No Grafana panel for this yet — same open "TODO 2"-style gap as the
`radio`/`twamp` dashboards below, not yet captured as repo-tracked JSON.

## `location` bucket: `gps_fix`

Written by `cmd/gps-exporter` (the `gps/` package's InfluxDB exporter — see
`CLAUDE.md`'s `gps/` package section for the detection/backend logic this
schema reports on). Deployed and running on `6global`/`dawn6g` via the
AT-over-serial backend; `aaeon`'s `gps-exporter` is currently stopped/
disabled (independent of hardware — `aaeon` does have a working GNSS
antenna, confirmed via manual testing, see `CLAUDE.md`).

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

Tags: `device`, `modem_id`, `method` (`"ModemManager"` or `"AT"` — raw QMI
is implemented but deliberately disabled, see `CLAUDE.md`).

**No real `gps_fix` point has landed from `6global`/`dawn6g` yet** — both
run cleanly with zero errors but have no GPS antenna connected, so they
never get past "engine enabled, polling, no fix." `aaeon` does have a
working antenna (confirmed via manual AT testing, decoding to
41.549747, -8.413069 — see `CLAUDE.md`), but that was outside
`gps-exporter`, which remains stopped there; no point has been written to
this bucket from any device yet as of this writing.

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
- **TODO 2:** provisioned Grafana dashboard JSON in `grafana/provisioning/dashboards/`
  (device variable from `schema.tagValues(bucket:"radio", tag:"device")`, refresh on time range change;
  RSRP/RSRQ/SINR time series with `aggregateWindow(every: v.windowPeriod, fn: mean)`, display name
  `${__field.labels.device} ${__field.labels.rat}/${__field.labels.role}`; Stat panel with `last()`;
  State timeline for `modem_state`; later: annotations from `events`).

## Verifying writes on the server

```bash
cd ~/dawn6g-metrics
ix() { docker compose exec -T influxdb sh -c 'influx query -o wavecom -t "$(cat /run/secrets/influxdb_admin_token)" "$1"' _ "$1"; }
ix 'import "influxdata/influxdb/schema" schema.tagValues(bucket: "radio", tag: "device", start: -1h)'
sudo grep "/api/v2/write" /var/log/apache2/dawn6g-influxdb-access.log | tail
```

Note: don't `group()` across all fields in `radio` — string and float fields collide; filter by
`_measurement`/`_field` first.
