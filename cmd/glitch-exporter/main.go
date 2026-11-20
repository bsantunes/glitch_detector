// Command glitch-exporter runs the passive stall/retransmission
// monitor (glitch/ package) continuously and pushes aggregated stats to
// InfluxDB's "network" bucket - meant to run as a service (systemd on
// the Debian gateway; no OpenWrt procd init script yet for the
// afpacket backend, though unlike the eBPF one it works on WaveOS -
// see CLAUDE.md). For ad hoc/interactive inspection without InfluxDB,
// use cmd/glitch-tool instead.
//
// Two capture backends, selected via -capture: "ebpf" (default;
// requires root, Debian/clsact-capable boards only) or "afpacket"
// (glitch/afpacket - works on WaveOS, which has no clsact/TC-BPF
// support at all; also requires root, for the raw AF_PACKET socket).
// Both run the identical algorithm (glitch/core, ported from
// bpf_src/tc_glitch.bpf.c) - see CLAUDE.md's glitch/ package section
// for why results from the two are not directly comparable (AF_PACKET
// sees egress after the qdisc and after GSO segmentation; eBPF sees
// it before).
//
// Schema:
//
//   - measurement "glitch_summary", one point per -interval, always
//     written even when every field is zero (a heartbeat).
//     Tags: device, iface, capture (ebpf/afpacket - additive).
//     Fields: stall_episodes (STALL_ENDs seen this interval),
//     stall_max_ms, stall_sum_ms, stall_open (episodes started but not
//     yet ended as of this point), retrans_count, dropped_events (the
//     delta of Handle.Dropped() since the last tick), plus its
//     breakdown: kernel_dropped_events/user_dropped_events/
//     untruncated_over_limit_events (afpacket.Capture-only, always 0
//     for -capture ebpf - see glitch/afpacket's own doc comments) and
//     write_queue_dropped_events (points dropped by the bounded queue
//     in front of the InfluxDB write client when it couldn't keep up -
//     see the writeQueue comment in run()).
//   - measurement "glitch_event", one point per stall_start/stall_end/
//     stall_abandoned - never for a retransmission, which is counted
//     only, via glitch_summary.retrans_count. Timestamped with the
//     kernel event's own time (or, for stall_abandoned, the moment the
//     timeout was detected - there is no kernel event to read a time
//     from), not InfluxDB write time.
//     Tags: device, iface, type, capture (ebpf/afpacket - additive).
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
	"sync/atomic"
	"syscall"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api/write"

	"dawn-agent/glitch"
	"dawn-agent/glitch/afpacket"
)

// writeQueueSize bounds the queue between the event-consumer loop and
// writeAPI.WritePoint. Sized generously above anything a normal
// -interval heartbeat plus a stall burst should produce between two
// drains, since its only job is absorbing a temporary write stall, not
// steady-state throughput.
const writeQueueSize = 4096

// abandonedCheckInterval is how often Aggregator.CheckAbandoned runs -
// independent of, and shorter than, -interval, so a dead connection's
// episode doesn't wait for the next summary tick (which could be
// minutes away on a long -interval) before being reported as abandoned.
const abandonedCheckInterval = 5 * time.Second

