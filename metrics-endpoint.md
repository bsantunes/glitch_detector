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
  `events` (365d, RadioEvent: PLMN/PCI/TAC change, EN-DC add/drop, RRC re-est.).
- Tokens: admin (server only), Grafana read-only on the 3 buckets, **one write-only token per device**
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
