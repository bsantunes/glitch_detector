// Package gps reads GPS/GNSS fixes from a modem's own GNSS receiver —
// ModemManager's and raw QMI's location services, specifically (see
// Supports). It's a standalone package in the same shape as twamp/: its
// own process (cmd/gps-tool, cmd/gps-exporter), not something wired into
// dawn-agent's existing active-monitoring loop or the modem/ package's
// stateless per-call backends.
package gps

import "time"

// Fix is a single GPS/GNSS position reading.
type Fix struct {
	Latitude  float64
	Longitude float64
	Altitude  float64
	// Valid is false when the receiver has no satellite fix yet (no
	// antenna connected, indoors, still acquiring, ...) — Latitude/
	// Longitude/Altitude are meaningless in that case and shouldn't be
	// reported as real data.
	Valid bool
	// Time is when this fix was read/parsed, not necessarily the GNSS
	// receiver's own fix timestamp (NMEA's time-of-day field alone can't
	// be turned into a full timestamp without also knowing the date).
	Time time.Time
}