// healthCheckInterval/healthWriteTimeout govern the one place in this
// program that calls InfluxDB's BLOCKING write API, deliberately: the
// async WriteAPI used for everything else is fire-and-forget by design
// (see writeQueue's own comment) and genuinely cannot tell this
// process whether a write ever actually succeeded - a silent total
// write failure (wrong URL, bad token, a token that can write some
// tags but not others, the write path down) looks identical in its
// logs to "working fine," which is exactly what happened the first
// time this exporter was pointed at a bad token: zero errors logged,
// zero points written, nothing to suggest anything was wrong short of
// noticing the absence of data days later. This goroutine is isolated
// from the main event loop (its own ticker, its own timeout) so a
// stuck or failing health write can never block h.Events().
const (
	healthCheckInterval = 30 * time.Second
	healthWriteTimeout  = 20 * time.Second
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	iface := flag.String("iface", "wwan0", "network interface to attach to")
	capture := flag.String("capture", "ebpf", "capture backend: ebpf (default) or afpacket (for boards with no clsact/TC-BPF support)")
	interval := flag.Duration("interval", 10*time.Second, "how often to write a glitch_summary point")
	influxURL := flag.String("influx-url", "", "InfluxDB base URL, e.g. https://wservices.wavesys.pt:8086 (required)")
	influxToken := flag.String("influx-token", "", "InfluxDB API token (falls back to INFLUX_TOKEN env var)")
	influxOrg := flag.String("influx-org", "wavecom", "InfluxDB org")
	influxBucket := flag.String("influx-bucket", "network", "InfluxDB bucket")
	deviceID := flag.String("device-id", "", "tag identifying this device in InfluxDB (defaults to hostname)")
	maxEventsPerSec := flag.Int("max-events-per-sec", 20, "max glitch_event points written per second (excess is still counted in glitch_summary)")
	noWriteAlertAfter := flag.Duration("no-write-alert-after", 2*time.Minute, "log an error if no InfluxDB write has been confirmed successful for this long (see the health-check goroutine)")
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

	var h glitch.Source
	var afCap *afpacket.Capture // non-nil only for -capture afpacket, for its own drop-breakdown counters below
	switch *capture {
	case "", "ebpf":
		var err error
		h, err = glitch.Open(*iface)
		if err != nil {
			return fmt.Errorf("glitch.Open(%s): %w", *iface, err)
		}
	case "afpacket":
		var err error
		afCap, err = afpacket.Open(*iface, nil)
		if err != nil {
			return fmt.Errorf("afpacket.Open(%s): %w", *iface, err)
		}
		h = afCap
	default:
		return fmt.Errorf("unknown -capture %q (want ebpf or afpacket)", *capture)
	}
	defer h.Close()

	client := influxdb2.NewClient(*influxURL, token)
	defer client.Close()
	writeAPI := client.WriteAPI(*influxOrg, *influxBucket)
	var asyncWriteErrors uint64
	go func() {
		for werr := range writeAPI.Errors() {
			atomic.AddUint64(&asyncWriteErrors, 1)
			log.Printf("influx async write error: %v", werr)
		}
	}()

	// writeAPI.WritePoint sends on an unbuffered channel to a background
	// goroutine that itself sends on another unbuffered channel to the
	// goroutine actually doing the HTTP write — so if that HTTP write is
	// stuck (e.g. this device's own uplink is down, same outage the
	// capture side is trying to report on, and the underlying TCP
	// connection is blackholed rather than refused), WritePoint can
	// block for as long as that write takes to time out. Confirmed by
	// reading the client library's own source (unbuffered the whole
	// way down), not assumed. Calling WritePoint directly from this
	// loop would mean a stuck write starves h.Events() of attention,
	// producing exactly the kind of consumer-side drop glitch-tool's
	// own synchronous print loop already showed under a much smaller
	// burst than a real outage can produce — except gated on network
	// retry/timeout behavior instead of a stdout syscall, so potentially
	// far longer-lived. writeQueue decouples the two: this loop only
	// ever does a non-blocking send into a bounded channel, drained by
	// its own dedicated goroutine that's free to sit blocked in
	// WritePoint for as long as it takes. A full queue drops the point
	// and counts it (writeQueueDropped) rather than blocking — reported
	// in glitch_summary so a sustained problem here is visible, not
	// just a few points quietly late.
	writeQueue := make(chan *write.Point, writeQueueSize)
	var writeQueueDropped, pointsEnqueued uint64
	go func() {
		for p := range writeQueue {
			writeAPI.WritePoint(p)
		}
	}()
	enqueue := func(p *write.Point) {
		select {
		case writeQueue <- p:
			atomic.AddUint64(&pointsEnqueued, 1)
		default:
			atomic.AddUint64(&writeQueueDropped, 1)
		}
	}

	// The one place this program calls InfluxDB's BLOCKING write API —
	// see healthCheckInterval's own doc comment for why. Isolated in
	// its own goroutine with its own ticker and a bounded per-attempt
	// context, so it can never affect the main event loop below.
	var lastSuccessfulWrite atomic.Int64
	lastSuccessfulWrite.Store(time.Now().Unix())
	writeAPIBlocking := client.WriteAPIBlocking(*influxOrg, *influxBucket)
	go func() {
		ticker := time.NewTicker(healthCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				wctx, cancel := context.WithTimeout(context.Background(), healthWriteTimeout)
				p := influxdb2.NewPoint("glitch_exporter_health",
					map[string]string{"device": *deviceID, "iface": *iface, "capture": *capture},
					map[string]interface{}{"up": 1}, time.Now())
				err := writeAPIBlocking.WritePoint(wctx, p)
				cancel()
				since := time.Since(time.Unix(lastSuccessfulWrite.Load(), 0))
				if err != nil {
					log.Printf("InfluxDB health-check write FAILED: %v (last confirmed success: %s ago)", err, since.Round(time.Second))
				} else {
					lastSuccessfulWrite.Store(time.Now().Unix())
					since = 0
				}
				log.Printf("influx health: points_enqueued=%d write_queue_dropped=%d async_write_errors=%d last_confirmed_success=%s ago",
					atomic.LoadUint64(&pointsEnqueued), atomic.LoadUint64(&writeQueueDropped), atomic.LoadUint64(&asyncWriteErrors), since.Round(time.Second))
				if since > *noWriteAlertAfter {
					log.Printf("ERROR: no InfluxDB write confirmed successful in over %s — writes may be silently failing (check URL/org/bucket/token/network)", noWriteAlertAfter)
				}
			}
		}
	}()

	agg := NewAggregator(time.Now, *maxEventsPerSec)

	log.Printf("exporting glitch stats for %s via %s to %s (org=%s bucket=%s device=%s) every %s",
		*iface, *capture, *influxURL, *influxOrg, *influxBucket, *deviceID, *interval)

	summaryTicker := time.NewTicker(*interval)
	defer summaryTicker.Stop()
	abandonedTicker := time.NewTicker(abandonedCheckInterval)
	defer abandonedTicker.Stop()

	var lastDropped, lastKernelDropped, lastUserDropped, lastUntruncated, lastWriteQueueDropped uint64
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
				enqueue(eventInfluxPoint(*deviceID, *iface, *capture, p))
			}
		case <-abandonedTicker.C:
			for _, p := range agg.CheckAbandoned() {
				enqueue(eventInfluxPoint(*deviceID, *iface, *capture, p))
			}
		case <-summaryTicker.C:
			dropped := h.Dropped()
			drops := DropCounts{Total: dropped - lastDropped}
			lastDropped = dropped
			if afCap != nil {
				kernel, user, untrunc := afCap.KernelDropped(), afCap.UserDropped(), afCap.UntruncatedOverLimit()
				drops.Kernel = kernel - lastKernelDropped
				drops.User = user - lastUserDropped
				drops.UntruncatedOverLimit = untrunc - lastUntruncated
				lastKernelDropped, lastUserDropped, lastUntruncated = kernel, user, untrunc
			}
			wqd := atomic.LoadUint64(&writeQueueDropped)
			drops.WriteQueue = wqd - lastWriteQueueDropped
			lastWriteQueueDropped = wqd
			if drops != (DropCounts{}) {
				log.Printf("drops this interval: total=%d kernel=%d user=%d untruncated_over_limit=%d write_queue=%d",
					drops.Total, drops.Kernel, drops.User, drops.UntruncatedOverLimit, drops.WriteQueue)
			}
			enqueue(summaryInfluxPoint(*deviceID, *iface, *capture, agg.Summary(drops)))
		}
	}
}

