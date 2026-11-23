# Makefile for 5G-Glitch-Monitor

BPF_SRC := bpf_src/tc_glitch.bpf.c
BPF_OBJ := tc_glitch.o
GO_BIN  := dawn-agent
MODEM_BIN := modem-tool
EXPORTER_BIN := modem-exporter
TWAMP_BIN := twamp-tool
TWAMP_EXPORTER_BIN := twamp-exporter
GPS_BIN := gps-tool
GPS_EXPORTER_BIN := gps-exporter
GLITCH_BIN := glitch-tool
GLITCH_EXPORTER_BIN := glitch-exporter
INSTALL_DIR := /opt/dawn

# Build-time version stamp for every cmd/ binary (see version/version.go)
# — "what commit is this board actually running" shouldn't depend on
# remembering which directory someone built from (see the on-device
# dev-checkout note below). --dirty flags a build from uncommitted
# changes; falls back to "dev" outside a git checkout (e.g. a release
# tarball with no .git, not something this project currently ships, but
# cheap insurance against a hard failure if it ever does).
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X dawn-agent/version.Version=$(VERSION)"

# dawn-agent is superseded by glitch-exporter + twamp-exporter +
# modem-exporter (see README) - not built or installed by default
# anymore. $(BPF_OBJ) stays a default-build dependency since glitch-tool
# and glitch-exporter both need it too (see glitch/tc_glitch.o below).
# Use `make dawn-agent` / `make install-dawn-agent` explicitly if you
# still need the old eBPF-only agent.
all: $(BPF_OBJ)

# standalone modem detection/signal-quality CLI (modem/ package) — not
# part of the default build, since it's for developing modem support
# independently of the eBPF/TWAMP agent.
modem-tool:
	go build $(LDFLAGS) -o $(MODEM_BIN) ./cmd/modem-tool

# long-running service that polls modem/ and pushes signal+cell info to
# InfluxDB — meant to run continuously (systemd/procd), not interactively.
modem-exporter:
	go build $(LDFLAGS) -o $(EXPORTER_BIN) ./cmd/modem-exporter

# standalone native-Go TWAMP-Test prober (twamp/ package) — no external
# twampy/Python dependency, so it also works on OpenWrt/WaveOS.
twamp-tool:
	go build $(LDFLAGS) -o $(TWAMP_BIN) ./cmd/twamp-tool

# long-running service that probes twamp/ and pushes RTT/jitter/loss to
# InfluxDB's "network" bucket — meant to run continuously (systemd/procd).
twamp-exporter:
	go build $(LDFLAGS) -o $(TWAMP_EXPORTER_BIN) ./cmd/twamp-exporter

# standalone GPS/GNSS fix prober (gps/ package) — ModemManager/raw-QMI
# backends only so far, see gps.Supports.
gps-tool:
	go build $(LDFLAGS) -o $(GPS_BIN) ./cmd/gps-tool

# long-running service that polls gps/ and pushes GPS fixes to
# InfluxDB's "location" bucket — meant to run continuously (systemd/procd).
gps-exporter:
	go build $(LDFLAGS) -o $(GPS_EXPORTER_BIN) ./cmd/gps-exporter

# copy for go:embed — the glitch/ package embeds tc_glitch.o from its
# own directory (go:embed can't reach ../), so this mirrors the
# repo-root object built above rather than compiling it twice.
glitch/tc_glitch.o: $(BPF_OBJ)
	cp $(BPF_OBJ) glitch/tc_glitch.o

# convenience alias: run this once on a fresh checkout before using
# `go vet ./...`/`go test ./...` directly (bypassing make) — go:embed
# makes glitch/tc_glitch.o a compile-time dependency for the whole
# module, not just a dawn-agent runtime one.
embed: glitch/tc_glitch.o

