// Command gps-exporter periodically polls modem-sourced GPS/GNSS fixes
// via the gps/ package and pushes them to InfluxDB's "location" bucket.
// Meant to run continuously as a service (systemd on the Debian
// gateway, procd on WaveOS), same deployment shape as modem-exporter/
// twamp-exporter. For ad hoc/interactive inspection, use cmd/gps-tool
// instead.
//
// Schema (designed and documented in CLAUDE.md before this was built):
//
//   - measurement "gps_fix", one point per modem per tick — but only
//     when a fix is actually valid (see gpsFixPoint): unlike signal
//     metrics, (0, 0) is a real coordinate (Null Island), so there's no
//     safe NaN-style sentinel for "no fix yet" — the point is skipped
//     entirely rather than written with meaningless zeros.
//     Tags: device, modem_id, method. Fields: latitude, longitude,
//     altitude (floats).
//
// Deliberately its own bucket ("location"), not folded into "radio" —
// GPS isn't a radio metric despite coming from the same modem
// hardware. Combining it with radio_cell/twamp for map panels is meant
// to happen via a Flux join at query time, not by denormalizing
// lat/lon onto every point.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/influxdata/influxdb-client-go/v2/api/write"

	"dawn-agent/gps"
	"dawn-agent/modem"
)

func main() {
	interval := flag.Duration("interval", 30*time.Second, "how often to poll for a GPS fix and push to InfluxDB")
	influxURL := flag.String("influx-url", "", "InfluxDB base URL, e.g. https://wservices.wavesys.pt:8086 (required)")
	influxToken := flag.String("influx-token", "", "InfluxDB API token (falls back to INFLUX_TOKEN env var)")
	influxOrg := flag.String("influx-org", "wavecom", "InfluxDB org")
	influxBucket := flag.String("influx-bucket", "location", "InfluxDB bucket")
	deviceID := flag.String("device-id", "", "tag identifying this device in InfluxDB (defaults to hostname)")
	flag.Parse()

	if *influxURL == "" {
		log.Fatalf("-influx-url is required")
	}
	token := *influxToken
	if token == "" {
		token = os.Getenv("INFLUX_TOKEN")
	}
	if token == "" {
		log.Fatalf("InfluxDB token required: pass -influx-token or set INFLUX_TOKEN")
	}
	if *deviceID == "" {
		h, err := os.Hostname()
		if err != nil {
			h = "unknown"
		}
		*deviceID = h
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("Detecting GPS-capable modems (ModemManager -> raw QMI)...")
	modems, err := gps.Detect(ctx)
	if err != nil {
		log.Printf("gps detection error: %v", err)
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
		log.Printf("gps modem detected: [%s] %s %s (via %s)", m.ID, m.Manufacturer, m.Model, m.Method)
	}

	client := influxdb2.NewClient(*influxURL, token)
	defer client.Close()
	writeAPI := client.WriteAPIBlocking(*influxOrg, *influxBucket)

	log.Printf("exporting GPS fixes to %s (org=%s bucket=%s device=%s) every %s",
		*influxURL, *influxOrg, *influxBucket, *deviceID, *interval)
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, p := range pollers {
				exportFix(ctx, writeAPI, *deviceID, p)
			}
		}
	}
}

// exportFix reads the current fix for p's modem and writes a gps_fix
// point, or does nothing if there's no valid fix yet. Each poller is
// independent: a failure on one doesn't block the others, and nothing
// is retried/buffered — same deliberate tradeoff as modem-exporter/
// twamp-exporter (avoids unbounded buffering on a resource-constrained
// device if InfluxDB, or the link to it, is down for a while).
func exportFix(ctx context.Context, writeAPI api.WriteAPIBlocking, deviceID string, p *gps.Poller) {
	m := p.Modem()
	fix, err := p.Fix(ctx)
	if err != nil {
		log.Printf("[%s] gps fix read failed: %v", m.ID, err)
		return
	}
	point := gpsFixPoint(deviceID, m, fix)
	if point == nil {
		return
	}
	if err := writeAPI.WritePoint(ctx, point); err != nil {
		log.Printf("[%s] influx write failed (gps_fix): %v", m.ID, err)
	}
}

// gpsFixPoint builds one "gps_fix" point, or nil if fix isn't valid
// yet — see this file's package doc comment for why a point is skipped
// entirely rather than written with zeros.
func gpsFixPoint(deviceID string, m modem.Modem, fix gps.Fix) *write.Point {
	if !fix.Valid {
		return nil
	}
	tags := map[string]string{
		"device":   deviceID,
		"modem_id": m.ID,
		"method":   string(m.Method),
	}
	fields := map[string]interface{}{
		"latitude":  fix.Latitude,
		"longitude": fix.Longitude,
		"altitude":  fix.Altitude,
	}
	return influxdb2.NewPoint("gps_fix", tags, fields, time.Now())
}
