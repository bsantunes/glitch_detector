// Package afpacket is the AF_PACKET/TPACKET_V3 equivalent of package
// glitch's eBPF-based capture, for boards with no clsact/TC-BPF kernel
// support at all — confirmed on WaveOS (see CLAUDE.md's glitch/
// package section). Decodes just enough of each packet to build a
// glitch/core.PacketMeta and feeds it to a core.Engine, which runs
// the identical stall/retransmission algorithm tc_glitch.bpf.c runs
// in the kernel on Debian gateways.
//
// Must build CGO_ENABLED=0: everything here is golang.org/x/sys/unix
// raw syscalls plus golang.org/x/net/bpf for the classic BPF filter —
// no libpcap, no cgo. gopacket's own afpacket package was tried first
// and rejected: its pinned release (v1.1.19) fails to build at all
// ("undefined: pageSize" — nothing in that package defines it), a
// genuine upstream gap, not a config issue on our end.
package afpacket

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"

	"dawn-agent/glitch"
	"dawn-agent/glitch/core"
)

func htons(h uint16) uint16 { return h<<8&0xff00 | h>>8 }

const (
	// Confirmed live (aaeon, x86_64) via a byte-level dump of real
	// TPACKET_V3 ring frames on wwan0 — a raw-IP interface, same
	// class as the WaveOS boards' lteN interfaces this package
	// targets — not assumed from documentation alone, which doesn't
	// clearly state this for the ring/mmap capture path specifically
	// (as opposed to the better-documented recvfrom()-based path). A
	// populated struct sockaddr_ll — including sll_pkttype, exactly
	// what distinguishes PACKET_OUTGOING (egress) from PACKET_HOST
	// (ingress) — sits at a fixed offset immediately after each
	// packet's own tpacket3_hdr (not prepended to the captured packet
	// bytes at the mac/net offset the way a "cooked" capture's
	// userspace-facing data normally would carry it): family(2) +
	// protocol(2) + ifindex(4) + hatype(2) + pkttype(1) — hence +10.
	// The ring frame's own mac/net offset (96 on this fleet) already
	// points straight at the real IP header with no synthetic L2/SLL
	// header to skip past — this device has none, mac == net.
	sockaddrLLOffset = unix.SizeofTpacket3Hdr // 48
	pkttypeOffset    = sockaddrLLOffset + 10

	tpacketBlockHdrLen = 48 // version(4) + offset_to_priv(4) + tpacket_hdr_v1(40)

	// defaultBlockSize/defaultBlockNr/defaultFrameSize size the ring
	// generously relative to this project's own packet volume (TCP
	// control-plane-ish traffic plus a short opt-in UDP port list,
	// never a full-rate data-plane mirror) — confirmed sufficient
	// live, not derived from a specific throughput target. There is
	// no CLI flag for the UDP port list yet, same as the eBPF side's
	// own UDP_MONITOR_PORTS, which needs a source edit and rebuild to
	// change — udpMonitorPorts is nil (no UDP monitoring) from every
	// current caller.
	defaultBlockSize = 1 << 17 // 128 KiB
	defaultBlockNr   = 64
	defaultFrameSize = 1 << 11 // 2 KiB — comfortably over any tpacket3_hdr (48B) + sockaddr_ll gap (48B) + snapLen (128B)
	// retireBlockTimeoutMs bounds how long the kernel waits before
	// handing back a partially-full block anyway, so a quiet
	// interface doesn't leave recent packets stuck in a block the
	// kernel is still "filling" indefinitely.
	retireBlockTimeoutMs = 100
	// pollTimeoutMs is how long each poll() call blocks waiting for
	// the current block to become ready — bounded so readLoop checks
	// for Close() periodically even on a silent interface, the same
	// role glitch.Handle's ring buffer Close()-driven unblock plays
	// for the eBPF path.
	pollTimeoutMs = 200
	// statsPollInterval is how often readLoop asks the kernel for
	// PACKET_STATISTICS — see Dropped's doc comment.
	statsPollInterval = 2 * time.Second
	// sweepInterval is how often readLoop calls c.engine.Sweep to
	// evict long-idle flows — see core.Engine's own doc comment for
	// why this package is the one responsible for calling it
	// periodically (Engine itself runs no timer).
	sweepInterval = 30 * time.Second
)

