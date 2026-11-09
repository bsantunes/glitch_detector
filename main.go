// Package main implements the DAWN6G glitch-detector gateway.
// It combines passive kernel instrumentation via an eBPF program
// with active measurements (TWAMP and radio signal metrics) to
// provide real-time telemetry and alerting when the 5G link
// exhibits stalls or packet loss.
//
// The eBPF object (tc_glitch.o) must be built beforehand; see the
// project README for build instructions. By default the code
// attaches to interface "wwan0" (a 5G modem); supply a different
// interface with the -iface flag when starting the agent.
//
// Running:
//   go run main.go
//
// Passive events are logged with emojis, and active statistics are
// printed every 5 seconds.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/vishvananda/netlink"

	"dawn-agent/modem"
)

// default constants; overridden by flags if provided
const (
	// Magic handle number used when installing the clsact ingress
	// filter for eBPF. This value comes from the BPF program logic.
	// HANDLE_MINIGRESS = 0xFFF3
	// Change HANDLE_MINIGRESS (0xFFF3) to HANDLE_MINEGRESS (0xFFF2)
	HANDLE_MINEGRESS = 0xFFF2
)

// glitch types as seen in the event structure
const (
	GlitchTypeStall   = 1
	GlitchTypeTCPLoss = 2
)

type GlitchEvent struct {
	SrcIP uint32
	DstIP uint32
	Value uint64
	Type  uint32 // one of GlitchType*
	Proto uint8
}

// bpfObjects holds the programs/maps we load from the object file.
type bpfObjects struct {
	EgressMonitor *ebpf.Program `ebpf:"egress_monitor"`
	Events        *ebpf.Map     `ebpf:"events"`
}

func main() {
	// command-line options
	iface := flag.String("iface", "wwan0", "network interface to attach to")
	rfl := flag.String("reflector", "10.30.30.1:862", "TWAMP reflector address")
	flag.Parse()

	// convert interrupt signals into context cancellation
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// 1. Load eBPF (Passive)
	spec, err := ebpf.LoadCollectionSpec("tc_glitch.o")
	if err != nil {
		log.Fatalf("Failed to load BPF: %v", err)
	}
	objs := bpfObjects{}
	if err := spec.LoadAndAssign(&objs, nil); err != nil {
		log.Fatalf("LoadAndAssign: %v", err)
	}

	// 2. Attach to Interface
	link, err := netlink.LinkByName(*iface)
	if err != nil {
		log.Fatalf("LinkByName(%s): %v", *iface, err)
	}
	qdisc := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{LinkIndex: link.Attrs().Index, Handle: netlink.MakeHandle(0xffff, 0), Parent: netlink.HANDLE_CLSACT},
		QdiscType:  "clsact",
	}
	if err := netlink.QdiscAdd(qdisc); err != nil {
		// ignore "file exists" since the qdisc may already be present
		if !errors.Is(err, syscall.EEXIST) {
			log.Fatalf("QdiscAdd: %v", err)
		}
		log.Printf("note: qdisc already exists, continuing")
	}
	defer netlink.QdiscDel(qdisc)

	filter := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{LinkIndex: link.Attrs().Index, Parent: netlink.MakeHandle(0xffff, HANDLE_MINEGRESS), Handle: 1, Protocol: syscall.ETH_P_ALL, Priority: 1},
		Fd:          objs.EgressMonitor.FD(),
		Name:        "egress_monitor",
		DirectAction: true,
	}
	if err := netlink.FilterAdd(filter); err != nil {
		if !errors.Is(err, syscall.EEXIST) {
			log.Fatalf("FilterAdd: %v", err)
		}
		log.Printf("note: filter already exists, continuing")
	}
	defer netlink.FilterDel(filter)

	fmt.Printf("🚀 DAWN6G Gateway Active on %s to %s\n", *iface, *rfl)

	// Detect whatever modem(s) are present, trying ModemManager first,
	// then QMI/MBIM, then raw AT — this replaces the previous hardcoded
	// assumption of a single ModemManager modem at index 0, so different
	// modem models/vendors are picked up automatically.
	modems, err := modem.Detect(ctx)
	if err != nil {
		log.Printf("modem detection error: %v", err)
	}
	if len(modems) == 0 {
		log.Printf("warning: no modems detected (tried ModemManager, QMI/MBIM, AT); radio metrics will be unavailable")
	}
	for _, m := range modems {
		fmt.Printf("📶 Modem detected: [%s] %s %s (via %s)\n", m.ID, m.Manufacturer, m.Model, m.Method)
	}

	// 3. Start Multi-Sensor Loops
	go runActiveMonitoring(ctx, *rfl, modems) // TWAMP & Radio metrics
	
	// 4. Passive Event Loop (eBPF)
	rd, err := ringbuf.NewReader(objs.Events)
	if err != nil {
		log.Fatalf("ringbuf.NewReader: %v", err)
	}

	// send decoded events into a channel so we can select on ctx.Done()
	eventCh := make(chan GlitchEvent)
	errCh := make(chan error, 1)
	go func() {
		for {
			rec, err := rd.Read()
			if err != nil {
				errCh <- err
				return
			}
			var ev GlitchEvent
			binary.Read(bytes.NewBuffer(rec.RawSample), binary.LittleEndian, &ev)
			eventCh <- ev
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-eventCh:
			if ev.Type == GlitchTypeTCPLoss {
				fmt.Printf("🔴 [KERNEL] TCP Loss Detected | Dest: %s\n", int2ip(ev.DstIP))
			} else {
				fmt.Printf("⚠️  [KERNEL] Stall: %.1fms | Dest: %s\n", float64(ev.Value)/1e6, int2ip(ev.DstIP))
			}
		case e := <-errCh:
			log.Printf("ringbuf reader error: %v", e)
			return
		}
	}
}

