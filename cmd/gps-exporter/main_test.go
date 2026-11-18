package main

import (
	"testing"

	"dawn-agent/gps"
	"dawn-agent/modem"
)

func TestGpsFixPoint_InvalidFixReturnsNil(t *testing.T) {
	// Mirrors what gps/at.go's parseCGPSINFO/parseQGPSLOC return for a
	// "no fix yet" reply (empty +CGPSINFO fields, or +CME ERROR: 516)
	// - Valid=false, zero coordinates. (0, 0) is itself a real
	// coordinate (Null Island), so gpsFixPoint must key off Valid, not
	// infer "no fix" from zero values - this is the point-level
	// check complementing gps/at_test.go's parser-level
	// TestParseCGPSINFO_NoFix/TestParseQGPSLOC_NoFix.
	m := modem.Modem{ID: "/dev/ttyUSB2", Method: modem.MethodAT}
	fix := gps.Fix{Valid: false}
	if p := gpsFixPoint("aaeon", m, fix); p != nil {
		t.Errorf("gpsFixPoint with Valid=false = %+v, want nil (no point written)", p)
	}
}

func TestGpsFixPoint_ValidFixProducesPoint(t *testing.T) {
	// Sanity check the other direction, using aaeon's real reference
	// fix (see CLAUDE.md's gps/ package section) - a valid fix must
	// actually produce a point to write, with the right fields.
	m := modem.Modem{ID: "/dev/ttyUSB2", Method: modem.MethodAT}
	fix := gps.Fix{Latitude: 41.549747, Longitude: -8.413069, Altitude: 0, Valid: true}
	p := gpsFixPoint("aaeon", m, fix)
	if p == nil {
		t.Fatalf("gpsFixPoint with Valid=true = nil, want a point")
	}
}