// eventsChanSize: raised from 256 after a real burst on dawn6g's own
// link (single-stream, single 2s outage, iperf3_retr~1000-1100) showed
// genuine channel-depth drops even with retransmissions no longer
// competing for a slot here (core.Engine counts them directly instead —
// see Engine.RetransCount) — the channel itself, not just a consumer's
// per-event processing cost, was the bottleneck at this device's real
// burst scale. 4096 is margin, not a measured minimum. Matches
// glitch.Handle's own eventsChanSize for the same reason.
const eventsChanSize = 4096

// Capture reads raw packets off one interface via a TPACKET_V3
// AF_PACKET ring and feeds them through a core.Engine, same public
// shape as glitch.Handle (Events/Dropped/Close) so cmd/glitch-tool and
// cmd/glitch-exporter can select either capture backend behind one
// small interface — see those commands' -capture flag.
type Capture struct {
	lockFile  *os.File
	fd        int
	ring      []byte
	blockSize int
	blockNr   int
	engine    *core.Engine

	events          chan glitch.Event
	userDropped     uint64 // atomic — events dropped because Events() wasn't drained fast enough
	kernelDropped   uint64 // atomic — unix.TpacketStatsV3.Drops, polled periodically (see pollKernelDrops)
	packetsSeen     uint64 // atomic — every packet the filter let through and this package parsed, regardless of whether it produced an Event
	untruncatedOver uint64 // atomic — see UntruncatedOverLimit

	mu   sync.Mutex
	err  error
	done chan struct{}
	stop chan struct{}
}

