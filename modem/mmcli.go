package modem

import (
	"context"
	"fmt"
	"log"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var mmcliModemPathRe = regexp.MustCompile(`/Modem/(\d+)\b`)

// mmcliSetupRetries/mmcliSetupRetryDelay bound how hard the one-time
// root/PolicyKit-gated setup calls (--signal-setup,
// --location-enable-3gpp) are retried against a startup race with
// ModemManager itself: systemd's After=ModemManager.service only
// guarantees start *order*, not that ModemManager is actually
// D-Bus/PolicyKit-ready yet, so the very first attempt right after a
// reboot can fail even though retrying moments later succeeds —
// observed live (radio_cell telemetry silently going dark on reboot
// until the service was manually restarted).
const (
	mmcliSetupRetries    = 5
	mmcliSetupRetryDelay = 2 * time.Second
)

// runMmcliSetup runs a one-shot mmcli setup command, retrying on
// failure to ride out that race. Logs only the final failure, not each
// attempt, so a modem that will never succeed (e.g. genuinely no
// PolicyKit grant) doesn't spam the log.
func runMmcliSetup(ctx context.Context, desc string, args ...string) {
	var err error
	for attempt := 1; attempt <= mmcliSetupRetries; attempt++ {
		err = exec.CommandContext(ctx, "mmcli", args...).Run()
		if err == nil {
			return
		}
		if attempt < mmcliSetupRetries {
			select {
			case <-ctx.Done():
				return
			case <-time.After(mmcliSetupRetryDelay):
			}
		}
	}
	log.Printf("modem: %s failed after %d attempts: %v", desc, mmcliSetupRetries, err)
}

// mmcliTechnologies lists the ModemManager signal sections to check, in
// priority order (most capable/specific first). A modem only exposes
// the sections its currently registered technology supports.
var mmcliTechnologies = []string{"5g", "lte", "umts", "gsm"}

// DetectModemManager lists modems known to ModemManager via `mmcli -L`
// and fetches identifying details for each one. Returns (nil, err) only
// when mmcli itself can't be run at all; an empty slice with a nil
// error means mmcli ran fine but ModemManager isn't managing any modem.
func DetectModemManager(ctx context.Context) ([]Modem, error) {
	if _, err := exec.LookPath("mmcli"); err != nil {
		return nil, fmt.Errorf("mmcli not found in PATH: %w", err)
	}

	out, err := exec.CommandContext(ctx, "mmcli", "-L").Output()
	if err != nil {
		return nil, fmt.Errorf("mmcli -L: %w", err)
	}

	var modems []Modem
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		match := mmcliModemPathRe.FindStringSubmatch(line)
		if match == nil || seen[match[1]] {
			continue
		}
		seen[match[1]] = true
		idx := match[1]

		info, err := mmcliKeyValue(ctx, "-m", idx, "--output-keyvalue")
		if err != nil {
			log.Printf("modem: mmcli -m %s failed: %v", idx, err)
			continue
		}

		// Ask ModemManager to (re)start polling signal quality every 5s;
		// best effort since some firmwares reject repeated setup calls.
		runMmcliSetup(ctx, fmt.Sprintf("mmcli -m %s --signal-setup", idx),
			"-m", idx, "--signal-setup=5")

		// Enable the 3GPP location source so --location-get can later
		// report TAC/Cell ID; both this and --location-get itself need
		// root/PolicyKit, same as --signal-setup above.
		runMmcliSetup(ctx, fmt.Sprintf("mmcli -m %s --location-enable-3gpp", idx),
			"-m", idx, "--location-enable-3gpp")

		modems = append(modems, modemFromKeyValue(idx, info))
	}
	return modems, nil
}

// modemFromKeyValue builds a Modem from `mmcli -m <idx> --output-keyvalue`
// output. Pure/testable: no exec involved.
func modemFromKeyValue(idx string, info map[string]string) Modem {
	return Modem{
		ID:           idx,
		Manufacturer: info["modem.generic.manufacturer"],
		Model:        info["modem.generic.model"],
		Port:         info["modem.generic.primary-port"],
		Method:       MethodModemManager,
	}
}

func getSignalsModemManager(ctx context.Context, m Modem) ([]Signal, error) {
	info, err := mmcliKeyValue(ctx, "-m", m.ID, "--signal-get", "--output-keyvalue")
	if err != nil {
		return nil, fmt.Errorf("mmcli -m %s --signal-get: %w", m.ID, err)
	}
	return signalsFromKeyValue(info), nil
}

func getCellInfoModemManager(ctx context.Context, m Modem) (CellInfo, error) {
	info, err := mmcliKeyValue(ctx, "-m", m.ID, "--output-keyvalue")
	if err != nil {
		return CellInfo{}, fmt.Errorf("mmcli -m %s: %w", m.ID, err)
	}
	ci := cellInfoFrom3GPPKeyValue(info)

	// TAC/Cell ID need a separate call: the 3gpp-lac-ci location source
	// enabled (DetectModemManager does this) plus root/PolicyKit. Degrade
	// gracefully rather than failing the whole call over it.
	loc, err := mmcliKeyValue(ctx, "-m", m.ID, "--location-get", "--output-keyvalue")
	if err != nil {
		log.Printf("modem: mmcli -m %s --location-get failed (needs root + location enabled): %v", m.ID, err)
		return ci, nil
	}
	applyLocationKeyValue(&ci, loc)
	return ci, nil
}

// mmcliStringOrEmpty normalizes mmcli's "--" (no data) sentinel to "".
func mmcliStringOrEmpty(s string) string {
	if s == "--" {
		return ""
	}
	return s
}

