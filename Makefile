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
GO_MOD  := go.mod
INSTALL_DIR := /opt/dawn

all: $(BPF_OBJ) $(GO_BIN)

# standalone modem detection/signal-quality CLI (modem/ package) — not
# part of the default build, since it's for developing modem support
# independently of the eBPF/TWAMP agent.
modem-tool: deps
	go build -o $(MODEM_BIN) ./cmd/modem-tool

# long-running service that polls modem/ and pushes signal+cell info to
# InfluxDB — meant to run continuously (systemd/procd), not interactively.
modem-exporter: deps
	go build -o $(EXPORTER_BIN) ./cmd/modem-exporter

# standalone native-Go TWAMP-Test prober (twamp/ package) — no external
# twampy/Python dependency, so it also works on OpenWrt/WaveOS.
twamp-tool: deps
	go build -o $(TWAMP_BIN) ./cmd/twamp-tool

# long-running service that probes twamp/ and pushes RTT/jitter/loss to
# InfluxDB's "network" bucket — meant to run continuously (systemd/procd).
twamp-exporter: deps
	go build -o $(TWAMP_EXPORTER_BIN) ./cmd/twamp-exporter

# standalone GPS/GNSS fix prober (gps/ package) — ModemManager/raw-QMI
# backends only so far, see gps.Supports.
gps-tool: deps
	go build -o $(GPS_BIN) ./cmd/gps-tool

# long-running service that polls gps/ and pushes GPS fixes to
# InfluxDB's "location" bucket — meant to run continuously (systemd/procd).
gps-exporter: deps
	go build -o $(GPS_EXPORTER_BIN) ./cmd/gps-exporter

# ensure go.mod exists and dependencies are fetched
$(GO_MOD):
	@echo "initializing go module and fetching dependencies"
	go mod init dawn-agent
	go get github.com/cilium/ebpf
	go get github.com/vishvananda/netlink
	go get github.com/influxdata/influxdb-client-go/v2

deps: $(GO_MOD)
	go mod tidy

$(GO_BIN): deps
	go build -o $(GO_BIN)

$(BPF_OBJ): $(BPF_SRC)
	clang -O2 -g -target bpf \
	    -I/usr/include/$(shell uname -m)-linux-gnu \
	    -c $< -o $@


clean:
	rm -f $(BPF_OBJ) $(GO_BIN) $(MODEM_BIN) $(EXPORTER_BIN) $(TWAMP_BIN) $(TWAMP_EXPORTER_BIN) $(GPS_BIN) $(GPS_EXPORTER_BIN)

# install the agent, BPF object and init script into $(INSTALL_DIR)
# (creates the directory if needed) and register the systemd unit.
# Run with sudo: sudo make install
install: all
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

.PHONY: all clean install modem-tool modem-exporter twamp-tool twamp-exporter gps-tool gps-exporter install-modem-tool install-modem-exporter install-twamp-tool install-twamp-exporter install-gps-tool install-gps-exporter
