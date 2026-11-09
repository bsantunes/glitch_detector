// Package twamp implements a native TWAMP-Test sender (RFC 5357,
// unauthenticated mode — "TWAMP Light": no TWAMP-Control handshake,
// just direct UDP TWAMP-Test packets against a reflector that's
// already listening, e.g. `twampy reflector`). It exists so RTT/
// jitter/loss probing no longer depends on exec'ing the external
// twampy Python tool, which isn't installable on OpenWrt/WaveOS at all
// (no Python runtime in the image, confirmed on both WaveOS boards in
// this fleet).
//
// Packet formats and the T1/T2/T3/T4 RTT formula are per RFC 5357
// §4.1/§4.2 (sender/reflected packets) and RFC 4656 §4.1.1 (NTP-style
// timestamp). T2/T3 come from the reflector's own clock and are only
// ever used as a difference (its internal processing delay), so the
// sender and reflector clocks do not need to be synchronized for a
// correct RTT — only T1/T4 (both from the sender's own clock) need to
// be consistent with each other, which they are by construction.
package twamp

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

const (
	// ntpEpochOffset is the number of seconds between the NTP epoch
	// (1900-01-01) and the Unix epoch (1970-01-01).
	ntpEpochOffset = 2208988800

	// senderPacketLen is the 14-octet fixed sender header (Sequence
	// Number, Timestamp, Error Estimate) with NO padding. RFC 5357
	// states a 41-octet minimum (14 header + 27 padding) for
	// unauthenticated mode, but this project's actual reflector
	// (twampy) sends and expects exactly 14 bytes — confirmed by
	// straceing `twampy sender` against the real reflector at
	// 10.30.30.1:862 (live, 2026-11-09). Matching twampy's own
	// on-the-wire behavior, not the RFC's stated minimum, is what
	// actually interoperates here.
	senderPacketLen = 14

	// reflectedHeaderLen is the reflected-packet length this project's
	// reflector actually sends: 38 bytes, not RFC 5357's full 41 —
	// it omits the trailing Sender TTL + MBZ (3 bytes). Also confirmed
	// live: it reports an identical Timestamp and Receive Timestamp
	// (zero internal processing delay), which the RTT formula handles
	// fine either way.
	reflectedHeaderLen = 38
)

// Result summarizes one burst of TWAMP-Test probes.
type Result struct {
	Sent     int
	Received int
	// AvgRTT is the mean round-trip time across received replies, in ms.
	AvgRTT float64
	// Jitter is the mean absolute difference between consecutive RTT
	// samples (in send order), in ms — a simple, easily-reasoned-about
	// inter-packet delay variation estimate, not RFC 3550's recursive
	// estimator.
	Jitter float64
	// LossPct is the percentage of probes that got no reply at all.
	LossPct float64
}

// Measure sends count TWAMP-Test probes to reflectorAddr (UDP
// "host:port"), probeInterval apart, and waits up to timeout after the
// last one for trailing replies. reflectorAddr must already be a
// TWAMP-Test reflector listening directly on that UDP port — there's
// no TWAMP-Control session setup here, matching how this project's
// reflector is actually run (`twampy reflector`, a fixed UDP port).
func Measure(ctx context.Context, reflectorAddr string, count int, probeInterval, timeout time.Duration) (Result, error) {
	if count <= 0 {
		return Result{}, fmt.Errorf("count must be positive, got %d", count)
	}

	raddr, err := net.ResolveUDPAddr("udp", reflectorAddr)
	if err != nil {
		return Result{}, fmt.Errorf("resolve %s: %w", reflectorAddr, err)
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return Result{}, fmt.Errorf("dial %s: %w", reflectorAddr, err)
	}
	defer conn.Close()

	// Force any blocked Read to return promptly if the caller cancels.
	go func() {
		<-ctx.Done()
		conn.SetReadDeadline(time.Now())
	}()

	go sendBurst(ctx, conn, count, probeInterval)

	deadline := time.Now().Add(time.Duration(count-1)*probeInterval + timeout)
	conn.SetReadDeadline(deadline)

	var rtts []float64
	buf := make([]byte, 512)
	for len(rtts) < count {
		n, err := conn.Read(buf)
		if err != nil {
			break // deadline hit (or ctx cancelled) — stop waiting for stragglers
		}
		rtt, ok := rttFromReflected(buf[:n], time.Now())
		if !ok {
			continue // not a well-formed reflected packet; ignore and keep listening
		}
		rtts = append(rtts, rtt)
	}

	return summarize(count, rtts), nil
}

