package modem

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// candidateQMIDevices returns plausible QMI/MBIM control device nodes.
// Both protocols are exposed through the same cdc-wdm character device
// class, so the same glob covers both; which protocol actually answers
// is determined by which CLI tool succeeds against it.
func candidateQMIDevices() []string {
	devs, _ := filepath.Glob("/dev/cdc-wdm*")
	return devs
}

func toolExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// DetectQMIMBIM probes /dev/cdc-wdm* control devices with qmicli and,
// failing that, mbimcli. This is the fallback for modems ModemManager
// doesn't recognize (or isn't running/installed) but that still expose
// a standard QMI or MBIM control port.
//
// NOTE: qmicli/mbimcli text output varies across libqmi/libmbim
// versions; the parsing below targets the commonly documented format
// and should be validated against the actual tool versions on the
// gateway board.
func DetectQMIMBIM(ctx context.Context) ([]Modem, error) {
	haveQMI := toolExists("qmicli")
	haveMBIM := toolExists("mbimcli")
	if !haveQMI && !haveMBIM {
		return nil, fmt.Errorf("neither qmicli nor mbimcli found in PATH")
	}

	var modems []Modem
	for _, dev := range candidateQMIDevices() {
		if haveQMI {
			if m, ok := probeQMI(ctx, dev); ok {
				modems = append(modems, m)
				continue
			}
		}
		if haveMBIM {
			if m, ok := probeMBIM(ctx, dev); ok {
				modems = append(modems, m)
			}
		}
	}
	return modems, nil
}

func probeQMI(ctx context.Context, dev string) (Modem, bool) {
	// qmicli rejects multiple --dms-get-* actions in one invocation
	// ("error: too many DMS actions requested"), so manufacturer and
	// model must be fetched separately.
	manuOut, err := exec.CommandContext(ctx, "qmicli", "-d", dev, "--dms-get-manufacturer").Output()
	if err != nil {
		return Modem{}, false
	}
	modelOut, err := exec.CommandContext(ctx, "qmicli", "-d", dev, "--dms-get-model").Output()
	if err != nil {
		return Modem{}, false
	}
	kv := parseCLIKeyValue(string(manuOut) + string(modelOut))
	return Modem{
		ID:           dev,
		Manufacturer: kv["manufacturer"],
		Model:        kv["model"],
		Port:         dev,
		Method:       MethodQMI,
	}, true
}

func probeMBIM(ctx context.Context, dev string) (Modem, bool) {
	out, err := exec.CommandContext(ctx, "mbimcli", "-d", dev, "--query-device-caps").Output()
	if err != nil {
		return Modem{}, false
	}
	kv := parseCLIKeyValue(string(out))
	return Modem{
		ID:           dev,
		Manufacturer: kv["deviceid"],
		Model:        kv["hardwareinfo"],
		Port:         dev,
		Method:       MethodMBIM,
	}, true
}

// parseCLIKeyValue extracts "Key: 'value'" style lines common to both
// qmicli and mbimcli human-readable output.
var cliFieldRe = regexp.MustCompile(`^\s*([A-Za-z0-9 _/-]+):\s*'([^']*)'`)

func parseCLIKeyValue(s string) map[string]string {
	res := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		m := cliFieldRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(m[1]), " ", ""))
		res[key] = m[2]
	}
	return res
}

func getSignalQMI(ctx context.Context, m Modem) (Signal, error) {
	out, err := exec.CommandContext(ctx, "qmicli", "-d", m.Port, "--nas-get-signal-info").CombinedOutput()
	if err == nil {
		return parseQMISignalOutput(string(out)), nil
	}
	// Older libqmi versions only have the coarser --nas-get-signal-strength,
	// which is structured differently (metric-name sections containing
	// "Network '<tech>': 'value'" lines, rather than technology sections).
	out, err = exec.CommandContext(ctx, "qmicli", "-d", m.Port, "--nas-get-signal-strength").CombinedOutput()
	if err != nil {
		return NaNSignal(), fmt.Errorf("qmicli signal query on %s: %w", m.Port, err)
	}
	return parseQMISignalStrengthOutput(string(out)), nil
}

var qmiTechHeaderRe = regexp.MustCompile(`^([A-Za-z0-9/ -]+):\s*$`)
var qmiMetricRe = regexp.MustCompile(`(?i)^\s*(RSSI|RSRQ|RSRP|SNR|SINR|ECIO|RSCP)\s*:\s*'?(-?[0-9.]+)`)

