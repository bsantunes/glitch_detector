package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/vishvananda/netlink"
)

// Use hex for egress handle
const HANDLE_MINIGRESS = 0xFFF3

// --- Updated Structs ---

// Matches C struct glitch_event
type GlitchEvent struct {
	SrcIP   uint32
	DstIP   uint32
	DeltaNS uint64
	Len     uint32
	Proto   uint8
}

// Matches C struct global_metrics
type GlobalMetrics struct {
	TotalBytes uint64
	TotalPkts  uint64
}

func main() {
	spec, err := ebpf.LoadCollectionSpec("tc_glitch.o")
	if err != nil {
		log.Fatalf("Failed to load BPF spec: %v", err)
	}

	// Define objects map: Program + 2 Maps
	objs := struct {
		EgressMonitor *ebpf.Program `ebpf:"egress_monitor"`
		Events        *ebpf.Map     `ebpf:"events"`
		GlobalStats   *ebpf.Map     `ebpf:"global_stats"`
	}{}

	if err := spec.LoadAndAssign(&objs, nil); err != nil {
		log.Fatalf("Failed to load objects: %v", err)
	}
	defer objs.EgressMonitor.Close()
	defer objs.Events.Close()
	defer objs.GlobalStats.Close()

	// --- Attach Logic (Same as before) ---
	IfaceName := "eth1" // CHANGE THIS to wwan0 or eth0
	link, err := netlink.LinkByName(IfaceName)
	if err != nil {
		log.Fatalf("Interface %s not found", IfaceName)
	}

	qdisc := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: link.Attrs().Index,
			Handle:    netlink.MakeHandle(0xffff, 0),
			Parent:    netlink.HANDLE_CLSACT,
		},
		QdiscType: "clsact",
	}
	// Try adding qdisc, ignore if exists
	netlink.QdiscAdd(qdisc)

	filter := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: link.Attrs().Index,
			Parent:    netlink.MakeHandle(0xffff, HANDLE_MINIGRESS),
			Handle:    1,
			Protocol:  syscall.ETH_P_ALL,
			Priority:  1,
		},
		Fd:           objs.EgressMonitor.FD(),
		Name:         "egress_monitor",
		DirectAction: true,
	}
	netlink.FilterDel(filter) // Cleanup old
	if err := netlink.FilterAdd(filter); err != nil {
		log.Fatalf("Failed to attach filter: %v", err)
	}
	fmt.Printf("Monitoring started on %s!\n", IfaceName)

	// --- Start Background Routines ---

	// 1. Stats Monitor (Ticker)
	go monitorThroughput(objs.GlobalStats)

	// 2. Event Reader (Main Loop)
	rd, _ := ringbuf.NewReader(objs.Events)
	defer rd.Close()

	// Cleanup on Ctrl+C
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt)
		<-stop
		fmt.Println("\nCleaning up...")
		netlink.FilterDel(filter)
		os.Exit(0)
	}()

	var event GlitchEvent
	for {
		record, err := rd.Read()
		if err != nil { continue }

		if err := binary.Read(bytes.NewBuffer(record.RawSample), binary.LittleEndian, &event); err != nil {
			continue
		}

		// Pretty print glitch
		protoStr := "TCP"
		if event.Proto == 17 { protoStr = "UDP" }
		latencyMs := float64(event.DeltaNS) / 1_000_000.0

		fmt.Printf("⚠️  [GLITCH] %s Packet | %d bytes | Gap: %.2f ms | IP: %s\n", 
			protoStr, event.Len, latencyMs, int2ip(event.DstIP))
	}
}

// Polling loop for Live Bandwidth
func monitorThroughput(statsMap *ebpf.Map) {
	ticker := time.NewTicker(1 * time.Second)
	var prevBytes, prevPkts uint64
	var metrics GlobalMetrics
	key := uint32(0) // Map index 0

	for range ticker.C {
		if err := statsMap.Lookup(&key, &metrics); err != nil {
			fmt.Printf("Error reading stats: %v\n", err)
			continue
		}

		// Calculate Deltas
		bytesDiff := metrics.TotalBytes - prevBytes
		pktsDiff := metrics.TotalPkts - prevPkts

		// Convert to Mbps
		mbps := (float64(bytesDiff) * 8) / 1_000_000.0

		// Update previous state
		prevBytes = metrics.TotalBytes
		prevPkts = metrics.TotalPkts

		// Print Status Line (Overwrite line with \r for clean UI)
		if mbps > 0.01 {
			fmt.Printf("📊 [LIVE] Upload: %.2f Mbps | %d PPS\r", mbps, pktsDiff)
		}
	}
}

func int2ip(nn uint32) net.IP {
	ip := make(net.IP, 4)
	binary.LittleEndian.PutUint32(ip, nn)
	return ip
}