# standalone passive eBPF stall/retransmission monitor (glitch/
# package) — same event stream as dawn-agent's passive loop, no
# TWAMP/modem side, no InfluxDB.
glitch-tool: glitch/tc_glitch.o
	go build $(LDFLAGS) -o $(GLITCH_BIN) ./cmd/glitch-tool

# long-running service that runs the eBPF passive monitor and pushes
# aggregated stall-episode/retransmission stats to InfluxDB's "network"
# bucket — meant to run continuously (systemd; no OpenWrt procd init
# script, since the eBPF/TC side doesn't work on WaveOS at all).
glitch-exporter: glitch/tc_glitch.o
	go build $(LDFLAGS) -o $(GLITCH_EXPORTER_BIN) ./cmd/glitch-exporter

$(GO_BIN):
	go build -o $(GO_BIN)

$(BPF_OBJ): $(BPF_SRC)
	clang -O2 -g -Wall -target bpf \
	    -I/usr/include/$(shell uname -m)-linux-gnu \
	    -c $< -o $@


clean:
	rm -f $(BPF_OBJ) $(GO_BIN) $(MODEM_BIN) $(EXPORTER_BIN) $(TWAMP_BIN) $(TWAMP_EXPORTER_BIN) $(GPS_BIN) $(GPS_EXPORTER_BIN) $(GLITCH_BIN) $(GLITCH_EXPORTER_BIN) glitch/tc_glitch.o

# install the agent, BPF object and init script into $(INSTALL_DIR)
# (creates the directory if needed) and register the systemd unit.
# dawn-agent is superseded (see README) - this is no longer the plain
# `install` target, so it's never pulled in by accident.
# Run with sudo: sudo make install-dawn-agent
install-dawn-agent: $(BPF_OBJ) $(GO_BIN)
	install -d $(INSTALL_DIR)
	install -m 755 $(GO_BIN) init_gateway.sh $(INSTALL_DIR)/
	install -m 644 $(BPF_OBJ) $(INSTALL_DIR)/
	install -m 644 dawn-agent.service /etc/systemd/system/
	systemctl daemon-reload
	@echo "✅ Installed to $(INSTALL_DIR). Enable with: sudo systemctl enable --now dawn-agent"

# install modem-tool as a continuous background logging service (no
# InfluxDB involved — just journalctl). Run with sudo: sudo make install-modem-tool
install-modem-tool: modem-tool
	install -d $(INSTALL_DIR)
	install -m 755 $(MODEM_BIN) $(INSTALL_DIR)/
	install -m 644 modem-tool.service /etc/systemd/system/
	systemctl daemon-reload
	@echo "✅ Installed to $(INSTALL_DIR). Enable with: sudo systemctl enable --now modem-tool"
	@echo "   Logs: journalctl -u modem-tool -f"

# install modem-exporter, which pushes to InfluxDB. Requires
# /opt/dawn/modem-exporter.env with a real INFLUX_TOKEN — see
# modem-exporter.env.example. Run with sudo: sudo make install-modem-exporter
install-modem-exporter: modem-exporter
	install -d $(INSTALL_DIR)
	install -m 755 $(EXPORTER_BIN) $(INSTALL_DIR)/
	install -m 644 modem-exporter.service /etc/systemd/system/
	systemctl daemon-reload
	@echo "✅ Installed to $(INSTALL_DIR)."
	@echo "   Now: cp modem-exporter.env.example $(INSTALL_DIR)/modem-exporter.env"
	@echo "        edit it with the real INFLUX_TOKEN, chmod 600, chown root:root"
	@echo "   Then enable with: sudo systemctl enable --now modem-exporter"
	@echo "   Logs: journalctl -u modem-exporter -f"

# install twamp-tool as a continuous background logging service (no
# InfluxDB involved — just journalctl). Run with sudo: sudo make install-twamp-tool
install-twamp-tool: twamp-tool
	install -d $(INSTALL_DIR)
	install -m 755 $(TWAMP_BIN) $(INSTALL_DIR)/
	install -m 644 twamp-tool.service /etc/systemd/system/
	systemctl daemon-reload
	@echo "✅ Installed to $(INSTALL_DIR). Enable with: sudo systemctl enable --now twamp-tool"
	@echo "   Logs: journalctl -u twamp-tool -f"

