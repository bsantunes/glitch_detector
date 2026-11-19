package glitch

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestWireEventDecode decodes a hand-built byte sequence matching the
// real C struct glitch_event layout exactly (45 bytes of real fields,
// 3 bytes of the compiler's own trailing padding to the three __u64
// members' 8-byte struct alignment, 48 total) — not a Go-to-Go round
// trip, which would pass even if wireEvent's field order didn't
// actually match the C struct.
func TestWireEventDecode(t *testing.T) {
	raw := []byte{
		0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01, // ts = 0x0102030405060708 (byte-order-sensitive pattern)
		0x88, 0x77, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11, // value = 0x1122334455667788
		0xef, 0xcd, 0xab, 0x90, 0x78, 0x56, 0x34, 0x12, // baseline_ns = 0x1234567890abcdef
		0x7f, 0x00, 0x00, 0x01, // src_ip = 127.0.0.1, stored the way the kernel already does
		0x08, 0x08, 0x08, 0x08, // dst_ip = 8.8.8.8
		0x03, 0x00, 0x00, 0x00, // type = 3 (stall_end)
		0x07, 0x00, 0x00, 0x00, // steps = 7
		0x39, 0x30, // src_port = 12345
		0xbb, 0x01, // dst_port = 443
		0x06,             // proto = 6 (TCP)
		0x00, 0x00, 0x00, // compiler's trailing pad to 48 bytes — never read
	}
	if len(raw) != 48 {
		t.Fatalf("test fixture is %d bytes, want 48 (sizeof(struct glitch_event) with its natural padding)", len(raw))
	}

	var got wireEvent
	if err := binary.Read(bytes.NewReader(raw), binary.LittleEndian, &got); err != nil {
		t.Fatalf("binary.Read: %v", err)
	}

	want := wireEvent{
		Ts:         0x0102030405060708,
		Value:      0x1122334455667788,
		BaselineNs: 0x1234567890abcdef,
		SrcIP:      0x0100007f,
		DstIP:      0x08080808,
		Type:       uint32(KindStallEnd),
		Steps:      7,
		SrcPort:    12345,
		DstPort:    443,
		Proto:      6,
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestInt2IP(t *testing.T) {
	// int2ip must match the BPF program's own byte order: the kernel
	// stores an IPv4 address's already-network-order bytes into a
	// __u32, and reinterpreting those same bytes little-endian here
	// happens to recover the correct dotted-quad order — same scheme
	// main.go used before this package existed. 127.0.0.1 (asymmetric)
	// actually exercises the byte order; 8.8.8.8 is a sanity check only.
	tests := []struct {
		in   uint32
		want string
	}{
		{0x0100007f, "127.0.0.1"},
		{0x08080808, "8.8.8.8"},
	}
	for _, tt := range tests {
		if got := int2ip(tt.in).String(); got != tt.want {
			t.Errorf("int2ip(0x%08x) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestKindString(t *testing.T) {
	tests := []struct {
		k    Kind
		want string
	}{
		{KindStallStart, "stall_start"},
		{KindRetrans, "retrans"},
		{KindStallEnd, "stall_end"},
		{KindStallAbandoned, "stall_abandoned"},
		{Kind(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.k.String(); got != tt.want {
			t.Errorf("Kind(%d).String() = %q, want %q", tt.k, got, tt.want)
		}
	}
}
