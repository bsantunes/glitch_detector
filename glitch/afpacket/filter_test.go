package afpacket

import (
	"encoding/binary"
	"testing"

	"golang.org/x/net/bpf"
)

// runFilter assembles buildFilter(udpPorts) and runs it against pkt
// through golang.org/x/net/bpf's own software VM — no root, no real
// socket, no kernel involved. This is what makes hand-written classic
// BPF (no named jump targets, only "skip N instructions" offsets)
// tractable to get right: the same safety net this project has
// relied on throughout (bpftool prog run for the eBPF side; here, an
// equivalent for the classic BPF side).
func runFilter(t *testing.T, udpPorts []uint16, pkt []byte) int {
	t.Helper()
	insts, err := buildFilter(udpPorts)
	if err != nil {
		t.Fatalf("buildFilter(%v): %v", udpPorts, err)
	}
	vm, err := bpf.NewVM(insts)
	if err != nil {
		t.Fatalf("bpf.NewVM: %v", err)
	}
	n, err := vm.Run(pkt)
	if err != nil {
		t.Fatalf("vm.Run: %v", err)
	}
	return n
}

// ipHeader builds a minimal 20-byte IPv4 header (IHL=5, no options).
func ipHeader(proto byte) []byte {
	h := make([]byte, 20)
	h[0] = 0x45 // version 4, IHL 5
	h[9] = proto
	return h
}

func tcpPacket(srcPort, dstPort uint16) []byte {
	ip := ipHeader(tcpProto)
	tcp := make([]byte, 20)
	binary.BigEndian.PutUint16(tcp[0:2], srcPort)
	binary.BigEndian.PutUint16(tcp[2:4], dstPort)
	tcp[12] = 5 << 4 // doff=5, no options
	return append(ip, tcp...)
}

func udpPacket(srcPort, dstPort uint16) []byte {
	ip := ipHeader(udpProto)
	udp := make([]byte, 8)
	binary.BigEndian.PutUint16(udp[0:2], srcPort)
	binary.BigEndian.PutUint16(udp[2:4], dstPort)
	return append(ip, udp...)
}

func TestBuildFilter_TCPAlwaysAccepted(t *testing.T) {
	for _, udpPorts := range [][]uint16{nil, {4000}, {4000, 5000}} {
		if n := runFilter(t, udpPorts, tcpPacket(51000, 443)); n == 0 {
			t.Errorf("udpPorts=%v: TCP packet rejected, want accepted", udpPorts)
		}
	}
}

func TestBuildFilter_UDPRejectedWithNoPortsConfigured(t *testing.T) {
	if n := runFilter(t, nil, udpPacket(4000, 9999)); n != 0 {
		t.Errorf("got accepted (n=%d) for UDP with no monitored ports, want rejected", n)
	}
}

func TestBuildFilter_UDPAcceptedOnSrcOrDstPortMatch(t *testing.T) {
	ports := []uint16{4000}
	if n := runFilter(t, ports, udpPacket(4000, 9999)); n == 0 {
		t.Errorf("src port 4000 matches a monitored port, expected accepted")
	}
	if n := runFilter(t, ports, udpPacket(9999, 4000)); n == 0 {
		t.Errorf("dst port 4000 matches a monitored port, expected accepted")
	}
	if n := runFilter(t, ports, udpPacket(1111, 2222)); n != 0 {
		t.Errorf("neither port matches, expected rejected, got n=%d", n)
	}
}

func TestBuildFilter_UDPMultiplePortsEachMatchable(t *testing.T) {
	ports := []uint16{4000, 5000, 6000}
	for _, p := range ports {
		if n := runFilter(t, ports, udpPacket(p, 1)); n == 0 {
			t.Errorf("src port %d is in the monitored list, expected accepted", p)
		}
		if n := runFilter(t, ports, udpPacket(1, p)); n == 0 {
			t.Errorf("dst port %d is in the monitored list, expected accepted", p)
		}
	}
	if n := runFilter(t, ports, udpPacket(1, 2)); n != 0 {
		t.Errorf("no port matches, expected rejected, got n=%d", n)
	}
}

func TestBuildFilter_OtherProtocolsRejected(t *testing.T) {
	// ICMP (proto 1) — never TCP, never UDP, must never pass.
	pkt := ipHeader(1)
	if n := runFilter(t, []uint16{4000}, pkt); n != 0 {
		t.Errorf("ICMP packet accepted (n=%d), want rejected", n)
	}
}

func TestBuildFilter_SnapLen(t *testing.T) {
	n := runFilter(t, nil, tcpPacket(1, 2))
	if n != snapLen {
		t.Errorf("accepted length = %d, want snapLen (%d)", n, snapLen)
	}
}