// Open binds a TPACKET_V3 ring to iface, attaches a classic BPF
// filter keeping TCP plus any udpMonitorPorts (see buildFilter), and
// starts feeding decoded packets into a new core.Engine (constructed
// with the same udpMonitorPorts, so its own UDP eligibility check
// agrees with what the kernel filter actually let through) in the
// background. Call Close when done.
//
// Exclusivity is its own flock on /var/run/glitch-afpacket-<iface>.lock —
// deliberately a different lock file than glitch.Handle's
// /var/run/glitch-<iface>.lock, not shared with it: the two capture
// backends must be able to run side by side on the same interface at
// once (Phase 2's whole point is comparing eBPF and AF_PACKET against
// identical live traffic), so AF_PACKET's own lock only has to stop
// two AF_PACKET instances from colliding with each other, never the
// eBPF path's.
func Open(iface string, udpMonitorPorts []uint16) (c *Capture, err error) {
	link, lerr := net.InterfaceByName(iface)
	if lerr != nil {
		return nil, fmt.Errorf("glitch/afpacket: InterfaceByName(%s): %w", iface, lerr)
	}

	// /var/run, not /run: confirmed live on dawn6g (WaveOS) that /run
	// doesn't exist there at all (no tmpfs mounted at that path) — only
	// /var/run does. On Debian/aaeon /var/run is the standard symlink
	// to /run, so this is a no-op change there; on WaveOS it's the only
	// path that actually works. Found deploying Phase 3: glitch-exporter
	// crash-looped on dawn6g with "open lock file: ... no such file or
	// directory" until this was traced to the path itself, not a
	// permissions or ordering problem.
	lockFile, lferr := os.OpenFile(fmt.Sprintf("/var/run/glitch-afpacket-%s.lock", iface), os.O_CREATE|os.O_RDWR, 0o644)
	if lferr != nil {
		return nil, fmt.Errorf("glitch/afpacket: open lock file: %w", lferr)
	}
	defer func() {
		if err != nil {
			lockFile.Close()
		}
	}()
	if ferr := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); ferr != nil {
		if errors.Is(ferr, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("glitch/afpacket: another AF_PACKET capture is already attached to %s", iface)
		}
		return nil, fmt.Errorf("glitch/afpacket: flock: %w", ferr)
	}

	fd, serr := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM, int(htons(unix.ETH_P_ALL)))
	if serr != nil {
		return nil, fmt.Errorf("glitch/afpacket: socket: %w", serr)
	}
	defer func() {
		if err != nil {
			unix.Close(fd)
		}
	}()

	if verr := unix.SetsockoptInt(fd, unix.SOL_PACKET, unix.PACKET_VERSION, unix.TPACKET_V3); verr != nil {
		return nil, fmt.Errorf("glitch/afpacket: setsockopt PACKET_VERSION: %w", verr)
	}

	if ferr := attachFilter(fd, udpMonitorPorts); ferr != nil {
		return nil, fmt.Errorf("glitch/afpacket: attach filter: %w", ferr)
	}

	req := &unix.TpacketReq3{
		Block_size:     defaultBlockSize,
		Block_nr:       defaultBlockNr,
		Frame_size:     defaultFrameSize,
		Retire_blk_tov: retireBlockTimeoutMs,
	}
	req.Frame_nr = (req.Block_size / req.Frame_size) * req.Block_nr
	if rerr := unix.SetsockoptTpacketReq3(fd, unix.SOL_PACKET, unix.PACKET_RX_RING, req); rerr != nil {
		return nil, fmt.Errorf("glitch/afpacket: setsockopt PACKET_RX_RING: %w", rerr)
	}

	ringSize := int(req.Block_size) * int(req.Block_nr)
	ring, merr := unix.Mmap(fd, 0, ringSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if merr != nil {
		return nil, fmt.Errorf("glitch/afpacket: mmap: %w", merr)
	}
	defer func() {
		if err != nil {
			unix.Munmap(ring)
		}
	}()

	sa := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: link.Index}
	if berr := unix.Bind(fd, sa); berr != nil {
		return nil, fmt.Errorf("glitch/afpacket: bind(%s): %w", iface, berr)
	}

	c = &Capture{
		lockFile:  lockFile,
		fd:        fd,
		ring:      ring,
		blockSize: int(req.Block_size),
		blockNr:   int(req.Block_nr),
		engine:    core.NewEngine(udpMonitorPorts),
		events:    make(chan glitch.Event, eventsChanSize),
		done:      make(chan struct{}),
		stop:      make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// attachFilter builds and installs the classic BPF filter from
// buildFilter via SO_ATTACH_FILTER.
func attachFilter(fd int, udpMonitorPorts []uint16) error {
	insts, err := buildFilter(udpMonitorPorts)
	if err != nil {
		return err
	}
	raw, err := bpf.Assemble(insts)
	if err != nil {
		return err
	}
	filters := make([]unix.SockFilter, len(raw))
	for i, r := range raw {
		filters[i] = unix.SockFilter{Code: r.Op, Jt: r.Jt, Jf: r.Jf, K: r.K}
	}
	prog := &unix.SockFprog{Len: uint16(len(filters)), Filter: &filters[0]}
	return unix.SetsockoptSockFprog(fd, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, prog)
}

// readLoop walks the ring block by block, round-robin, the way every
// TPACKET_V3 consumer must: a block only ever becomes "ours" (bit
// TP_STATUS_USER set in its own status word) once the kernel has
// filled it or its retire timeout fired, and must be handed back
// (status set to TP_STATUS_KERNEL) before that slot can be reused.
func (c *Capture) readLoop() {
	defer close(c.done)
	defer unix.Munmap(c.ring)
	defer unix.Close(c.fd)
	defer c.lockFile.Close() // releases the flock — no explicit LOCK_UN needed

	blockIdx := 0
	nextStatsPoll := time.Now().Add(statsPollInterval)
	nextSweep := time.Now().Add(sweepInterval)
	for {
		select {
		case <-c.stop:
			return
		default:
		}

		blockOff := blockIdx * c.blockSize
		status := binary.LittleEndian.Uint32(c.ring[blockOff+8:])
		if status&unix.TP_STATUS_USER == 0 {
			pfd := []unix.PollFd{{Fd: int32(c.fd), Events: unix.POLLIN}}
			if _, perr := unix.Poll(pfd, pollTimeoutMs); perr != nil && !errors.Is(perr, unix.EINTR) {
				c.setErr(fmt.Errorf("glitch/afpacket: poll: %w", perr))
				return
			}
			c.maybeSweep(&nextSweep)
			continue
		}

		c.processBlock(blockOff)
		binary.LittleEndian.PutUint32(c.ring[blockOff+8:], unix.TP_STATUS_KERNEL)
		blockIdx = (blockIdx + 1) % c.blockNr

		if now := time.Now(); now.After(nextStatsPoll) {
			c.pollKernelDrops()
			nextStatsPoll = now.Add(statsPollInterval)
		}
		c.maybeSweep(&nextSweep)
	}
}

// maybeSweep calls engine.Sweep once nextSweep has passed, forwarding
// any resulting events (stall_abandoned for flows evicted while an
// episode was open) the same non-blocking way processBlock forwards
// everything else. Checked on every loop iteration, including the
// poll-timeout path, so a long-idle interface still gets swept
// promptly instead of only when real traffic happens to arrive.
func (c *Capture) maybeSweep(nextSweep *time.Time) {
	now := time.Now()
	if now.Before(*nextSweep) {
		return
	}
	*nextSweep = now.Add(sweepInterval)
	for _, ev := range c.engine.Sweep(now) {
		select {
		case c.events <- ev:
		default:
			atomic.AddUint64(&c.userDropped, 1)
		}
	}
}

// processBlock walks every packet in one ready block, decoding and
// feeding each into c.engine.
func (c *Capture) processBlock(blockOff int) {
	numPkts := binary.LittleEndian.Uint32(c.ring[blockOff+12:])
	offsetToFirst := binary.LittleEndian.Uint32(c.ring[blockOff+16:])
	pktOff := blockOff + int(offsetToFirst)

	for i := uint32(0); i < numPkts; i++ {
		nextOffset := binary.LittleEndian.Uint32(c.ring[pktOff:])
		sec := binary.LittleEndian.Uint32(c.ring[pktOff+4:])
		nsec := binary.LittleEndian.Uint32(c.ring[pktOff+8:])
		snaplen := binary.LittleEndian.Uint32(c.ring[pktOff+12:])
		tpLen := binary.LittleEndian.Uint32(c.ring[pktOff+16:]) // tp_len: the packet's real on-wire length, before any filter truncation
		mac := binary.LittleEndian.Uint16(c.ring[pktOff+24:])
		pkttype := c.ring[pktOff+pkttypeOffset]

		// Confirms the classic BPF filter's RetConstant{Val: snapLen}
		// (see filter.go) actually truncates in the ring, as opposed to
		// a full packet landing there regardless — the leading
		// candidate explanation investigated for a live capacity
		// question (562 kernel/user drops seen on aaeon during a
		// congestion-recovery burst, Phase 2). snaplen == tpLen on a
		// packet longer than the filter's own 128-byte limit means
		// truncation did NOT happen for it.
		if snaplen == tpLen && tpLen > snapLen {
			atomic.AddUint64(&c.untruncatedOver, 1)
		}

		dataStart := pktOff + int(mac)
		dataEnd := dataStart + int(snaplen)
		if dataStart >= 0 && dataEnd <= len(c.ring) && dataEnd > dataStart {
			dir := core.DirIngress
			if pkttype == unix.PACKET_OUTGOING {
				dir = core.DirEgress
			}
			// tpacket3_hdr's own Sec/Nsec are kernel (CLOCK_REALTIME
			// by default) timestamps taken when the packet arrived —
			// see the brief's "kernel timestamps from the tpacket3
			// header," used here instead of time.Now() at read time
			// so Event.Time reflects when the packet actually crossed
			// the interface, not when this goroutine got around to
			// processing its block.
			pktTime := time.Unix(int64(sec), int64(nsec))
			if pm, ok := parseIPPacket(c.ring[dataStart:dataEnd], pktTime, dir); ok {
				atomic.AddUint64(&c.packetsSeen, 1)
				for _, ev := range c.engine.HandlePacket(pm) {
					select {
					case c.events <- ev:
					default:
						atomic.AddUint64(&c.userDropped, 1)
					}
				}
			}
		}

		if nextOffset == 0 {
			break
		}
		pktOff += int(nextOffset)
	}
}

// parseIPPacket decodes data (the raw IP packet, no link-layer header
// — see this file's package doc comment) into a core.PacketMeta.
// Mirrors tc_glitch.bpf.c's own egress_monitor parsing exactly: IPv4
// only, IHL-based L4 offset, non-first-fragment detection, TCP
// seq/ack/payload-length computed from the IP total-length field.
func parseIPPacket(data []byte, pktTime time.Time, dir core.Direction) (core.PacketMeta, bool) {
	if len(data) < 20 {
		return core.PacketMeta{}, false
	}
	if data[0]>>4 != 4 {
		return core.PacketMeta{}, false // IPv4 only, same as tc_glitch.bpf.c
	}
	ihl := int(data[0] & 0x0f)
	if ihl < 5 {
		return core.PacketMeta{}, false
	}
	l4Off := ihl * 4
	if len(data) < l4Off {
		return core.PacketMeta{}, false
	}

	fragOff := binary.BigEndian.Uint16(data[6:8])
	pm := core.PacketMeta{
		Time:      pktTime,
		Direction: dir,
		SrcIP:     net.IP(append([]byte(nil), data[12:16]...)),
		DstIP:     net.IP(append([]byte(nil), data[16:20]...)),
		Proto:     data[9],
		Frag:      fragOff&0x1FFF != 0, // non-first fragment — see tc_glitch.bpf.c's own comment
	}
	totalLen := binary.BigEndian.Uint16(data[2:4])

	switch pm.Proto {
	case 6: // TCP
		if len(data) < l4Off+14 {
			return pm, true // header doesn't fully fit this capture — IP-level info only
		}
		pm.SrcPort = binary.BigEndian.Uint16(data[l4Off : l4Off+2])
		pm.DstPort = binary.BigEndian.Uint16(data[l4Off+2 : l4Off+4])
		pm.Seq = binary.BigEndian.Uint32(data[l4Off+4 : l4Off+8])
		pm.Ack = binary.BigEndian.Uint32(data[l4Off+8 : l4Off+12])
		pm.Flags = data[l4Off+13]
		tcpHLen := int(data[l4Off+12]>>4) * 4
		l4HLen := l4Off + tcpHLen
		if int(totalLen) > l4HLen {
			pm.PayloadLen = totalLen - uint16(l4HLen)
		}
	case 17: // UDP
		if len(data) < l4Off+4 {
			return pm, true
		}
		pm.SrcPort = binary.BigEndian.Uint16(data[l4Off : l4Off+2])
		pm.DstPort = binary.BigEndian.Uint16(data[l4Off+2 : l4Off+4])
	}
	return pm, true
}

// pollKernelDrops folds PACKET_STATISTICS' cumulative Drops counter
// into Dropped() — the kernel's own count of packets it couldn't
// queue into the ring at all (e.g. userspace falling behind at the
// socket level, before anything here ever saw them), distinct from
// userDropped (events this package itself discarded because Events()
// wasn't being drained fast enough). Tracks the kernel's running
// total itself (lastKernelTotal) and folds in only the delta, since
// GetsockoptTpacketStatsV3 resets its own counter to 0 on each read.
func (c *Capture) pollKernelDrops() {
	stats, err := unix.GetsockoptTpacketStatsV3(c.fd, unix.SOL_PACKET, unix.PACKET_STATISTICS)
	if err != nil {
		return
	}
	atomic.AddUint64(&c.kernelDropped, uint64(stats.Drops))
}

func (c *Capture) setErr(err error) {
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
}

// Events returns the channel of decoded glitch events — closed once
// the capture loop stops (normally only after Close, but also on an
// unexpected poll error; check Err() once a receive reports the
// channel closed to tell the two apart). Same contract as
// glitch.Handle.Events().
func (c *Capture) Events() <-chan glitch.Event {
	return c.events
}

// Err returns the error that caused the capture loop to stop, if any.
func (c *Capture) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Dropped returns the cumulative count of events lost either because
// Events() wasn't drained fast enough, or because the kernel itself
// couldn't queue a packet into the ring (PACKET_STATISTICS) — see
// pollKernelDrops. Combined into one counter since both represent the
// same thing to a caller: glitch events that should have existed but
// don't.
func (c *Capture) Dropped() uint64 {
	return atomic.LoadUint64(&c.userDropped) + atomic.LoadUint64(&c.kernelDropped)
}

// KernelDropped and UserDropped break Dropped() apart into its two,
// causally distinct halves — not part of the Source interface (which
// only promises the combined Dropped(), the one thing glitch.Handle's
// own, differently-shaped drop counting can also express), for a
// caller (currently just cmd/glitch-tool's -capture afpacket path)
// that wants to tell them apart. Added after a test run reported
// Dropped()=562 during a congestion-recovery burst with no way to
// tell whether that meant the kernel ring itself couldn't keep up
// (KernelDropped — a real AF_PACKET capacity problem, relevant to
// Phase 3 sizing on WaveOS) or this process's own 256-slot Events()
// channel simply wasn't being drained fast enough by that particular
// consumer (UserDropped — e.g. glitch-tool synchronously fmt.Printf-
// ing one line per event to a redirected file, a property of that
// tool, not of capture/Engine; glitch-exporter's own async batching
// WriteAPI consumer is a different shape and may not reproduce it at
// all). Investigate KernelDropped first; it's the one that would
// also affect glitch-exporter in production.
func (c *Capture) KernelDropped() uint64 {
	return atomic.LoadUint64(&c.kernelDropped)
}

func (c *Capture) UserDropped() uint64 {
	return atomic.LoadUint64(&c.userDropped)
}

// UntruncatedOverLimit returns the cumulative count of ring packets
// longer than the classic BPF filter's own 128-byte snapLen (see
// filter.go) whose tp_snaplen nonetheless equals their real tp_len —
// i.e. the filter's truncation did not take effect for them. Expected
// to stay at 0; a nonzero count means full, untruncated packets are
// consuming many times their expected ring footprint, which is a
// direct, concrete explanation for KernelDropped during a burst.
func (c *Capture) UntruncatedOverLimit() uint64 {
	return atomic.LoadUint64(&c.untruncatedOver)
}

// PacketsSeen returns the cumulative count of packets the kernel
// filter let through and this package successfully parsed, whether
// or not any produced an Event — a basic liveness signal distinct
// from Dropped/Events, useful for confirming the capture is actually
// seeing real traffic (e.g. in glitch-tool's own startup/diagnostic
// output) rather than silently seeing nothing.
func (c *Capture) PacketsSeen() uint64 {
	return atomic.LoadUint64(&c.packetsSeen)
}

// Evictions returns the cumulative count of flows evicted from the
// underlying core.Engine's flow table (idle timeout or the cap being
// reached) — see core.Engine.Evictions and the maxFlows/idleTimeout
// bounding this package relies on to run unattended for weeks without
// unbounded memory growth.
func (c *Capture) Evictions() uint64 {
	return c.engine.Evictions()
}

// RetransCount returns the cumulative count of TCP retransmissions seen
// so far — see core.Engine.RetransCount for why these are counted
// directly instead of being emitted as Events.
func (c *Capture) RetransCount() uint64 {
	return c.engine.RetransCount()
}

// Close stops the capture loop and releases the socket/ring. Safe to
// call once; the underlying fd/mmap are released by readLoop itself
// on exit.
func (c *Capture) Close() error {
	close(c.stop)
	<-c.done
	close(c.events)
	return nil
}
