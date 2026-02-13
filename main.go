package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/vishvananda/netlink"
)

// CHANGE THIS to your 5G Interface Name!
const IfaceName = "eth0" // e.g., wwan0, usb0, eth0
// Add this manually because the library doesn't export it
//const HANDLE_MINIGRESS = 0xFFF3

// Must match the C struct in tc_glitch.bpf.c
type GlitchEvent struct {
	SrcIP   uint32
	DstIP   uint32
	DeltaNS uint64
}

func main() {
	// 1. Load the compiled BPF object
	// This looks for "tc_glitch.o" in the current directory
	spec, err := ebpf.LoadCollectionSpec("tc_glitch.o")
	if err != nil {
		log.Fatalf("Failed to load BPF spec: %v", err)
	}

	// Define the objects we want to load from the C file
	objs := struct {
		EgressMonitor *ebpf.Program `ebpf:"egress_monitor"`
		Events        *ebpf.Map     `ebpf:"events"`
	}{}
	
	// Load them into the kernel
	if err := spec.LoadAndAssign(&objs, nil); err != nil {
		log.Fatalf("Failed to load BPF objects: %v", err)
	}
	defer objs.EgressMonitor.Close()
	defer objs.Events.Close()

	// 2. Find the Network Interface
	link, err := netlink.LinkByName(IfaceName)
	if err != nil {
		log.Fatalf("Interface %s not found: %v", IfaceName, err)
	}

	// 3. Create the qdisc (Queueing Discipline)
	// clsact is a special qdisc for attaching eBPF to ingress/egress
	qdisc := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: link.Attrs().Index,
			Handle:    netlink.MakeHandle(0xffff, 0),
			Parent:    netlink.HANDLE_CLSACT,
		},
		QdiscType: "clsact",
	}
	
	// Try to add it (ignore error if it already exists)
	if err := netlink.QdiscAdd(qdisc); err != nil {
		fmt.Printf("Note: qdisc might already exist (%v), continuing...\n", err)
	}

	// 4. Attach the Filter (The eBPF Program)
	filter := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: link.Attrs().Index,
			Parent:    netlink.MakeHandle(0xffff, 0xfff3), // Use your manual constant here
			Handle:    1,
			Protocol:  syscall.ETH_P_ALL,
			Priority:  1,
		},
		Fd:           objs.EgressMonitor.FD(),
		Name:         "egress_monitor",
		DirectAction: true,
	}

	// Clean up any old filters first, then add the new one
	netlink.FilterDel(filter) 
	if err := netlink.FilterAdd(filter); err != nil {
		log.Fatalf("Failed to attach TC filter: %v", err)
	}
	fmt.Printf("eBPF Glitch Monitor successfully attached to %s!\n", IfaceName)

	// 5. Read Events from the Ring Buffer
	rd, err := ringbuf.NewReader(objs.Events)
	if err != nil {
		log.Fatalf("Failed to open ringbuf: %v", err)
	}
	defer rd.Close()

	// Handle Ctrl+C to clean up
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt)
		<-stop
		fmt.Println("\nDetaching filter and exiting...")
		netlink.FilterDel(filter) // Clean up kernel hook
		os.Exit(0)
	}()

	// 6. Event Loop
	fmt.Println("Listening for upload glitches...")
	var event GlitchEvent
	for {
		record, err := rd.Read()
		if err != nil {
			if err == ringbuf.ErrClosed {
				return
			}
			continue
		}

		// Decode the binary data
		if err := binary.Read(bytes.NewBuffer(record.RawSample), binary.LittleEndian, &event); err != nil {
			continue
		}

		// Convert Nanoseconds to Milliseconds
		latencyMs := float64(event.DeltaNS) / 1_000_000.0
		
		fmt.Printf("⚠️  [GLITCH] Upload Gap: %.2f ms | DestIP: %d\n", latencyMs, event.DstIP)
	}
}
