// Command gps-tool is a standalone GPS/GNSS fix probe for modems that
// expose a location service — ModemManager's gps-raw source or raw
// QMI's LOC service (see gps.Supports). Detection phase first, then
// continuous polling, mirroring modem-tool/twamp-tool's shape. No
// InfluxDB, no eBPF.
//
// Deliberately does NOT reuse modem.Detect()'s full cascade: that
// cascade prioritizes WaveOS's ubus backend over raw QMI specifically
// to avoid QMI client-ID contention with the uqmi-ubus daemon, but
// ubus's "location" method only returns cell-ID/LAC-CI data, never real
// GPS coordinates — so blindly using Detect()'s pick would mean gps-tool
// finds nothing on any WaveOS board (confirmed live on 6global). GPS
// detection is its own concern: try ModemManager first (as the main
// cascade does), and if that finds nothing, independently probe raw QMI
// devices regardless of whether ubus would otherwise win.
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
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Println("Detecting GPS-capable modems (ModemManager -> raw QMI)...")
	modems, err := detectGPSCapable(ctx)
	if err != nil {
		log.Fatalf("detection failed: %v", err)
	}
	if len(modems) == 0 {
		log.Fatalf("no modems found via ModemManager or raw QMI")
	}

	type poller struct {
		m modem.Modem
		p *gps.Poller
	}
	var pollers []poller
	for _, m := range modems {
		if !gps.Supports(m.Method) {
			fmt.Printf("  [%s] %s (via: %s) — GPS not supported for this backend, skipping\n",
				m.ID, orUnknown(m.Model), m.Method)
			continue
		}
		p, err := gps.NewPoller(ctx, m)
		if err != nil {
			log.Printf("  [%s] gps.NewPoller failed: %v", m.ID, err)
			continue
		}
		fmt.Printf("  [%s] %s (via: %s) — polling GPS\n", m.ID, orUnknown(m.Model), m.Method)
		pollers = append(pollers, poller{m: m, p: p})
	}
	defer func() {
		for _, pl := range pollers {
			pl.p.Close()
		}
	}()

	if len(pollers) == 0 {
		log.Fatalf("no GPS-capable modems found (ModemManager or QMI backends only)")
	}

	fmt.Printf("\nPolling GPS fixes every %s (Ctrl+C to stop)...\n\n", *interval)
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, pl := range pollers {
				fix, err := pl.p.Fix(ctx)
				if err != nil {
					log.Printf("[%s] gps fix read failed: %v", pl.m.ID, err)
					continue
				}
				if !fix.Valid {
					fmt.Printf("[%s] no GPS fix yet\n", pl.m.ID)
					continue
				}
				fmt.Printf("[%s] lat=%.6f lon=%.6f alt=%.1fm\n", pl.m.ID, fix.Latitude, fix.Longitude, fix.Altitude)
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

// detectGPSCapable finds modems via the two backends gps.Supports
// actually implements — ModemManager first, then raw QMI if that finds
// nothing — independent of modem.Detect()'s own cascade/priority (see
// this file's package doc comment for why).
func detectGPSCapable(ctx context.Context) ([]modem.Modem, error) {
	modems, err := modem.DetectModemManager(ctx)
	if err != nil {
		log.Printf("gps-tool: ModemManager detection unavailable: %v", err)
	} else if len(modems) > 0 {
		return modems, nil
	}

	modems, err = modem.DetectQMIMBIM(ctx)
	if err != nil {
		log.Printf("gps-tool: QMI/MBIM detection unavailable: %v", err)
	}
	// MBIM-detected modems will simply be skipped later by gps.Supports —
	// only QMI-method ones from this call are usable.
	return modems, nil
}
