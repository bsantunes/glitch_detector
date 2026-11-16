// Command glitch-tool is a standalone passive eBPF stall/retransmission
// monitor built on the glitch/ package — the same printed event shape
// as dawn-agent's own passive loop, but no TWAMP/modem active-
// monitoring side and no InfluxDB. Requires root to load the eBPF
// program and attach the clsact qdisc/filter, same as dawn-agent.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"dawn-agent/glitch"
)

func main() {
	iface := flag.String("iface", "wwan0", "network interface to attach to")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	h, err := glitch.Open(*iface)
	if err != nil {
		log.Fatalf("glitch.Open(%s): %v", *iface, err)
	}
	defer h.Close()

	fmt.Printf("🚀 glitch-tool watching %s (Ctrl+C to stop)\n", *iface)

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-h.Events():
			if !ok {
				return
			}
			if ev.Type == glitch.KindRetrans {
				fmt.Printf("🔴 [KERNEL] TCP Loss Detected | Dest: %s\n", ev.DstIP)
			} else {
				fmt.Printf("⚠️  [KERNEL] Stall: %.1fms | Dest: %s\n", float64(ev.Value)/1e6, ev.DstIP)
			}
		}
	}
}
