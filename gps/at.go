package gps

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dawn-agent/modem"
)

// atGPSVendor identifies which AT GPS command dialect a modem's
// ATI-reported manufacturer implies. Confirmed live on both 6global
// and dawn6g's SIMCom (AT+CGPS/AT+CGPSINFO) and Quectel (AT+QGPS/
// AT+QGPSLOC) modems: on both boards the GNSS engine enables
// successfully and the query command returns a well-formed "no fix
// yet" reply (SIMCom: all-empty +CGPSINFO fields; Quectel: +CME ERROR:
// 516) — neither board has an antenna connected, so no real fix has
// been obtained yet, same as every other GPS backend in this package.
type atGPSVendor int

const (
	atGPSVendorUnknown atGPSVendor = iota
	atGPSVendorSIMCom
	atGPSVendorQuectel
)

// detectATGPSVendor maps an AT-detected modem's manufacturer string
// (from modem.DetectAT's ATI probe) to the GPS command dialect to use.
func detectATGPSVendor(m modem.Modem) atGPSVendor {
	s := strings.ToLower(m.Manufacturer + " " + m.Model)
	switch {
	case strings.Contains(s, "simcom"):
		return atGPSVendorSIMCom
	case strings.Contains(s, "quectel"):
		return atGPSVendorQuectel
	default:
		return atGPSVendorUnknown
	}
}

// atEnableRetries/atEnableRetryDelay mirror the ModemManager/QMI
// backends' own retry pattern for the one-time GNSS-engine-enable call.
const (
	atEnableRetries    = 5
	atEnableRetryDelay = 2 * time.Second
)

var gnssStateRe = regexp.MustCompile(`\+[CQ]GPS:\s*(\d+)`)

// parseGNSSState reports whether a vendor GNSS state query's reply
// (AT+CGPS?/AT+QGPS?) says the engine is already on. Both
// "+CGPS: <state>,<mode>" (SIMCom) and "+QGPS: <state>" (Quectel) put
// a 1/0 state flag right after the colon, hence one shared regexp.
// Pure/testable: no exec involved.
func parseGNSSState(resp string) bool {
	m := gnssStateRe.FindStringSubmatch(resp)
	return m != nil && m[1] == "1"
}

// gnssAlreadyRunning queries the vendor's own GNSS state command
// (stateCmd) and reports whether the engine is already enabled.
// Confirmed live that both vendors reject a *second* enable call when
// the engine is already on, but signal it differently — SIMCom with a
// plain "ERROR" (no CME code at all), Quectel with "+CME ERROR: 504"
// ("Session is ongoing" per Quectel's own error table and a
// ModemManager bug report reading it the same way). Checking the
// state command's ground truth afterward, rather than matching each
// vendor's specific error text, handles both uniformly and doesn't
// depend on that text staying stable across firmware versions.
func gnssAlreadyRunning(port, stateCmd string) bool {
	resp, err := atSend(port, stateCmd, 3*time.Second)
	if err != nil {
		return false
	}
	return parseGNSSState(resp)
}

// enableAT turns on the modem's own GNSS engine via the vendor-
// specific AT command, retried best-effort the same way
// enableModemManager/NewQMISession's --loc-start are. A failed enable
// attempt is treated as success if gnssAlreadyRunning confirms the
// engine is already on — see that function's doc comment for why.
func enableAT(ctx context.Context, m modem.Modem, vendor atGPSVendor) error {
	var cmd, stateCmd string
	switch vendor {
	case atGPSVendorSIMCom:
		cmd, stateCmd = "AT+CGPS=1,1", "AT+CGPS?" // mode 1 = standalone GNSS, no AGPS assistance server
	case atGPSVendorQuectel:
		cmd, stateCmd = "AT+QGPS=1", "AT+QGPS?"
	default:
		return fmt.Errorf("gps: AT GPS not supported for manufacturer %q (only SIMCom/Quectel recognized)", m.Manufacturer)
	}

	var lastErr error
	for attempt := 1; attempt <= atEnableRetries; attempt++ {
		resp, err := atSend(m.Port, cmd, 3*time.Second)
		if err == nil && strings.Contains(resp, "OK") {
			return nil
		}
		if err == nil && gnssAlreadyRunning(m.Port, stateCmd) {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("unexpected response to %s: %q", cmd, strings.TrimSpace(resp))
		}
		if attempt < atEnableRetries {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(atEnableRetryDelay):
			}
		}
	}
	return fmt.Errorf("%s on %s failed after %d attempts: %w", cmd, m.Port, atEnableRetries, lastErr)
}

// getFixAT queries the modem's current GNSS fix via the vendor-
// specific AT command and parses the reply.
func getFixAT(m modem.Modem, vendor atGPSVendor) (Fix, error) {
	switch vendor {
	case atGPSVendorSIMCom:
		resp, err := atSend(m.Port, "AT+CGPSINFO", 3*time.Second)
		if err != nil {
			return Fix{}, fmt.Errorf("AT+CGPSINFO on %s: %w", m.Port, err)
		}
		return parseCGPSINFO(resp)
	case atGPSVendorQuectel:
		resp, err := atSend(m.Port, "AT+QGPSLOC=2", 3*time.Second)
		if err != nil {
			return Fix{}, fmt.Errorf("AT+QGPSLOC=2 on %s: %w", m.Port, err)
		}
		return parseQGPSLOC(resp)
	default:
		return Fix{}, fmt.Errorf("gps: AT GPS not supported for manufacturer %q", m.Manufacturer)
	}
}

var cgpsinfoRe = regexp.MustCompile(`\+CGPSINFO:\s*(.*)`)