// sendBurst writes count TWAMP-Test sender packets, probeInterval
// apart. Runs in its own goroutine so sends and receives overlap
// (matching how a real burst probe behaves) rather than waiting for
// each reply before sending the next packet.
func sendBurst(ctx context.Context, conn *net.UDPConn, count int, probeInterval time.Duration) {
	for seq := uint32(0); seq < uint32(count); seq++ {
		if ctx.Err() != nil {
			return
		}
		conn.Write(senderPacket(seq))
		if seq+1 < uint32(count) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(probeInterval):
			}
		}
	}
}

// senderPacket builds an unauthenticated TWAMP-Test sender packet
// (RFC 5357 §4.1.2 / RFC 4656 §4.1.2): Sequence Number, Timestamp,
// Error Estimate, then zero-filled padding to the 41-octet minimum.
func senderPacket(seq uint32) []byte {
	buf := make([]byte, senderPacketLen)
	binary.BigEndian.PutUint32(buf[0:4], seq)
	binary.BigEndian.PutUint64(buf[4:12], ntpTimestamp(time.Now()))
	// Error Estimate: S=0 (not synchronized to an external clock), Z=0,
	// Scale=0, Multiplier=1 (MUST NOT be 0 per RFC 4656). Not used by our
	// own RTT calculation; set to a valid, honest value for any other
	// receiver that does look at it.
	binary.BigEndian.PutUint16(buf[12:14], 0x0001)
	return buf
}

// rttFromReflected parses an unauthenticated TWAMP-Test reflected
// packet (RFC 5357 §4.2.1) and computes RTT in milliseconds:
// RTT = (T4-T1) - (T3-T2), where T1/T4 are the sender's own clock
// (T1 echoed back by the reflector, T4 = now, the caller's receive
// time) and T2/T3 are the reflector's clock (receive/transmit — only
// their difference, the reflector's internal processing delay, is
// used). now is passed in (rather than read via time.Now() here) so
// this stays deterministically testable.
func rttFromReflected(b []byte, now time.Time) (rttMs float64, ok bool) {
	if len(b) < reflectedHeaderLen {
		return 0, false
	}
	t3 := binary.BigEndian.Uint64(b[4:12])  // reflector Timestamp (its send time)
	t2 := binary.BigEndian.Uint64(b[16:24]) // reflector Receive Timestamp
	t1 := binary.BigEndian.Uint64(b[28:36]) // echoed Sender Timestamp
	t4 := ntpTimestamp(now)

	rtt := (ntpSeconds(t4) - ntpSeconds(t1)) - (ntpSeconds(t3) - ntpSeconds(t2))
	return rtt * 1000, true
}

// ntpTimestamp encodes t as a 64-bit NTP-style timestamp (RFC 1305 /
// RFC 4656 §4.1.1): 32-bit seconds since the NTP epoch (1900-01-01),
// 32-bit fraction in units of 2^-32s.
func ntpTimestamp(t time.Time) uint64 {
	secs := uint64(t.Unix() + ntpEpochOffset)
	frac := uint64(float64(t.Nanosecond()) / 1e9 * (1 << 32))
	return secs<<32 | frac
}

// ntpSeconds converts a raw 64-bit NTP timestamp to seconds (float64).
// Only differences between two such values are used (never an absolute
// time), so the large epoch offset doesn't cost meaningful precision:
// float64 keeps sub-microsecond resolution even at ~2^32-magnitude
// second counts.
func ntpSeconds(v uint64) float64 {
	secs := float64(v >> 32)
	frac := float64(v&0xFFFFFFFF) / float64(uint64(1)<<32)
	return secs + frac
}

// summarize computes Result stats from the RTT samples actually
// received, in the order they arrived.
func summarize(sent int, rtts []float64) Result {
	r := Result{Sent: sent, Received: len(rtts)}
	if sent > 0 {
		r.LossPct = float64(sent-len(rtts)) / float64(sent) * 100
	}
	if len(rtts) == 0 {
		return r
	}

	sum := 0.0
	for _, v := range rtts {
		sum += v
	}
	r.AvgRTT = sum / float64(len(rtts))

	if len(rtts) > 1 {
		var jitterSum float64
		for i := 1; i < len(rtts); i++ {
			d := rtts[i] - rtts[i-1]
			if d < 0 {
				d = -d
			}
			jitterSum += d
		}
		r.Jitter = jitterSum / float64(len(rtts)-1)
	}
	return r
}
