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
)

var mmcliModemPathRe = regexp.MustCompile(`/Modem/(\d+)\b`)

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
		if err := exec.CommandContext(ctx, "mmcli", "-m", idx, "--signal-setup=5").Run(); err != nil {
			log.Printf("modem: mmcli -m %s --signal-setup failed: %v", idx, err)
		}

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

func getSignalModemManager(ctx context.Context, m Modem) (Signal, error) {
	info, err := mmcliKeyValue(ctx, "-m", m.ID, "--signal-get", "--output-keyvalue")
	if err != nil {
		return NaNSignal(), fmt.Errorf("mmcli -m %s --signal-get: %w", m.ID, err)
	}
	return signalFromKeyValue(info), nil
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

// signalFromKeyValue applies the 5g -> lte -> umts -> gsm priority
// cascade to `mmcli --signal-get --output-keyvalue` output and returns
// the first technology with any valid reading. Pure/testable: no exec
// involved.
func signalFromKeyValue(info map[string]string) Signal {
	for _, tech := range mmcliTechnologies {
		switch tech {
		case "gsm":
			// GSM only ever reports RSSI in ModemManager.
			rssi := mmcliParseFloat(info["modem.signal.gsm.rssi"])
			if !math.IsNaN(rssi) {
				sig := NaNSignal()
				sig.Technology, sig.RSSI = tech, rssi
				return sig
			}
		case "umts":
			// UMTS reports RSCP/EC-IO rather than RSRP/RSRQ; mapped onto
			// those fields here (they're the closest equivalents) so
			// callers can treat Signal uniformly across technologies.
			rscp := mmcliParseFloat(info["modem.signal.umts.rscp"])
			ecio := mmcliParseFloat(info["modem.signal.umts.ecio"])
			rssi := mmcliParseFloat(info["modem.signal.umts.rssi"])
			if !math.IsNaN(rscp) || !math.IsNaN(rssi) {
				return Signal{Technology: tech, RSRP: rscp, RSRQ: ecio, RSSI: rssi, SINR: math.NaN()}
			}
		default: // "5g", "lte"
			rsrp := mmcliParseFloat(info[fmt.Sprintf("modem.signal.%s.rsrp", tech)])
			rsrq := mmcliParseFloat(info[fmt.Sprintf("modem.signal.%s.rsrq", tech)])
			rssi := mmcliParseFloat(info[fmt.Sprintf("modem.signal.%s.rssi", tech)])
			sinr := mmcliParseFloat(info[fmt.Sprintf("modem.signal.%s.snr", tech)])
			if !math.IsNaN(rsrp) || !math.IsNaN(rssi) {
				return Signal{Technology: tech, RSRP: rsrp, RSRQ: rsrq, RSSI: rssi, SINR: sinr}
			}
		}
	}
	return NaNSignal()
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
