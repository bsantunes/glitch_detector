package glitch

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestWireEventRoundTrip(t *testing.T) {
	want := wireEvent{
		SrcIP: 0x0100007f, // stored little-endian the same way the kernel already does
		DstIP: 0x08080808,
		Value: 150_000_000,
		Type:  uint32(KindStall),
		Proto: 6,
	}
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, want); err != nil {
		t.Fatalf("binary.Write: %v", err)
	}

	var got wireEvent
	if err := binary.Read(bytes.NewReader(buf.Bytes()), binary.LittleEndian, &got); err != nil {
		t.Fatalf("binary.Read: %v", err)
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
		{KindStall, "stall"},
		{KindRetrans, "retrans"},
		{Kind(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.k.String(); got != tt.want {
			t.Errorf("Kind(%d).String() = %q, want %q", tt.k, got, tt.want)
		}
	}
}
