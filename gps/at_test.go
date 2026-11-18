package gps

import (
	"math"
	"reflect"
	"testing"
	"time"

	"dawn-agent/modem"
)

func TestDetectATGPSVendor(t *testing.T) {
	tests := []struct {
		manufacturer, model string
		want                atGPSVendor
	}{
		{"SIMCOM INCORPORATED", "SIMCOM_SIM8262E-M2", atGPSVendorSIMCom},
		{"Quectel", "RM520N-GL", atGPSVendorQuectel},
		{"", "", atGPSVendorUnknown},
		{"QUALCOMM INCORPORATED", "0", atGPSVendorUnknown},
	}
	for _, tt := range tests {
		m := modem.Modem{Manufacturer: tt.manufacturer, Model: tt.model}
		if got := detectATGPSVendor(m); got != tt.want {
			t.Errorf("detectATGPSVendor(%q, %q) = %v, want %v", tt.manufacturer, tt.model, got, tt.want)
		}
	}
}

func TestParseCGPSINFO_NoFix(t *testing.T) {
	// Real captured output from a live SIMCom SIM8262E-M2 (6global and
	// dawn6g, no antenna connected, AT+CGPS=1,1 already sent).
	resp := "AT+CGPSINFO\r\n+CGPSINFO: ,,,,,,,,\r\n\r\nOK\r\n"
	fix, err := parseCGPSINFO(resp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fix.Valid {
		t.Errorf("expected Valid=false for all-empty fields, got %+v", fix)
	}
}

func TestParseCGPSINFO_ValidFix(t *testing.T) {
	// Documented example (DFRobot SIMCom AT command handbook), not a
	// live capture — no real fix has been obtained on either board
	// (no antenna). Format: lat,N/S,lon,E/W,date,UTC,altitude,speed,course.
	resp := "AT+CGPSINFO\r\n+CGPSINFO: 1234.567891,N,12345.678912,E,141023,012523.0,455.1,0.0,\r\n\r\nOK\r\n"
	fix, err := parseCGPSINFO(resp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fix.Valid {
		t.Fatalf("expected Valid=true, got %+v", fix)
	}
	// 1234.567891 -> 12 deg + 34.567891/60 min
	wantLat := 12 + 34.567891/60
	if math.Abs(fix.Latitude-wantLat) > 1e-6 {
		t.Errorf("Latitude = %v, want ~%v", fix.Latitude, wantLat)
	}
	wantLon := 123 + 45.678912/60
	if math.Abs(fix.Longitude-wantLon) > 1e-6 {
		t.Errorf("Longitude = %v, want ~%v", fix.Longitude, wantLon)
	}
	if fix.Altitude != 455.1 {
		t.Errorf("Altitude = %v, want 455.1", fix.Altitude)
	}
}

func TestParseCGPSINFO_RealReferenceFix(t *testing.T) {
	// Ground-truth check against a real fix obtained via manual
	// AT-over-serial testing on aaeon's SIMCom modem (antenna
	// confirmed working, XTRA assistance data enabled) - captured as
	// $GPRMC,192721.00,A,4132.984808,N,00824.784113,W,... (see
	// CLAUDE.md's gps/ package section), decoding to 41.549747,
	// -8.413069 (Portugal). +CGPSINFO uses the same NMEA ddmm.mmmm
	// lat/lon encoding as the raw $GPRMC sentence, so the reply below
	// is built from those exact field values, not a literal captured
	// +CGPSINFO line - the point is testing our parser against known
	// real-world numbers, not just documented/community examples.
	resp := "AT+CGPSINFO\r\n+CGPSINFO: 4132.984808,N,00824.784113,W,171126,192721.0,0.0,0.0,\r\n\r\nOK\r\n"
	fix, err := parseCGPSINFO(resp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fix.Valid {
		t.Fatalf("expected Valid=true, got %+v", fix)
	}
	const wantLat, wantLon = 41.549747, -8.413069
	if math.Abs(fix.Latitude-wantLat) > 1e-6 {
		t.Errorf("Latitude = %v, want %v", fix.Latitude, wantLat)
	}
	if math.Abs(fix.Longitude-wantLon) > 1e-6 {
		t.Errorf("Longitude = %v, want %v", fix.Longitude, wantLon)
	}
}

func TestParseCGPSINFO_Malformed(t *testing.T) {
	for _, resp := range []string{
		"AT+CGPSINFO\r\nOK\r\n",      // no +CGPSINFO line at all
		"+CGPSINFO: ,,,,,\r\nOK\r\n", // too few fields
	} {
		if _, err := parseCGPSINFO(resp); err == nil {
			t.Errorf("parseCGPSINFO(%q): expected error, got none", resp)
		}
	}
}

func TestParseQGPSLOC_NoFix(t *testing.T) {
	// Real captured output from a live Quectel RM520N-GL (6global and
	// dawn6g, no antenna connected, AT+QGPS=1 already sent).
	resp := "AT+QGPSLOC=2\r\n+CME ERROR: 516\r\n"
	fix, err := parseQGPSLOC(resp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fix.Valid {
		t.Errorf("expected Valid=false for CME ERROR 516, got %+v", fix)
	}
}

func TestParseQGPSLOC_OtherCMEError(t *testing.T) {
	// A CME error code other than the documented "not fixed" (516)
	// should surface as a real error, not be treated as "no fix yet".
	resp := "AT+QGPSLOC=2\r\n+CME ERROR: 100\r\n"
	_, err := parseQGPSLOC(resp)
	if err == nil {
		t.Fatalf("expected error for CME ERROR 100, got none")
	}
}

func TestParseQGPSLOC_ValidFix(t *testing.T) {
	// Community-captured example (Quectel mode-2 output, widely cited
	// in Quectel forum/SDK examples), not a live capture — no real fix
	// has been obtained on either board (no antenna).
	resp := "AT+QGPSLOC=2\r\n+QGPSLOC: 061951.00,41.02044,28.99797,0.7,62.2,2,0.00,0.0,0.0,110513,09\r\n\r\nOK\r\n"
	fix, err := parseQGPSLOC(resp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fix.Valid {
		t.Fatalf("expected Valid=true, got %+v", fix)
	}
	if fix.Latitude != 41.02044 {
		t.Errorf("Latitude = %v, want 41.02044", fix.Latitude)
	}
	if fix.Longitude != 28.99797 {
		t.Errorf("Longitude = %v, want 28.99797", fix.Longitude)
	}
	if fix.Altitude != 62.2 {
		t.Errorf("Altitude = %v, want 62.2", fix.Altitude)
	}
}

func TestParseQGPSLOC_WesternLongitudeStaysNegative(t *testing.T) {
	// AT+QGPSLOC=2 reports lat/lon as signed decimal degrees directly
	// (not NMEA ddmm.mmmm) - a western longitude must come back
	// negative, unchanged. This is a synthetic example (same reference
	// coordinates as aaeon's real SIMCom fix, 41.549747,-8.413069, for
	// a realistic western-hemisphere case), not a live Quectel
	// capture - it only confirms our own parser doesn't introduce a
	// sign error on top of whatever the modem reports. The
	// community-reported sign bug on some Quectel firmware (positive
	// values returned for western longitudes) remains UNVERIFIED
	// either way against real hardware: no Quectel board has gotten a
	// real fix yet to check against a known reference point.
	resp := "AT+QGPSLOC=2\r\n+QGPSLOC: 192721.00,41.549747,-8.413069,0.7,0.0,2,0.00,0.0,0.0,171126,09\r\n\r\nOK\r\n"
	fix, err := parseQGPSLOC(resp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fix.Valid {
		t.Fatalf("expected Valid=true, got %+v", fix)
	}
	if fix.Longitude >= 0 {
		t.Errorf("Longitude = %v, want negative (western)", fix.Longitude)
	}
	const wantLon = -8.413069
	if math.Abs(fix.Longitude-wantLon) > 1e-6 {
		t.Errorf("Longitude = %v, want %v", fix.Longitude, wantLon)
	}
}

func TestParseQGPSLOC_Malformed(t *testing.T) {
	for _, resp := range []string{
		"AT+QGPSLOC=2\r\nOK\r\n",              // no +QGPSLOC line and no CME ERROR
		"+QGPSLOC: 061951.00,41.02\r\nOK\r\n", // too few fields
	} {
		if _, err := parseQGPSLOC(resp); err == nil {
			t.Errorf("parseQGPSLOC(%q): expected error, got none", resp)
		}
	}
}

func TestEnableAssistanceData_SIMCom_EngineAlreadyRunning(t *testing.T) {
	// Confirmed to matter in practice, not just theoretical: SIMCom's
	// AT+CGPSXE=1 can fail while the GNSS engine is already running.
	// Exercises the stop-engine/retry-XTRA path via a fake transport,
	// since this needs no real serial port — just the exact command
	// sequence enableAssistanceData is expected to send.
	var calls []string
	xtraAttempt := 0
	fake := atSendFunc(func(dev, cmd string, timeout time.Duration) (string, error) {
		calls = append(calls, cmd)
		switch cmd {
		case "AT+CGPSXE=1":
			xtraAttempt++
			if xtraAttempt == 1 {
				return "ERROR", nil // fails the first time — engine is on
			}
			return "OK", nil // succeeds after the engine was stopped
		case "AT+CGPS?":
			return "+CGPS: 1,1\r\nOK", nil // engine already running
		case "AT+CGPS=0":
			return "OK", nil // engine stopped
		case "AT+CGPSXE?":
			return "+CGPSXE: 1\r\nOK", nil
		default:
			t.Fatalf("unexpected AT command: %q", cmd)
			return "", nil
		}
	})

	enableAssistanceData(modem.Modem{Port: "/dev/ttyUSB2"}, atGPSVendorSIMCom, fake)

	want := []string{"AT+CGPSXE=1", "AT+CGPS?", "AT+CGPS=0", "AT+CGPSXE=1", "AT+CGPSXE?"}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("call sequence = %v, want %v", calls, want)
	}
}

func TestEnableAssistanceData_Quectel_EngineAlreadyRunning(t *testing.T) {
	// Same retry path as the SIMCom test above, but AT+QGPSXTRA=1/
	// AT+QGPS?/AT+QGPSEND — Quectel's documentation says XTRA must be
	// set while the engine is off, so this is the expected path
	// whenever a restarted process finds the engine still on.
	var calls []string
	xtraAttempt := 0
	fake := atSendFunc(func(dev, cmd string, timeout time.Duration) (string, error) {
		calls = append(calls, cmd)
		switch cmd {
		case "AT+QGPSXTRA=1":
			xtraAttempt++
			if xtraAttempt == 1 {
				return "+CME ERROR: 504", nil // fails the first time — engine is on
			}
			return "OK", nil
		case "AT+QGPS?":
			return "+QGPS: 1\r\nOK", nil
		case "AT+QGPSEND":
			return "OK", nil
		case "AT+QGPSXTRADATA?":
			return "+QGPSXTRADATA: 1,48\r\nOK", nil
		default:
			t.Fatalf("unexpected AT command: %q", cmd)
			return "", nil
		}
	})

	enableAssistanceData(modem.Modem{Port: "/dev/ttyUSB3"}, atGPSVendorQuectel, fake)

	want := []string{"AT+QGPSXTRA=1", "AT+QGPS?", "AT+QGPSEND", "AT+QGPSXTRA=1", "AT+QGPSXTRADATA?"}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("call sequence = %v, want %v", calls, want)
	}
}

func TestParseGNSSState(t *testing.T) {
	tests := []struct {
		name string
		resp string
		want bool
	}{
		{
			// Real captured output from a live Quectel RM520N-GL after
			// a prior test left its GNSS engine running.
			name: "Quectel running",
			resp: "AT+QGPS?\r\n+QGPS: 1\r\n\r\nOK\r\n",
			want: true,
		},
		{
			// Real captured output from a live SIMCom SIM8262E-M2 after
			// a prior test left its GNSS engine running (state,mode).
			name: "SIMCom running",
			resp: "AT+CGPS?\r\n+CGPS: 1,1\r\n\r\nOK\r\n",
			want: true,
		},
		{
			name: "Quectel not running",
			resp: "AT+QGPS?\r\n+QGPS: 0\r\n\r\nOK\r\n",
			want: false,
		},
		{
			name: "no state line at all",
			resp: "AT+QGPS?\r\nERROR\r\n",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseGNSSState(tt.resp); got != tt.want {
				t.Errorf("parseGNSSState(%q) = %v, want %v", tt.resp, got, tt.want)
			}
		})
	}
}
