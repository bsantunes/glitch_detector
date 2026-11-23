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
//   - measurement "modem_state", one point per modem per tick (only
//     when it has something to report at all).
//     Tags: device, modem_id. Fields: operator_name,
//     registration_state, packet_service_state (all strings, omitted
//     when empty), stale_signal_fields (int, omitted when 0 - how many
//     of this modem's rsrp/rsrq/rssi/sinr fields staleTracker withheld
//     from radio_cell this tick, see staleness.go).
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
	modems, err := modem.DetectValidated(ctx)
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

	// stale: a signal field repeated for 30 minutes straight (or, for
	// ubus specifically, whose own source timestamp hasn't advanced in
	// 3x -interval) is withheld rather than re-written as fresh — see
	// staleness.go's own doc comment.
	stale := newStaleTracker(time.Now)

	// sourceStaleByModem: whether each modem's own data source (ubus
	// only, currently — see exportModem) looked stale as of its last
	// poll. Read by the redetect ticker below to decide whether to fall
	// back off the current method even to a nominally worse-ranked one —
	// a working worse method beats a frozen better one. Single
	// goroutine, no locking needed.
	sourceStaleByModem := map[string]bool{}

	// redetect: periodically re-runs the full detection cascade and
	// switches method if either (a) a better-ranked method has become
	// available (confirmed live on dawn6g that a reboot can win the
	// method cascade race onto a worse/contention-prone method simply
	// because uqmi-ubus wasn't ready yet at the single startup-time
	// Detect call, and nothing before this retried later once it was),
	// or (b) the current method's own data has gone stale (confirmed
	// live: ubus's own source timestamp can freeze for hours while
	// still answering ubus calls without error) — falls back even to a
	// worse rank in that case, and switches back once the better method
	// recovers, via the same upgrade check on a later tick. Every
	// switch is logged with which of the two reasons triggered it.
	// Never a partial per-modem swap — see maybeSwitchModems.
	const redetectInterval = 5 * time.Minute
	redetectTicker := time.NewTicker(redetectInterval)
	defer redetectTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, m := range modems {
				exportModem(ctx, writeAPI, *deviceID, m, stale, sourceStaleByModem, *interval)
			}
		case <-redetectTicker.C:
			modems = maybeSwitchModems(ctx, modems, sourceStaleByModem)
		}
	}
}

// maybeSwitchModems re-runs the detection cascade and swaps to its
// result if either every currently-tracked modem is on a method ranked
// worse than what the fresh cascade found (an upgrade), or any current
// modem's own data source is known stale right now (a fallback, which
// explicitly excludes the stale method from the fresh cascade — see
// modem.DetectValidatedExcluding's own doc comment for why DetectValidated
// alone can't be trusted to skip it). Either way this is all-or-nothing
// across every tracked modem, never a partial per-modem swap — a partial
// swap would risk losing track of a modem the fresh cascade didn't
// happen to re-find this particular time. Returns the original list
// unchanged on any detection error, an empty result, or no actual
// improvement.
func maybeSwitchModems(ctx context.Context, current []modem.Modem, sourceStaleByModem map[string]bool) []modem.Modem {
	worstCurrent := 0
	anyStale := false
	var staleMethod modem.Method
	for _, m := range current {
		if r := methodRank(m.Method); r > worstCurrent {
			worstCurrent = r
		}
		if sourceStaleByModem[m.ID] {
			anyStale = true
			staleMethod = m.Method
		}
	}
	if worstCurrent == 0 && !anyStale {
		return current // already ModemManager for everything, and nothing is stale
	}

	var fresh []modem.Modem
	var err error
	if anyStale {
		fresh, err = modem.DetectValidatedExcluding(ctx, staleMethod)
	} else {
		fresh, err = modem.DetectValidated(ctx)
	}
	if err != nil || len(fresh) == 0 {
		return current
	}
	bestFresh := 5
	for _, m := range fresh {
		if r := methodRank(m.Method); r < bestFresh {
			bestFresh = r
		}
	}

	switch {
	case bestFresh < worstCurrent:
		log.Printf("modem: re-detection found a better method (rank %d -> %d) — switching", worstCurrent, bestFresh)
	case anyStale:
		log.Printf("modem: current method (%s) data is stale — falling back to rank %d", staleMethod, bestFresh)
	default:
		return current
	}
	for _, m := range fresh {
		log.Printf("modem detected: [%s] %s %s (via %s)", m.ID, m.Manufacturer, m.Model, m.Method)
	}
	return fresh
}

