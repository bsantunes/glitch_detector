package gps

import (
	"context"
	"fmt"
	"log"

	"dawn-agent/modem"
)

// Detect finds modems via the two backends Supports actually
// implements — ModemManager first, then raw QMI if that finds nothing.
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
	} else if len(modems) > 0 {
		return modems, nil
	}

	modems, err = modem.DetectQMIMBIM(ctx)
	if err != nil {
		log.Printf("gps: QMI/MBIM detection unavailable: %v", err)
	}
	// MBIM-detected modems are simply skipped later by Supports — only
	// QMI-method ones from this call are usable.
	return modems, nil
}

// Supports reports whether GPS fixes are available for a modem detected
// via method. Only ModemManager (gps-raw location source) is enabled:
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
//   - AT-over-serial hasn't been tested live yet.
func Supports(method modem.Method) bool {
	switch method {
	case modem.MethodModemManager:
		return true
	default:
		return false
	}
}

// Poller gets GPS fixes for one modem. Construct with NewPoller and
// Close when done — QMI-backed pollers own a persistent background
// session (see QMISession) that must be torn down explicitly;
// ModemManager-backed ones hold no resources and Close is a no-op.
type Poller struct {
	m       modem.Modem
	session *QMISession // nil for ModemManager
}

// NewPoller sets up GPS polling for m. For ModemManager, this enables
// the gps-raw location source (best-effort, retried — see
// enableModemManager) and every Fix call makes a fresh mmcli call.
//
// The QMI path (NewQMISession — a persistent background NMEA session,
// since qmicli has no one-shot "get current fix" command) is
// implemented but deliberately refused here, not just gated at
// Supports/NewPollers — see Supports' doc comment for why. This keeps
// the disable in effect even for a caller that constructs a Poller
// directly instead of going through NewPollers.
func NewPoller(ctx context.Context, m modem.Modem) (*Poller, error) {
	switch m.Method {
	case modem.MethodModemManager:
		enableModemManager(ctx, m)
		return &Poller{m: m}, nil
	case modem.MethodQMI:
		return nil, fmt.Errorf("gps: QMI backend is disabled (see Supports' doc comment) — refusing to start a session for %s", m.Port)
	default:
		return nil, fmt.Errorf("gps not supported for method %q", m.Method)
	}
}

// Fix returns the current GPS fix for this poller's modem. For
// ModemManager this makes a fresh mmcli call; for QMI it's an instant
// read of the background session's cache (see QMISession.Fix).
func (p *Poller) Fix(ctx context.Context) (Fix, error) {
	if p.session != nil {
		return p.session.Fix()
	}
	return getFixModemManager(ctx, p.m)
}

// Close releases any resources this poller holds (the QMI backend's
// background session; a no-op for ModemManager).
func (p *Poller) Close() {
	if p.session != nil {
		p.session.Close()
	}
}

// Modem returns the modem this poller reads GPS fixes from.
func (p *Poller) Modem() modem.Modem {
	return p.m
}

// NewPollers builds one Poller for every modem in modems that Supports
// reports as GPS-capable, logging (and skipping) any that don't or
// that fail to initialize. Callers must Close each returned Poller
// when done.
func NewPollers(ctx context.Context, modems []modem.Modem) []*Poller {
	var pollers []*Poller
	for _, m := range modems {
		if !Supports(m.Method) {
			log.Printf("gps: [%s] %s (via %s) — GPS not supported for this backend, skipping", m.ID, m.Model, m.Method)
			continue
		}
		p, err := NewPoller(ctx, m)
		if err != nil {
			log.Printf("gps: [%s] NewPoller failed: %v", m.ID, err)
			continue
		}
		pollers = append(pollers, p)
	}
	return pollers
}
