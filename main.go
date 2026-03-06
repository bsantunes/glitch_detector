package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
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
)

const (
	IfaceName        = "wwan0" // Change to wwan0 for 5G
	ReflectorAddress = "dawn6g.duckdns.org"
	HANDLE_MINIGRESS = 0xFFF3
)

type GlitchEvent struct {
	SrcIP uint32
	DstIP uint32
	Value uint64
	Type  uint32
	Proto uint8
}

func main() {
	// 1. Load eBPF (Passive)
	spec, err := ebpf.LoadCollectionSpec("tc_glitch.o")
	if err != nil {
		log.Fatalf("Failed to load BPF: %v", err)
	}
	objs := struct {
		EgressMonitor *ebpf.Program `ebpf:"egress_monitor"`
		Events        *ebpf.Map     `ebpf:"events"`
	}{}
	if err := spec.LoadAndAssign(&objs, nil); err != nil {
		log.Fatalf("LoadAndAssign: %v", err)
	}

	// 2. Attach to Interface
	link, _ := netlink.LinkByName(IfaceName)
	qdisc := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{LinkIndex: link.Attrs().Index, Handle: netlink.MakeHandle(0xffff, 0), Parent: netlink.HANDLE_CLSACT},
		QdiscType:  "clsact",
	}
	netlink.QdiscAdd(qdisc)
	filter := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{LinkIndex: link.Attrs().Index, Parent: netlink.MakeHandle(0xffff, HANDLE_MINIGRESS), Handle: 1, Protocol: syscall.ETH_P_ALL, Priority: 1},
		Fd:          objs.EgressMonitor.FD(),
		Name:        "egress_monitor",
		DirectAction: true,
	}
	netlink.FilterAdd(filter)

	fmt.Printf("🚀 DAWN6G Gateway Active on %s\n", IfaceName)

	// 3. Start Multi-Sensor Loops
	go runActiveMonitoring() // TWAMP & Radio metrics
	
	// 4. Passive Event Loop (eBPF)
	rd, _ := ringbuf.NewReader(objs.Events)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	go func() {
		var event GlitchEvent
		for {
			record, err := rd.Read()
			if err != nil { continue }
			binary.Read(bytes.NewBuffer(record.RawSample), binary.LittleEndian, &event)
			
			if event.Type == 2 {
				fmt.Printf("🔴 [KERNEL] TCP Loss Detected | Dest: %s\n", int2ip(event.DstIP))
			} else {
				fmt.Printf("⚠️  [KERNEL] Stall: %.1fms | Dest: %s\n", float64(event.Value)/1e6, int2ip(event.DstIP))
			}
		}
	}()

	<-stop
	netlink.FilterDel(filter)
}

func runActiveMonitoring() {
	ticker := time.NewTicker(5 * time.Second)
	for range ticker.C {
		// Sensor 1: Radio Metrics (via ModemManager)
		rsrp, rsrq, rssi := getRadioMetrics()

		// Sensor 2: TWAMP Metrics
		avgRTT, jitter, loss := getTwampMetrics()

		fmt.Printf("📊 [STATUS] Radio: %s RSRP / %s RSRQ / %s RSSI | TWAMP: %.1fms RTT / %.1fms Jitter / %.1f%% Loss\n",
			rsrp, rsrq, rssi, avgRTT, jitter, loss)

	}
}

// Now returns 3 strings: RSRP, RSRQ, RSSI
func getRadioMetrics() (string, string, string) {
	out, err := exec.Command("mmcli", "-m", "0", "--signal-get", "--output-keyvalue").Output()
	if err != nil {
		return "ERR", "ERR", "ERR"
	}
	
	m := parseKeyValue(string(out))
	
	// Try 5G first
	rsrp := m["modem.signal.5g.rsrp"]
	rsrq := m["modem.signal.5g.rsrq"]
	rssi := m["modem.signal.5g.rssi"] 

	// Fallback to LTE
	if rsrp == "" || rsrp == "-32768" || rsrp == "-32768.00" {
		rsrp = m["modem.signal.lte.rsrp"] + " (LTE)"
		rsrq = m["modem.signal.lte.rsrq"] + " (LTE)"
		rssi = m["modem.signal.lte.rssi"] + " (LTE)"
	}
	
	// Clean up empty RSSI values (5G often doesn't report traditional RSSI)
	if rssi == "" || strings.HasPrefix(rssi, "-32768") {
		rssi = "N/A"
	}
	
	return rsrp, rsrq, rssi
}

func getTwampMetrics() (float64, float64, float64) {
	// Use CombinedOutput to capture both standard output and error messages (stderr)
	cmd := exec.Command("twampy", "controller", ReflectorAddress, "--count", "5", "--interval", "100")
	out, err := cmd.CombinedOutput() 
	
	if err != nil { 
		// Print EXACTLY why it failed directly to your terminal
		fmt.Printf("🔴 [DEBUG] TWAMP Failed: %v | Output: %s\n", err, string(bytes.TrimSpace(out)))
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
func parseKeyValue(input string) map[string]string {
	res := make(map[string]string)
	lines := strings.Split(input, "\n")
	for _, l := range lines {
		parts := strings.Split(l, " : ")
		if len(parts) == 2 {
			res[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return res
}

func int2ip(nn uint32) net.IP {
	ip := make(net.IP, 4)
	binary.LittleEndian.PutUint32(ip, nn)
	return ip
}
