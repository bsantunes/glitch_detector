package twamp

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNTPTimestampRoundTrip(t *testing.T) {
	cases := []time.Time{
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 9, 15, 30, 45, 123456000, time.UTC),
		time.Now(),
	}
	for _, want := range cases {
		got := ntpSeconds(ntpTimestamp(want))
		wantSecs := float64(want.Unix()+ntpEpochOffset) + float64(want.Nanosecond())/1e9
		if math.Abs(got-wantSecs) > 1e-6 {
			t.Errorf("ntpSeconds(ntpTimestamp(%v)) = %v, want %v (diff %v)", want, got, wantSecs, got-wantSecs)
		}
	}
}

func TestSenderPacketLayout(t *testing.T) {
	seq := uint32(42)
	before := time.Now()
	pkt := senderPacket(seq)
	after := time.Now()

	if len(pkt) != senderPacketLen {
		t.Fatalf("senderPacket() length = %d, want %d", len(pkt), senderPacketLen)
	}
	if got := binary.BigEndian.Uint32(pkt[0:4]); got != seq {
		t.Errorf("sequence number = %d, want %d", got, seq)
	}
	ts := ntpSeconds(binary.BigEndian.Uint64(pkt[4:12]))
	lo := float64(before.Unix()+ntpEpochOffset) + float64(before.Nanosecond())/1e9
	hi := float64(after.Unix()+ntpEpochOffset) + float64(after.Nanosecond())/1e9
	if ts < lo-1e-6 || ts > hi+1e-6 {
		t.Errorf("embedded timestamp %v outside [%v, %v]", ts, lo, hi)
	}
	ee := binary.BigEndian.Uint16(pkt[12:14])
	if ee&0xFF == 0 {
		t.Errorf("Error Estimate Multiplier must not be 0, got %#04x", ee)
	}
}

// buildReflectedPacket constructs a synthetic unauthenticated TWAMP-Test
// reflected packet (RFC 5357 §4.2.1) with the given T1 (echoed sender
// timestamp), T2 (reflector receive time) and T3 (reflector send time),
// for testing rttFromReflected without a real network round trip.
func buildReflectedPacket(t1, t2, t3 time.Time) []byte {
	buf := make([]byte, reflectedHeaderLen)
	binary.BigEndian.PutUint32(buf[0:4], 1)                       // reflector's own seq, irrelevant here
	binary.BigEndian.PutUint64(buf[4:12], ntpTimestamp(t3))       // Timestamp (T3)
	binary.BigEndian.PutUint16(buf[12:14], 0x0001)                // Error Estimate
	// buf[14:16] MBZ, left zero
	binary.BigEndian.PutUint64(buf[16:24], ntpTimestamp(t2))      // Receive Timestamp (T2)
	binary.BigEndian.PutUint32(buf[24:28], 0)                     // Sender Sequence Number
	binary.BigEndian.PutUint64(buf[28:36], ntpTimestamp(t1))      // Sender Timestamp (T1)
	binary.BigEndian.PutUint16(buf[36:38], 0x0001)                // Sender Error Estimate
	// buf[38:40] MBZ, buf[40] Sender TTL, left zero
	return buf
}

func TestRTTFromReflected(t *testing.T) {
	// Fixed base time (not time.Now()) so the test is reproducible and
	// doesn't depend on which fractional-nanosecond instant it happens
	// to run at.
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 500_000_000, time.UTC)
	t2 := t1.Add(12 * time.Millisecond) // one-way trip out took 12ms
	t3 := t2.Add(5 * time.Millisecond)  // reflector took 5ms to turn it around
	t4 := t1.Add(30 * time.Millisecond) // we received the reply 30ms after sending

	pkt := buildReflectedPacket(t1, t2, t3)
	gotRTT, ok := rttFromReflected(pkt, t4)
	if !ok {
		t.Fatalf("rttFromReflected() ok = false, want true")
	}

	// RTT = (T4-T1) - (T3-T2) = 30ms - 5ms = 25ms: the reflector's own
	// 5ms processing delay is subtracted out, leaving pure network time.
	// Tolerance accounts for the NTP format's 2^-32s fixed-point
	// quantization (each encode/decode can round by a fraction of a
	// microsecond) — utterly negligible at the millisecond scale this is
	// actually used at.
	const want = 25.0
	if math.Abs(gotRTT-want) > 1e-3 {
		t.Errorf("rttFromReflected() = %.6fms, want %.6fms", gotRTT, want)
	}
}

