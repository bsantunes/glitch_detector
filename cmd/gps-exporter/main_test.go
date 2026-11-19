package main

import (
	"math"
	"testing"

	"github.com/influxdata/influxdb-client-go/v2/api/write"

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
	if pointHasField(p, "satellites") {
		t.Errorf("non-NMEA fix must not write satellites/speed_knots/course_deg/hdop or a source tag")
	}
	if pointHasTag(p, "source") {
		t.Errorf("non-NMEA fix must not write a source tag")
	}
}

func TestGpsFixPoint_NMEASourcedFixAddsExtraFields(t *testing.T) {
	// Additive fields/tag only ever written for an NMEA-sourced fix
	// (gps/nmea.go) — see this file's package doc comment. HDOP
	// deliberately NaN here to confirm the NaN-omitted convention
	// (matching modem-exporter's radio_cell) extends to these too.
	m := modem.Modem{ID: "/dev/ttyUSB8", Method: modem.MethodAT}
	fix := gps.Fix{
		Latitude: 41.548052, Longitude: -8.416491, Altitude: 201.8, Valid: true,
		SpeedKnots: 1.2, CourseDeg: 90, HDOP: math.NaN(), Satellites: 9, Source: "nmea",
	}
	p := gpsFixPoint("dawn6g", m, fix)
	if p == nil {
		t.Fatalf("gpsFixPoint with Valid=true = nil, want a point")
	}
	if !pointHasTag(p, "source") {
		t.Errorf("expected a source tag for an NMEA-sourced fix")
	}
	if !pointHasField(p, "satellites") || !pointHasField(p, "speed_knots") || !pointHasField(p, "course_deg") {
		t.Errorf("expected satellites/speed_knots/course_deg fields for an NMEA-sourced fix")
	}
	if pointHasField(p, "hdop") {
		t.Errorf("expected hdop to be omitted when NaN, same convention as modem-exporter's radio_cell")
	}
}

func pointHasField(p *write.Point, key string) bool {
	for _, f := range p.FieldList() {
		if f.Key == key {
			return true
		}
	}
	return false
}

func pointHasTag(p *write.Point, key string) bool {
	for _, tg := range p.TagList() {
		if tg.Key == key {
			return true
		}
	}
	return false
}
