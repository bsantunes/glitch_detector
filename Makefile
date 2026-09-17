# Makefile for 5G-Glitch-Monitor

BPF_SRC := bpf_src/tc_glitch.bpf.c
BPF_OBJ := tc_glitch.o
GO_BIN  := dawn-agent
MODEM_BIN := modem-tool
GO_MOD  := go.mod
INSTALL_DIR := /opt/dawn

all: $(BPF_OBJ) $(GO_BIN)

# standalone modem detection/signal-quality CLI (modem/ package) — not
# part of the default build, since it's for developing modem support
# independently of the eBPF/TWAMP agent.
modem-tool: deps
	go build -o $(MODEM_BIN) ./cmd/modem-tool

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
	rm -f $(BPF_OBJ) $(GO_BIN) $(MODEM_BIN)

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

.PHONY: all clean install modem-tool