// cellInfoFrom3GPPKeyValue extracts PLMN/registration fields from the
// same `mmcli -m <idx> --output-keyvalue` dump DetectModemManager
// already fetches for identity — no extra mmcli call needed. Pure/
// testable: no exec involved.
func cellInfoFrom3GPPKeyValue(info map[string]string) CellInfo {
	return CellInfo{
		PLMN:               mmcliStringOrEmpty(info["modem.3gpp.operator-code"]),
		OperatorName:       mmcliStringOrEmpty(info["modem.3gpp.operator-name"]),
		RegistrationState:  mmcliStringOrEmpty(info["modem.3gpp.registration-state"]),
		PacketServiceState: mmcliStringOrEmpty(info["modem.3gpp.packet-service-state"]),
	}
}

// applyLocationKeyValue fills in TAC/CellID from `mmcli -m <idx>
// --location-get --output-keyvalue`. ModemManager reports both as hex
// strings (e.g. "00BEBE"); normalized here to decimal so CellInfo is
// consistent across backends (uqmi reports decimal natively). Pure/
// testable: no exec involved.
func applyLocationKeyValue(ci *CellInfo, loc map[string]string) {
	ci.TAC = mmcliHexToDecimal(loc["modem.location.3gpp.tac"])
	ci.CellID = mmcliHexToDecimal(loc["modem.location.3gpp.cid"])
}

// mmcliHexToDecimal converts one of ModemManager's hex location fields
// to a decimal string; "" (or unparseable) stays "".
func mmcliHexToDecimal(s string) string {
	s = mmcliStringOrEmpty(s)
	if s == "" {
		return ""
	}
	v, err := strconv.ParseInt(s, 16, 64)
	if err != nil {
		return ""
	}
	return strconv.FormatInt(v, 10)
}

// mmcliParseFloat treats mmcli's various "no data" spellings as NaN.
func mmcliParseFloat(s string) float64 {
	if s == "" || s == "--" || s == "-32768" || s == "-32768.00" {
		return math.NaN()
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return math.NaN()
	}
	return v
}

// mmcliSignalForTech extracts Signal fields (Role left unset — callers
// assign roles afterward) for one mmcli technology section
// (5g/lte/umts/gsm) from `mmcli --signal-get --output-keyvalue` output,
// or ok=false if that section has no valid data. Pure/testable: no
// exec involved.
func mmcliSignalForTech(info map[string]string, tech string) (Signal, bool) {
	switch tech {
	case "gsm":
		// GSM only ever reports RSSI in ModemManager.
		rssi := mmcliParseFloat(info["modem.signal.gsm.rssi"])
		if math.IsNaN(rssi) {
			return Signal{}, false
		}
		sig := NaNSignal()
		sig.Technology, sig.RSSI = tech, rssi
		return sig, true
	case "umts":
		// UMTS reports RSCP/EC-IO rather than RSRP/RSRQ; mapped onto
		// those fields here (they're the closest equivalents) so
		// callers can treat Signal uniformly across technologies.
		rscp := mmcliParseFloat(info["modem.signal.umts.rscp"])
		ecio := mmcliParseFloat(info["modem.signal.umts.ecio"])
		rssi := mmcliParseFloat(info["modem.signal.umts.rssi"])
		if math.IsNaN(rscp) && math.IsNaN(rssi) {
			return Signal{}, false
		}
		return Signal{Technology: tech, RSRP: rscp, RSRQ: ecio, RSSI: rssi, SINR: math.NaN()}, true
	default: // "5g", "lte"
		rsrp := mmcliParseFloat(info[fmt.Sprintf("modem.signal.%s.rsrp", tech)])
		rsrq := mmcliParseFloat(info[fmt.Sprintf("modem.signal.%s.rsrq", tech)])
		rssi := mmcliParseFloat(info[fmt.Sprintf("modem.signal.%s.rssi", tech)])
		sinr := mmcliParseFloat(info[fmt.Sprintf("modem.signal.%s.snr", tech)])
		if math.IsNaN(rsrp) && math.IsNaN(rssi) {
			return Signal{}, false
		}
		return Signal{Technology: tech, RSRP: rsrp, RSRQ: rsrq, RSSI: rssi, SINR: sinr}, true
	}
}

// signalsFromKeyValue scans all mmcli technology sections (5g/lte/
// umts/gsm) in `mmcli --signal-get --output-keyvalue` output and
// returns one Signal per currently active leg, roles assigned via
// assignRoles — so an EN-DC modem reporting both LTE and 5G NR data
// simultaneously yields two Signals (lte anchor + nr scg), not one.
// Pure/testable: no exec involved.
func signalsFromKeyValue(info map[string]string) []Signal {
	var candidates []Signal
	for _, tech := range mmcliTechnologies {
		if sig, ok := mmcliSignalForTech(info, tech); ok {
			candidates = append(candidates, sig)
		}
	}
	return assignRoles(candidates)
}

// mmcliKeyValue runs mmcli with the given args and parses its
// "--output-keyvalue" formatted output into a map.
func mmcliKeyValue(ctx context.Context, args ...string) (map[string]string, error) {
	out, err := exec.CommandContext(ctx, "mmcli", args...).Output()
	if err != nil {
		return nil, err
	}
	return parseKeyValue(string(out)), nil
}

// parseKeyValue converts lines of the form "key : value" (as emitted by
// mmcli --output-keyvalue) into a map. Blank lines and malformed
// entries are ignored.
func parseKeyValue(input string) map[string]string {
	res := make(map[string]string)
	for _, l := range strings.Split(input, "\n") {
		parts := strings.SplitN(l, " : ", 2)
		if len(parts) == 2 {
			res[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return res
}
