# Deploying to a New Gateway

End-to-end runbook for bringing a new gateway board onto the fleet: provision
its InfluxDB token, install the right set of binaries for its OS, and confirm
it shows up in Grafana. See [CLAUDE.md](CLAUDE.md) for the architecture this
assumes, and [MODEM_TOOL.md](MODEM_TOOL.md) for `modem-tool`-specific detail
not repeated here.

## 0. Decide what this device is

- **Debian + ModemManager** (like `aaeon`) → eBPF works. Install everything:
  `dawn-agent`, `modem-tool`, `modem-exporter`, `twamp-tool`, `twamp-exporter`.
- **WaveOS / OpenWrt** (like `6global`, `dawn6g`) → **no eBPF** (confirmed
  missing `clsact`/TC-BPF kernel support — see CLAUDE.md's Known Issues).
  Install only `modem-tool`, `modem-exporter`, `twamp-tool`, `twamp-exporter`.
  Don't bother with `dawn-agent` here; it will fail to load its qdisc.

Pick a short, stable **device ID** now (e.g. the hostname — `aaeon`,
`6global`, `dawn6g`) — it's used as both the InfluxDB token's name and the
`device` tag on every point this board writes, and it's what will show up in
Grafana's device dropdown.

## 1. Provision an InfluxDB token (do this first, on `wservices`)

```bash
ssh wservices
cd ~/dawn6g-metrics
./add-device.sh <device-id>          # e.g. ./add-device.sh newboard01
cat secrets/devices/<device-id>.token
```

This creates **one write-only token scoped to all three buckets**
(`radio`/`network`/`events`) — the same token is reused by `modem-exporter`
*and* `twamp-exporter`, no need to provision separately per exporter.

**Always use `add-device.sh`, not a manual `influx auth create`.** It also
writes `secrets/devices/<device-id>.authid` (the auth entry's own ID), which
`revoke-device.sh` needs to revoke the token later — without it you'd have to
go find the auth ID by hand via `influx auth list` (this already happened to
`6global`/`dawn6g`, whose tokens predate this script; don't repeat it for a
new device).

Copy the printed token somewhere you can paste it in step 3/4 — you won't be
able to retrieve it again later (InfluxDB only shows a token at creation
time); if lost, revoke and recreate.

## 2. Get the code onto the device

Either clone the repo directly on the device (if it has git + internet) or
cross-compile and copy just the binaries — see step 3A/3B, which cover both.

```bash
git clone https://github.com/bsantunes/glitch_detector.git
cd glitch_detector
```

## 3A. Install on a Debian gateway (systemd)

Run on the device itself (it has a Go toolchain):

```bash
cd glitch_detector
make                               # tc_glitch.o + dawn-agent
make modem-tool modem-exporter twamp-tool twamp-exporter

sudo make install                  # dawn-agent → /opt/dawn, systemd unit
sudo make install-modem-tool
sudo make install-modem-exporter   # needs modem-exporter.env — see below
sudo make install-twamp-tool
sudo make install-twamp-exporter   # reuses the same modem-exporter.env
```

Create the token file (mode 600, root-owned) **before** enabling the
exporters — both `modem-exporter.service` and `twamp-exporter.service` read
`INFLUX_TOKEN` from it:

```bash
sudo cp modem-exporter.env.example /opt/dawn/modem-exporter.env
sudo nano /opt/dawn/modem-exporter.env   # paste the token from step 1
sudo chmod 600 /opt/dawn/modem-exporter.env
sudo chown root:root /opt/dawn/modem-exporter.env
```

Enable and start everything:

```bash
sudo systemctl enable --now dawn-agent modem-tool modem-exporter twamp-tool twamp-exporter
```

(Running `modem-tool`/`twamp-tool` continuously *alongside* their exporters
on the same box is redundant — both poll/probe independently — but harmless;
disable the `-tool` ones here if you only want the InfluxDB-backed telemetry
and don't need a local `journalctl`-readable feed too.)

Check logs:

```bash
journalctl -u dawn-agent -u modem-exporter -u twamp-exporter -f
```

## 3B. Install on a WaveOS gateway (procd)

WaveOS boards have **no Go/clang toolchain and tight storage** — cross-compile
a static binary elsewhere (e.g. on the Debian board, or any Linux/amd64 host
with Go) and copy just the binaries over. WaveOS boards seen so far are all
`x86_64`.

On the build host:

```bash
cd glitch_detector
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o modem-tool      ./cmd/modem-tool
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o modem-exporter  ./cmd/modem-exporter
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o twamp-tool      ./cmd/twamp-tool
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o twamp-exporter  ./cmd/twamp-exporter
```

`CGO_ENABLED=0` matters: a normal `go build` is dynamically linked against
the build host's glibc, which won't necessarily match WaveOS's. The static
build sidesteps that.

Copy the binaries and the matching `openwrt/*.init` scripts to the device:

```bash
scp -P <port> modem-tool modem-exporter twamp-tool twamp-exporter root@<waveos-ip>:/opt/dawn/
scp -P <port> openwrt/modem-tool.init      root@<waveos-ip>:/etc/init.d/modem-tool
scp -P <port> openwrt/modem-exporter.init  root@<waveos-ip>:/etc/init.d/modem-exporter
scp -P <port> openwrt/twamp-tool.init      root@<waveos-ip>:/etc/init.d/twamp-tool
scp -P <port> openwrt/twamp-exporter.init  root@<waveos-ip>:/etc/init.d/twamp-exporter
```

On the device, **before enabling anything**, edit the two exporter init
scripts to set the real `DEVICE_ID` (they ship with a placeholder — see each
file's `DEVICE_ID=...` line near the top) and create the shared token file,
reused by both exporters:

```bash
ssh -p <port> root@<waveos-ip>
vi /etc/init.d/modem-exporter   # set DEVICE_ID=<device-id>
vi /etc/init.d/twamp-exporter   # set DEVICE_ID=<device-id> (same value)

cat > /opt/dawn/modem-exporter.env <<'EOF'
INFLUX_TOKEN=<paste the token from step 1>
EOF
chmod 600 /opt/dawn/modem-exporter.env

chmod +x /opt/dawn/modem-tool /opt/dawn/modem-exporter /opt/dawn/twamp-tool /opt/dawn/twamp-exporter
chmod +x /etc/init.d/modem-tool /etc/init.d/modem-exporter /etc/init.d/twamp-tool /etc/init.d/twamp-exporter

for svc in modem-tool modem-exporter twamp-tool twamp-exporter; do
  /etc/init.d/$svc enable
  /etc/init.d/$svc start
done
```

Check logs:

```bash
logread | grep -Ei "modem-exporter|twamp-exporter|modem-tool|twamp-tool" | tail -30
```

## 4. Verify data is actually landing in InfluxDB

From `wservices` (or anywhere with `docker compose` access to the stack):

```bash
cd ~/dawn6g-metrics
docker compose exec -T influxdb influx query '
from(bucket:"radio") |> range(start:-5m)
  |> filter(fn:(r)=>r.device=="<device-id>")
  |> filter(fn:(r)=>r._measurement=="radio_cell" or r._measurement=="modem_state")
  |> last()'

docker compose exec -T influxdb influx query '
from(bucket:"network") |> range(start:-5m)
  |> filter(fn:(r)=>r.device=="<device-id>")
  |> filter(fn:(r)=>r._measurement=="twamp")
  |> last()'
```

If `radio`/`radio_cell` comes back empty but `modem_state` has rows, the modem
likely isn't registered/has no SIM yet — that's expected and documented in
CLAUDE.md's "no SIM" note, not a bug. If **both** come back empty, check the
service logs first (step 3A/3B) before suspecting the server side; the token
itself being wrong/revoked is the single most common cause of total silence
(look for `unauthorized`/`403` in the exporter's log output).

## 5. Grafana — no manual "add device" step needed

The dashboard's `device` variable is query-driven
(`tagValues(bucket: "...", tag: "device")`), so **a new device appears in the
dropdown automatically** the first time it writes at least one point to the
relevant bucket — nothing to configure in Grafana itself. Just:

1. Wait for step 4 to show data (or refresh the dashboard — the `device`
   variable re-queries on dashboard load/time-range change).
2. Open the device dropdown and select the new device (or "All", if the
   variable has that option enabled) — it'll be listed alongside
   `aaeon`/`6global`/`dawn6g`.

No datasource change is needed either: the `influxdb-flux` datasource's
`grafana-read` token already has read access to all three buckets
(`radio`/`network`/`events`), so a brand-new device's data in any of them is
immediately queryable — this was confirmed directly against `influx auth
list` while debugging a separate "no data" issue, not assumed.

## 6. Gotchas checklist

- **Use `add-device.sh`, never create the token by hand** — see step 1.
- **WaveOS exporters share one token file** (`/opt/dawn/modem-exporter.env`)
  — don't create a separate one for `twamp-exporter`; it reads the same file.
- **Set `DEVICE_ID` in *both* WaveOS init scripts** (`modem-exporter` and
  `twamp-exporter`) — they default to a placeholder value that will
  silently mislabel your data under the wrong device tag if left unedited.
- **No SIM ≠ no data.** A SIM-less modem can still report real
  RSRP/RSRQ/RSSI/SINR/PLMN/TAC/CellID by camping on a broadcasting cell's
  physical-layer system information — don't assume a missing SIM means the
  deployment is broken.
- **WaveOS's `uqmi-ubus` daemon can starve raw QMI clients** (QMI client-ID
  exhaustion) if `modem-tool`/`modem-exporter` end up falling back past the
  `ubus` backend to raw `qmicli`/`uqmi` — check the log line's `via:` field
  says `ubus` (or `ModemManager` on Debian), not `QMI`/`uqmi`, after a fresh
  boot. See CLAUDE.md's Known Issues for the underlying startup-race cause.
- **`dawn-agent` will not run on WaveOS** — its eBPF qdisc load fails
  (`ENOENT`) on every WaveOS kernel seen so far. Don't install/enable it
  there.
