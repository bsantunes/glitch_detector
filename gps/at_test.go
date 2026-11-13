package gps

import (
	"math"
	"testing"

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

func TestParseCGPSINFO_Malformed(t *testing.T) {
	for _, resp := range []string{
		"AT+CGPSINFO\r\nOK\r\n",     // no +CGPSINFO line at all
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

func TestParseQGPSLOC_Malformed(t *testing.T) {
	for _, resp := range []string{
		"AT+QGPSLOC=2\r\nOK\r\n",           // no +QGPSLOC line and no CME ERROR
		"+QGPSLOC: 061951.00,41.02\r\nOK\r\n", // too few fields
	} {
		if _, err := parseQGPSLOC(resp); err == nil {
			t.Errorf("parseQGPSLOC(%q): expected error, got none", resp)
		}
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