// parseCGPSINFO parses SIMCom's AT+CGPSINFO reply: 9 comma-separated
// fields — latitude, N/S, longitude, E/W, date, UTC time, altitude,
// speed, course. Latitude/longitude are NMEA-style ddmm.mmmmmm (same
// encoding as the QMI backend's $GxGGA sentences, see qmi.go's
// nmeaCoordToDecimal), not decimal degrees. Confirmed live (no SIM, no
// antenna, on both 6global and dawn6g):
// "+CGPSINFO: ,,,,,,,,\r\n\r\nOK" — all 9 fields empty means "GNSS
// engine running, no fix yet", a valid state (Valid=false), not an
// error.
func parseCGPSINFO(resp string) (Fix, error) {
	m := cgpsinfoRe.FindStringSubmatch(resp)
	if m == nil {
		return Fix{}, fmt.Errorf("no +CGPSINFO in response: %q", strings.TrimSpace(resp))
	}
	fields := strings.Split(strings.TrimSpace(m[1]), ",")
	if len(fields) < 7 {
		return Fix{}, fmt.Errorf("malformed +CGPSINFO reply (got %d fields): %q", len(fields), m[1])
	}
	if fields[0] == "" {
		return Fix{Time: time.Now()}, nil
	}
	lat, ok := nmeaCoordToDecimal(fields[0], fields[1])
	if !ok {
		return Fix{}, fmt.Errorf("unparseable latitude in +CGPSINFO: %q", m[1])
	}
	lon, ok := nmeaCoordToDecimal(fields[2], fields[3])
	if !ok {
		return Fix{}, fmt.Errorf("unparseable longitude in +CGPSINFO: %q", m[1])
	}
	alt, _ := strconv.ParseFloat(fields[6], 64)
	return Fix{Latitude: lat, Longitude: lon, Altitude: alt, Valid: true, Time: time.Now()}, nil
}

var qgpslocRe = regexp.MustCompile(`\+QGPSLOC:\s*(.*)`)
var cmeErrorRe = regexp.MustCompile(`\+CME ERROR:\s*(\d+)`)

// qgpslocNoFixError is the CME error code Quectel's AT+QGPSLOC=2
// returns when the GNSS engine hasn't acquired a fix yet — confirmed
// live on both 6global and dawn6g (neither has an antenna connected).
// Not something we're just assuming from a datasheet: reproduced the
// same way on two different boards.
const qgpslocNoFixError = "516"

// parseQGPSLOC parses Quectel's AT+QGPSLOC=2 reply: 11 comma-separated
// fields — UTC time, latitude, longitude, HDOP, altitude, fix mode,
// course, speed(km/h), speed(knots), date, satellite count. Mode 2
// reports latitude/longitude directly as signed decimal degrees, not
// NMEA ddmm.mmmm (confirmed via Quectel documentation/community
// examples — not confirmed live, since no real fix has been obtained
// on either board yet). Some Quectel firmware/modules are
// community-reported to return an incorrect (positive) sign on
// longitude in mode 2 — worth double-checking against a known
// reference point once a real fix is available on this RM520N-GL
// specifically, not yet verified either way.
func parseQGPSLOC(resp string) (Fix, error) {
	if m := cmeErrorRe.FindStringSubmatch(resp); m != nil {
		if m[1] == qgpslocNoFixError {
			return Fix{Time: time.Now()}, nil
		}
		return Fix{}, fmt.Errorf("AT+QGPSLOC=2 CME ERROR %s: %q", m[1], strings.TrimSpace(resp))
	}
	m := qgpslocRe.FindStringSubmatch(resp)
	if m == nil {
		return Fix{}, fmt.Errorf("no +QGPSLOC in response: %q", strings.TrimSpace(resp))
	}
	fields := strings.Split(strings.TrimSpace(m[1]), ",")
	if len(fields) < 5 {
		return Fix{}, fmt.Errorf("malformed +QGPSLOC reply (got %d fields): %q", len(fields), m[1])
	}
	lat, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return Fix{}, fmt.Errorf("unparseable latitude in +QGPSLOC: %q", m[1])
	}
	lon, err := strconv.ParseFloat(fields[2], 64)
	if err != nil {
		return Fix{}, fmt.Errorf("unparseable longitude in +QGPSLOC: %q", m[1])
	}
	alt, _ := strconv.ParseFloat(fields[4], 64)
	return Fix{Latitude: lat, Longitude: lon, Altitude: alt, Valid: true, Time: time.Now()}, nil
}

// atSend configures the serial port with `stty` (raw mode, no echo,
// 115200 8N1) and writes a single AT command, returning whatever the
// modem replies within the given timeout. Duplicated from
// modem/at.go's unexported sendAT rather than exported across packages
// for this one shared helper — same reasoning as gps/mmcli.go's
// parseMmcliKeyValue.
func atSend(dev, cmd string, timeout time.Duration) (string, error) {
	// stty commonly exits non-zero here ("unable to perform all requested
	// operations") even though it applies the settings that matter and the
	// port works fine afterward — so a failure is logged, not fatal.
	if err := exec.Command("stty", "-F", dev, "115200", "raw", "-echo").Run(); err != nil {
		log.Printf("gps: stty %s: %v (continuing anyway)", dev, err)
	}

	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := f.WriteString(cmd + "\r\n"); err != nil {
		return "", err
	}

	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 4096)
		var sb strings.Builder
		for {
			n, err := f.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
				if strings.Contains(sb.String(), "OK") || strings.Contains(sb.String(), "ERROR") {
					ch <- result{sb.String(), nil}
					return
				}
			}
			if err != nil {
				ch <- result{sb.String(), err}
				return
			}
		}
	}()

	select {
	case r := <-ch:
		if r.err != nil && r.s == "" {
			return "", r.err
		}
		return r.s, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("timeout waiting for response from %s", dev)
	}
}