// methodRank orders modem.Method by preference, matching the cascade
// order modem.DetectValidated itself tries them in — lower is better.
func methodRank(m modem.Method) int {
	switch m {
	case modem.MethodModemManager:
		return 0
	case modem.MethodUbus:
		return 1
	case modem.MethodQMI, modem.MethodMBIM:
		return 2
	case modem.MethodUQMI:
		return 3
	case modem.MethodAT:
		return 4
	default:
		return 5
	}
}

// exportModem reads the current signal (every active leg) and, where
// supported, cell info for m, and writes the resulting radio_cell
// point(s) plus a modem_state point to InfluxDB. Each write is
// independent: a failure on one doesn't block the others, and nothing
// is retried/buffered (acceptable for monitoring data, and avoids
// unbounded buffering on a resource-constrained device if InfluxDB, or
// the link to it, is down for a while).
func exportModem(ctx context.Context, writeAPI api.WriteAPIBlocking, deviceID string, m modem.Modem, stale *staleTracker, sourceStaleByModem map[string]bool, interval time.Duration) {
	signals, err := modem.GetSignals(ctx, m)
	if err != nil {
		log.Printf("[%s] signal read failed: %v", m.ID, err)
		return
	}

	// ubus is the only backend with its own source timestamp to check
	// (confirmed live: it can keep answering ubus calls without error
	// while its underlying daemon has stopped actually refreshing that
	// data) — every other backend falls back to staleTracker's own
	// change-detection instead, which needs no source timestamp at all.
	hasSourceTimestamp := false
	sourceStale := false
	if m.Method == modem.MethodUbus {
		if ts, ok, tErr := modem.UbusMetricsTimestamp(ctx, m); tErr == nil && ok {
			hasSourceTimestamp = true
			sourceStale = time.Since(ts) >= 3*interval
		}
	}
	sourceStaleByModem[m.ID] = hasSourceTimestamp && sourceStale

	var staleFieldsThisPoll int
	for i, sig := range signals {
		out, withheld, msg := stale.Check(signalKey{modemID: m.ID, role: sig.Role}, sig, hasSourceTimestamp, sourceStale)
		if msg != "" {
			log.Printf("[%s] %s", m.ID, msg)
		}
		signals[i] = out
		staleFieldsThisPoll += withheld
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

	if point := modemStatePoint(deviceID, m, cell, haveCellInfo, staleFieldsThisPoll); point != nil {
		if err := writeAPI.WritePoint(ctx, point); err != nil {
			log.Printf("[%s] influx write failed (modem_state): %v", m.ID, err)
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
// attach state, plus staleFieldsCount — how many of this modem's signal
// fields staleTracker withheld this poll, 0-4, additive and omitted when
// zero so a normal poll looks exactly like it did before this field
// existed), or nil if none of those have anything to report right now.
func modemStatePoint(deviceID string, m modem.Modem, cell modem.CellInfo, haveCellInfo bool, staleFieldsCount int) *write.Point {
	fields := map[string]interface{}{}
	if haveCellInfo {
		addString := func(key, v string) {
			if v != "" {
				fields[key] = v
			}
		}
		addString("operator_name", cell.OperatorName)
		addString("registration_state", cell.RegistrationState)
		addString("packet_service_state", cell.PacketServiceState)
	}
	if staleFieldsCount > 0 {
		fields["stale_signal_fields"] = staleFieldsCount
	}

	if len(fields) == 0 {
		return nil
	}
	tags := map[string]string{"device": deviceID, "modem_id": m.ID}
	return influxdb2.NewPoint("modem_state", tags, fields, time.Now())
}
