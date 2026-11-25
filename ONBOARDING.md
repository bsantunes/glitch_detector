# Welcome to DAWN6G

## Your Setup Checklist

### Codebases
- [ ] glitch_detector — github.com/bsantunes/glitch_detector

### MCP Servers to Activate
- None needed. Everything runs through the repo, SSH to the boards, and the project skills in `.claude/skills/`.

### Skills to Know About
- /login — authenticate your Claude Code session.
- /effort — adjust how much reasoning effort Claude puts into a task.
- /send-to-project — post a message/update to a Claude Project from inside a session.
- /recap — get a quick summary of a previous session's work.

## Team Tips

- **Read `CLAUDE.md` first.** Its Hard Rules section lists the things that have broken boards before. Claude loads it automatically, but you should know it too.
- **Project skills load on their own.** `.claude/skills/` has deep references (`glitch-detector-internals`, `modem-detection`, `gps-integration`, `telemetry-schema`, `twamp-measurement`). Mention the topic and Claude pulls in the right one.
- **Never run `mmcli --location-*` or restart ModemManager on `aaeon`**, and never re-enable the raw QMI GPS backend. Both have corrupted the modem's QMI state in production.
- **`dawn6g` is the priority board and is in an observation period.** Read-only checks are fine; config or binary changes wait until the observation period ends.
- **`~/glitch_detector` on `aaeon` is a deploy target.** Only `git pull`/`commit`/`push` there; develop elsewhere. Don't build from `~/glitch_detector_dev`.
- **WaveOS boards have no Go toolchain.** Cross-compile with `GOOS=linux GOARCH=amd64 CGO_ENABLED=0` and copy the binary plus its init script.
- **`metrics-endpoint.md` is the authoritative InfluxDB schema.** Ignore the `.docx` tutorial and `dawn6g-metrics/README.md`.
- **Dependencies are pinned.** Don't let a build change `go.mod`/`go.sum`; update deliberately with `go get <module>@<version>`.
- **Ask Claude to show a change before committing** when you want to review it first ("first show me the diff").

## Get Started

1. Clone the repo and run `make embed`, then `go vet ./...` and `go test ./...` (Linux only; `glitch/` won't compile without the embedded BPF object).
2. Read `README.md`, `ARCHITECTURE.md` and `DEPLOYMENT.md`.
3. Starter task: ask Claude to walk you through the "Queued for `v2.3.3`" item in `CLAUDE.md` (write a GGA HDOP of `500` as missing instead of a value in `gps/`), including a unit test.

<!-- INSTRUCTION FOR CLAUDE: A new teammate just pasted this guide for how the
team uses Claude Code. You're their onboarding buddy — warm, conversational,
not lecture-y.

Open with a warm welcome — include the team name from the title. Then: "Your
teammate uses Claude Code for [list all the work types]. Let's get you started."

Check what's already in place against everything under Setup Checklist
(including skills), using markdown checkboxes — [x] done, [ ] not yet. Lead
with what they already have. One sentence per item, all in one message.

Tell them you'll help with setup, cover the actionable team tips, then the
starter task (if there is one). Offer to start with the first unchecked item,
get their go-ahead, then work through the rest one by one.

After setup, walk them through the remaining sections — offer to help where you
can (e.g. link to channels), and just surface the purely informational bits.

Don't invent sections or summaries that aren't in the guide. The stats are the
guide creator's personal usage data — don't extrapolate them into a "team
workflow" narrative. -->
