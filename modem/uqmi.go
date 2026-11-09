package modem

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
)

// candidateUQMIDevices prefers WaveOS's stable /dev/qmi* symlinks
// (e.g. /dev/qmi0 -> /dev/cdc-wdm0, assigned per physical USB port
// rather than USB enumeration order) and falls back to a plain
// /dev/cdc-wdm* glob for other OpenWrt-style systems that don't have
// that convention. Only one scheme is used per run to avoid detecting
// the same underlying device twice under two different paths.
func candidateUQMIDevices() []string {
	if stable, _ := filepath.Glob("/dev/qmi[0-9]*"); len(stable) > 0 {
		return stable
	}
	devs, _ := filepath.Glob("/dev/cdc-wdm*")
	return devs
}

// DetectUQMI probes candidate devices with OpenWrt's own lightweight
// uqmi client. Used on systems (like WaveOS) that have uqmi but not
// ModemManager or libqmi-utils/libmbim-utils (qmicli/mbimcli).
func DetectUQMI(ctx context.Context) ([]Modem, error) {
	if _, err := exec.LookPath("uqmi"); err != nil {
		return nil, fmt.Errorf("uqmi not found in PATH: %w", err)
	}

	var modems []Modem
	for _, dev := range candidateUQMIDevices() {
		// uqmi has no manufacturer/model query at all; --get-imei is used
		// purely as a liveness check that something QMI-capable answers
		// on dev (IMEI is a hardware property, available regardless of
		// SIM/registration state).
		if _, err := runUQMI(ctx, dev, "--get-imei"); err != nil {
			continue
		}
		modems = append(modems, Modem{ID: dev, Port: dev, Method: MethodUQMI})
	}
	return modems, nil
}

// runUQMI runs `uqmi -d <dev> <args...>` and returns its raw stdout —
// either a JSON object or, when the modem has nothing to report yet, a
// bare JSON string (e.g. "Information unavailable").
func runUQMI(ctx context.Context, dev string, args ...string) ([]byte, error) {
	full := append([]string{"-d", dev}, args...)
	return exec.CommandContext(ctx, "uqmi", full...).Output()
}

func getSignalUQMI(ctx context.Context, m Modem) (Signal, error) {
	raw, err := runUQMI(ctx, m.Port, "--get-signal-info")
	if err != nil {
		return NaNSignal(), fmt.Errorf("uqmi -d %s --get-signal-info: %w", m.Port, err)
	}
	return signalFromUQMIJSON(raw), nil
}

// signalFromUQMIJSON parses `uqmi --get-signal-info` output. Pure/
// testable: no exec involved.
func signalFromUQMIJSON(raw []byte) Signal {
	var info struct {
		Type string   `json:"type"`
		RSSI *float64 `json:"rssi"`
		RSRQ *float64 `json:"rsrq"`
		RSRP *float64 `json:"rsrp"`
		SNR  *float64 `json:"snr"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		// uqmi returns a bare JSON string (e.g. "Information unavailable")
		// instead of an object when the modem has nothing to report yet
		// (observed on a modem in power-save/not-registered state) —
		// that's a normal state, not an error.
		return NaNSignal()
	}

	sig := NaNSignal()
	sig.Technology = info.Type
	if info.RSSI != nil {
		sig.RSSI = *info.RSSI
	}
	if info.RSRQ != nil {
		sig.RSRQ = *info.RSRQ
	}
	if info.RSRP != nil {
		sig.RSRP = *info.RSRP
	}
	if info.SNR != nil {
		sig.SINR = *info.SNR
	}
	return sig
}

func getCellInfoUQMI(ctx context.Context, m Modem) (CellInfo, error) {
	var ci CellInfo

	raw, err := runUQMI(ctx, m.Port, "--get-serving-system")
	if err != nil {
		return CellInfo{}, fmt.Errorf("uqmi -d %s --get-serving-system: %w", m.Port, err)
	}
	applyUQMIServingSystem(&ci, raw)

	// --get-system-info failing (e.g. modem busy) shouldn't drop the
	// PLMN/registration info already gathered above.
	if raw, err := runUQMI(ctx, m.Port, "--get-system-info"); err == nil {
		applyUQMISystemInfo(&ci, raw)
	}

	return ci, nil
}

// applyUQMIServingSystem fills PLMN/registration from `uqmi --get-serving-system`.
// PLMN is built from separate mcc/mnc ints by zero-padding mnc to 2
// digits (e.g. mcc=268, mnc=6 -> "26806") — correct for the common
// 2-digit-MNC case, not for countries using 3-digit MNCs. Pure/testable.
func applyUQMIServingSystem(ci *CellInfo, raw []byte) {
	var ss struct {
		Registration string `json:"registration"`
		PLMNMCC      *int   `json:"plmn_mcc"`
		PLMNMNC      *int   `json:"plmn_mnc"`
		PLMNDesc     string `json:"plmn_description"`
	}
	if err := json.Unmarshal(raw, &ss); err != nil {
		return
	}
	ci.RegistrationState = ss.Registration
	ci.OperatorName = ss.PLMNDesc
	if ss.PLMNMCC != nil && ss.PLMNMNC != nil {
		ci.PLMN = fmt.Sprintf("%d%02d", *ss.PLMNMCC, *ss.PLMNMNC)
	}
}

// uqmiRATInfo is the subset of fields `uqmi --get-system-info` reports
// per technology section that we care about; the sections carry many
// other fields (service_status, domain, roaming_status, ...) which are
// simply ignored by encoding/json here.
type uqmiRATInfo struct {
	TAC      *int `json:"tracking_area_code"`
	ENodeBID *int `json:"enodeb_id"`
	CellID   *int `json:"cell_id"`
}

// uqmiRATPriority lists get-system-info's top-level technology sections
// to check, most-capable first. "nr5g" is unverified against a live
// capture (the test modem wasn't on NR during development) — it's
// uqmi's documented key name elsewhere, assumed consistent here.
var uqmiRATPriority = []string{"nr5g", "lte", "wcdma", "gsm"}

// applyUQMISystemInfo fills TAC/CellID from `uqmi --get-system-info`,
// normalized to decimal strings (TAC is already decimal; CellID is
// composed from the separate enodeb_id/cell_id fields using the
// standard 28-bit E-UTRAN Cell Identity formula: eNB ID in the upper 20
// bits, local cell ID in the lower 8 — matching the single composite
// Cell ID ModemManager reports). Pure/testable.
func applyUQMISystemInfo(ci *CellInfo, raw []byte) {
	var sections map[string]uqmiRATInfo
	if err := json.Unmarshal(raw, &sections); err != nil {
		return
	}
	for _, rat := range uqmiRATPriority {
		info, ok := sections[rat]
		if !ok || info.TAC == nil {
			continue
		}
		ci.TAC = strconv.Itoa(*info.TAC)
		if info.ENodeBID != nil && info.CellID != nil {
			eci := int64(*info.ENodeBID)<<8 | int64(*info.CellID)
			ci.CellID = strconv.FormatInt(eci, 10)
		}
		return
	}
}
