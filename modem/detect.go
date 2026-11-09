package modem

import (
	"context"
	"fmt"
	"log"
)

// Detect runs the hybrid detection cascade: ModemManager first (the
// richest and most common source on Linux gateways), then WaveOS's own
// "qmi" ubus object (reads its uqmi-ubus daemon's already-arbitrated
// session instead of spawning a competing raw client — tried before
// QMI/MBIM/uqmi specifically to avoid the QMI client-ID contention
// those can cause alongside that daemon), then QMI/MBIM device probing
// (qmicli/mbimcli), then OpenWrt's own uqmi client (used on systems
// that have neither ModemManager, WaveOS's daemon, nor
// libqmi-utils/libmbim-utils), then raw AT-over-serial as a last
// resort. It returns as soon as one method finds at least one modem; a
// method that isn't installed or that finds nothing just falls through
// to the next one.
func Detect(ctx context.Context) ([]Modem, error) {
	modems, err := DetectModemManager(ctx)
	if err != nil {
		log.Printf("modem: ModemManager detection unavailable: %v", err)
	} else if len(modems) > 0 {
		return modems, nil
	}

	modems, err = DetectUbus(ctx)
	if err != nil {
		log.Printf("modem: ubus (WaveOS) detection unavailable: %v", err)
	} else if len(modems) > 0 {
		return modems, nil
	}

	modems, err = DetectQMIMBIM(ctx)
	if err != nil {
		log.Printf("modem: QMI/MBIM detection unavailable: %v", err)
	} else if len(modems) > 0 {
		return modems, nil
	}

	modems, err = DetectUQMI(ctx)
	if err != nil {
		log.Printf("modem: uqmi detection unavailable: %v", err)
	} else if len(modems) > 0 {
		return modems, nil
	}

	modems, err = DetectAT(ctx)
	if err != nil {
		log.Printf("modem: AT detection unavailable: %v", err)
	} else if len(modems) > 0 {
		return modems, nil
	}

	return nil, nil
}

// GetSignals returns one Signal per currently active radio leg for m —
// usually just one ("anchor"), but two when the modem is in EN-DC dual
// connectivity (an LTE "anchor" plus an NR "scg" leg reported
// simultaneously). Only the ModemManager and QMI (qmicli, via its
// modern --nas-get-signal-info) backends can currently tell multiple
// simultaneously-active legs apart, since they're the only ones whose
// output exposes independent per-technology sections; MBIM/uqmi/AT and
// qmicli's older --nas-get-signal-strength fallback always return a
// single anchor-only Signal.
func GetSignals(ctx context.Context, m Modem) ([]Signal, error) {
	switch m.Method {
	case MethodModemManager:
		return getSignalsModemManager(ctx, m)
	case MethodUbus:
		return getSignalsUbus(ctx, m)
	case MethodQMI:
		return getSignalsQMI(ctx, m)
	case MethodMBIM:
		return singleAnchorSignal(getSignalMBIM(ctx, m))
	case MethodUQMI:
		return singleAnchorSignal(getSignalUQMI(ctx, m))
	case MethodAT:
		return singleAnchorSignal(getSignalAT(ctx, m))
	default:
		return nil, fmt.Errorf("unknown detection method %q", m.Method)
	}
}

// singleAnchorSignal wraps a single-Signal backend result into the
// []Signal shape GetSignals returns, tagging it as the anchor leg.
func singleAnchorSignal(sig Signal, err error) ([]Signal, error) {
	if err != nil {
		return nil, err
	}
	sig.Role = "anchor"
	return []Signal{sig}, nil
}

// GetSignal returns just the anchor leg for m — a convenience for
// callers (cmd/modem-tool, dawn-agent's status line) that only want a
// single reading and don't care about EN-DC's secondary NR leg. Use
// GetSignals directly to see every active leg.
func GetSignal(ctx context.Context, m Modem) (Signal, error) {
	signals, err := GetSignals(ctx, m)
	if err != nil {
		return NaNSignal(), err
	}
	if len(signals) == 0 {
		return NaNSignal(), nil
	}
	return signals[0], nil
}

// GetCellInfo returns network/cell identity info (PLMN, registration
// state, TAC, cell ID) for m. Currently implemented for the
// ModemManager, ubus, QMI and uqmi backends (ubus doesn't yet populate
// RegistrationState/PacketServiceState — see getCellInfoUbus); MBIM/AT
// support is future work (no MBIM-only or AT-only modem has been
// available to validate against yet).
func GetCellInfo(ctx context.Context, m Modem) (CellInfo, error) {
	switch m.Method {
	case MethodModemManager:
		return getCellInfoModemManager(ctx, m)
	case MethodUbus:
		return getCellInfoUbus(ctx, m)
	case MethodQMI:
		return getCellInfoQMI(ctx, m)
	case MethodUQMI:
		return getCellInfoUQMI(ctx, m)
	default:
		return CellInfo{}, fmt.Errorf("cell info not implemented for method %q", m.Method)
	}
}

// SupportsCellInfo reports whether GetCellInfo is implemented for
// method — callers can use this to skip polling quietly for backends
// that don't support it yet, instead of logging the same error forever.
func SupportsCellInfo(method Method) bool {
	switch method {
	case MethodModemManager, MethodUbus, MethodQMI, MethodUQMI:
		return true
	default:
		return false
	}
}
