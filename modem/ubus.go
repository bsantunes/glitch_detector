package modem

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// DetectUbus probes WaveOS's own "qmi" ubus object, backed by its
// uqmi-ubus daemon (/lib/wavesys/lib/lua/uqmi/*.lua). That daemon
// already owns a single, serialized QMI/AT session per modem; reading
// its cached results over ubus avoids spawning a competing raw
// qmicli/uqmi client, which was observed live to exhaust the modem's
// QMI client-ID pool when run alongside it. WaveOS-specific: depends
// on the "qmi" ubus object existing, not a generic OpenWrt capability
// the way uqmi is.
func DetectUbus(ctx context.Context) ([]Modem, error) {
	if _, err := exec.LookPath("ubus"); err != nil {
		return nil, fmt.Errorf("ubus not found in PATH: %w", err)
	}
	if !ubusObjectExists(ctx, "qmi") {
		return nil, fmt.Errorf(`ubus object "qmi" not found (not a WaveOS uqmi-ubus host)`)
	}

	raw, err := runUbus(ctx, "qmi", "list")
	if err != nil {
		return nil, fmt.Errorf("ubus call qmi list: %w", err)
	}
	devices, err := modemsFromUbusList(raw)
	if err != nil {
		return nil, fmt.Errorf("ubus call qmi list: %w", err)
	}
	return devices, nil
}

func ubusObjectExists(ctx context.Context, name string) bool {
	out, err := exec.CommandContext(ctx, "ubus", "list").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == name {
			return true
		}
	}
	return false
}

func runUbus(ctx context.Context, object, method string) ([]byte, error) {
	return exec.CommandContext(ctx, "ubus", "call", object, method).Output()
}

// modemsFromUbusList parses `ubus call qmi list` output, keyed by
// netifd interface alias (e.g. "lte0", "lte1") rather than a device
// path — that alias is also what every other "qmi" ubus method expects
// to find a specific modem's data under, so it's used as both Modem.ID
// and Modem.Port here. Pure/testable: no exec involved.
func modemsFromUbusList(raw []byte) ([]Modem, error) {
	var devices map[string]struct {
		Manufacturer string `json:"manufacturer"`
		Model        string `json:"model"`
	}
	if err := json.Unmarshal(raw, &devices); err != nil {
		return nil, fmt.Errorf("unexpected output: %w", err)
	}

	var modems []Modem
	for alias, info := range devices {
		modems = append(modems, Modem{
			ID:           alias,
			Manufacturer: info.Manufacturer,
			Model:        info.Model,
			Port:         alias,
			Method:       MethodUbus,
		})
	}
	return modems, nil
}

func getSignalsUbus(ctx context.Context, m Modem) ([]Signal, error) {
	raw, err := runUbus(ctx, "qmi", "metrics")
	if err != nil {
		return nil, fmt.Errorf("ubus call qmi metrics: %w", err)
	}
	return signalsFromUbusMetrics(raw, m.ID)
}

// ubusRATKeys are the top-level keys uqmi-ubus's "metrics" context
// object uses for per-technology data — mirrors quectel.lua's
// servingcell(), which always populates at most rsp["LTE"],
// rsp["WCDMA"], rsp["NR5G"] regardless of NSA/SA/single-RAT. NOT
// verified against a live registered modem (both WaveOS test modems
// stayed unregistered throughout development) — based on reading
// /lib/wavesys/lib/lua/uqmi/{quectel,uqmi-ubus}.lua source. Validate
// against real output once a modem actually registers.
var ubusRATKeys = []string{"NR5G", "LTE", "WCDMA"}

