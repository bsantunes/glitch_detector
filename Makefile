# Makefile for 5G-Glitch-Monitor

BPF_SRC := bpf_src/tc_glitch.bpf.c
BPF_OBJ := tc_glitch.o
GO_BIN  := dawn-agent

all: $(BPF_OBJ) $(GO_BIN)

$(BPF_OBJ): $(BPF_SRC)
	clang -O2 -g -target bpf \
	    -I/usr/include/$(shell uname -m)-linux-gnu \
	    -c $< -o $@

$(GO_BIN):
	go build -o $(GO_BIN)

clean:
	rm -f $(BPF_OBJ) $(GO_BIN)

.PHONY: all clean
