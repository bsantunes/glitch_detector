package glitch

// Source is the common interface both capture backends satisfy —
// *Handle (eBPF, this package) and *afpacket.Capture (AF_PACKET,
// glitch/afpacket) — so cmd/glitch-tool and cmd/glitch-exporter can
// select either one behind the same code via -capture, with no
// special-casing beyond construction. Declared here rather than in
// glitch/afpacket specifically so this package never has to import
// afpacket (which already imports glitch, for Event) — Go interfaces
// are satisfied structurally, so afpacket.Capture implementing this
// needs no import back.
type Source interface {
	Events() <-chan Event
	Dropped() uint64
	// RetransCount returns the cumulative count of TCP retransmissions
	// seen so far — counted directly by whichever capture backend is in
	// use (see glitch.Handle.RetransCount / afpacket.Capture.RetransCount)
	// rather than emitted as Events, which both no longer produce
	// KindRetrans for at all. A caller wanting a per-interval count
	// computes its own delta since the last read, the same way every
	// other cumulative counter here already works.
	RetransCount() uint64
	Err() error
	Close() error
}
