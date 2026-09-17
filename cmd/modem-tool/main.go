// Command modem-tool is a standalone detection + signal-quality probe
// for cellular modems, independent of dawn-agent's eBPF/TWAMP pipeline.
// It exists to develop and test modem/ against real hardware (different
// vendors/models) without needing root, a clsact qdisc, or a TWAMP
// reflector. Detection phase first, then continuous polling.
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

	"dawn-agent/modem"
)

func main() {
	interval := flag.Duration("interval", 5*time.Second, "polling interval for signal readings")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Println("Detecting modems (ModemManager -> QMI/MBIM -> AT)...")
	modems, err := modem.Detect(ctx)
	if err != nil {
		log.Fatalf("detection failed: %v", err)
	}
	if len(modems) == 0 {
		log.Fatalf("no modems found via ModemManager, QMI/MBIM, or AT")
	}

	fmt.Printf("Found %d modem(s):\n", len(modems))
	for _, m := range modems {
		fmt.Printf("  [%s] %s %s (port: %s, via: %s)\n",
			m.ID, orUnknown(m.Manufacturer), orUnknown(m.Model), orUnknown(m.Port), m.Method)
	}

	fmt.Printf("\nPolling signal quality every %s (Ctrl+C to stop)...\n\n", *interval)
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, m := range modems {
				sig, err := modem.GetSignal(ctx, m)
				if err != nil {
					log.Printf("[%s] signal read failed: %v", m.ID, err)
					continue
				}
				fmt.Printf("[%s] tech=%-4s RSRP=%s RSRQ=%s RSSI=%s SINR=%s\n",
					m.ID, orUnknown(sig.Technology),
					modem.FormatValue(sig.RSRP), modem.FormatValue(sig.RSRQ),
					modem.FormatValue(sig.RSSI), modem.FormatValue(sig.SINR))
			}
		}
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
