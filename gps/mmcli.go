package gps

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"dawn-agent/modem"
)

// mmcliEnableRetries/mmcliEnableRetryDelay mirror modem/mmcli.go's
// runMmcliSetup: --location-enable-gps-raw is root/PolicyKit-gated and
// can race ModemManager's own readiness right after a reboot.
const (
	mmcliEnableRetries    = 5
	mmcliEnableRetryDelay = 2 * time.Second
)

// enableModemManager turns on the gps-raw location source for m, once,
// best-effort, retrying to ride out the same ModemManager startup race
// documented in modem/mmcli.go's runMmcliSetup.
func enableModemManager(ctx context.Context, m modem.Modem) {
	var err error
	for attempt := 1; attempt <= mmcliEnableRetries; attempt++ {
		err = exec.CommandContext(ctx, "mmcli", "-m", m.ID, "--location-enable-gps-raw").Run()
		if err == nil {
			return
		}
		if attempt < mmcliEnableRetries {
			select {
			case <-ctx.Done():
				return
			case <-time.After(mmcliEnableRetryDelay):
			}
		}
	}
	log.Printf("gps: mmcli -m %s --location-enable-gps-raw failed after %d attempts: %v", m.ID, mmcliEnableRetries, err)
}

// getFixModemManager reads the current gps-raw fix via `mmcli -m <idx>
// --location-get --output-keyvalue`. ModemManager reports already-parsed
// decimal latitude/longitude/altitude here — no NMEA parsing needed,
// unlike the QMI backend.
func getFixModemManager(ctx context.Context, m modem.Modem) (Fix, error) {
	out, err := exec.CommandContext(ctx, "mmcli", "-m", m.ID, "--location-get", "--output-keyvalue").Output()
	if err != nil {
		return Fix{}, fmt.Errorf("mmcli -m %s --location-get: %w", m.ID, err)
	}
	return fixFromMmcliKeyValue(parseMmcliKeyValue(string(out))), nil
}

// fixFromMmcliKeyValue extracts a Fix from the gps-raw keys. Pure/
// testable: no exec involved.
func fixFromMmcliKeyValue(info map[string]string) Fix {
	lat, latOK := mmcliParseGPSFloat(info["modem.location.gps.latitude"])
	lon, lonOK := mmcliParseGPSFloat(info["modem.location.gps.longitude"])
	if !latOK || !lonOK {
		return Fix{Time: time.Now()}
	}
	alt, _ := mmcliParseGPSFloat(info["modem.location.gps.altitude"])
	return Fix{Latitude: lat, Longitude: lon, Altitude: alt, Valid: true, Time: time.Now()}
}

// mmcliParseGPSFloat treats mmcli's "--" (no data) sentinel, and an
// unset/empty string, as "no fix yet" rather than a parse error.
func mmcliParseGPSFloat(s string) (float64, bool) {
	if s == "" || s == "--" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parseMmcliKeyValue converts lines of the form "key : value" (as
// emitted by mmcli --output-keyvalue) into a map. Duplicated from
// modem/mmcli.go's unexported parseKeyValue rather than exported across
// packages for this one shared helper.
func parseMmcliKeyValue(input string) map[string]string {
	res := make(map[string]string)
	for _, l := range strings.Split(input, "\n") {
		parts := strings.SplitN(l, " : ", 2)
		if len(parts) == 2 {
			res[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return res
}
