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
	// Logic lives in run() so its deferred h.Close() always executes on
	// any return path, including an error one — os.Exit (which
	// log.Fatalf calls internally) skips deferred calls entirely, which
	// would leave the filter attached. main only decides the process
	// exit code after run has fully returned.
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	iface := flag.String("iface", "wwan0", "network interface to attach to")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	h, err := glitch.Open(*iface)
	if err != nil {
		return fmt.Errorf("glitch.Open(%s): %w", *iface, err)
	}
	defer h.Close()

	fmt.Printf("🚀 glitch-tool watching %s (Ctrl+C to stop)\n", *iface)

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-h.Events():
			if !ok {
				return h.Err()
			}
			if ev.Type == glitch.KindRetrans {
				fmt.Printf("🔴 [KERNEL] TCP Loss Detected | Dest: %s\n", ev.DstIP)
			} else {
				fmt.Printf("⚠️  [KERNEL] Stall: %.1fms | Dest: %s\n", float64(ev.Value)/1e6, ev.DstIP)
			}
		}
	}
}
