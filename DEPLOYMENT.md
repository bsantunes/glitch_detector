# Deploying to a New Gateway

End-to-end runbook for bringing a new gateway board onto the fleet: provision
its InfluxDB token, install the right set of binaries for its OS, and confirm
it shows up in Grafana. See [CLAUDE.md](CLAUDE.md) for the architecture this
assumes, and [MODEM_TOOL.md](MODEM_TOOL.md) for `modem-tool`-specific detail
not repeated here.

## 0. Decide what this device is

- **Debian + ModemManager** (like `aaeon`) → eBPF works. Install
  `glitch-exporter`, `modem-exporter`, `twamp-exporter` (plus `modem-tool`/
  `twamp-tool`/`glitch-tool` if you also want local, journalctl-readable
  diagnostics alongside the InfluxDB push — see the redundancy note below).
  **Don't install `dawn-agent`** — it's superseded by those three exporters
  running side by side (see CLAUDE.md's Project Overview) and its own eBPF
  decode logic no longer matches the current `tc_glitch.o` at all.
- **WaveOS / OpenWrt** (like `6global`, `dawn6g`) → **no eBPF** (confirmed
  missing `clsact`/TC-BPF kernel support — see CLAUDE.md's Project Overview).
  Install `modem-exporter`, `twamp-exporter`, **`glitch-exporter` with
  `-capture afpacket`** (the AF_PACKET backend needs no eBPF), and
  **`gps-exporter`** (AT-over-serial), plus `modem-tool`/`twamp-tool` if
  you want local diagnostics. This is exactly what runs on `dawn6g`. Don't
  install `dawn-agent`, and never run `glitch-tool`/`glitch-exporter` with
  the default `-capture ebpf` here — the eBPF path fails to load its qdisc
  on this kernel.

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

This creates **one write-only token scoped to all four buckets**
(`radio`/`network`/`events`/`location`) — the same token is reused by
`modem-exporter`, `twamp-exporter`, `gps-exporter`, *and* `glitch-exporter`,
no need to provision separately per exporter.

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
make                               # tc_glitch.o only, by default (see CLAUDE.md) — dawn-agent is superseded, not built here
make modem-tool modem-exporter twamp-tool twamp-exporter glitch-tool glitch-exporter

sudo make install-modem-tool
sudo make install-modem-exporter      # needs modem-exporter.env — see below
sudo make install-twamp-tool
sudo make install-twamp-exporter      # reuses the same modem-exporter.env
sudo make install-glitch-exporter     # reuses the same modem-exporter.env
```

(`glitch-tool` has no install target — it's interactive-only, run it directly
for diagnostics; see below. `dawn-agent`'s own `sudo make install-dawn-agent`
exists but is intentionally not part of this flow — see step 0.)

Create the token file (mode 600, root-owned) **before** enabling the
exporters — `modem-exporter.service`, `twamp-exporter.service`, *and*
`glitch-exporter.service` all read `INFLUX_TOKEN` from it:

```bash
sudo cp modem-exporter.env.example /opt/dawn/modem-exporter.env
sudo nano /opt/dawn/modem-exporter.env   # paste the token from step 1
sudo chmod 600 /opt/dawn/modem-exporter.env
sudo chown root:root /opt/dawn/modem-exporter.env
```

Enable and start everything:

```bash
sudo systemctl enable --now modem-tool modem-exporter twamp-tool twamp-exporter glitch-exporter
```

(Running `modem-tool`/`twamp-tool` continuously *alongside* their exporters
on the same box is redundant — each pair runs its own independent
detect-and-poll loop with no shared state, so you get the same modem/reflector
queried twice: once printed locally via `journalctl`, once pushed to
InfluxDB. That's harmless *correctness*-wise (both are read-only queries) —
but on WaveOS specifically, if detection ever falls through past the `ubus`
backend to raw `qmicli`/`uqmi` (the startup-race bug in CLAUDE.md's Known
Issues), running two independent QMI clients instead of one doubles your
exposure to QMI client-ID exhaustion. Disable the `-tool` ones here if you
only want the InfluxDB-backed telemetry and don't need a local
`journalctl`-readable feed too.)

Check logs:

```bash
journalctl -u modem-exporter -u twamp-exporter -u glitch-exporter -f
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
make embed   # glitch/ embeds tc_glitch.o at compile time, even for -capture afpacket
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o glitch-exporter ./cmd/glitch-exporter
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o gps-exporter    ./cmd/gps-exporter
```

Build from a tagged checkout (`git checkout vX.Y.Z`) and add
`-ldflags "-X dawn-agent/version.Version=$(git describe --tags --always --dirty)"`
to each line (the `Makefile` does this for native builds) so `-version` and
the `agent_info` point report what's actually running.

`CGO_ENABLED=0` matters: a normal `go build` is dynamically linked against
the build host's glibc, which won't necessarily match WaveOS's. The static
build sidesteps that.

Copy the binaries and the matching `openwrt/*.init` scripts to the device:

```bash
scp -P <port> modem-tool modem-exporter twamp-tool twamp-exporter glitch-exporter gps-exporter root@<waveos-ip>:/opt/dawn/
scp -P <port> openwrt/modem-tool.init      root@<waveos-ip>:/etc/init.d/modem-tool
scp -P <port> openwrt/modem-exporter.init  root@<waveos-ip>:/etc/init.d/modem-exporter
scp -P <port> openwrt/twamp-tool.init      root@<waveos-ip>:/etc/init.d/twamp-tool
scp -P <port> openwrt/twamp-exporter.init  root@<waveos-ip>:/etc/init.d/twamp-exporter
scp -P <port> openwrt/glitch-exporter.init root@<waveos-ip>:/etc/init.d/glitch-exporter
scp -P <port> openwrt/gps-exporter.init    root@<waveos-ip>:/etc/init.d/gps-exporter
```

What the two extra exporters run (matching `dawn6g`):

- **`glitch-exporter`**: `-capture afpacket -iface lte1` (set `IFACE` in the
  init script to the interface actually carrying traffic — `lte1`/`mwan1` on
  `dawn6g`; `lte0`/`mwan0` is disabled there). The AF_PACKET backend takes an
  exclusivity lock at **`/var/run/glitch-afpacket-<iface>.lock`** — `/var/run`,
  not `/run`, because WaveOS has no `/run` at all (glitch-exporter crash-looped
  on `dawn6g` with `open lock file: ... no such file or directory` until the
  path was changed). Only one AF_PACKET capture per interface can hold it: a
  `glitch-tool -capture afpacket` on the same interface will refuse to start
  while the exporter runs (stop the exporter first for a manual test).
- **`gps-exporter`**: `-method at -port usb:2c7c:0801:03` — the Quectel
  RM520N-GL's secondary AT port (interface 03), pinned by USB
  vendor/product/interface because `ttyUSB` numbers aren't stable across
  reboots on a two-modem board. **No `-nmea-port` on WaveOS**: the Quectel's
  NMEA streaming is unresolved (see the `gps-integration` skill); AT fixes
  arrive fine there. (`dawn6g`'s installed copy pins `/dev/ttyUSB10`, the same
  interface today; new installs should use the `usb:` form.)

On the device, **before enabling anything**, edit all four exporter init
scripts to set the real `DEVICE_ID` (they ship with a placeholder — see each
file's `DEVICE_ID=...` line near the top; `glitch-exporter` also has `IFACE`)
and create the shared token file, reused by all four exporters:

```bash
ssh -p <port> root@<waveos-ip>
vi /etc/init.d/modem-exporter   # set DEVICE_ID=<device-id>
vi /etc/init.d/twamp-exporter   # set DEVICE_ID=<device-id> (same value)
vi /etc/init.d/glitch-exporter  # set DEVICE_ID=<device-id>, IFACE=<traffic iface>
vi /etc/init.d/gps-exporter     # set DEVICE_ID=<device-id>; check GPS_PORT

cat > /opt/dawn/modem-exporter.env <<'EOF'
INFLUX_TOKEN=<paste the token from step 1>
EOF
chmod 600 /opt/dawn/modem-exporter.env

chmod +x /opt/dawn/modem-tool /opt/dawn/modem-exporter /opt/dawn/twamp-tool /opt/dawn/twamp-exporter /opt/dawn/glitch-exporter /opt/dawn/gps-exporter
chmod +x /etc/init.d/modem-tool /etc/init.d/modem-exporter /etc/init.d/twamp-tool /etc/init.d/twamp-exporter /etc/init.d/glitch-exporter /etc/init.d/gps-exporter

for svc in modem-tool modem-exporter twamp-tool twamp-exporter glitch-exporter gps-exporter; do
  /etc/init.d/$svc enable
  /etc/init.d/$svc start
done
```

Check logs:

```bash
logread | grep -Ei "modem-exporter|twamp-exporter|glitch-exporter|gps-exporter|modem-tool|twamp-tool" | tail -30
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

# glitch-exporter — any board (eBPF on Debian, -capture afpacket on WaveOS).
# glitch_summary writes a point every interval regardless of activity, so
# this should never come back empty on a device that has it running.
docker compose exec -T influxdb influx query '
from(bucket:"network") |> range(start:-5m)
  |> filter(fn:(r)=>r.device=="<device-id>")
  |> filter(fn:(r)=>r._measurement=="glitch_summary")
  |> last()'

# gps-exporter — only writes gps_fix while the receiver has a valid fix, so
# an empty result can just mean no fix yet (indoor antenna). Check its log
# before suspecting the exporter; NMEA-sourced points carry source="nmea".
docker compose exec -T influxdb influx query '
from(bucket:"location") |> range(start:-1h)
  |> filter(fn:(r)=>r.device=="<device-id>" and r._measurement=="gps_fix")
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
- **Set `DEVICE_ID` in *every* WaveOS exporter init script** (`modem-exporter`,
  `twamp-exporter`, `glitch-exporter`, `gps-exporter`) — they ship with a
  placeholder value that will silently mislabel your data under the wrong
  device tag if left unedited.
- **WaveOS has no `/run`** — anything writing runtime/lock files must use
  `/var/run` (see `glitch-exporter`'s AF_PACKET lock above).
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
  there — and don't install it on Debian either; it's superseded (step 0).
- **`glitch-exporter.service` sets `Conflicts=dawn-agent.service`** — if
  `dawn-agent` happens to already be installed/enabled on a device (it
  shouldn't be, per step 0), starting `glitch-exporter` will stop it. Both
  would otherwise try to attach their own eBPF program to the same
  interface.

## 7. GPS on a Debian gateway: reserving a dedicated AT port

`gps-exporter -method at` needs its own serial port, never shared with
ModemManager or anything else that might open it concurrently. Before
installing/enabling `gps-exporter` with `-method at -port <dev>` on a new
Debian board, reserve that port with a udev rule so ModemManager (and
`modem.DetectAT`'s own fallback scan) never touches it either.

**Identify the right port first** — read-only, no `mmcli --location-*`
calls:

```bash
mmcli -m 0 | sed -n '/ports:/,/---/p'      # find the AT_SECONDARY port — never AT_PRIMARY, that one stays ModemManager's
udevadm info -q property -n /dev/ttyUSBx   # per candidate: note ID_VENDOR_ID, ID_MODEL_ID, ID_USB_INTERFACE_NUM
```

**Install the rule** (example from `aaeon`'s SIMCom, USB interface `03` —
substitute the vendor/product/interface values for the board at hand):

```bash
sudo tee /etc/udev/rules.d/99-dawn-gps.rules > /dev/null <<'EOF'
SUBSYSTEM=="tty", ATTRS{idVendor}=="1e0e", ATTRS{idProduct}=="9001", ENV{ID_USB_INTERFACE_NUM}=="03", ENV{ID_MM_PORT_IGNORE}="1", SYMLINK+="dawn-gps"
EOF
sudo udevadm control --reload-rules
```

**Validate with a dry run before rebooting** — the reserved port must show
`ID_MM_PORT_IGNORE=1` and a `dawn-gps` symlink action; every other AT port
(especially `AT_PRIMARY`) must show neither:

```bash
sudo udevadm test /sys/class/tty/ttyUSBx 2>&1 | grep -iE "ID_MM_PORT_IGNORE|dawn-gps|ID_USB_INTERFACE_NUM"
```

**Reboot, then confirm**:

```bash
mmcli -m 0 | sed -n '/ports:/,/---/p'   # reserved port now shows as "ignored"
ls -l /dev/dawn-gps                     # symlink exists, points at the reserved port
sudo fuser -v /dev/dawn-gps             # must print nothing — no process holds it
ip -4 addr show wwan0                   # still has an IP — the reboot didn't break the data session
systemctl is-active modem-exporter twamp-exporter glitch-exporter   # all still "active"
```

**Rollback**, if the rule ever needs to come out:

```bash
sudo rm /etc/udev/rules.d/99-dawn-gps.rules
sudo udevadm control --reload-rules
sudo reboot
# then confirm the port is back in mmcli's normal (non-"ignored") port list
```

Confirmed live on `aaeon` (2026-11-18): `ttyUSB3` — SIMCom's `AT_SECONDARY`
port — reserved this way as `/dev/dawn-gps`; `ttyUSB2` (`AT_PRIMARY`)
correctly untouched; `modem-exporter`/`twamp-exporter`/`glitch-exporter` all
stayed up across the reboot. See CLAUDE.md's `gps/` package section
("Phase C") for the full writeup.
