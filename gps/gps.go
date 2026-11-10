package gps

import (
	"context"
	"fmt"

	"dawn-agent/modem"
)

// Supports reports whether GPS fixes are available for a modem detected
// via method. Only ModemManager (gps-raw location source) and raw QMI
// (qmicli's LOC service) have real hardware behind them so far:
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
	case modem.MethodModemManager, modem.MethodQMI:
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
// enableModemManager) and every Fix call makes a fresh mmcli call. For
// QMI, this starts a persistent background NMEA session (see
// QMISession) since qmicli has no one-shot "get current fix" command;
// every Fix call just reads the latest cached sentence.
func NewPoller(ctx context.Context, m modem.Modem) (*Poller, error) {
	switch m.Method {
	case modem.MethodModemManager:
		enableModemManager(ctx, m)
		return &Poller{m: m}, nil
	case modem.MethodQMI:
		session, err := NewQMISession(ctx, m.Port)
		if err != nil {
			return nil, err
		}
		return &Poller{m: m, session: session}, nil
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
