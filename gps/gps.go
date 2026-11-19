package gps

import (
	"context"
	"fmt"
	"log"

	"dawn-agent/modem"
)

// Detect finds modems via the backends Supports actually implements —
// ModemManager, then raw QMI (currently always unusable — see
// Supports), then AT-over-serial — stopping as soon as a step yields
// at least one modem Supports will actually use, rather than just the
// first step to find *any* modem. That distinction matters here: on
// WaveOS, modem.DetectQMIMBIM(ctx) reliably finds modems (raw qmicli
// answers cdc-wdm0/1 fine), but every one of them is useless for GPS
// right now (QMI is disabled) — stopping there like modem.Detect()'s
// own cascade does would mean never reaching AT at all.
//
// Deliberately independent of modem.Detect()'s own cascade: that
// cascade prioritizes WaveOS's ubus backend over raw QMI specifically
// to avoid QMI client-ID contention with the uqmi-ubus daemon, but
// ubus has no GPS support at all (see Supports) — so reusing its pick
// would mean finding no GPS-capable modem on any WaveOS board
// (confirmed live on 6global). GPS detection is its own concern.
func Detect(ctx context.Context) ([]modem.Modem, error) {
	modems, err := modem.DetectModemManager(ctx)
	if err != nil {
		log.Printf("gps: ModemManager detection unavailable: %v", err)
	} else if anySupported(modems) {
		return modems, nil
	}

	qmiModems, err := modem.DetectQMIMBIM(ctx)
	if err != nil {
		log.Printf("gps: QMI/MBIM detection unavailable: %v", err)
	} else if anySupported(qmiModems) {
		return qmiModems, nil
	}

	atModems, err := modem.DetectAT(ctx)
	if err != nil {
		log.Printf("gps: AT detection unavailable: %v", err)
	}
	return atModems, nil
}

// DetectWithMethod resolves which modem(s) to use for GPS based on an
// operator-chosen method ("", "auto" — Detect's own cascade,
// unchanged; "mm" — ModemManager only; "at" — AT-over-serial only)
// and, for "at", an optional port override (pin exactly one device
// instead of scanning every candidate — see DetectATPort). Forcing a
// method skips the others entirely rather than falling through to
// them: the whole point of forcing one is to avoid probing/depending
// on a backend that must not be touched on a given board (e.g.
// aaeon's hard constraint against ever calling ModemManager's
// location interface — see CLAUDE.md's gps/ package section).
// port is only valid together with method "at"; it's an error
// otherwise, rather than silently ignored.
func DetectWithMethod(ctx context.Context, method, port string) ([]modem.Modem, error) {
	if port != "" && method != "at" {
		return nil, fmt.Errorf("gps: -port requires -method=at (got method %q)", method)
	}
	switch method {
	case "", "auto":
		return Detect(ctx)
	case "mm":
		return modem.DetectModemManager(ctx)
	case "at":
		if port != "" {
			m, err := DetectATPort(ctx, port)
			if err != nil {
				return nil, err
			}
			return []modem.Modem{m}, nil
		}
		return modem.DetectAT(ctx)
	default:
		return nil, fmt.Errorf("gps: unknown -method %q (want auto, mm, or at)", method)
	}
}

// DescribeMethod renders a human-readable description of what
// DetectWithMethod(ctx, method, port) is about to do, for a startup
// log line — describing the real cascade order when auto-detecting,
// or the forced method/port when one was given, rather than a
// hardcoded "ModemManager -> raw QMI" that doesn't match either case.
func DescribeMethod(method, port string) string {
	switch method {
	case "", "auto":
		return "ModemManager -> QMI/MBIM -> AT-over-serial (auto)"
	case "mm":
		return "ModemManager only (forced)"
	case "at":
		if port != "" {
			return fmt.Sprintf("AT-over-serial only, pinned to %s (forced)", port)
		}
		return "AT-over-serial only (forced)"
	default:
		return fmt.Sprintf("unknown method %q", method)
	}
}

// anySupported reports whether at least one modem in modems is one
// Supports actually approves of.
func anySupported(modems []modem.Modem) bool {
	for _, m := range modems {
		if Supports(m.Method) {
			return true
		}
	}
	return false
}

// Supports reports whether GPS fixes are available for a modem detected
// via method. ModemManager (gps-raw location source) and AT-over-serial
// (SIMCom/Quectel vendor commands — see detectATGPSVendor; other
// manufacturers refused by NewPoller, not here, same as the QMI
// disable) are enabled:
//   - raw QMI (qmicli's LOC service) is **implemented but deliberately
//     disabled** (see NewPoller/NewQMISession) after repeatedly
//     corrupting a modem's QMI client state in production, even with a
//     graceful shutdown (--loc-stop before any force-kill) — twice, on
//     two different boards (6global, dawn6g), the second time with no
//     abrupt kill involved at all. Root cause not understood (possibly
//     a qmi-proxy daemon fragility under repeated LOC session churn);
//     there's no non-production QMI hardware available to investigate
//     further without risking a live board again. Re-enabling this
//     needs either a real fix or dedicated test hardware — see
//     CLAUDE.md's gps/ package section for the full incident history.
//   - ubus (WaveOS's own "qmi" object) has a "location" method, but it
//     only returns cell-ID-based location (LAC/CI), not GPS coordinates
//     at all — getting real GNSS out of it would need a patch to the
//     vendor's uqmi-ubus.lua, same category as the already-documented
//     PCI/EARFCN-discard limitation in modem/'s Known Issues.
//   - uqmi lists "pds" as a supported service name but this build
//     exposes no actual PDS/location CLI action to call.
func Supports(method modem.Method) bool {
	switch method {
	case modem.MethodModemManager, modem.MethodAT:
		return true
	default:
		return false
	}
}

