# Running `modem-tool` on Another Device

`modem-tool` is the standalone modem detection / signal-quality CLI built on
the `modem/` package (see [ARCHITECTURE.md](ARCHITECTURE.md#modem-detection-modem-package)).
It has **no dependency on the eBPF/TWAMP side of `dawn-agent`** — it doesn't
load BPF, doesn't touch `tc`/netlink, and doesn't need a TWAMP reflector — so
it can be built and run on its own, on a different device than the gateway.

## Prerequisites

- **OS: Linux.** It shells out to Linux-specific tools (`mmcli`, `qmicli`,
  `mbimcli`, `stty`) and paths (`/dev/ttyUSB*`, `/dev/cdc-wdm*`), so it won't
  work on macOS/Windows.
- **Go toolchain, 1.21+ recommended.** The `modem/` and `cmd/modem-tool` code
  itself has **zero third-party dependencies** — standard library only.
- **Root/sudo to actually run it.** `mmcli --signal-setup` (ModemManager path)
  and direct QMI/MBIM/AT device access both require it.
- Optional, only needed for detection to actually find something (install
  whichever backend matches the target device's modem):
  - **ModemManager** (`sudo apt install modemmanager`) — provides `mmcli`,
    the primary/preferred detection path.
  - **libqmi-utils** / **libmbim-utils** (`sudo apt install libqmi-utils libmbim-utils`)
    — provides `qmicli`/`mbimcli`, the fallback path for modems ModemManager
    isn't managing. (On OpenWrt-based systems, `qmicli` may already be bundled
    with the `libqmi` package itself rather than a separate `-utils` package —
    check with `which qmicli` rather than the package manager alone.)
  - **`uqmi`** — OpenWrt's own lightweight QMI client; used as a further
    fallback on systems (like WaveOS) that have it but not `qmicli`/`mbimcli`.
  - `stty` — part of coreutils, present on essentially every Linux system
    already; used for the raw-AT-over-serial fallback.

## Files needed

Just two directories from this repo — nothing else:

```
modem/
  types.go
  detect.go
  mmcli.go
  qmi.go
  uqmi.go
  at.go
  mmcli_test.go   (optional — only needed to run `go test`)
  qmi_test.go     (optional)
  uqmi_test.go    (optional)
  at_test.go      (optional)
cmd/modem-tool/
  main.go
```

## Option A — clone the whole repo (simplest, if the device has git + GitHub access)

```bash
git clone https://github.com/bsantunes/glitch_detector.git
cd glitch_detector
make modem-tool
sudo ./modem-tool
```

**Caveat:** `make modem-tool` depends on the same `deps` Makefile target as
the full `dawn-agent` build, so it will also fetch `cilium/ebpf`,
`vishvananda/netlink` and the InfluxDB client — none of which `modem-tool`
actually uses. That's harmless but pulls in extra network dependencies and,
on an older Go toolchain, may force a Go version bump (`cilium/ebpf`
currently requires Go 1.25+). If the target device is offline, has an older
Go toolchain you don't want to touch, or you just don't want the extra
weight, use Option B instead.

## Option B — copy just the modem-tool files (minimal, no unrelated dependencies)

1. From this machine, copy the two directories to the target device,
   preserving their relative paths (`rsync -R`/`--relative` keeps `modem/`
   and `cmd/modem-tool/` under the destination folder):

   ```bash
   rsync -avR modem cmd/modem-tool user@device:~/modem-tool/
   ```

   (Swap in `scp -r` + manual directory creation, or a USB drive, if `rsync`
   isn't available — the only requirement is that `modem/` and
   `cmd/modem-tool/` end up as siblings on the target.)

2. On the target device:

   ```bash
   cd ~/modem-tool
   go mod init dawn-agent          # module name MUST be "dawn-agent" —
                                    # cmd/modem-tool/main.go imports "dawn-agent/modem"
   go build -o modem-tool ./cmd/modem-tool
   ```

   No `go get` is needed — there are no third-party imports to resolve.

3. Run it:

   ```bash
   sudo ./modem-tool                  # detect, then poll signal quality every 5s
   sudo ./modem-tool -interval 10s    # custom poll interval
   ```

## What to expect

Detection phase first, then continuous polling:

```
Detecting modems (ModemManager -> QMI/MBIM -> AT)...
Found 1 modem(s):
  [0] QUALCOMM INCORPORATED 0 (port: cdc-wdm0, via: ModemManager)

Polling signal quality every 5s (Ctrl+C to stop)...

[0] tech=lte  RSRP=-86.0 RSRQ=-10.0 RSSI=-55.0 SINR=12.8
```

(A `Model` field showing a bare number like `0` is real — some modems,
including the SIM8202G-M2 used for development, genuinely report that via
ModemManager. Not a parsing bug.)

## Running on WaveOS (OpenWrt)

WaveOS is an OpenWrt-based distribution (confirmed on an x86_64 Supermicro
board, OpenWrt 25.01-based, glibc — not musl — per `DISTRIB_TAINTS`) with no
`systemd` (uses `procd`), `opkg` instead of `apt`, and no `ModemManager` at
all. It also tends to run with very little free storage on the root overlay
(a few hundred MB), so it isn't a build host — cross-compile elsewhere and
ship a single static binary.

**Caveat — this doc is `modem-tool`/signal side only.** The full `dawn-agent`
(eBPF passive stall/retransmit detection) does **not** work on at least this
WaveOS kernel build: it lacks the `clsact` qdisc (`netlink.QdiscAdd` fails
with `no such file or directory`, confirmed via `modprobe cls_bpf`/
`sch_ingress` also failing to find those modules). `modem-tool`, which only
needs userspace tools + a QMI/MBIM/AT device node, is unaffected by that and
is what's been validated here.

### Build and deploy

No Go, no `clang`, no package installs needed on the WaveOS box itself —
build a static binary on any Linux/amd64 machine with Go and copy just that
one file over:

```bash
# On a Linux/amd64 build host (e.g. the Debian gateway board):
cd glitch_detector
CGO_ENABLED=0 go build -o modem-tool ./cmd/modem-tool   # static — no glibc version to match

# Copy to the WaveOS box:
scp -P <port> modem-tool root@<waveos-ip>:/root/modem-tool

# On WaveOS:
ssh -p <port> root@<waveos-ip>
chmod +x /root/modem-tool
./modem-tool                  # root required either way, already logged in as root here
./modem-tool -interval 10s
```

`CGO_ENABLED=0` matters here specifically: a default `go build` on Debian
produces a *dynamically* linked glibc binary, which isn't guaranteed to run
against WaveOS's own (differently-versioned) glibc. The static build sidesteps
that entirely.

### What's actually there

- **No `mmcli`/ModemManager** — detection logs `mmcli not found in PATH` and
  falls through; this is expected, not an error.
- **`qmicli` is present** — bundled via the `libqmi` opkg package itself on
  this WaveOS build, *not* a separate `libqmi-utils`-style package, so
  `opkg list-installed | grep qmicli` finds nothing even though the binary is
  right there. Check with `which qmicli` instead of trusting a package-name
  grep (this cost real debugging time — don't repeat the mistake).
- **`uqmi` is also present** but never reached in the cascade on *this* box,
  since `qmicli` answers first. It's there for other OpenWrt/WaveOS targets
  that have `uqmi` but not `qmicli` — only unit-tested so far (against real
  captured JSON), not exercised end-to-end on live hardware.
- **No `mbimcli`.**
- **Two modems observed** on the reference box: a SIMCom 5G module and a
  Quectel RM520N-GL, exposed as `/dev/cdc-wdm0`/`/dev/cdc-wdm1` (and, on this
  specific image, stable `/dev/qmi0`/`/dev/qmi1` symlinks to the same nodes,
  assigned per physical USB port rather than enumeration order — `uqmi`'s
  device globbing prefers these stable names when present).

Real captured output, both modems via `qmicli`:

```
Detecting modems (ModemManager -> QMI/MBIM -> AT)...
Found 2 modem(s):
  [/dev/cdc-wdm0] QUALCOMM INCORPORATED 0 (port: /dev/cdc-wdm0, via: QMI)
  [/dev/cdc-wdm1] Quectel RM520N-GL (port: /dev/cdc-wdm1, via: QMI)

Polling signal quality every 8s (Ctrl+C to stop)...

[/dev/cdc-wdm0] tech=lte  RSRP=-122.0 RSRQ=-15.0 RSSI=-98.0 SINR=2.6
[/dev/cdc-wdm0] PLMN=26806 (unknown) reg=not-registered-searching pkt=unknown TAC=48830 CellID=263731
[/dev/cdc-wdm1] tech=unknown RSRP=N/A RSRQ=N/A RSSI=-128.0 SINR=N/A
[/dev/cdc-wdm1] PLMN=unknown (unknown) reg=not-registered pkt=unknown TAC=unknown CellID=unknown
```

The Quectel modem above was in power-save/not-registered state — note it
still reports a real floor RSSI (-128 dBm) while everything else correctly
shows N/A/unknown rather than garbage.

### WaveOS-specific notes

- `qmicli`'s `--nas-get-cell-location-info` is **not supported** on this
  qmicli version/build (`error: Unknown option`) — TAC/CellID come from
  `--nas-get-system-info` instead, which works fine.
- A modem with no serving cell at all reports its network as `Network
  'none'` (via `--nas-get-signal-strength`) instead of a real technology —
  normalized to "unknown" rather than surfacing the literal string `none`.

## Troubleshooting

- **`no modems found via ModemManager, QMI/MBIM, or AT`** — check the modem
  is visible at all: `mmcli -L`, or `ls /dev/cdc-wdm* /dev/ttyUSB*` if
  ModemManager isn't managing it.
- **Modem detected, but all signal values are `N/A`** — you're not running
  as root. `mmcli --signal-setup` (which makes ModemManager actually start
  polling the radio) needs PolicyKit/root authorization.
- **`mbimcli` errors with "device is closed"** — normal if the modem is
  QMI-only (as the SIM8202G-M2 is); MBIM will never work on such a modem,
  and that's expected, not a bug.
- **`stty: ... unable to perform all requested operations`** printed to
  stderr during AT detection — harmless; the code logs it and continues,
  since the serial port typically still works fine afterward.

## Optional: run the unit tests too

If you copied the `*_test.go` files as well:

```bash
go test ./modem/... -v
```

These are pure parser tests (no external tools or hardware required) —
several are built directly from real `mmcli`/`qmicli` output captured during
development.