func eventInfluxPoint(deviceID, iface, capture string, p eventPoint) *write.Point {
	tags := map[string]string{
		"device":  deviceID,
		"iface":   iface,
		"type":    p.Type,
		"capture": capture,
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

func summaryInfluxPoint(deviceID, iface, capture string, s summaryPoint) *write.Point {
	tags := map[string]string{"device": deviceID, "iface": iface, "capture": capture}
	fields := map[string]interface{}{
		"stall_episodes": s.StallEpisodes,
		"stall_max_ms":   s.StallMaxMs,
		"stall_sum_ms":   s.StallSumMs,
		"stall_open":     s.StallOpen,
		"retrans_count":  s.RetransCount,
		"dropped_events": s.DroppedEvents,
		// Breakdown of dropped_events, additive — kernel/user/
		// untruncated_over_limit are always 0 for -capture ebpf (no
		// equivalent split exists there), write_queue_dropped is 0
		// unless the bounded queue in front of the InfluxDB write
		// client had to drop a point rather than block the consumer
		// loop. See DropCounts' own doc comment.
		"kernel_dropped_events":         s.KernelDroppedEvents,
		"user_dropped_events":           s.UserDroppedEvents,
		"untruncated_over_limit_events": s.UntruncatedOverLimitEvents,
		"write_queue_dropped_events":    s.WriteQueueDroppedEvents,
	}
	return influxdb2.NewPoint("glitch_summary", tags, fields, time.Now())
}
