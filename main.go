package main

import (
	"bufio"
	"bytes"
	"context"
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

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/vishvananda/netlink"
)

const (
	IfaceName        = "wwan0" // 5G Interface
	ReflectorAddress = "dawn6g.duckdns.org"
	InfluxURL        = "http://dawn6g.duckdns.org:8086"
	InfluxToken      = "YOUR_TOKEN_HERE"
	Org              = "dawn6g"
	Bucket           = "gateway_telemetry"
)

type GlitchEvent struct {
	SrcIP uint32
	DstIP uint32
	Value uint64
	Type  uint32
	Proto uint8
}

func main() {
	// --- 1. SETUP INFLUXDB ---
	client := influxdb2.NewClient(InfluxURL, InfluxToken)
	writeAPI := client.WriteAPIBlocking(Org, Bucket)
	defer client.Close()

	// --- 2. SETUP eBPF ---
	spec, err := ebpf.LoadCollectionSpec("tc_glitch.o")
	if err != nil {
		log.Fatalf("Failed to load BPF: %v", err)
	}
	objs := struct {
		EgressMonitor *ebpf.Program `ebpf:"egress_monitor"`
		Events        *ebpf.Map     `ebpf:"events"`
	}{}
	spec.LoadAndAssign(&objs, nil)

	// --- 3. ATTACH TO INTERFACE ---
	link, _ := netlink.LinkByName(IfaceName)
	qdisc := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{LinkIndex: link.Attrs().Index, Handle: netlink.MakeHandle(0xffff, 0), Parent: netlink.HANDLE_CLSACT},
		QdiscType:  "clsact",
	}
	netlink.QdiscAdd(qdisc)
	filter := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{LinkIndex: link.Attrs().Index, Parent: netlink.MakeHandle(0xffff, 0xfff3), Handle: 1, Protocol: syscall.ETH_P_ALL, Priority: 1},
		Fd:          objs.EgressMonitor.FD(),
		DirectAction: true,
	}
	netlink.FilterAdd(filter)

	fmt.Println("🛰️  DAWN6G Unified Agent Running...")

	// --- 4. START MULTI-SENSOR LOOPS ---
	go runActiveMonitoring(writeAPI)
	
	// eBPF Event Reader
	rd, _ := ringbuf.NewReader(objs.Events)
	for {
		record, err := rd.Read()
		if err != nil { continue }
		var event GlitchEvent
		binary.Read(bytes.NewBuffer(record.RawSample), binary.LittleEndian, &event)
		
		eventType := "stall"
		if event.Type == 2 { eventType = "retrans" }

		// Log Glitch to Influx
		p := influxdb2.NewPoint("glitch_events",
			map[string]string{"type": eventType, "proto": fmt.Sprint(event.Proto)},
			map[string]interface{}{"value": float64(event.Value)},
			time.Now())
		writeAPI.WritePoint(context.Background(), p)
	}
}

func handleRoaming(currentPLMN string) {
    privatePLMN := "999-01" // Example Private 5G PLMN
    
    if currentPLMN == privatePLMN {
        fmt.Println("🏰 [ROAMING] Connected to Private NPN. Activating Ultra-Low Latency Mode.")
        // Here you might trigger higher-frequency telemetry
    } else {
        fmt.Println("🌐 [ROAMING] Connected to Public Carrier. Engaging Data Compression.")
        // Here you might throttle non-essential traffic to save on roaming costs
    }
}

func runActiveMonitoring(writeAPI api.WriteAPIBlocking) {
	ticker := time.NewTicker(10 * time.Second)
	profiles := []string{"be", "ef"}

	for range ticker.C {
		// A. Radio Metrics
		out, _ := exec.Command("mmcli", "-m", "0", "--signal-get", "--output-keyvalue").Output()
		metrics := parseKV(string(out))
		
		// B. TWAMP per Profile (Mapping to 5QI)
		for _, pStr := range profiles {
			cmd := exec.Command("twampy", "controller", ReflectorAddress, "--count", "5", "--dscp", pStr)
			twOut, _ := cmd.Output()
			rtt, jit, loss := parseTwamp(twOut)

			// C. PUSH CONSOLIDATED METRIC
			p := influxdb2.NewPoint("network_health",
				map[string]string{
					"profile": pStr,
					"operator": metrics["modem.generic.operator-name"],
					"cell_id":  metrics["modem.signal.5g.cell-id"],
				},
				map[string]interface{}{
					"rtt": rtt, "jitter": jit, "loss": loss,
					"rsrp": parseToFloat(metrics["modem.signal.5g.rsrp"]),
					"rsrq": parseToFloat(metrics["modem.signal.5g.rsrq"]),
				}, time.Now())
			writeAPI.WritePoint(context.Background(), p)
		}
	}
}

// Helpers for parsing (Simplified for brevity)
func parseTwamp(out []byte) (float64, float64, float64) {
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "Roundtrip:") {
			f := strings.Fields(scanner.Text())
			if len(f) >= 6 {
				r, _ := time.ParseDuration(f[3])
				j, _ := time.ParseDuration(f[4])
				l, _ := strconv.ParseFloat(strings.TrimSuffix(f[5], "%"), 64)
				return float64(r.Milliseconds()), float64(j.Microseconds()) / 1000.0, l
			}
		}
	}
	return 0, 0, 0
}

func getDetailedModemStats() (string, string) {
    // Querying SIMCom specific QoS status
    // AT+CGACT? shows which contexts are active
    // AT+CGCONTRDP=2 shows the assigned QoS for context 2
    out, _ := exec.Command("mmcli", "-m", "0", "--command=AT+CGCONTRDP=2").Output()
    
    // We look for the 5QI value in the response
    // Response format: +CGCONTRDP: 2,5,"apn",...
    // The second number (5) is often the assigned QCI/5QI
    return parse5QI(string(out))
}

func parseKV(in string) map[string]string {
	res := make(map[string]string)
	for _, l := range strings.Split(in, "\n") {
		parts := strings.Split(l, " : ")
		if len(parts) == 2 { res[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1]) }
	}
	return res
}

func parseToFloat(s string) float64 {
	f, _ := strconv.ParseFloat(strings.Trim(s, " dBm%"), 64)
	return f
}
