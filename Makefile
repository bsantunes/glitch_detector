# Makefile for 5G-Glitch-Monitor

BPF_SRC := bpf_src/tc_glitch.bpf.c
BPF_OBJ := tc_glitch.o
GO_BIN  := dawn-agent
GO_MOD  := go.mod

all: $(BPF_OBJ) $(GO_BIN)

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
	rm -f $(BPF_OBJ) $(GO_BIN)

.PHONY: all clean