// signalsFromUbusMetrics parses `ubus call qmi metrics` output for one
// device (keyed by its netifd alias), accumulating one Signal per
// present per-technology context key, roles assigned via assignRoles.
// Each metric is decoded leniently as either a quoted numeric string
// (quectel.lua's convention, via Lua's tostring()) or a bare JSON
// number, since simcom.lua's encoding hasn't been read/confirmed to
// match. Pure/testable: no exec involved.
func signalsFromUbusMetrics(raw []byte, deviceID string) ([]Signal, error) {
	var all map[string]struct {
		Context map[string]json.RawMessage `json:"context"`
	}
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("unexpected output: %w", err)
	}
	dev, ok := all[deviceID]
	if !ok {
		return nil, fmt.Errorf("device %q not present in response", deviceID)
	}

	var candidates []Signal
	for _, key := range ubusRATKeys {
		legRaw, ok := dev.Context[key]
		if !ok {
			continue
		}
		var leg struct {
			RSRP json.RawMessage `json:"rsrp"`
			RSRQ json.RawMessage `json:"rsrq"`
			RSSI json.RawMessage `json:"rssi"`
			SINR json.RawMessage `json:"sinr"`
		}
		if err := json.Unmarshal(legRaw, &leg); err != nil {
			// Not an object we recognize (e.g. null, or a shape we
			// haven't seen) — skip rather than fail the whole call.
			continue
		}

		sig := NaNSignal()
		sig.Technology = normalizeTechnology(key)
		haveData := false
		if v, ok := ubusParseFloat(leg.RSRP); ok {
			sig.RSRP, haveData = v, true
		}
		if v, ok := ubusParseFloat(leg.RSRQ); ok {
			sig.RSRQ, haveData = v, true
		}
		if v, ok := ubusParseFloat(leg.RSSI); ok {
			sig.RSSI, haveData = v, true
		}
		if v, ok := ubusParseFloat(leg.SINR); ok {
			sig.SINR, haveData = v, true
		}
		if haveData {
			candidates = append(candidates, sig)
		}
	}
	return assignRoles(candidates), nil
}

// ubusParseFloat decodes a JSON scalar that's either a quoted numeric
// string or a bare JSON number into a float64; null/empty/unparseable
// reports ok=false.
func ubusParseFloat(raw json.RawMessage) (float64, bool) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// ubusRawString decodes a JSON scalar (quoted string, bare number, or
// null) into a plain Go string; null/empty becomes "".
func ubusRawString(raw json.RawMessage) string {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "null" {
		return ""
	}
	return s
}

func getCellInfoUbus(ctx context.Context, m Modem) (CellInfo, error) {
	var ci CellInfo

	locRaw, err := runUbus(ctx, "qmi", "location")
	if err != nil {
		return CellInfo{}, fmt.Errorf("ubus call qmi location: %w", err)
	}
	ci = cellInfoFromUbusLocation(locRaw, m.ID)

	// Operator name comes from a separate call; failing it shouldn't
	// drop the location info already gathered above.
	if metricsRaw, err := runUbus(ctx, "qmi", "metrics"); err == nil {
		ci.OperatorName = operatorNameFromUbusMetrics(metricsRaw, m.ID)
	}

	// RegistrationState/PacketServiceState aren't mapped yet: reading
	// the Lua source, the daemon does compute a registration state
	// (work.registration), but it isn't clear which ubus method (if
	// any) actually exposes it — left empty until confirmed against a
	// live registered modem rather than guessed.
	return ci, nil
}

// cellInfoFromUbusLocation parses `ubus call qmi location` output for
// one device. The "cell" sub-object is an empty Lua table encoded as a
// JSON array ("cell": []) when there's no data yet — observed live on
// both WaveOS test modems while unregistered — which fails to decode
// as the populated-object shape; that failure is treated the same as
// "no data" rather than an error. Pure/testable: no exec involved.
func cellInfoFromUbusLocation(raw []byte, deviceID string) CellInfo {
	var all map[string]struct {
		Cell json.RawMessage `json:"cell"`
	}
	if err := json.Unmarshal(raw, &all); err != nil {
		return CellInfo{}
	}
	dev, ok := all[deviceID]
	if !ok {
		return CellInfo{}
	}

	var cell struct {
		MCC    json.RawMessage `json:"mcc"`
		MNC    json.RawMessage `json:"mnc"`
		TAC    json.RawMessage `json:"tac"`
		CellID json.RawMessage `json:"cellid"`
	}
	if err := json.Unmarshal(dev.Cell, &cell); err != nil {
		return CellInfo{}
	}

	var ci CellInfo
	mcc := ubusRawString(cell.MCC)
	mnc := ubusRawString(cell.MNC)
	if mcc != "" && mnc != "" {
		mccInt, mccErr := strconv.Atoi(mcc)
		mncInt, mncErr := strconv.Atoi(mnc)
		if mccErr == nil && mncErr == nil {
			ci.PLMN = fmt.Sprintf("%d%02d", mccInt, mncInt)
		}
	}
	ci.TAC = ubusRawString(cell.TAC)
	ci.CellID = ubusRawString(cell.CellID)
	return ci
}

// operatorNameFromUbusMetrics extracts labels.opername for one device
// from `ubus call qmi metrics` output; "" if absent. Pure/testable.
func operatorNameFromUbusMetrics(raw []byte, deviceID string) string {
	var all map[string]struct {
		Labels struct {
			OperName string `json:"opername"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(raw, &all); err != nil {
		return ""
	}
	return all[deviceID].Labels.OperName
}
