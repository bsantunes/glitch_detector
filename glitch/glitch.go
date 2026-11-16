// Package glitch loads the eBPF passive stall/retransmission detector
// (bpf_src/tc_glitch.bpf.c, embedded here as tc_glitch.o), attaches it
// to an interface's egress path via a clsact qdisc + BPF filter, and
// decodes events from its ring buffer into a clean Go type. Extracted
// from main.go/dawn-agent so cmd/glitch-tool and cmd/glitch-exporter
// can share the loading/attach/decode logic instead of duplicating it
// — same shape as modem/, twamp/, and gps/. main.go itself is left on
// its own original, unextracted copy of this logic for now, so this
// change carries zero risk to the one piece of this project that can't
// be unit-tested (needs root and real hardware).
package glitch

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// go:embed can only reach files in its own package directory, never a
// parent (`../`) — the Makefile's `glitch/tc_glitch.o` rule copies the
// repo-root-built object here specifically so this works, rather than
// compiling it twice. Run `make glitch/tc_glitch.o` (or `make embed`)
// before `go vet ./...`/`go test ./...` on a fresh checkout — this is
// now a compile-time dependency for the whole module, not just a
// dawn-agent runtime one.
//
//go:embed tc_glitch.o
var bpfObject []byte

// handleMinEgress is the clsact egress hook handle the BPF filter
// attaches to. Moved verbatim from main.go (was HANDLE_MINEGRESS there).
const handleMinEgress = 0xFFF2

// Kind identifies the type of glitch an Event reports.
type Kind uint32

const (
	KindStall   Kind = 1
	KindRetrans Kind = 2
)

func (k Kind) String() string {
	switch k {
	case KindStall:
		return "stall"
	case KindRetrans:
		return "retrans"
	default:
		return "unknown"
	}
}

// Event is one decoded glitch_event from the BPF ring buffer.
//
// Value is the raw stall duration in nanoseconds for KindStall, or the
// retransmission's sequence-number delta for KindRetrans, matching
// main.go's original GlitchEvent's (only partially interpreted there —
// it never printed Value for retrans) usage. Time is wall-clock, not
// the kernel's raw monotonic timestamp — see Open's doc comment for
// how that conversion works.
type Event struct {
	Time    time.Time
	SrcIP   net.IP
	DstIP   net.IP
	SrcPort uint16
	DstPort uint16
	Value   uint64
	Type    Kind
	Proto   uint8
}

// wireEvent mirrors bpf_src/tc_glitch.bpf.c's struct glitch_event
// byte-for-byte — decoded via binary.Read, little-endian. Keep these
// two in sync; see that file's own struct comment. 33 bytes of real
// fields, padded by the compiler to 40 to satisfy the two __u64
// members' 8-byte struct alignment (confirmed via a native-ABI host
// compile, not just hand-counted offsets); binary.Read only consumes
// the fields declared below, so that trailing pad is simply never read
// — see glitch_test.go's TestWireEventDecode for a fixture built from
// the real 40-byte layout, not a Go-to-Go round trip.
type wireEvent struct {
	Ts      uint64
	Value   uint64
	SrcIP   uint32
	DstIP   uint32
	Type    uint32
	SrcPort uint16
	DstPort uint16
	Proto   uint8
}

type bpfObjects struct {
	EgressMonitor *ebpf.Program `ebpf:"egress_monitor"`
	Events        *ebpf.Map     `ebpf:"events"`
}

// Handle owns one loaded+attached instance of the glitch BPF program on
// one interface. Call Close when done to remove the filter and qdisc
// (whichever this Handle actually created — see ownsQdisc) and release
// kernel resources.
type Handle struct {
	lockFile    *os.File
	qdisc       *netlink.GenericQdisc
	ownsQdisc   bool // only true if this Handle's own QdiscAdd created it (not a pre-existing EEXIST)
	filter      *netlink.BpfFilter
	objs        bpfObjects
	rd          *ringbuf.Reader
	clockOffset int64 // atomic: wall_ns - mono_ns, refreshed periodically — see refreshClockOffset

	events  chan Event
	dropped uint64 // atomic

	mu   sync.Mutex
	err  error // non-nil once readLoop sees a real (non-ErrClosed) reader error
	done chan struct{}

	stopOffsetRefresh chan struct{}
	offsetRefreshDone chan struct{}
}

// clockOffsetRefreshInterval is how often Open's background goroutine
// recomputes clockOffset. A one-time offset captured only at Open would
// drift out of sync with reality if the system clock steps after boot
// (e.g. an NTP correction once connectivity comes up) — recomputing it
// periodically keeps later events' wall-clock Time accurate without
// needing per-event clock reads.
const clockOffsetRefreshInterval = 10 * time.Second

