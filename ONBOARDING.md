# Welcome to DAWN6G

## Your Setup Checklist

### Codebases
- [ ] glitch_detector — github.com/bsantunes/glitch_detector

### Board and server access
- [ ] An SSH key authorized on the boards you'll work on (`dawn6g`, `6global` as `root`; `aaeon` as a user with sudo) and on `wservices`. Ask a teammate to add yours.
- [ ] sudo on `aaeon`/`wservices` for anything beyond read-only checks. The InfluxDB tokens on the gateways are write-only, so reading data means querying on `wservices` (`DEPLOYMENT.md` step 4) or using Grafana.

### MCP Servers to Activate
- None needed. Everything runs through the repo, SSH to the boards, and the project skills in `.claude/skills/`.

### Skills to Know About
- Project skills load on their own: `.claude/skills/` has deep references (`glitch-detector-internals`, `modem-detection`, `gps-integration`, `telemetry-schema`, `twamp-measurement`). Mention the topic and Claude pulls in the right one.

## Team Tips

- **Read `CLAUDE.md` first.** Its Hard Rules section lists the things that have broken boards before. Claude loads it automatically, but you should know it too.
- **The priority boards are `dawn6g` and `6global`** (WaveOS, live SIM + antennas, all four exporters). Both are in an observation period: read-only checks are fine; config or binary changes wait until it ends.
- **`aaeon` is a build machine only** (no SIM, no antennas). Only `git pull` in `~/glitch_detector` there. Release binaries: **tag first, then build from a clean clone of the tag** (`~/build/<tag>`), so every binary reports exactly that tag (`DEPLOYMENT.md` step 3B). Don't build from `~/glitch_detector_dev`.
- **Never run `mmcli --location-*` or restart ModemManager on `aaeon`**, and never re-enable the raw QMI GPS backend. Both have corrupted the modem's QMI state in production.
- **Never send AT commands by hand on `ttyUSB9` while `uqmi-ubus` runs** on a WaveOS board. It holds that port, and replies go to whichever reader takes them first.
- **WaveOS boards have no Go toolchain.** Cross-compile with `GOOS=linux GOARCH=amd64 CGO_ENABLED=0` and copy the binary plus its init script.
- **On WaveOS, traffic to `wservices` goes over the wired link**, not cellular (WireGuard's endpoint route). That's why `twamp-exporter` runs with `-bind-dev lte1` against `51.91.57.3:862`.
- **Test scripts in `bpf_src/`** (each prints PASS/FAIL per run):
  - **Test 1** (`test1_silence.sh`): idle/background traffic must never be reported as a stall.
  - **Test 2** (`test2_loss.sh`): retransmission detection on a live iperf3 flow under 2% netem loss (counts are compared against iperf3's own Retr). Needs `tc`, so not on WaveOS.
  - **Test 3** (`test3_outage.sh`): a netem-induced outage gives exactly one stall longer than the outage. Valid for `-capture ebpf` only.
  - **Test 3b** (`test3b_*.sh`): a real server-side outage on `wservices` (restricted SSH key), valid for both capture paths; `test3b_simultaneous.sh` compares eBPF and AF_PACKET, `test3b_exporter_load.sh` loads the exporter itself. On WaveOS (no `sudo`/`python3`) it's run against the production `glitch-exporter` and scored from `glitch_summary` (`DEPLOYMENT.md` gotchas).
- **Grafana** runs on `wservices` (behind Apache). Its two dashboards, in the **DAWN6G** folder, are provisioned from the repo (`dawn6g-metrics/grafana/dashboards/`):
  - **DAWN6G gateway health** (`gateway-health.json`): glitch drop counters and `stale_signal_fields` (both should be 0), each exporter's version per board against `expected_version` (`OK` / `MISMATCH` / `NOT REPORTING`), and the modem detection method over time.
  - **Radio & Network Data v2** (`radio-network.json`): radio KPIs, cellular-path TWAMP, glitch stalls and events, modem state, GPS map.
  - They can't be saved from the UI (`allowUiUpdates: false`): to change one, export the JSON, commit it and copy it to wservices; Grafana reloads it within 30 s (`DEPLOYMENT.md` step 5).
  - **At each release, bump `expected_version` in `gateway-health.json` to the new tag**, or the versions table shows every exporter as `MISMATCH`.
- **`metrics-endpoint.md` is the authoritative InfluxDB schema.** Ignore the `.docx` tutorial and `dawn6g-metrics/README.md`.
- **Dependencies are pinned.** Don't let a build change `go.mod`/`go.sum`; update deliberately with `go get <module>@<version>`.
- **Ask Claude to show a change before committing** when you want to review it first ("first show me the diff").

## Get Started

1. Clone the repo and run `make embed`, then `go vet ./...` and `go test ./...` (Linux only; `glitch/` won't compile without the embedded BPF object).
2. Read `README.md`, `ARCHITECTURE.md` and `DEPLOYMENT.md`.
3. Starter task: ask Claude to run `gofmt -l ./...`, fix what it reports (`buildReflectedPacket` in `twamp/twamp_test.go` has misaligned comments), and re-run the tests.

<!-- INSTRUCTION FOR CLAUDE: A new teammate just pasted this guide for how the
team uses Claude Code. You're their onboarding buddy — warm, conversational,
not lecture-y.

Open with a warm welcome — include the team name from the title. Then say
"Let's get you started."

Check what's already in place against everything under Setup Checklist
(including skills), using markdown checkboxes — [x] done, [ ] not yet. Lead
with what they already have. One sentence per item, all in one message.

Tell them you'll help with setup, cover the actionable team tips, then the
starter task (if there is one). Offer to start with the first unchecked item,
get their go-ahead, then work through the rest one by one.

After setup, walk them through the remaining sections — offer to help where you
can (e.g. link to channels), and just surface the purely informational bits.

Don't invent sections or summaries that aren't in the guide. -->
