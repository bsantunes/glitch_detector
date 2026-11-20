// Command glitch-tool is a standalone passive stall/retransmission
// monitor built on the glitch/ package — the same printed event shape
// as dawn-agent's own passive loop, but no TWAMP/modem active-
// monitoring side and no InfluxDB. Two capture backends, selected via
// -capture: "ebpf" (default on Debian; requires root to load the
// program and attach the clsact qdisc/filter, same as dawn-agent) or
// "afpacket" (glitch/afpacket — for boards with no clsact/TC-BPF
// kernel support at all, confirmed on WaveOS; also requires root, for
// the raw AF_PACKET socket). Both run the identical algorithm
// (glitch/core, ported from bpf_src/tc_glitch.bpf.c) — see
// CLAUDE.md's glitch/ package section for why results from the two
// are not directly comparable (AF_PACKET sees egress after the qdisc
// and after GSO segmentation; eBPF sees it before).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"dawn-agent/glitch"
	"dawn-agent/glitch/afpacket"
)

func main() {
	// Logic lives in run() so its deferred h.Close() always executes on
	// any return path, including an error one — os.Exit (which
	// log.Fatalf calls internally) skips deferred calls entirely, which
	// would leave the filter attached. main only decides the process
	// exit code after run has fully returned.
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	iface := flag.String("iface", "wwan0", "network interface to attach to")
	capture := flag.String("capture", "ebpf", "capture backend: ebpf (default) or afpacket (for boards with no clsact/TC-BPF support)")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var h glitch.Source
	switch *capture {
	case "", "ebpf":
		var err error
		h, err = glitch.Open(*iface)
		if err != nil {
			return fmt.Errorf("glitch.Open(%s): %w", *iface, err)
		}
	case "afpacket":
		var err error
		h, err = afpacket.Open(*iface, nil)
		if err != nil {
			return fmt.Errorf("afpacket.Open(%s): %w", *iface, err)
		}
	default:
		return fmt.Errorf("unknown -capture %q (want ebpf or afpacket)", *capture)
	}
	defer h.Close()

	fmt.Printf("🚀 glitch-tool watching %s via %s (Ctrl+C to stop)\n", *iface, *capture)
	tag := strings.ToUpper(*capture)
	if tag == "" {
		tag = "EBPF"
	}

	// Printed periodically, not just on a clean exit: a SIGKILL skips
	// every deferred/exit-path print, so this is the only way to see
	// whether the 256-slot Events() channel is backpressuring (events
	// silently dropped because nothing drained it fast enough) rather
	// than having to trust a final count that might never get printed.
	dropTicker := time.NewTicker(5 * time.Second)
	defer dropTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Printf("📊 dropped=%d (final)\n", h.Dropped())
			return nil
		case <-dropTicker.C:
			fmt.Printf("📊 dropped=%d\n", h.Dropped())
		case ev, ok := <-h.Events():
			if !ok {
				return h.Err()
			}
			ts := ev.Time.UTC().Format(time.RFC3339Nano)
			switch ev.Type {
			case glitch.KindRetrans:
				fmt.Printf("%s 🔴 [%s] TCP Loss Detected | %s %s:%d -> %s:%d\n", ts, tag, protoName(ev.Proto), ev.SrcIP, ev.SrcPort, ev.DstIP, ev.DstPort)
			case glitch.KindStallStart:
				fmt.Printf("%s 🟡 [%s] Stall START: gap %.1fms (baseline %.1fms) | %s %s:%d -> %s:%d\n", ts, tag, float64(ev.Value)/1e6, ev.Baseline.Seconds()*1000, protoName(ev.Proto), ev.SrcIP, ev.SrcPort, ev.DstIP, ev.DstPort)
			case glitch.KindStallEnd:
				fmt.Printf("%s ✅ [%s] Stall END: duration %.1fms, steps=%d (baseline %.1fms) | %s %s:%d -> %s:%d\n", ts, tag, float64(ev.Value)/1e6, ev.Steps, ev.Baseline.Seconds()*1000, protoName(ev.Proto), ev.SrcIP, ev.SrcPort, ev.DstIP, ev.DstPort)
			case glitch.KindStallAbandoned:
				fmt.Printf("%s 💀 [%s] Stall ABANDONED: duration %.1fms (baseline %.1fms) | %s %s:%d -> %s:%d\n", ts, tag, float64(ev.Value)/1e6, ev.Baseline.Seconds()*1000, protoName(ev.Proto), ev.SrcIP, ev.SrcPort, ev.DstIP, ev.DstPort)
			}
		}
	}
}

func protoName(proto uint8) string {
	switch proto {
	case 6:
		return "TCP"
	case 17:
		return "UDP"
	default:
		return fmt.Sprintf("proto(%d)", proto)
	}
}
