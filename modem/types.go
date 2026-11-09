// Package modem detects cellular modems and reads their radio signal
// quality across whatever backend is actually available on the host:
// ModemManager (mmcli), QMI/MBIM (qmicli/mbimcli), or raw AT commands
// over serial as a last resort. It exists to let dawn-agent (and the
// standalone cmd/modem-tool CLI) work across modem models/vendors
// instead of assuming a single SIMCom device at ModemManager index 0.
package modem

import (
	"fmt"
	"math"
)

// Method identifies which backend detected/queries a Modem.
type Method string

const (
	MethodModemManager Method = "ModemManager"
	MethodQMI          Method = "QMI"
	MethodMBIM         Method = "MBIM"
	// MethodUQMI is OpenWrt's own lightweight QMI client (uqmi) — a
	// different tool/output format than libqmi's qmicli, used on systems
	// (like WaveOS) that have uqmi but not ModemManager or libqmi-utils.
	MethodUQMI Method = "uqmi"
	// MethodUbus reads WaveOS's own "qmi" ubus object (backed by its
	// uqmi-ubus daemon) instead of spawning a competing raw qmicli/uqmi
	// client — avoids the QMI client-ID contention those can hit when
	// run alongside that daemon. WaveOS-specific.
	MethodUbus Method = "ubus"
	MethodAT   Method = "AT"
)

// Modem describes one detected modem, regardless of which method found it.
type Modem struct {
	// ID is the mmcli modem index for ModemManager, or the device node
	// path (e.g. /dev/cdc-wdm0, /dev/ttyUSB2) for the other backends.
	ID           string
	Manufacturer string
	Model        string
	// Port is the device node used to query this modem for signal data.
	Port   string
	Method Method
}

// Signal is a technology-agnostic snapshot of radio signal quality for
// one active radio leg. A modem can have more than one simultaneously
// active leg — e.g. EN-DC dual connectivity, where an LTE cell (the
// "anchor") and an NR secondary cell group ("scg") both report real
// values at the same time — see GetSignals, which returns one Signal
// per active leg. Fields a given leg doesn't report are math.NaN().
type Signal struct {
	// Technology is "5g", "lte", "umts", "gsm", or "" if unknown.
	Technology string
	// Role is "anchor" (the primary/serving leg — the only leg present
	// outside of dual connectivity) or "scg" (secondary cell group, e.g.
	// EN-DC's NR leg). Always set by GetSignals/GetSignal, never "".
	Role string
	RSRP float64
	RSRQ float64
	RSSI float64
	SINR float64
}

// NaNSignal returns a Signal with every field set to NaN, used whenever
// a backend can run but has nothing (yet) to report.
func NaNSignal() Signal {
	return Signal{RSRP: math.NaN(), RSRQ: math.NaN(), RSSI: math.NaN(), SINR: math.NaN()}
}

// assignRoles applies the anchor/scg convention to a set of
// simultaneously active radio legs: when both an LTE and an NR leg are
// active (EN-DC), LTE is always the anchor (3GPP defines EN-DC that
// way — NR is never the anchor when paired with LTE) and NR becomes the
// secondary cell group; otherwise whichever single leg is active is the
// anchor. candidates must not include inactive/no-data legs. Shared by
// the ModemManager and QMI backends, the only two that can currently
// tell multiple simultaneously-active legs apart.
func assignRoles(candidates []Signal) []Signal {
	if len(candidates) == 0 {
		return nil
	}
	lteIdx, nrIdx := -1, -1
	for i, s := range candidates {
		switch s.Technology {
		case "lte":
			lteIdx = i
		case "5g":
			nrIdx = i
		}
	}
	if lteIdx >= 0 && nrIdx >= 0 {
		candidates[lteIdx].Role = "anchor"
		candidates[nrIdx].Role = "scg"
		return []Signal{candidates[lteIdx], candidates[nrIdx]}
	}
	// No recognized dual-connectivity combo — first active leg is anchor.
	candidates[0].Role = "anchor"
	return candidates[:1]
}

// FormatValue renders a signal metric for humans: "N/A" for NaN,
// otherwise one decimal place.
func FormatValue(v float64) string {
	if math.IsNaN(v) {
		return "N/A"
	}
	return fmt.Sprintf("%.1f", v)
}

// CellInfo captures network/cell identity — as opposed to Signal's raw
// radio quality numbers. Fields a backend can't report are "".
type CellInfo struct {
	// PLMN is the combined MCC+MNC, e.g. "26806".
	PLMN         string
	OperatorName string
	// RegistrationState is e.g. "home", "roaming", "searching".
	RegistrationState string
	// PacketServiceState is e.g. "attached", "detached". Not every backend
	// has a clean equivalent (e.g. uqmi) — "" when unavailable.
	PacketServiceState string
	// TAC is the Tracking Area Code, normalized to a decimal string
	// across backends (ModemManager reports it in hex; uqmi in decimal).
	TAC string
	// CellID is the cell identity (ECI for LTE; NCGI support for 5G SA
	// varies by modem/plugin), normalized to a decimal string across
	// backends same as TAC.
	CellID string
}