// Poller gets GPS fixes for one modem. Construct with NewPoller and
// Close when done — QMI-backed pollers own a persistent background
// session (see QMISession) that must be torn down explicitly;
// ModemManager/AT-backed ones hold no resources and Close is a no-op
// (every Fix call is a fresh mmcli/AT command, nothing kept open
// between polls).
type Poller struct {
	m        modem.Modem
	session  *QMISession // nil for ModemManager/AT
	atVendor atGPSVendor // atGPSVendorUnknown for ModemManager/QMI
	nmea     *NMEAReader // non-nil only for an AT-backed poller given -nmea-port
}

// NewPoller sets up GPS polling for m. For ModemManager, this enables
// the gps-raw location source (best-effort, retried — see
// enableModemManager) and every Fix call makes a fresh mmcli call. For
// AT, this enables the modem's own GNSS engine via its vendor-specific
// command (see enableAT) — optionally preceded by enabling GNSS
// assistance data if agps is true, see enableAssistanceData — and
// every Fix call makes a fresh AT query (see getFixAT) — same
// one-shot-per-call shape as ModemManager, not a persistent session.
// agps is ignored for ModemManager/QMI, which have no equivalent
// concept here.
//
// The QMI path (NewQMISession — a persistent background NMEA session,
// since qmicli has no one-shot "get current fix" command) is
// implemented but deliberately refused here, not just gated at
// Supports/NewPollers — see Supports' doc comment for why. This keeps
// the disable in effect even for a caller that constructs a Poller
// directly instead of going through NewPollers.
//
// nmeaPort, if non-empty, opens a background NMEAReader on the
// modem's dedicated NMEA port (already resolved to a real device path
// by the caller — see modem.ResolvePort — same as -port itself) and
// makes Fix prefer it over the AT request/response path. Only valid
// together with method AT (m.Method == modem.MethodAT); NewPoller
// doesn't enforce that itself — see DetectWithMethod/cmd/ layer for
// the -nmea-port/-method validation — it simply has no effect for any
// other method. See CLAUDE.md's gps/ package section for why: AT+QGPSLOC=2
// can intermittently report "no fix" even with a continuous lock,
// confirmed live, while the modem's own NMEA stream does not.
func NewPoller(ctx context.Context, m modem.Modem, agps bool, nmeaPort string) (*Poller, error) {
	switch m.Method {
	case modem.MethodModemManager:
		enableModemManager(ctx, m)
		return &Poller{m: m}, nil
	case modem.MethodAT:
		vendor := detectATGPSVendor(m)
		if vendor == atGPSVendorUnknown {
			return nil, fmt.Errorf("gps: AT GPS not supported for manufacturer %q (only SIMCom/Quectel recognized)", m.Manufacturer)
		}
		if err := enableAT(ctx, m, vendor, agps); err != nil {
			return nil, err
		}
		p := &Poller{m: m, atVendor: vendor}
		if nmeaPort != "" {
			p.nmea = OpenNMEAReader(ctx, nmeaPort)
		}
		return p, nil
	case modem.MethodQMI:
		return nil, fmt.Errorf("gps: QMI backend is disabled (see Supports' doc comment) — refusing to start a session for %s", m.Port)
	default:
		return nil, fmt.Errorf("gps not supported for method %q", m.Method)
	}
}

// Fix returns the current GPS fix for this poller's modem. The NMEA
// reader, when configured (see NewPoller's nmeaPort), is preferred
// over everything else; otherwise this makes a fresh mmcli/AT call,
// or for QMI, an instant read of the background session's cache (see
// QMISession.Fix).
func (p *Poller) Fix(ctx context.Context) (Fix, error) {
	if p.nmea != nil {
		return p.nmea.Fix(), nil
	}
	if p.session != nil {
		return p.session.Fix()
	}
	if p.atVendor != atGPSVendorUnknown {
		return getFixAT(p.m, p.atVendor)
	}
	return getFixModemManager(ctx, p.m)
}

// Close releases any resources this poller holds (the QMI backend's
// background session, or the NMEA reader's; a no-op for ModemManager
// with neither).
func (p *Poller) Close() {
	if p.session != nil {
		p.session.Close()
	}
	if p.nmea != nil {
		p.nmea.Close()
	}
}

// Modem returns the modem this poller reads GPS fixes from.
func (p *Poller) Modem() modem.Modem {
	return p.m
}

// NewPollers builds one Poller for every modem in modems that Supports
// reports as GPS-capable, logging (and skipping) any that don't or
// that fail to initialize. agps/nmeaPort are forwarded to NewPoller
// unchanged. Callers must Close each returned Poller when done.
func NewPollers(ctx context.Context, modems []modem.Modem, agps bool, nmeaPort string) []*Poller {
	var pollers []*Poller
	for _, m := range modems {
		if !Supports(m.Method) {
			log.Printf("gps: [%s] %s (via %s) — GPS not supported for this backend, skipping", m.ID, m.Model, m.Method)
			continue
		}
		p, err := NewPoller(ctx, m, agps, nmeaPort)
		if err != nil {
			log.Printf("gps: [%s] NewPoller failed: %v", m.ID, err)
			continue
		}
		pollers = append(pollers, p)
	}
	return pollers
}
