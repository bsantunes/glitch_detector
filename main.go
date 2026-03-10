package main
// Package main implements the DAWN6G glitch‑detector gateway.
// It attaches an eBPF program to a network interface to passively
// detect stalls and losses, and concurrently runs “active” probes
// (TWAMP and modem radio metrics) for richer telemetry.
//
// Usage:
//   go run main.go
//
// The eBPF object (tc_glitch.o) must be built beforehand; see
// the project README for build instructions.
//
// The default interface is wwan0; change IfaceName to suit your
// environment.  Passive events are logged with emojis, active
// stats are printed every 5 seconds.
// Package main implements the DAWN6G glitch-detector gateway.
// It combines passive kernel instrumentation via an eBPF program
// with active measurements (TWAMP and radio signal metrics) to
// provide real-time telemetry and alerting when the 5G link
// exhibits stalls or packet loss.
//
// The eBPF object (tc_glitch.o) must be built beforehand; see the
// project README for build instructions. By default the code
// attaches to interface "wwan0" (a 5G modem); change IfaceName as
// necessary for your environment.
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
	// IfaceName is the network interface the eBPF program will
	// attach to. On a cellular gateway this is usually the modem
	// interface (wwan0 for 5G). Change this if your device differs.
	IfaceName        = "wwan0" // Change to wwan0 for 5G

	// Address of the TWAMP reflector we probe for active RTT
	// measurements.
	ReflectorAddress = "dawn6g.duckdns.org"

	// Magic handle number used when installing the clsact ingress
	// filter for eBPF. This value comes from the BPF program logic.
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

// runActiveMonitoring periodically gathers active telemetry from
// the modem (rsrp/rsrq/rssi) and from a TWAMP reflector.  It
// prints a human-readable status line every 5 seconds.
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
// getRadioMetrics queries ModemManager for signal strength values.
// It prefers 5G metrics, falling back to LTE if the 5G fields are
// empty or contain the -32768 sentinel value (meaning "no data").
// RSSI is optional on 5G and may be reported as -32768 as well.
// Returns three user-facing strings for RSRP, RSRQ, and RSSI.
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

// getTwampMetrics runs a TWAMP controller against our reflector and
// parses the printed summary line.  On error it reports 100% loss so
// that the status output clearly indicates a problem.
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
// parseKeyValue converts lines of the form "key : value" (as
// emitted by mmcli with --output-keyvalue) into a map.  Blank lines
// and malformed entries are ignored.
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

// int2ip converts a little‑endian 32‑bit integer (as produced by the
// BPF program) into a standard net.IP value.
func int2ip(nn uint32) net.IP {
	ip := make(net.IP, 4)
	binary.LittleEndian.PutUint32(ip, nn)
	return ip
}
