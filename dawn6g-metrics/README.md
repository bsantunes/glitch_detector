# DAWN6G metrics endpoint

InfluxDB 2.7 + Grafana in Docker, published through the host's existing Apache. Apache also serves Nagios.

| What | Public URL | Apache forwards to |
|---|---|---|
| Grafana | https://wservices.wavesys.pt/grafana/ | 127.0.0.1:3000 |
| InfluxDB API (devices) and UI | https://wservices.wavesys.pt:8086 | 127.0.0.1:18086 |

The containers listen on loopback only. Apache handles TLS, using the same certificate it uses for Nagios.

## Files

| File | Purpose |
|---|---|
| `docker-compose.yml` | InfluxDB and Grafana |
| `.env` | non-secret settings |
| `gen-secrets.sh` | generates `secrets/*` once |
| `bootstrap.sh` | creates buckets and the Grafana read-only token, then starts everything (safe to re-run) |
| `add-device.sh` / `revoke-device.sh` | per-gateway write tokens |
| `apache/dawn6g-grafana.inc` | include inside the existing `*:443` vhost |
| `apache/dawn6g-influxdb.conf` | new `*:8086` TLS vhost |

## Buckets

| Bucket | Content | Retention |
|---|---|---|
| `radio` | `RadioSample` periodic KPIs | 90d |
| `network` | TWAMP, HTB/QoS, nDPI, eBPF micro-interruptions | 90d |
| `events` | `RadioEvent` (PLMN/PCI/TAC change, EN-DC add/drop, RRC re-est.) | 365d |

You can change the retention values in `.env` before the first start. After that, use `influx bucket update`.

## Tokens

| Token | Scope | Stored in |
|---|---|---|
| Admin | everything; never leaves the server | `secrets/influxdb_admin_token` |
| Grafana | read-only on the 3 buckets | `secrets/grafana_influx_token` |
| Per-device | write-only on the 3 buckets | `secrets/devices/<id>.token` |

```bash
./add-device.sh gw01      # then copy secrets/devices/gw01.token to the gateway
./revoke-device.sh gw01
```

## Smoke test from a device

```bash
curl -i "https://wservices.wavesys.pt:8086/api/v2/write?org=wavecom&bucket=radio&precision=s" \
  -H "Authorization: Token $(cat gw01.token)" \
  --data-binary "radio_cell,device=gw01,vendor=quectel,rat=lte,role=anchor,dc_mode=endc rsrp=-98,rsrq=-11,sinr=12 $(date +%s)"
# expect: HTTP/1.1 204 No Content
```

## Go agent (influxdb-client-go v2)

```go
opts := influxdb2.DefaultOptions().
    SetBatchSize(500).SetFlushInterval(1000). // ms
    SetMaxRetries(10).SetRetryBufferLimit(100000). // points kept in RAM while the link is down
    SetPrecision(time.Millisecond)
client := influxdb2.NewClientWithOptions("https://wservices.wavesys.pt:8086", token, opts)
defer client.Close()

radio := client.WriteAPI("wavecom", "radio")
go func() { for err := range radio.Errors() { log.Println("influx:", err) } }()

radio.WritePoint(influxdb2.NewPoint("radio_cell",
    map[string]string{"device": "gw01", "vendor": "quectel", "rat": "nr", "role": "scg", "dc_mode": "endc"},
    map[string]interface{}{"rsrp": -95.0, "rsrq": -11.0, "sinr": 18.5, "pci": 412},
    time.Now()))
```

The retry buffer lives in RAM only, so it does not survive a reboot.

**Schema tips**

Use tags for values you group or filter by and that have bounded cardinality: `device`, `vendor`, `rat`, `role`, `dc_mode`, `plmn`. Use fields for measured values. Put `plmn` and `tac` only on `role=anchor` rows.

## Notes

- InfluxDB init runs only once. Changing `influxdb_admin_*` secrets later has no effect; rotate them with the `influx` CLI instead.
- The secret files are 0644 inside a 0700 `secrets/` directory, so containers can read them but other host users cannot. Do not commit `secrets/`.
- Backups: `docker compose exec influxdb sh -c 'influx backup /tmp/bk -t "$(cat /run/secrets/influxdb_admin_token)"'`, then `docker compose cp influxdb:/tmp/bk ./backup-$(date +%F)`.