// runActiveMonitoring periodically gathers active telemetry from
// each detected modem (rsrp/rsrq/rssi) and from a TWAMP reflector. It
// prints a human-readable status line per modem every 5 seconds.
func runActiveMonitoring(ctx context.Context, reflector string, modems []modem.Modem) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Sensor 1: TWAMP Metrics
			avgRTT, jitter, loss := getTwampMetrics(ctx, reflector)

			// Sensor 2: Radio Metrics, per detected modem
			for _, m := range modems {
				sig, err := modem.GetSignal(ctx, m)
				if err != nil {
					log.Printf("modem [%s]: signal read failed: %v", m.ID, err)
					continue
				}
				fmt.Printf("📊 [STATUS] Modem[%s/%s] Radio: %s RSRP / %s RSRQ / %s RSSI | TWAMP: %.1fms RTT / %.1fms Jitter / %.1f%% Loss\n",
					m.ID, sig.Technology, modem.FormatValue(sig.RSRP), modem.FormatValue(sig.RSRQ), modem.FormatValue(sig.RSSI), avgRTT, jitter, loss)

				// Skip quietly for backends without cell info support.
				if !modem.SupportsCellInfo(m.Method) {
					continue
				}
				cell, err := modem.GetCellInfo(ctx, m)
				if err != nil {
					log.Printf("modem [%s]: cell info read failed: %v", m.ID, err)
					continue
				}
				fmt.Printf("📡 [CELL] Modem[%s] PLMN=%s (%s) reg=%s pkt=%s TAC=%s CellID=%s\n",
					m.ID, cell.PLMN, cell.OperatorName, cell.RegistrationState, cell.PacketServiceState, cell.TAC, cell.CellID)
			}
		}
	}
}

// getTwampMetrics runs a TWAMP sender against our reflector and
// parses the printed summary line.  On error it reports 100% loss so
// that the status output clearly indicates a problem.
func getTwampMetrics(ctx context.Context, reflector string) (float64, float64, float64) {
	// Use CombinedOutput to capture both standard output and error messages (stderr)
	cmd := exec.CommandContext(ctx, "twampy", "sender", reflector, "--count", "5", "--interval", "100")
	out, err := cmd.CombinedOutput() 
	
	if err != nil { 
		// Print EXACTLY why it failed directly to your terminal
		log.Printf("🔴 [DEBUG] TWAMP Failed: %v | Output: %s", err, string(bytes.TrimSpace(out)))
		return 0, 0, 100 
	}

	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Roundtrip:") {
			f := strings.Fields(line)
			if len(f) >= 6 {
				rtt, _ := time.ParseDuration(f[3])
				jit, _ := time.ParseDuration(f[4])
				lStr := strings.TrimSuffix(f[5], "%")
				l, _ := strconv.ParseFloat(lStr, 64)
				return float64(rtt.Milliseconds()), float64(jit.Microseconds()) / 1000.0, l
			}
		}
	}
	return 0, 0, 100
}
// int2ip converts a little‑endian 32‑bit integer (as produced by the
// BPF program) into a standard net.IP value.
func int2ip(nn uint32) net.IP {
	ip := make(net.IP, 4)
	binary.LittleEndian.PutUint32(ip, nn)
	return ip
}
