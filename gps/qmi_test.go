package gps

import (
	"math"
	"testing"
)

func TestParseNMEAGGA_NoFix(t *testing.T) {
	// Real captured output from a live QMI LOC session with no antenna
	// connected (qmicli -d /dev/cdc-wdm0 --loc-follow-nmea on 6global).
	fix, ok := parseNMEAGGA("$GPGGA,,,,,,0,,,,,,,,*66")
	if !ok {
		t.Fatalf("expected ok=true (valid GGA sentence, just no fix)")
	}
	if fix.Valid {
		t.Errorf("expected Valid=false for fix quality 0, got %+v", fix)
	}
}

func TestParseNMEAGGA_ValidFix(t *testing.T) {
	// Canonical NMEA GGA example (48°07.038'N, 11°31.000'E, 545.4m alt).
	fix, ok := parseNMEAGGA("$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47")
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if !fix.Valid {
		t.Fatalf("expected Valid=true for fix quality 1, got %+v", fix)
	}
	if math.Abs(fix.Latitude-48.1173) > 1e-3 {
		t.Errorf("Latitude = %v, want ~48.1173", fix.Latitude)
	}
	if math.Abs(fix.Longitude-11.516667) > 1e-3 {
		t.Errorf("Longitude = %v, want ~11.516667", fix.Longitude)
	}
	if fix.Altitude != 545.4 {
		t.Errorf("Altitude = %v, want 545.4", fix.Altitude)
	}
}

func TestParseNMEAGGA_NotAGGASentence(t *testing.T) {
	for _, line := range []string{
		"$GPVTG,,T,,M,,N,,K,N*2C",
		"$GPRMC,,V,,,,,,,,,,N,V*29",
		"",
		"garbage",
		"$GP", // too short to even check the sentence type
	} {
		if _, ok := parseNMEAGGA(line); ok {
			t.Errorf("parseNMEAGGA(%q) = ok=true, want false", line)
		}
	}
}

func TestParseNMEAGGA_TalkerIDVariants(t *testing.T) {
	// Multi-GNSS receivers report GGA under different talker IDs
	// (GP/GN/GB/...) — all seen live on 6global's modems.
	for _, prefix := range []string{"$GPGGA", "$GNGGA", "$GBGGA"} {
		line := prefix + ",,,,,,0,,,,,,,,*00"
		if _, ok := parseNMEAGGA(line); !ok {
			t.Errorf("parseNMEAGGA(%q) = ok=false, want true", line)
		}
	}
}

func TestNmeaCoordToDecimal(t *testing.T) {
	tests := []struct {
		value, hemisphere string
		want              float64
		ok                bool
	}{
		{"4807.038", "N", 48.1173, true},
		{"4807.038", "S", -48.1173, true},
		{"01131.000", "E", 11.516667, true},
		{"01131.000", "W", -11.516667, true},
		{"", "N", 0, false},
		{"4807.038", "", 0, false},
		{"not-a-number", "N", 0, false},
	}
	for _, tt := range tests {
		got, ok := nmeaCoordToDecimal(tt.value, tt.hemisphere)
		if ok != tt.ok {
			t.Errorf("nmeaCoordToDecimal(%q, %q) ok = %v, want %v", tt.value, tt.hemisphere, ok, tt.ok)
			continue
		}
		if ok && math.Abs(got-tt.want) > 1e-3 {
			t.Errorf("nmeaCoordToDecimal(%q, %q) = %v, want ~%v", tt.value, tt.hemisphere, got, tt.want)
		}
	}
}
