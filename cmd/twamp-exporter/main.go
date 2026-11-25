// Command twamp-exporter periodically probes a TWAMP-Test reflector via
// the twamp/ package and pushes RTT/jitter/loss to InfluxDB, for the
// "network" bucket (currently unused — see metrics-endpoint.md, which
// earmarks it for "TWAMP, QoS/HTB, nDPI, eBPF micro-interruptions").
// Meant to run continuously as a service (systemd on the Debian
// gateway, procd on WaveOS), same deployment shape as modem-exporter.
// For ad hoc/interactive inspection, use cmd/twamp-tool instead.
//
// Schema (new — not yet reviewed against any existing convention, since
// metrics-endpoint.md earmarks the "network" bucket but doesn't specify
// a schema for it):
//
//   - measurement "twamp", one point per probe burst.
//     Tags: device, reflector, plus iface when -bind-dev is set (the
//     interface the probes were pinned to). Fields: rtt_ms, jitter_ms,
//     loss_pct (all float), sent, received (ints).
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
	"github.com/influxdata/influxdb-client-go/v2/api"

	"dawn-agent/twamp"
	"dawn-agent/version"
)

func main() {
	reflector := flag.String("reflector", "10.30.30.1:862", "TWAMP-Test reflector address (UDP host:port); use wservices' public 51.91.57.3:862 with -bind-dev")
	bindDev := flag.String("bind-dev", "", "send probes only through this interface (SO_BINDTODEVICE, like iperf3 --bind-dev), e.g. lte1; empty = normal routing")
	interval := flag.Duration("interval", 30*time.Second, "how often to probe and push to InfluxDB")
	count := flag.Int("count", 5, "packets per burst")
	probeInterval := flag.Duration("probe-interval", 100*time.Millisecond, "spacing between packets within a burst")
	timeout := flag.Duration("timeout", 2*time.Second, "extra time to wait for trailing replies after the last packet in a burst")
	influxURL := flag.String("influx-url", "", "InfluxDB base URL, e.g. https://wservices.wavesys.pt:8086 (required)")
	influxToken := flag.String("influx-token", "", "InfluxDB API token (falls back to INFLUX_TOKEN env var)")
	influxOrg := flag.String("influx-org", "wavecom", "InfluxDB org")
	influxBucket := flag.String("influx-bucket", "network", "InfluxDB bucket")
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	client := influxdb2.NewClient(*influxURL, token)
	defer client.Close()
	writeAPI := client.WriteAPIBlocking(*influxOrg, *influxBucket)

	log.Printf("twamp-exporter %s", version.Version)
	writeAgentInfo(ctx, writeAPI, *deviceID, "twamp-exporter")

	via := "normal routing"
	if *bindDev != "" {
		via = "bind-dev " + *bindDev
	}
	log.Printf("exporting TWAMP metrics for %s (%s) to %s (org=%s bucket=%s device=%s) every %s",
		*reflector, via, *influxURL, *influxOrg, *influxBucket, *deviceID, *interval)

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	agentInfoTicker := time.NewTicker(time.Hour)
	defer agentInfoTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-agentInfoTicker.C:
			writeAgentInfo(ctx, writeAPI, *deviceID, "twamp-exporter")
		case <-ticker.C:
			result, err := twamp.Measure(ctx, *reflector, *bindDev, *count, *probeInterval, *timeout)
			if err != nil {
				log.Printf("probe against %s failed: %v", *reflector, err)
				continue
			}

			point := influxdb2.NewPoint("twamp", twampTags(*deviceID, *reflector, *bindDev),
				map[string]interface{}{
					"rtt_ms":    result.AvgRTT,
					"jitter_ms": result.Jitter,
					"loss_pct":  result.LossPct,
					"sent":      result.Sent,
					"received":  result.Received,
				},
				time.Now())
			// Logged and dropped on failure, not retried/buffered — same
			// deliberate tradeoff as modem-exporter (avoids unbounded
			// in-RAM buffering on constrained devices).
			if err := writeAPI.WritePoint(ctx, point); err != nil {
				log.Printf("influx write failed: %v", err)
			}
		}
	}
}

// twampTags builds a twamp point's tags. iface is only added when the
// probes were pinned to an interface (-bind-dev), so points from an
// unbound exporter keep exactly the tag set they always had.
func twampTags(deviceID, reflector, bindDev string) map[string]string {
	tags := map[string]string{"device": deviceID, "reflector": reflector}
	if bindDev != "" {
		tags["iface"] = bindDev
	}
	return tags
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