# install twamp-exporter, which pushes to InfluxDB. Reuses the same
# /opt/dawn/modem-exporter.env token as modem-exporter (add-device.sh
# tokens are scoped to write across all three buckets already).
# Run with sudo: sudo make install-twamp-exporter
install-twamp-exporter: twamp-exporter
	install -d $(INSTALL_DIR)
	install -m 755 $(TWAMP_EXPORTER_BIN) $(INSTALL_DIR)/
	install -m 644 twamp-exporter.service /etc/systemd/system/
	systemctl daemon-reload
	@echo "✅ Installed to $(INSTALL_DIR)."
	@echo "   Reuses /opt/dawn/modem-exporter.env for INFLUX_TOKEN (see install-modem-exporter)."
	@echo "   Then enable with: sudo systemctl enable --now twamp-exporter"
	@echo "   Logs: journalctl -u twamp-exporter -f"

# install gps-tool as a continuous background logging service (no
# InfluxDB involved — just journalctl). Run with sudo: sudo make install-gps-tool
install-gps-tool: gps-tool
	install -d $(INSTALL_DIR)
	install -m 755 $(GPS_BIN) $(INSTALL_DIR)/
	install -m 644 gps-tool.service /etc/systemd/system/
	systemctl daemon-reload
	@echo "✅ Installed to $(INSTALL_DIR). Enable with: sudo systemctl enable --now gps-tool"
	@echo "   Logs: journalctl -u gps-tool -f"

# install gps-exporter, which pushes to InfluxDB. Reuses the same
# /opt/dawn/modem-exporter.env token as modem-exporter/twamp-exporter
# (add-device.sh tokens are scoped to write across all four buckets).
# Run with sudo: sudo make install-gps-exporter
install-gps-exporter: gps-exporter
	install -d $(INSTALL_DIR)
	install -m 755 $(GPS_EXPORTER_BIN) $(INSTALL_DIR)/
	install -m 644 gps-exporter.service /etc/systemd/system/
	systemctl daemon-reload
	@echo "✅ Installed to $(INSTALL_DIR)."
	@echo "   Reuses /opt/dawn/modem-exporter.env for INFLUX_TOKEN (see install-modem-exporter)."
	@echo "   Then enable with: sudo systemctl enable --now gps-exporter"
	@echo "   Logs: journalctl -u gps-exporter -f"

# install glitch-exporter, which pushes to InfluxDB. Reuses the same
# /opt/dawn/modem-exporter.env token as modem-exporter/twamp-exporter/
# gps-exporter (add-device.sh tokens are scoped to write across all four
# buckets). Conflicts with dawn-agent.service (glitch-exporter.service),
# since both would otherwise try to attach their own eBPF program to the
# same interface. Run with sudo: sudo make install-glitch-exporter
install-glitch-exporter: glitch-exporter
	install -d $(INSTALL_DIR)
	install -m 755 $(GLITCH_EXPORTER_BIN) $(INSTALL_DIR)/
	install -m 644 glitch-exporter.service /etc/systemd/system/
	systemctl daemon-reload
	@echo "✅ Installed to $(INSTALL_DIR)."
	@echo "   Reuses /opt/dawn/modem-exporter.env for INFLUX_TOKEN (see install-modem-exporter)."
	@echo "   Then enable with: sudo systemctl enable --now glitch-exporter"
	@echo "   Logs: journalctl -u glitch-exporter -f"

.PHONY: all clean dawn-agent install-dawn-agent modem-tool modem-exporter twamp-tool twamp-exporter gps-tool gps-exporter glitch-tool glitch-exporter embed install-modem-tool install-modem-exporter install-twamp-tool install-twamp-exporter install-gps-tool install-gps-exporter install-glitch-exporter
