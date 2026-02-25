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

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/vishvananda/netlink"
)

// Configure your interface here!
const IfaceName = "eth0" // e.g. wwan0

type GlitchEvent struct {
	SrcIP uint32
	DstIP uint32
	Value uint64 // Duration (ns) OR Seq Diff
	Type  uint32 // 1=Stall, 2=Retrans
	Proto uint8
}

func main() {
	// 1. Load BPF
	spec, err := ebpf.LoadCollectionSpec("tc_glitch.o")
	if err != nil {
		log.Fatalf("Failed to load BPF: %v", err)
	}

	// Create Go objects to hold the references
	objs := struct {
		EgressMonitor *ebpf.Program `ebpf:"egress_monitor"`
		Events        *ebpf.Map     `ebpf:"events"`
	}{}
	// Actually load them into the Kernel
	if err := spec.LoadAndAssign(&objs, nil); err != nil {
		log.Fatalf("LoadAndAssign: %v", err)
	}
	defer objs.EgressMonitor.Close()
	defer objs.Events.Close()

	// 2. Attach to Interface
	link, err := netlink.LinkByName(IfaceName)
	if err != nil {
		log.Fatalf("Interface %s not found", IfaceName)
	}

	// Create qdisc (clsact)
	qdisc := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: link.Attrs().Index,
			Handle:    netlink.MakeHandle(0xffff, 0),
			Parent:    netlink.HANDLE_CLSACT,
		},
		QdiscType: "clsact",
	}
	if err := netlink.QdiscAdd(qdisc); err != nil && !os.IsExist(err) {
		log.Printf("Warning creating qdisc: %v", err)
	}

	// Attach Filter
	filter := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: link.Attrs().Index,
			Parent:    netlink.MakeHandle(0xffff, 0xfff3), // Egress Hook
			Handle:    1,
			Protocol:  syscall.ETH_P_ALL,
			Priority:  1,
		},
		Fd:           objs.EgressMonitor.FD(),		// The File Descriptor of our C code
		Name:         "egress_monitor",
		DirectAction: true,
	}
	netlink.FilterDel(filter) // Clean old
	if err := netlink.FilterAdd(filter); err != nil {
		log.Fatalf("Failed to attach filter: %v", err)
	}
	
	fmt.Printf("✅ Agent Running on %s\n", IfaceName)
	fmt.Println("   - Watching for Time Gaps > 100ms")
	fmt.Println("   - Watching for TCP Retransmissions")
	fmt.Println("------------------------------------------------")

	// 3. Read Events
	// Open a reader on the "events" map
	rd, _ := ringbuf.NewReader(objs.Events)
	defer rd.Close()

	// Handle Ctrl+C
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt)
		<-stop
		netlink.FilterDel(filter)
		fmt.Println("\nDetached and exiting.")
		os.Exit(0)
	}()

	var event GlitchEvent
	for {
		// Block until data arrives
		record, err := rd.Read()
		if err != nil { continue }

		if err := binary.Read(bytes.NewBuffer(record.RawSample), binary.LittleEndian, &event); err != nil {
			continue
		}
		
		dest := int2ip(event.DstIP)

		if event.Type == 2 {
			// Type 2 = RETRANSMISSION (Serious)
			fmt.Printf("🔴 [LOSS]  TCP Retransmission! | Dest: %-15s\n", dest)
		} else {
			// Type 1 = STALL (Might be idle)
			ms := float64(event.Value) / 1_000_000.0
			proto := "TCP"
			if event.Proto == 17 { proto = "UDP" }
			
			// Highlight major stalls
			// Visual logic: Is it a Micro-glitch or just Idle?
			icon := "⚠️ "
			if ms > 1000 { icon = "💤" } // Likely idle if > 1s

			fmt.Printf("%s [STALL] %s Gap: %6.1f ms | Dest: %-15s\n", icon, proto, ms, dest)
		}
	}
}

func int2ip(nn uint32) net.IP {
	ip := make(net.IP, 4)
	binary.LittleEndian.PutUint32(ip, nn)
	return ip
}
