package gps

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestParseNMEARMC_NoFix(t *testing.T) {
	fix, ok := parseNMEARMC("$GPRMC,,V,,,,,,,,,,N,V*29")
	if !ok {
		t.Fatalf("expected ok=true (valid RMC sentence, just no fix)")
	}
	if fix.Valid {
		t.Errorf("expected Valid=false for status V, got %+v", fix)
	}
}

func TestParseNMEARMC_ValidFix(t *testing.T) {
	// Real captured sentence from dawn6g's Quectel RM520N-GL NMEA port
	// (ttyUSB8, interface 01), the live incident that motivated this
	// reader: QGPSLOC=2 alternated "no fix" while this port showed a
	// continuous lock. Speed/course blank here, as in the real
	// capture — must parse as NaN, not as an error.
	fix, ok := parseNMEARMC("$GPRMC,141950.00,A,4132.883130,N,00824.989479,W,,,191126,,,A*63")
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if !fix.Valid {
		t.Fatalf("expected Valid=true for status A, got %+v", fix)
	}
	const wantLat, wantLon = 41.548052166666664, -8.416491316666667
	if math.Abs(fix.Latitude-wantLat) > 1e-6 {
		t.Errorf("Latitude = %v, want %v", fix.Latitude, wantLat)
	}
	if math.Abs(fix.Longitude-wantLon) > 1e-6 {
		t.Errorf("Longitude = %v, want %v", fix.Longitude, wantLon)
	}
	if !math.IsNaN(fix.SpeedKnots) {
		t.Errorf("SpeedKnots = %v, want NaN (blank in the real capture)", fix.SpeedKnots)
	}
	if !math.IsNaN(fix.CourseDeg) {
		t.Errorf("CourseDeg = %v, want NaN (blank in the real capture)", fix.CourseDeg)
	}
	if fix.Source != "nmea" {
		t.Errorf("Source = %q, want \"nmea\"", fix.Source)
	}
}

func TestParseNMEARMC_WithSpeedAndCourse(t *testing.T) {
	fix, ok := parseNMEARMC("$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*6A")
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if fix.SpeedKnots != 22.4 {
		t.Errorf("SpeedKnots = %v, want 22.4", fix.SpeedKnots)
	}
	if fix.CourseDeg != 84.4 {
		t.Errorf("CourseDeg = %v, want 84.4", fix.CourseDeg)
	}
}

func TestParseNMEARMC_NotAnRMCSentence(t *testing.T) {
	for _, line := range []string{
		"$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47",
		"not nmea at all",
		"",
	} {
		if _, ok := parseNMEARMC(line); ok {
			t.Errorf("parseNMEARMC(%q): expected ok=false", line)
		}
	}
}

func TestParseNMEARMC_TalkerIDVariants(t *testing.T) {
	for _, prefix := range []string{"$GPRMC", "$GNRMC", "$GLRMC"} {
		line := prefix + ",141950.00,A,4132.883130,N,00824.989479,W,,,191126,,,A*63"
		if _, ok := parseNMEARMC(line); !ok {
			t.Errorf("parseNMEARMC(%q): expected ok=true", line)
		}
	}
}

func TestNMEAReader_HandleLine_GGANeverTouchesValid(t *testing.T) {
	r := &NMEAReader{}

	// RMC first: a real fix, status A.
	r.handleLine("$GPRMC,141950.00,A,4132.883130,N,00824.989479,W,,,191126,,,A*63")
	if !r.fix.Valid {
		t.Fatalf("expected Valid=true after a status-A RMC, got %+v", r.fix)
	}

	// GGA with satellites/HDOP, quality 1 — must add satellites/HDOP
	// without changing Valid, which only RMC ever sets.
	r.handleLine("$GPGGA,141950,4132.883130,N,00824.989479,W,1,09,1.1,201.8,M,50.0,M,,*47")
	if !r.fix.Valid {
		t.Errorf("GGA must not have changed Valid, got %+v", r.fix)
	}
	if r.fix.Satellites != 9 {
		t.Errorf("Satellites = %d, want 9", r.fix.Satellites)
	}
	if math.Abs(r.fix.HDOP-1.1) > 1e-9 {
		t.Errorf("HDOP = %v, want 1.1", r.fix.HDOP)
	}
	if r.fix.Altitude != 201.8 {
		t.Errorf("Altitude = %v, want 201.8 (only GGA carries altitude)", r.fix.Altitude)
	}

	// A later status-V RMC flips Valid — this is the exact live
	// scenario the reader exists to track correctly.
	r.handleLine("$GPRMC,,V,,,,,,,,,,N,V*29")
	if r.fix.Valid {
		t.Errorf("expected Valid=false after a status-V RMC, got %+v", r.fix)
	}
}

func TestNMEAReader_ConsumeLines_HandlesPartialLineAcrossReads(t *testing.T) {
	r := &NMEAReader{}
	var pending strings.Builder

	// First chunk ends mid-sentence — nothing should be handled yet,
	// and the partial line must survive into the next chunk.
	pending.WriteString("$GPRMC,141950.00,A,4132.883130,N,00824.98")
	r.consumeLines(&pending)
	if r.fix.Valid {
		t.Fatalf("expected no sentence handled yet from a partial line, got %+v", r.fix)
	}

	// Second chunk completes the RMC sentence and starts a new one.
	pending.WriteString("9479,W,,,191126,,,A*63\r\n$GPGGA,14195")
	r.consumeLines(&pending)
	if !r.fix.Valid {
		t.Fatalf("expected the completed RMC sentence to be handled, got %+v", r.fix)
	}
	if remaining := pending.String(); remaining != "$GPGGA,14195" {
		t.Errorf("pending buffer = %q, want the still-incomplete GGA line preserved", remaining)
	}
}

func TestNMEAReader_Fix_StaleReturnsInvalid(t *testing.T) {
	r := &NMEAReader{}
	r.handleLine("$GPRMC,141950.00,A,4132.883130,N,00824.989479,W,,,191126,,,A*63")
	if !r.Fix().Valid {
		t.Fatalf("expected a fresh fix to be Valid")
	}

	r.mu.Lock()
	r.lastSeen = time.Now().Add(-2 * nmeaMaxFixAge)
	r.mu.Unlock()

	got := r.Fix()
	if got.Valid {
		t.Errorf("expected a stale fix (no sentence in > nmeaMaxFixAge) to report Valid=false, got %+v", got)
	}
}
