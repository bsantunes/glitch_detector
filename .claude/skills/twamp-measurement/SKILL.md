---
name: twamp-measurement
description: The native Go TWAMP-Light sender in twamp/ and the wservices.wavesys.pt reflector it talks to — packet-size quirks and reflector operations. Load when working on twamp/, cmd/twamp-tool, cmd/twamp-exporter, or the TWAMP reflector service.
---

# `twamp/` package

Native Go sender for "TWAMP Light" (RFC 5357-style, unauthenticated, no TWAMP-Control handshake — direct UDP TWAMP-Test packets). `Measure(ctx, reflectorAddr, bindDev, count, probeInterval, timeout) (Result, error)` sends a burst of packets and computes `Result{Sent, Received, AvgRTT, Jitter, LossPct}`. `bindDev` (`-bind-dev` on `twamp-exporter`/`twamp-tool`, since `v2.3.4`) pins the socket to an interface with SO_BINDTODEVICE (`twamp/bind_linux.go`); a missing interface fails the dial rather than falling back to normal routing. `twamp-exporter` adds an `iface` tag only when it's set.

**On the WaveOS boards, use `-bind-dev lte1 -reflector 51.91.57.3:862`.** Traffic to wservices goes over the wired `br-wan` there (WireGuard's endpoint host route), so probing `10.30.30.1` through `wg0` measures the wired link, not cellular — all `twamp` data before 2026-11-25 21:20:29 UTC (`6global`) / 23:27:09 UTC (`dawn6g`) did exactly that (see CLAUDE.md). The reflector listens on `0.0.0.0:862`, and 862/udp has an explicit ufw rule on wservices, so the public address works the same.

**Packet sizes deliberately diverge from RFC 5357's stated minimums** (`senderPacketLen = 14`, `reflectedHeaderLen = 38`, not the RFC's 41/41) — found via live `strace -f -e trace=network` on `twampy` itself, which showed the real deployed reflector (`10.30.30.1:862`) expects/sends packets smaller than the RFC minimums. This is intentional: it matches the actual wire behavior of this project's reflector, not the spec. If TWAMP is ever pointed at a different reflector and gets 100% loss, check packet sizes via `strace` again before assuming the sizes are wrong.

Confirmed live with 0% loss against `10.30.30.1:862` from `aaeon`, `6global`, and `dawn6g`.

**The reflector itself** (`10.30.30.1:862`) is `wservices.wavesys.pt` — the same box that hosts the InfluxDB/Grafana stack (`dawn6g-metrics/`), just reached over the private `10.30.30.x` VPN overlay instead of its public domain. It runs `twampy` (a third-party Python TWAMP implementation, **not** this repo's own `twamp/`/`twamp-tool`, which are the client/sender side only) in `responder` mode, installed in a venv at `/home/idwave/twampy/venv`. **Run it as `twampy-responder.service`** (tracked in this repo, reference copy — deploy by copying to `/etc/systemd/system/` on `wservices` and `systemctl enable --now twampy-responder`), not by hand in a terminal — a manually-backgrounded (`sudo ... &`) run is fragile and **has already gone silently stopped (`T` state) once** when its controlling SSH session ended, with no indication other than every gateway's TWAMP reads silently starting to show 100% loss. The systemd service survives reboots/updates and auto-restarts on crash.
