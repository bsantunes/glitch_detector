// Command glitch-exporter runs the eBPF passive stall/retransmission
// monitor (glitch/ package) continuously and pushes aggregated stats to
// InfluxDB's "network" bucket - meant to run as a service (systemd on
// the Debian gateway; no OpenWrt procd init script, since the eBPF/TC
// side this package depends on doesn't work on WaveOS at all - see
// CLAUDE.md). For ad hoc/interactive inspection without InfluxDB, use
// cmd/glitch-tool instead.
//
// Schema:
//
//   - measurement "glitch_summary", one point per -interval, always
//     written even when every field is zero (a heartbeat).
//     Tags: device, iface.
//     Fields: stall_episodes (STALL_ENDs seen this interval),
//     stall_max_ms, stall_sum_ms, stall_open (episodes started but not
//     yet ended as of this point), retrans_count, dropped_events (the
//     delta of Handle.Dropped() since the last tick).
//   - measurement "glitch_event", one point per stall_start/stall_end/
//     stall_abandoned - never for a retransmission, which is counted
//     only, via glitch_summary.retrans_count. Timestamped with the
//     kernel event's own time (or, for stall_abandoned, the moment the
//     timeout was detected - there is no kernel event to read a time
//     from), not InfluxDB write time.
//     Tags: device, iface, type.
//     Fields: proto, src_port, dst_port, dst_ip (string), baseline_ms,
//     and - stall_end/stall_abandoned only - duration_ms and steps.
//     Rate-limited to -max-events-per-sec points/second, shared across
//     all three types; anything over the limit still counts in
//     glitch_summary - see the Aggregator type in aggregate.go.
//
// Deliberately never tags on an IP address or port anywhere - those
// belong on per-flow glitch_event fields, not on every point's index.
//
// Uses the InfluxDB client's async, batching WriteAPI (not
// WriteAPIBlocking, unlike every other exporter in this repo) - the
// ring buffer reader must never block on InfluxDB, so a write that's
// slow or failing can't back up into more dropped kernel events on top
// of whatever the kernel side already drops under load.
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

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api/write"

	"dawn-agent/glitch"
)

// abandonedCheckInterval is how often Aggregator.CheckAbandoned runs -
// independent of, and shorter than, -interval, so a dead connection's
// episode doesn't wait for the next summary tick (which could be
// minutes away on a long -interval) before being reported as abandoned.
const abandonedCheckInterval = 5 * time.Second

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	iface := flag.String("iface", "wwan0", "network interface to attach to")
	interval := flag.Duration("interval", 10*time.Second, "how often to write a glitch_summary point")
	influxURL := flag.String("influx-url", "", "InfluxDB base URL, e.g. https://wservices.wavesys.pt:8086 (required)")
	influxToken := flag.String("influx-token", "", "InfluxDB API token (falls back to INFLUX_TOKEN env var)")
	influxOrg := flag.String("influx-org", "wavecom", "InfluxDB org")
	influxBucket := flag.String("influx-bucket", "network", "InfluxDB bucket")
	deviceID := flag.String("device-id", "", "tag identifying this device in InfluxDB (defaults to hostname)")
	maxEventsPerSec := flag.Int("max-events-per-sec", 20, "max glitch_event points written per second (excess is still counted in glitch_summary)")
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

	h, err := glitch.Open(*iface)
	if err != nil {
		return fmt.Errorf("glitch.Open(%s): %w", *iface, err)
	}
	defer h.Close()

	client := influxdb2.NewClient(*influxURL, token)
	defer client.Close()
	writeAPI := client.WriteAPI(*influxOrg, *influxBucket)
	go func() {
		for werr := range writeAPI.Errors() {
			log.Printf("influx async write error: %v", werr)
		}
	}()

	agg := NewAggregator(time.Now, *maxEventsPerSec)

	log.Printf("exporting glitch stats for %s to %s (org=%s bucket=%s device=%s) every %s",
		*iface, *influxURL, *influxOrg, *influxBucket, *deviceID, *interval)

	summaryTicker := time.NewTicker(*interval)
	defer summaryTicker.Stop()
	abandonedTicker := time.NewTicker(abandonedCheckInterval)
	defer abandonedTicker.Stop()

	var lastDropped uint64
	for {
		select {
		case <-ctx.Done():
			writeAPI.Flush()
			return nil
		case ev, ok := <-h.Events():
			if !ok {
				return h.Err()
			}
			for _, p := range agg.HandleEvent(ev) {
				writeAPI.WritePoint(eventInfluxPoint(*deviceID, *iface, p))
			}
		case <-abandonedTicker.C:
			for _, p := range agg.CheckAbandoned() {
				writeAPI.WritePoint(eventInfluxPoint(*deviceID, *iface, p))
			}
		case <-summaryTicker.C:
			dropped := h.Dropped()
			delta := dropped - lastDropped
			lastDropped = dropped
			writeAPI.WritePoint(summaryInfluxPoint(*deviceID, *iface, agg.Summary(delta)))
		}
	}
}

func eventInfluxPoint(deviceID, iface string, p eventPoint) *write.Point {
	tags := map[string]string{
		"device": deviceID,
		"iface":  iface,
		"type":   p.Type,
	}
	fields := map[string]interface{}{
		"proto":       p.Proto,
		"src_port":    p.SrcPort,
		"dst_port":    p.DstPort,
		"dst_ip":      p.DstIP,
		"baseline_ms": p.BaselineMs,
	}
	if p.Type == "stall_end" || p.Type == "stall_abandoned" {
		fields["duration_ms"] = p.DurationMs
		fields["steps"] = p.Steps
	}
	return influxdb2.NewPoint("glitch_event", tags, fields, p.Time)
}

func summaryInfluxPoint(deviceID, iface string, s summaryPoint) *write.Point {
	tags := map[string]string{"device": deviceID, "iface": iface}
	fields := map[string]interface{}{
		"stall_episodes": s.StallEpisodes,
		"stall_max_ms":   s.StallMaxMs,
		"stall_sum_ms":   s.StallSumMs,
		"stall_open":     s.StallOpen,
		"retrans_count":  s.RetransCount,
		"dropped_events": s.DroppedEvents,
	}
	return influxdb2.NewPoint("glitch_summary", tags, fields, time.Now())
}