func parseQMISignalOutput(s string) Signal {
	sig := NaNSignal()
	currentTech := ""
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)

		// Technology section headers are unindented lines like "LTE:";
		// metric lines are indented under them.
		if !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, " ") {
			if h := qmiTechHeaderRe.FindStringSubmatch(trimmed); h != nil {
				currentTech = normalizeTechnology(h[1])
				continue
			}
		}

		mm := qmiMetricRe.FindStringSubmatch(trimmed)
		if mm == nil {
			continue
		}
		v, err := strconv.ParseFloat(mm[2], 64)
		if err != nil {
			continue
		}
		switch strings.ToUpper(mm[1]) {
		case "RSSI":
			sig.RSSI = v
		case "RSRP", "RSCP":
			sig.RSRP = v
		case "RSRQ", "ECIO":
			sig.RSRQ = v
		case "SNR", "SINR":
			sig.SINR = v
		}
		// Track whichever section's metrics most recently updated sig, so
		// Technology stays consistent with the fields it's paired with.
		sig.Technology = currentTech
	}
	return sig
}

var qmiSSMetricHeaderRe = regexp.MustCompile(`^(RSSI|RSRQ|SNR|ECIO)\s*:\s*$`)
var qmiSSNetworkValueRe = regexp.MustCompile(`Network\s+'([A-Za-z0-9]+)'\s*:\s*'(-?[0-9.]+)`)

// parseQMISignalStrengthOutput handles the older `qmicli
// --nas-get-signal-strength` format, e.g.:
//
//	Current:
//	    Network 'lte': '-57 dBm'
//	RSSI:
//	    Network 'lte': '-57 dBm'
//	ECIO:
//	    Network 'lte': '-2.5 dBm'
//	IO: '-106 dBm'
//	SINR (8): '9.0 dB'
//	RSRQ:
//	    Network 'lte': '-10 dB'
//	SNR:
//	    Network 'lte': '14.2 dB'
//
// Unlike --nas-get-signal-info, sections here are metric names (RSSI,
// RSRQ, SNR, ECIO) rather than technologies, and the technology is
// embedded in each value line's "Network '<tech>'" instead. This
// command doesn't report RSRP at all, so that field is always NaN.
// Standalone lines with no "Network" breakdown (IO, "SINR (N)") aren't
// mapped onto Signal.
func parseQMISignalStrengthOutput(s string) Signal {
	sig := NaNSignal()
	currentMetric := ""
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)

		if !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, " ") {
			if h := qmiSSMetricHeaderRe.FindStringSubmatch(trimmed); h != nil {
				currentMetric = strings.ToUpper(h[1])
				continue
			}
			if strings.HasSuffix(trimmed, ":") {
				// An unindented header we don't track metrics for (e.g. "Current:").
				currentMetric = ""
				continue
			}
		}

		mm := qmiSSNetworkValueRe.FindStringSubmatch(trimmed)
		if mm == nil {
			continue
		}
		v, err := strconv.ParseFloat(mm[2], 64)
		if err != nil {
			continue
		}
		tech := normalizeTechnology(mm[1])
		switch currentMetric {
		case "RSSI":
			sig.RSSI = v
			sig.Technology = tech
		case "RSRQ":
			sig.RSRQ = v
			sig.Technology = tech
		case "SNR":
			sig.SINR = v
			sig.Technology = tech
		}
	}
	return sig
}

func normalizeTechnology(h string) string {
	h = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(h, ":")))
	switch {
	case strings.Contains(h, "5g"), strings.Contains(h, "nr"):
		return "5g"
	case strings.Contains(h, "lte"):
		return "lte"
	case strings.Contains(h, "umts"), strings.Contains(h, "wcdma"):
		return "umts"
	case strings.Contains(h, "gsm"):
		return "gsm"
	default:
		return h
	}
}

func getSignalMBIM(ctx context.Context, m Modem) (Signal, error) {
	out, err := exec.CommandContext(ctx, "mbimcli", "-d", m.Port, "--query-signal-state").CombinedOutput()
	if err != nil {
		return NaNSignal(), fmt.Errorf("mbimcli signal query on %s: %w", m.Port, err)
	}
	return parseMBIMSignalOutput(string(out)), nil
}

var mbimRSSIRe = regexp.MustCompile(`RSSI:\s*'(-?\d+)'(?:\s*\(([-0-9.]+)\s*dBm\))?`)

// parseMBIMSignalOutput handles the basic MBIM signal state query,
// which only exposes a coarse RSSI (0-31, 99 = unknown) or, on newer
// mbimcli builds, a dBm value in parentheses. RSRP/RSRQ/SINR require
// the MS signal state extension (--ms-query-signal-state), which isn't
// implemented yet — a natural next step once tested against real
// hardware.
func parseMBIMSignalOutput(s string) Signal {
	sig := NaNSignal()
	m := mbimRSSIRe.FindStringSubmatch(s)
	if m == nil {
		return sig
	}
	if m[2] != "" {
		if v, err := strconv.ParseFloat(m[2], 64); err == nil {
			sig.RSSI = v
		}
		return sig
	}
	if raw, err := strconv.Atoi(m[1]); err == nil && raw != 99 {
		sig.RSSI = -113 + 2*float64(raw)
	}
	return sig
}
