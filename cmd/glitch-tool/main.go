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
	"time"

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

	// Printed periodically, not just on a clean exit: a SIGKILL skips
	// every deferred/exit-path print, so this is the only way to see
	// whether the 256-slot Events() channel is backpressuring (events
	// silently dropped because nothing drained it fast enough) rather
	// than having to trust a final count that might never get printed.
	dropTicker := time.NewTicker(5 * time.Second)
	defer dropTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Printf("📊 dropped=%d (final)\n", h.Dropped())
			return nil
		case <-dropTicker.C:
			fmt.Printf("📊 dropped=%d\n", h.Dropped())
		case ev, ok := <-h.Events():
			if !ok {
				return h.Err()
			}
			switch ev.Type {
			case glitch.KindRetrans:
				fmt.Printf("🔴 [KERNEL] TCP Loss Detected | %s %s:%d -> %s:%d\n", protoName(ev.Proto), ev.SrcIP, ev.SrcPort, ev.DstIP, ev.DstPort)
			case glitch.KindStallStart:
				fmt.Printf("🟡 [KERNEL] Stall START: gap %.1fms (baseline %.1fms) | %s %s:%d -> %s:%d\n", float64(ev.Value)/1e6, ev.Baseline.Seconds()*1000, protoName(ev.Proto), ev.SrcIP, ev.SrcPort, ev.DstIP, ev.DstPort)
			case glitch.KindStallEnd:
				fmt.Printf("✅ [KERNEL] Stall END: duration %.1fms, steps=%d (baseline %.1fms) | %s %s:%d -> %s:%d\n", float64(ev.Value)/1e6, ev.Steps, ev.Baseline.Seconds()*1000, protoName(ev.Proto), ev.SrcIP, ev.SrcPort, ev.DstIP, ev.DstPort)
			}
		}
	}
}

func protoName(proto uint8) string {
	switch proto {
	case 6:
		return "TCP"
	case 17:
		return "UDP"
	default:
		return fmt.Sprintf("proto(%d)", proto)
	}
}