func TestRTTFromReflected_TooShort(t *testing.T) {
	if _, ok := rttFromReflected(make([]byte, reflectedHeaderLen-1), time.Now()); ok {
		t.Errorf("rttFromReflected() with a too-short packet: ok = true, want false")
	}
}

func TestSummarize(t *testing.T) {
	cases := []struct {
		name string
		sent int
		rtts []float64
		want Result
	}{
		{
			name: "no loss, varying RTT",
			sent: 3,
			rtts: []float64{10, 12, 8},
			want: Result{Sent: 3, Received: 3, AvgRTT: 10, Jitter: 3, LossPct: 0},
		},
		{
			name: "partial loss",
			sent: 5,
			rtts: []float64{10, 10},
			want: Result{Sent: 5, Received: 2, AvgRTT: 10, Jitter: 0, LossPct: 60},
		},
		{
			name: "total loss",
			sent: 5,
			rtts: nil,
			want: Result{Sent: 5, Received: 0, AvgRTT: 0, Jitter: 0, LossPct: 100},
		},
		{
			name: "single sample, no jitter possible",
			sent: 1,
			rtts: []float64{7},
			want: Result{Sent: 1, Received: 1, AvgRTT: 7, Jitter: 0, LossPct: 0},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summarize(tc.sent, tc.rtts)
			if got != tc.want {
				t.Errorf("summarize(%d, %v) = %+v, want %+v", tc.sent, tc.rtts, got, tc.want)
			}
		})
	}
}

// startFakeReflector runs a minimal TWAMP-Light reflector on 127.0.0.1:
// for each sender packet it echoes the sender's sequence and timestamp
// (T1) back in a reflected packet, with T2/T3 = now. Returns its address.
func startFakeReflector(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < senderPacketLen {
				continue
			}
			now := time.Now()
			out := make([]byte, reflectedHeaderLen)
			binary.BigEndian.PutUint64(out[4:12], ntpTimestamp(now))
			binary.BigEndian.PutUint16(out[12:14], 0x0001)
			binary.BigEndian.PutUint64(out[16:24], ntpTimestamp(now))
			copy(out[24:28], buf[0:4])  // sender sequence
			copy(out[28:36], buf[4:12]) // sender timestamp (T1)
			pc.WriteTo(out, addr)
		}
	}()
	return pc.LocalAddr().String()
}

func TestMeasure_LoopbackReflector(t *testing.T) {
	addr := startFakeReflector(t)
	res, err := Measure(context.Background(), addr, "", 3, 10*time.Millisecond, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if res.Sent != 3 || res.Received != 3 || res.LossPct != 0 {
		t.Errorf("result = %+v, want 3/3 received, 0%% loss", res)
	}
}

func TestMeasure_BindDevLoopback(t *testing.T) {
	// Pinned to "lo", probes to a 127.0.0.1 reflector must still get
	// through: the bind-dev path must not break a working route.
	addr := startFakeReflector(t)
	res, err := Measure(context.Background(), addr, "lo", 3, 10*time.Millisecond, 500*time.Millisecond)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("SO_BINDTODEVICE needs CAP_NET_RAW on this kernel: %v", err)
		}
		t.Fatalf("Measure with bind-dev lo: %v", err)
	}
	if res.Received != 3 {
		t.Errorf("result = %+v, want 3/3 received", res)
	}
}

func TestMeasure_BindDevUnknownInterface(t *testing.T) {
	// A missing interface must fail the dial with a clear error, not
	// silently fall back to normal routing (which would measure the
	// wrong path without anyone noticing).
	addr := startFakeReflector(t)
	_, err := Measure(context.Background(), addr, "nosuchdev0", 1, 10*time.Millisecond, 100*time.Millisecond)
	if err == nil {
		t.Fatalf("Measure with bind-dev nosuchdev0 = nil error, want a failure")
	}
	if !strings.Contains(err.Error(), "nosuchdev0") {
		t.Errorf("error %q should name the interface", err)
	}
}
