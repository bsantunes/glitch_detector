---
name: modem-detection
description: The hybrid modem-detection cascade (ModemManager → ubus → QMI/MBIM → uqmi → AT) in modem/, its Signal/CellInfo types, and known backend quirks. Load when working on modem/, cmd/modem-tool, or cmd/modem-exporter.
---

# `modem/` package

Hybrid detection cascade, each tried in order until one finds a modem: **ModemManager → `ubus` (WaveOS's own `qmi` object) → QMI (`qmicli`)/MBIM → `uqmi` → AT-over-serial**. `ubus` is tried before raw QMI/`uqmi` specifically to avoid QMI client-ID exhaustion when running alongside WaveOS's own `uqmi-ubus` daemon (see Known Issues in the root `CLAUDE.md`).

- `Signal{Technology, Role, RSRP, RSRQ, RSSI, SINR}` — `Role` is `"anchor"` or `"scg"`. `GetSignals()` returns one `Signal` per simultaneously active radio leg (EN-DC: LTE anchor + NR secondary, confirmed live on `aaeon`); `GetSignal()` is a convenience wrapper returning just the anchor leg. Only ModemManager and QMI (`qmicli`) can currently report >1 leg.
- `CellInfo{PLMN, OperatorName, RegistrationState, PacketServiceState, TAC, CellID}` — TAC/CellID normalized to **decimal** across backends, except the `ubus` backend, which currently passes Quectel's raw **hex** TAC/CellID straight through unconverted (known bug, not yet fixed — flagged but deferred).
- **A modem with no SIM can still report real RSRP/RSRQ/RSSI/SINR/PLMN/TAC/CellID** by camping on a cell's broadcast system information (physical-layer, unauthenticated) — it just can never complete registration/attach (no IMSI to authenticate with). Confirmed live on `6global`. Don't assume "no SIM" implies "no data."
- `SupportsCellInfo(method)` — only ModemManager/`ubus`/QMI/`uqmi` implement `GetCellInfo`; MBIM/AT don't yet (no real hardware to validate against).