// Open loads the embedded BPF object and attaches it to iface's egress
// via a clsact qdisc (reusing one if it already exists — shared qdiscs
// across multiple attached programs are normal) and a BPF filter, then
// starts reading its ring buffer in the background. Call Close when
// done.
//
// Exclusivity is enforced by an flock on /run/glitch-<iface>.lock, held
// for the Handle's lifetime — not by the tc filter's own presence. A tc
// filter is kernel state that outlives the process that installed it:
// after a crash, SIGKILL, or OOM kill, a stale filter would otherwise
// make every later Open fail as if another instance were still running,
// which under a Restart=always service is a permanent restart loop. The
// kernel drops an flock automatically when its holding process dies for
// any reason, so a stale filter no longer looks like a live instance —
// Open takes over a leftover filter via FilterReplace (not FilterAdd)
// once it holds the lock, rather than erroring on it.
//
// Each Event's Time is wall-clock, converted from the BPF program's
// raw bpf_ktime_get_ns() timestamp (CLOCK_MONOTONIC — an arbitrary
// epoch, not meaningful on its own) via an offset: (CLOCK_REALTIME at
// some moment) - (CLOCK_MONOTONIC at that same moment). Both clocks
// tick at the same real-world rate, so a single offset stays accurate
// between recomputations — but it's refreshed every
// clockOffsetRefreshInterval rather than taken once at Open, so a wall
// clock step after boot (e.g. an NTP correction once connectivity
// comes up) doesn't permanently skew every later event's Time.
func Open(iface string) (h *Handle, err error) {
	lockFile, err := os.OpenFile(fmt.Sprintf("/run/glitch-%s.lock", iface), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("glitch: open lock file: %w", err)
	}
	defer func() {
		if err != nil {
			lockFile.Close()
		}
	}()
	if ferr := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); ferr != nil {
		if errors.Is(ferr, unix.EWOULDBLOCK) {
			err = fmt.Errorf("glitch: another glitch monitor is already attached to %s", iface)
		} else {
			err = fmt.Errorf("glitch: flock: %w", ferr)
		}
		return nil, err
	}

	clockOffset, cerr := computeClockOffset()
	if cerr != nil {
		err = fmt.Errorf("glitch: ClockGettime(CLOCK_MONOTONIC): %w", cerr)
		return nil, err
	}

	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(bpfObject))
	if err != nil {
		return nil, fmt.Errorf("glitch: load collection spec: %w", err)
	}

	var objs bpfObjects
	if err = spec.LoadAndAssign(&objs, nil); err != nil {
		return nil, fmt.Errorf("glitch: load and assign: %w", err)
	}
	defer func() {
		if err != nil {
			objs.EgressMonitor.Close()
			objs.Events.Close()
		}
	}()

	link, err := netlink.LinkByName(iface)
	if err != nil {
		return nil, fmt.Errorf("glitch: LinkByName(%s): %w", iface, err)
	}

	qdisc := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{LinkIndex: link.Attrs().Index, Handle: netlink.MakeHandle(0xffff, 0), Parent: netlink.HANDLE_CLSACT},
		QdiscType:  "clsact",
	}
	ownsQdisc := false
	if qerr := netlink.QdiscAdd(qdisc); qerr != nil {
		if !errors.Is(qerr, syscall.EEXIST) {
			err = fmt.Errorf("glitch: QdiscAdd: %w", qerr)
			return nil, err
		}
		// Pre-existing qdisc, not ours — fine to share, but we must not
		// delete it later (see ownsQdisc below).
	} else {
		ownsQdisc = true
	}
	defer func() {
		if err != nil && ownsQdisc {
			netlink.QdiscDel(qdisc)
		}
	}()

	// FilterReplace, not FilterAdd: the flock above is what guarantees
	// exclusivity now, so a filter already present at this exact spot is
	// either ours from last time (impossible — Close always removes it
	// while the lock is held) or a stale leftover from a previous,
	// now-dead holder of the lock (the crash/SIGKILL/OOM case) — take it
	// over rather than erroring on it.
	filter := &netlink.BpfFilter{
		FilterAttrs:  netlink.FilterAttrs{LinkIndex: link.Attrs().Index, Parent: netlink.MakeHandle(0xffff, handleMinEgress), Handle: 1, Protocol: syscall.ETH_P_ALL, Priority: 1},
		Fd:           objs.EgressMonitor.FD(),
		Name:         "egress_monitor",
		DirectAction: true,
	}
	if ferr := netlink.FilterReplace(filter); ferr != nil {
		err = fmt.Errorf("glitch: FilterReplace: %w", ferr)
		return nil, err
	}
	defer func() {
		if err != nil {
			netlink.FilterDel(filter)
		}
	}()

	rd, err := ringbuf.NewReader(objs.Events)
	if err != nil {
		return nil, fmt.Errorf("glitch: ringbuf.NewReader: %w", err)
	}

	handle := &Handle{
		lockFile:          lockFile,
		qdisc:             qdisc,
		ownsQdisc:         ownsQdisc,
		filter:            filter,
		objs:              objs,
		rd:                rd,
		clockOffset:       clockOffset,
		events:            make(chan Event, 256),
		done:              make(chan struct{}),
		stopOffsetRefresh: make(chan struct{}),
		offsetRefreshDone: make(chan struct{}),
	}
	go handle.readLoop()
	go handle.refreshClockOffsetLoop()
	return handle, nil
}

