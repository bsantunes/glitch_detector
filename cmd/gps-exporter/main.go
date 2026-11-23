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
//   - Additive, NMEA-reader-only (see gps/nmea.go and -nmea-port; a
//     fix.Source other than "nmea" writes none of these): tag source
//     ("nmea"); fields speed_knots, course_deg, hdop (floats,
//     NaN-omitted, same convention as modem-exporter's radio_cell) and
//     satellites (int).
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
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/influxdata/influxdb-client-go/v2/api/write"

	"dawn-agent/gps"
	"dawn-agent/modem"
	"dawn-agent/version"
)

func main() {
	interval := flag.Duration("interval", 30*time.Second, "how often to poll for a GPS fix and push to InfluxDB")
	method := flag.String("method", "auto", "detection method: auto (default), mm (ModemManager only), at (AT-over-serial only)")
	port := flag.String("port", "", "pin AT-over-serial to this exact device (e.g. /dev/dawn-gps, or usb:<vid>:<pid>:<ifnum> e.g. usb:2c7c:0801:03 to survive ttyUSB numbers shifting across a reboot); requires -method=at")
	nmeaPort := flag.String("nmea-port", "", "read fixes from this NMEA port instead of AT+CGPSINFO/AT+QGPSLOC=2 (same forms as -port; typically the modem's interface 01) — AT still starts the engine and assistance data; requires -method=at")
	agps := flag.Bool("agps", true, "send the XTRA assistance-data enable command on AT-over-serial backends before starting the engine; the setting itself is stored in the modem and persists across reboots, so -agps=false only skips sending the command, it does not turn off assistance data already enabled")
	influxURL := flag.String("influx-url", "", "InfluxDB base URL, e.g. https://wservices.wavesys.pt:8086 (required)")
	influxToken := flag.String("influx-token", "", "InfluxDB API token (falls back to INFLUX_TOKEN env var)")
	influxOrg := flag.String("influx-org", "wavecom", "InfluxDB org")
	influxBucket := flag.String("influx-bucket", "location", "InfluxDB bucket")
	deviceID := flag.String("device-id", "", "tag identifying this device in InfluxDB (defaults to hostname)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Version)
		return
	}

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
	if *nmeaPort != "" && *method != "at" {
		log.Fatalf("-nmea-port requires -method=at (got method %q)", *method)
	}
	if *port != "" {
		resolved, err := modem.ResolvePort(*port)
		if err != nil {
			log.Fatalf("-port: %v", err)
		}
		*port = resolved
	}
	if *nmeaPort != "" {
		resolved, err := modem.ResolvePort(*nmeaPort)
		if err != nil {
			log.Fatalf("-nmea-port: %v", err)
		}
		*nmeaPort = resolved
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("Detecting GPS-capable modems (%s)...", gps.DescribeMethod(*method, *port))
	modems, err := gps.DetectWithMethod(ctx, *method, *port)
	if err != nil {
		log.Printf("gps detection error: %v", err)
	}
	if len(modems) == 0 {
		log.Fatalf("no modems found via %s", gps.DescribeMethod(*method, *port))
	}

	pollers := gps.NewPollers(ctx, modems, *agps, *nmeaPort)
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
		log.Printf("gps modem detected: [%s] %s %s (via %s)", m.ID, m.Manufacturer, m.Model, m.Method)
	}

	client := influxdb2.NewClient(*influxURL, token)
	defer client.Close()
	writeAPI := client.WriteAPIBlocking(*influxOrg, *influxBucket)

	log.Printf("gps-exporter %s", version.Version)
	writeAgentInfo(ctx, writeAPI, *deviceID, "gps-exporter")

	log.Printf("exporting GPS fixes to %s (org=%s bucket=%s device=%s) every %s",
		*influxURL, *influxOrg, *influxBucket, *deviceID, *interval)
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	agentInfoTicker := time.NewTicker(time.Hour)
	defer agentInfoTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, p := range pollers {
				exportFix(ctx, writeAPI, *deviceID, p)
			}
		case <-agentInfoTicker.C:
			writeAgentInfo(ctx, writeAPI, *deviceID, "gps-exporter")
		}
	}
}

// writeAgentInfo pushes one "agent_info" point — tags device, component;
// field version (string, from version.Version) — so "what version is
// this board running" is answerable from InfluxDB directly, per device
// and per binary. Logged and dropped on failure, not retried, same
// deliberate tradeoff as every other write in this file.
func writeAgentInfo(ctx context.Context, writeAPI api.WriteAPIBlocking, deviceID, component string) {
	point := influxdb2.NewPoint("agent_info",
		map[string]string{"device": deviceID, "component": component},
		map[string]interface{}{"version": version.Version},
		time.Now())
	if err := writeAPI.WritePoint(ctx, point); err != nil {
		log.Printf("influx write failed (agent_info): %v", err)
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
	// Additive, NMEA-reader-only fields — see this file's package doc
	// comment. AT/ModemManager/QMI fixes leave fix.Source empty, so
	// none of this is ever written for them.
	if fix.Source == "nmea" {
		tags["source"] = fix.Source
		if !math.IsNaN(fix.SpeedKnots) {
			fields["speed_knots"] = fix.SpeedKnots
		}
		if !math.IsNaN(fix.CourseDeg) {
			fields["course_deg"] = fix.CourseDeg
		}
		if !math.IsNaN(fix.HDOP) {
			fields["hdop"] = fix.HDOP
		}
		fields["satellites"] = fix.Satellites
	}
	return influxdb2.NewPoint("gps_fix", tags, fields, time.Now())
}
