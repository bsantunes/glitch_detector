// Command gps-tool is a standalone GPS/GNSS fix probe for modems that
// expose a location service — ModemManager's gps-raw source or raw
// QMI's LOC service (see gps.Supports). Detection phase first (via
// gps.Detect, deliberately independent of modem.Detect()'s own
// cascade — see that function's doc comment for why), then continuous
// polling, mirroring modem-tool/twamp-tool's shape. No InfluxDB, no
// eBPF.
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

	"dawn-agent/gps"
)

func main() {
	interval := flag.Duration("interval", 10*time.Second, "polling interval for GPS fixes")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Println("Detecting GPS-capable modems (ModemManager -> raw QMI)...")
	modems, err := gps.Detect(ctx)
	if err != nil {
		log.Fatalf("detection failed: %v", err)
	}
	if len(modems) == 0 {
		log.Fatalf("no modems found via ModemManager or raw QMI")
	}

	pollers := gps.NewPollers(ctx, modems)
	defer func() {
		for _, p := range pollers {
			p.Close()
		}
	}()
	if len(pollers) == 0 {
		log.Fatalf("no GPS-capable modems found (ModemManager or QMI backends only)")
	}
	for _, p := range pollers {
		m := p.Modem()
		fmt.Printf("  [%s] %s (via: %s) — polling GPS\n", m.ID, orUnknown(m.Model), m.Method)
	}

	fmt.Printf("\nPolling GPS fixes every %s (Ctrl+C to stop)...\n\n", *interval)
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, p := range pollers {
				id := p.Modem().ID
				fix, err := p.Fix(ctx)
				if err != nil {
					log.Printf("[%s] gps fix read failed: %v", id, err)
					continue
				}
				if !fix.Valid {
					fmt.Printf("[%s] no GPS fix yet\n", id)
					continue
				}
				fmt.Printf("[%s] lat=%.6f lon=%.6f alt=%.1fm\n", id, fix.Latitude, fix.Longitude, fix.Altitude)
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
