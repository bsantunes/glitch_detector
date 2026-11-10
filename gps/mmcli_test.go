package gps

import "testing"

func TestFixFromMmcliKeyValue_NoFix(t *testing.T) {
	// mmcli's "--" sentinel for "no data yet" (gps-raw enabled but no
	// satellite fix acquired).
	info := map[string]string{
		"modem.location.gps.latitude":  "--",
		"modem.location.gps.longitude": "--",
		"modem.location.gps.altitude":  "--",
	}
	fix := fixFromMmcliKeyValue(info)
	if fix.Valid {
		t.Errorf("expected Valid=false, got %+v", fix)
	}
}

func TestFixFromMmcliKeyValue_ValidFix(t *testing.T) {
	info := map[string]string{
		"modem.location.gps.latitude":  "38.722500",
		"modem.location.gps.longitude": "-9.139300",
		"modem.location.gps.altitude":  "95.000000",
	}
	fix := fixFromMmcliKeyValue(info)
	if !fix.Valid {
		t.Fatalf("expected Valid=true, got %+v", fix)
	}
	if fix.Latitude != 38.7225 {
		t.Errorf("Latitude = %v, want 38.7225", fix.Latitude)
	}
	if fix.Longitude != -9.1393 {
		t.Errorf("Longitude = %v, want -9.1393", fix.Longitude)
	}
	if fix.Altitude != 95.0 {
		t.Errorf("Altitude = %v, want 95.0", fix.Altitude)
	}
}

func TestFixFromMmcliKeyValue_MissingKeys(t *testing.T) {
	fix := fixFromMmcliKeyValue(map[string]string{})
	if fix.Valid {
		t.Errorf("expected Valid=false for empty input, got %+v", fix)
	}
}

func TestMmcliParseGPSFloat(t *testing.T) {
	tests := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"38.7225", 38.7225, true},
		{"--", 0, false},
		{"", 0, false},
		{"not-a-number", 0, false},
	}
	for _, tt := range tests {
		got, ok := mmcliParseGPSFloat(tt.in)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("mmcliParseGPSFloat(%q) = (%v, %v), want (%v, %v)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
