// Command gps-tool is a standalone GPS/GNSS fix probe for modems that
// expose a location service — ModemManager's gps-raw source or
// AT-over-serial (see gps.Supports; raw QMI is implemented but
// deliberately disabled). Detection phase first (via
// gps.DetectWithMethod — auto by default, or forced via -method/-port
// — deliberately independent of modem.Detect()'s own cascade, see
// gps.Detect's doc comment for why), then continuous polling,
// mirroring modem-tool/twamp-tool's shape. No InfluxDB, no eBPF.
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
	"dawn-agent/modem"
)

func main() {
	interval := flag.Duration("interval", 10*time.Second, "polling interval for GPS fixes")
	method := flag.String("method", "auto", "detection method: auto (default), mm (ModemManager only), at (AT-over-serial only)")
	port := flag.String("port", "", "pin AT-over-serial to this exact device (e.g. /dev/ttyUSB3, or usb:<vid>:<pid>:<ifnum> e.g. usb:2c7c:0801:03 to survive ttyUSB numbers shifting across a reboot); requires -method=at")
	agps := flag.Bool("agps", true, "send the XTRA assistance-data enable command on AT-over-serial backends before starting the engine; the setting itself is stored in the modem and persists across reboots, so -agps=false only skips sending the command, it does not turn off assistance data already enabled")
	flag.Parse()

	if *port != "" {
		resolved, err := modem.ResolvePort(*port)
		if err != nil {
			log.Fatalf("-port: %v", err)
		}
		*port = resolved
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Printf("Detecting GPS-capable modems (%s)...\n", gps.DescribeMethod(*method, *port))
	modems, err := gps.DetectWithMethod(ctx, *method, *port)
	if err != nil {
		log.Fatalf("detection failed: %v", err)
	}
	if len(modems) == 0 {
		log.Fatalf("no modems found via %s", gps.DescribeMethod(*method, *port))
	}

	pollers := gps.NewPollers(ctx, modems, *agps)
	defer func() {
		for _, p := range pollers {
			p.Close()
		}
	}()
	if len(pollers) == 0 {
		log.Fatalf("no GPS-capable modems found (ModemManager or AT-over-serial only — raw QMI is implemented but deliberately disabled)")
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
