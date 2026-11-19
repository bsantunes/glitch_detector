package afpacket

import (
	"golang.org/x/net/bpf"
)

// These offsets are relative to the start of the IP header — the
// packets this filter runs against are raw IP with no Ethernet
// header (same "5G modems are RAW IP" assumption tc_glitch.bpf.c
// itself makes), fed to the kernel filter after this package's own
// 16-byte Linux "cooked capture" (SLL) pseudo-header has already been
// skipped (see capture.go).
const (
	ipProtoOff = 9 // ip->protocol
	tcpProto   = 6
	udpProto   = 17
	// snapLen is how many bytes of a matching packet the kernel keeps
	// — more than enough for the IP + TCP/UDP headers this project
	// ever reads (tc_glitch.bpf.c only ever reads up to the L4
	// header, never payload), per the brief's "truncate to 128 bytes".
	snapLen = 128
)

// buildFilter assembles a classic BPF program equivalent to: keep a
// packet if it's TCP, or if it's UDP and either its source or
// destination port is in udpPorts — the same TCP-always/UDP-opt-in
// split tc_glitch.bpf.c's own UDP_MONITOR_PORTS makes, just enforced
// in the kernel filter here instead of after a full copy to
// userspace, which is the whole point of attaching a filter at all.
// An empty udpPorts drops all UDP outright — if no port is opted in
// there is nothing useful for glitch/core to do with a UDP packet,
// same conclusion its own udpPortMonitored reaches, so there is no
// reason to pay for capturing it in the first place.
//
// Unlike tc_glitch.bpf.c's fixed-size, load-time UDP_MONITOR_PORTS
// array, udpPorts here is an ordinary runtime slice — this filter is
// built fresh at startup for whatever ports were configured, not a
// compile-time constant.
//
// Returns the typed instruction list, not yet assembled — callers
// that need raw instructions for a real kernel attach (capture.go)
// call bpf.Assemble on the result themselves; filter_test.go instead
// feeds it straight to bpf.NewVM, which is what makes getting this
// classic-BPF-by-hand right at all tractable — a classic BPF program
// has no named jump targets, only "skip N instructions" offsets
// computed by hand below. (bpf.NewVM specifically needs the typed
// Instruction values, not a round-tripped RawInstruction, to
// recognize the terminal RetConstant — confirmed by the VM refusing a
// pre-assembled program with "must end with RetA or RetConstant"
// during this package's own development.)
func buildFilter(udpPorts []uint16) ([]bpf.Instruction, error) {
	var insts []bpf.Instruction

	insts = append(insts, bpf.LoadAbsolute{Off: ipProtoOff, Size: 1})

	if len(udpPorts) == 0 {
		// acceptIdx/dropIdx computed directly: 4 instructions total.
		insts = append(insts,
			bpf.JumpIf{Cond: bpf.JumpEqual, Val: tcpProto, SkipTrue: 1}, // idx1: TCP -> skip to ACCEPT (idx3)
			bpf.RetConstant{Val: 0},                                    // idx2: DROP
			bpf.RetConstant{Val: snapLen},                               // idx3: ACCEPT
		)
		return insts, nil
	}

	// Full layout with n UDP ports:
	//   idx0  LoadAbsolute proto
	//   idx1  JumpIf proto==TCP  -> ACCEPT
	//   idx2  JumpIf proto==UDP  -> fall through to idx3 (true=0 skip); false -> DROP
	//   idx3  LoadMemShift (X = IHL*4)
	//   idx4  LoadIndirect src port
	//   idx5..idx5+n-1   JumpIf srcport==port[i] -> ACCEPT
	//   idx5+n           LoadIndirect dst port
	//   idx5+n+1..idx5+2n JumpIf dstport==port[i] -> ACCEPT
	//   idx5+2n+1        DROP
	//   idx5+2n+2        ACCEPT
	n := len(udpPorts)
	dropIdx := 5 + 2*n + 1
	acceptIdx := dropIdx + 1

	skipTo := func(fromIdx, targetIdx int) uint8 {
		return uint8(targetIdx - (fromIdx + 1))
	}

	insts = append(insts,
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: tcpProto, SkipTrue: skipTo(1, acceptIdx)}, // idx1
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: udpProto, SkipTrue: 0, SkipFalse: skipTo(2, dropIdx)}, // idx2
		bpf.LoadMemShift{Off: 0},          // idx3
		bpf.LoadIndirect{Off: 0, Size: 2}, // idx4: src port
	)
	for i, port := range udpPorts {
		idx := 5 + i
		insts = append(insts, bpf.JumpIf{Cond: bpf.JumpEqual, Val: uint32(port), SkipTrue: skipTo(idx, acceptIdx)})
	}
	insts = append(insts, bpf.LoadIndirect{Off: 2, Size: 2}) // idx 5+n: dst port
	for i, port := range udpPorts {
		idx := 5 + n + 1 + i
		insts = append(insts, bpf.JumpIf{Cond: bpf.JumpEqual, Val: uint32(port), SkipTrue: skipTo(idx, acceptIdx)})
	}
	insts = append(insts,
		bpf.RetConstant{Val: 0},       // dropIdx
		bpf.RetConstant{Val: snapLen}, // acceptIdx
	)
	if len(insts) != acceptIdx+1 {
		panic("buildFilter: instruction count/index math is inconsistent — fix the layout comment and the code together")
	}
	return insts, nil
}
