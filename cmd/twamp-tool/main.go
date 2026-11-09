// Command twamp-tool is a standalone TWAMP-Test (RFC 5357, unauthenticated/
// "TWAMP Light") prober, independent of dawn-agent's eBPF/modem pipeline.
// It exists to develop and test twamp/ — including against OpenWrt/WaveOS
// targets, where the external twampy Python tool dawn-agent currently
// shells out to isn't installable at all (no Python in the image).
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

	"dawn-agent/twamp"
)

func main() {
	reflector := flag.String("reflector", "10.30.30.1:862", "TWAMP-Test reflector address (UDP host:port) — must already be listening directly, no TWAMP-Control handshake")
	interval := flag.Duration("interval", 5*time.Second, "how often to run a probe burst")
	count := flag.Int("count", 5, "packets per burst")
	probeInterval := flag.Duration("probe-interval", 100*time.Millisecond, "spacing between packets within a burst")
	timeout := flag.Duration("timeout", 2*time.Second, "extra time to wait for trailing replies after the last packet in a burst")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Printf("Probing %s every %s (%d packets/burst, %s apart, Ctrl+C to stop)...\n\n",
		*reflector, *interval, *count, *probeInterval)

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			result, err := twamp.Measure(ctx, *reflector, *count, *probeInterval, *timeout)
			if err != nil {
				log.Printf("probe failed: %v", err)
				continue
			}
			fmt.Printf("RTT=%.1fms Jitter=%.1fms Loss=%.1f%% (%d/%d received)\n",
				result.AvgRTT, result.Jitter, result.LossPct, result.Received, result.Sent)
		}
	}
}
