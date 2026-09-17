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
	MethodAT           Method = "AT"
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

// Signal is a technology-agnostic snapshot of radio signal quality.
// Fields a given modem/technology doesn't report are math.NaN().
type Signal struct {
	// Technology is "5g", "lte", "umts", "gsm", or "" if unknown.
	Technology string
	RSRP       float64
	RSRQ       float64
	RSSI       float64
	SINR       float64
}

// NaNSignal returns a Signal with every field set to NaN, used whenever
// a backend can run but has nothing (yet) to report.
func NaNSignal() Signal {
	return Signal{RSRP: math.NaN(), RSRQ: math.NaN(), RSSI: math.NaN(), SINR: math.NaN()}
}

// FormatValue renders a signal metric for humans: "N/A" for NaN,
// otherwise one decimal place.
func FormatValue(v float64) string {
	if math.IsNaN(v) {
		return "N/A"
	}
	return fmt.Sprintf("%.1f", v)
}