// computeClockOffset returns (CLOCK_REALTIME now) - (CLOCK_MONOTONIC now),
// both read as close together as possible.
func computeClockOffset() (int64, error) {
	var monoNow unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &monoNow); err != nil {
		return 0, err
	}
	return time.Now().UnixNano() - monoNow.Nano(), nil
}

// refreshClockOffsetLoop recomputes clockOffset every
// clockOffsetRefreshInterval until Close stops it. A failed
// ClockGettime is left for the next tick rather than treated as fatal —
// the existing offset simply keeps being used until it succeeds again.
func (h *Handle) refreshClockOffsetLoop() {
	defer close(h.offsetRefreshDone)
	ticker := time.NewTicker(clockOffsetRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopOffsetRefresh:
			return
		case <-ticker.C:
			if offset, err := computeClockOffset(); err == nil {
				atomic.StoreInt64(&h.clockOffset, offset)
			}
		}
	}
}

// readLoop decodes ring buffer records into Events, forwarding them on
// the buffered Events() channel without ever blocking: a full channel
// (a consumer not draining fast enough) is counted via Dropped()
// rather than blocking this goroutine and backing pressure up into the
// kernel ring buffer. This is new plumbing, not a behavior change under
// normal use — the channel's buffer only fills if a consumer falls
// behind, which none of this phase's callers do.
//
// Closes Events() and records any real error (everything except the
// expected ringbuf.ErrClosed from our own Close) in h.err, readable via
// Err() once Events() reports closed.
func (h *Handle) readLoop() {
	defer close(h.events)
	defer close(h.done)
	for {
		rec, err := h.rd.Read()
		if err != nil {
			if !errors.Is(err, ringbuf.ErrClosed) {
				h.mu.Lock()
				h.err = fmt.Errorf("glitch: ring buffer read: %w", err)
				h.mu.Unlock()
			}
			return
		}
		var w wireEvent
		if err := binary.Read(bytes.NewReader(rec.RawSample), binary.LittleEndian, &w); err != nil {
			continue
		}
		ev := Event{
			Time:    time.Unix(0, int64(w.Ts)+atomic.LoadInt64(&h.clockOffset)),
			SrcIP:   int2ip(w.SrcIP),
			DstIP:   int2ip(w.DstIP),
			SrcPort: w.SrcPort,
			DstPort: w.DstPort,
			Value:   w.Value,
			Type:    Kind(w.Type),
			Proto:   w.Proto,
		}
		select {
		case h.events <- ev:
		default:
			atomic.AddUint64(&h.dropped, 1)
		}
	}
}

// Events returns the channel of decoded glitch events. It is closed
// once the ring buffer reader stops — normally only after Close, but
// also on an unexpected read error; check Err() once a receive reports
// the channel closed to tell the two apart.
func (h *Handle) Events() <-chan Event {
	return h.events
}

// Err returns the error that caused the ring buffer reader to stop, if
// any. Always nil until Events() is closed; nil after a normal Close
// too, since that produces the expected ringbuf.ErrClosed, which isn't
// treated as a real error.
func (h *Handle) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// Dropped returns the cumulative count of events dropped because a
// caller wasn't draining Events() fast enough — see readLoop.
func (h *Handle) Dropped() uint64 {
	return atomic.LoadUint64(&h.dropped)
}

// Close removes the BPF filter, and the qdisc too if this Handle is
// the one that created it (see ownsQdisc) — the same order main.go's
// own deferred cleanup already ran in, filter before qdisc — releases
// the loaded program/map and ring buffer reader, and releases the
// exclusivity lock (closing the fd is enough; flock releases on close,
// no explicit LOCK_UN needed) so a subsequent Open can succeed.
func (h *Handle) Close() error {
	close(h.stopOffsetRefresh)
	<-h.offsetRefreshDone

	h.rd.Close() // unblocks readLoop's pending Read() with ringbuf.ErrClosed
	<-h.done

	var errs []error
	if err := netlink.FilterDel(h.filter); err != nil {
		errs = append(errs, fmt.Errorf("FilterDel: %w", err))
	}
	if h.ownsQdisc {
		if err := netlink.QdiscDel(h.qdisc); err != nil {
			errs = append(errs, fmt.Errorf("QdiscDel: %w", err))
		}
	}
	h.objs.EgressMonitor.Close()
	h.objs.Events.Close()
	h.lockFile.Close()
	return errors.Join(errs...)
}

// int2ip converts a little-endian 32-bit integer (as produced by the
// BPF program) into a standard net.IP value. Moved verbatim from
// main.go.
func int2ip(nn uint32) net.IP {
	ip := make(net.IP, 4)
	binary.LittleEndian.PutUint32(ip, nn)
	return ip
}
