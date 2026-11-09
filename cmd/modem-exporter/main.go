// Command modem-exporter periodically detects modem(s) via the modem/
// package and pushes signal quality + cell info to InfluxDB, for
// visualization in Grafana. Meant to run continuously as a service
// (systemd on the Debian gateway, procd on WaveOS), pushing out rather
// than waiting to be polled — these are cellular gateways, typically
// behind CGNAT, so outbound push avoids needing inbound reachability
// to the device. For ad hoc/interactive inspection, use cmd/modem-tool
// instead; this program is deliberately not for humans to read.
//
// Schema (shared across every gateway so one dashboard covers all of
// them):
//
//   - measurement "radio_cell", one point per active radio leg (usually
//     one; two under EN-DC — an LTE "anchor" plus an NR "scg" leg).
//     Tags: device, modem_id, method, rat ("lte"/"nr"/"umts"/"gsm"/
//     "unknown"), role ("anchor"/"scg"), and — anchor rows only — plmn/
//     tac. Fields: rsrp, rsrq, rssi, sinr (float, omitted when NaN) and
//     — anchor rows only — cell_id (string; changes with mobility, so
//     it's a field, not a tag).
//   - measurement "modem_state", one point per modem per tick.
//     Tags: device, modem_id. Fields: operator_name,
//     registration_state, packet_service_state (all strings, omitted
//     when empty).
//
// Note: PCI isn't sent yet — no backend has a verified source for it
// (ModemManager exposes no cell/PCI command at all; qmicli's
// --nas-get-cell-location-info is untested due to ModemManager holding
// its own QMI client open; uqmi only has it inside a malformed-JSON
// structure the modem/ package deliberately doesn't parse). Add it once
// one of those is confirmed working.
package main

import (
	"context"
	"flag"
	"log"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/influxdata/influxdb-client-go/v2/api/write"

	"dawn-agent/modem"
)

func main() {
	interval := flag.Duration("interval", 30*time.Second, "how often to poll modems and push to InfluxDB")
	influxURL := flag.String("influx-url", "", "InfluxDB base URL, e.g. http://10.30.30.1:8086 (required)")
	influxToken := flag.String("influx-token", "", "InfluxDB API token (falls back to INFLUX_TOKEN env var)")
	influxOrg := flag.String("influx-org", "wavecom", "InfluxDB org")
	influxBucket := flag.String("influx-bucket", "radio", "InfluxDB bucket")
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

	client := influxdb2.NewClient(*influxURL, token)
	defer client.Close()
	writeAPI := client.WriteAPIBlocking(*influxOrg, *influxBucket)

	log.Printf("Detecting modems (ModemManager -> QMI/MBIM -> uqmi -> AT)...")
	modems, err := modem.Detect(ctx)
	if err != nil {
		log.Printf("modem detection error: %v", err)
	}
	if len(modems) == 0 {
		log.Fatalf("no modems found via ModemManager, QMI/MBIM, uqmi, or AT")
	}
	for _, m := range modems {
		log.Printf("modem detected: [%s] %s %s (via %s)", m.ID, m.Manufacturer, m.Model, m.Method)
	}

	log.Printf("exporting to %s (org=%s bucket=%s device=%s) every %s",
		*influxURL, *influxOrg, *influxBucket, *deviceID, *interval)
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, m := range modems {
				exportModem(ctx, writeAPI, *deviceID, m)
			}
		}
	}
}

// exportModem reads the current signal (every active leg) and, where
// supported, cell info for m, and writes the resulting radio_cell
// point(s) plus a modem_state point to InfluxDB. Each write is
// independent: a failure on one doesn't block the others, and nothing
// is retried/buffered (acceptable for monitoring data, and avoids
// unbounded buffering on a resource-constrained device if InfluxDB, or
// the link to it, is down for a while).
func exportModem(ctx context.Context, writeAPI api.WriteAPIBlocking, deviceID string, m modem.Modem) {
	signals, err := modem.GetSignals(ctx, m)
	if err != nil {
		log.Printf("[%s] signal read failed: %v", m.ID, err)
		return
	}

	var cell modem.CellInfo
	haveCellInfo := false
	if modem.SupportsCellInfo(m.Method) {
		c, err := modem.GetCellInfo(ctx, m)
		if err != nil {
			log.Printf("[%s] cell info read failed: %v", m.ID, err)
		} else {
			cell, haveCellInfo = c, true
		}
	}

	for _, sig := range signals {
		attachCellTags := haveCellInfo && sig.Role == "anchor"
		point := radioCellPoint(deviceID, m, sig, cell, attachCellTags)
		if point == nil {
			continue
		}
		if err := writeAPI.WritePoint(ctx, point); err != nil {
			log.Printf("[%s] influx write failed (radio_cell, role=%s): %v", m.ID, sig.Role, err)
		}
	}

	if haveCellInfo {
		if point := modemStatePoint(deviceID, m, cell); point != nil {
			if err := writeAPI.WritePoint(ctx, point); err != nil {
				log.Printf("[%s] influx write failed (modem_state): %v", m.ID, err)
			}
		}
	}
}

// ratTagValue maps modem/'s internal Signal.Technology string to this
// schema's "rat" tag convention: "nr" instead of "5g" (3GPP's own term
// for New Radio), "unknown" instead of "" (a tag should never be
// empty), everything else unchanged.
func ratTagValue(tech string) string {
	switch tech {
	case "5g":
		return "nr"
	case "":
		return "unknown"
	default:
		return tech
	}
}

// radioCellPoint builds one "radio_cell" point for a single active
// radio leg, or nil if there's nothing worth writing (no numeric
// fields and no cell tags/fields to attach). PLMN/TAC/CellID are only
// ever attached on the anchor leg — a modem's CellInfo describes one
// serving context, not one per leg, so there's no distinct per-scg-leg
// value to report anyway.
func radioCellPoint(deviceID string, m modem.Modem, sig modem.Signal, cell modem.CellInfo, attachCellTags bool) *write.Point {
	fields := map[string]interface{}{}
	addFloat := func(key string, v float64) {
		if !math.IsNaN(v) {
			fields[key] = v
		}
	}
	addFloat("rsrp", sig.RSRP)
	addFloat("rsrq", sig.RSRQ)
	addFloat("rssi", sig.RSSI)
	addFloat("sinr", sig.SINR)

	tags := map[string]string{
		"device":   deviceID,
		"modem_id": m.ID,
		"method":   string(m.Method),
		"rat":      ratTagValue(sig.Technology),
		"role":     sig.Role,
	}

	if attachCellTags {
		if cell.PLMN != "" {
			tags["plmn"] = cell.PLMN
		}
		if cell.TAC != "" {
			tags["tac"] = cell.TAC
		}
		if cell.CellID != "" {
			fields["cell_id"] = cell.CellID
		}
	}

	if len(fields) == 0 {
		return nil
	}
	return influxdb2.NewPoint("radio_cell", tags, fields, time.Now())
}

// modemStatePoint builds one "modem_state" point (operator/registration/
// attach state), or nil if none of those fields have data right now.
func modemStatePoint(deviceID string, m modem.Modem, cell modem.CellInfo) *write.Point {
	fields := map[string]interface{}{}
	addString := func(key, v string) {
		if v != "" {
			fields[key] = v
		}
	}
	addString("operator_name", cell.OperatorName)
	addString("registration_state", cell.RegistrationState)
	addString("packet_service_state", cell.PacketServiceState)

	if len(fields) == 0 {
		return nil
	}
	tags := map[string]string{"device": deviceID, "modem_id": m.ID}
	return influxdb2.NewPoint("modem_state", tags, fields, time.Now())
}
