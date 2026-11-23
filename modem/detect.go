package modem

import (
	"context"
	"fmt"
	"log"
	"math"
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

// DetectValidated runs the same cascade as Detect, in the same order,
// but additionally confirms each candidate actually yields live signal
// data (at least one leg with a real, non-NaN RSRP or RSSI) before
// accepting it — a method that "finds" a modem but can only report N/A
// (e.g. a WaveOS ubus object that exists but hasn't registered with its
// uqmi-ubus daemon yet, or a QMI session returning empty sections while
// contending with that same daemon) is treated the same as not finding
// anything, falling through to the next method instead of permanently
// committing to a modem this cascade can never actually report real
// values for. Confirmed live on dawn6g: Detect alone accepted ubus
// moments before its daemon had anything to report, and separately
// accepted a QMI session whose signal reads then stayed frozen for
// 2.5 days — DetectValidated's job is to catch the first of those two
// (the frozen-after-accept case needs cmd/modem-exporter's own
// staleness tracking instead, since it isn't visible at detection time).
//
// GetCellInfo is deliberately not checked here — PLMN/registration text
// is a weaker signal than a real radio measurement, and at least one
// legitimate state (a modem with no SIM, still camped on a cell's
// broadcast system information) reports real RSRP/RSRQ while
// GetCellInfo stays empty — see this package's own doc comment.
func DetectValidated(ctx context.Context) ([]Modem, error) {
	return detectValidated(ctx, "")
}

// DetectValidatedExcluding behaves exactly like DetectValidated but
// never considers method, even if it would otherwise "find" a modem
// with real-looking (non-NaN) signal data — for falling back off a
// method whose data is known stale by some signal DetectValidated's own
// all-N/A check can't see (e.g. ubus's own source timestamp not
// advancing — see cmd/modem-exporter's staleness tracking), since a
// stuck cache can look exactly like a fresh reading to that check
// alone. Confirmed live this matters: without it, re-running
// DetectValidated after falling back off a stale ubus could simply
// re-select ubus right back, since its cached (stale) values are still
// non-NaN.
func DetectValidatedExcluding(ctx context.Context, method Method) ([]Modem, error) {
	return detectValidated(ctx, method)
}

func detectValidated(ctx context.Context, exclude Method) ([]Modem, error) {
	try := func(method Method, detect func(context.Context) ([]Modem, error), name string) []Modem {
		if method == exclude {
			return nil
		}
		modems, err := detect(ctx)
		if err != nil {
			log.Printf("modem: %s detection unavailable: %v", name, err)
			return nil
		}
		if len(modems) == 0 {
			return nil
		}
		for _, m := range modems {
			signals, serr := GetSignals(ctx, m)
			if serr == nil && hasRealSignal(signals) {
				return modems
			}
		}
		log.Printf("modem: %s found %d modem(s) but none report real signal data (all N/A) — trying the next method", name, len(modems))
		return nil
	}

	if modems := try(MethodModemManager, DetectModemManager, "ModemManager"); modems != nil {
		return modems, nil
	}
	if modems := try(MethodUbus, DetectUbus, "ubus (WaveOS)"); modems != nil {
		return modems, nil
	}
	if modems := try(MethodQMI, DetectQMIMBIM, "QMI/MBIM"); modems != nil {
		return modems, nil
	}
	if modems := try(MethodUQMI, DetectUQMI, "uqmi"); modems != nil {
		return modems, nil
	}
	if modems := try(MethodAT, DetectAT, "AT"); modems != nil {
		return modems, nil
	}
	return nil, nil
}

func hasRealSignal(signals []Signal) bool {
	for _, s := range signals {
		if !math.IsNaN(s.RSRP) || !math.IsNaN(s.RSSI) {
			return true
		}
	}
	return false
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
