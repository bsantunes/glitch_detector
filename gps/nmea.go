package gps

import (
	"context"
	"log"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// parseNMEARMC parses an NMEA "RMC" sentence (any talker ID) into a
// Fix carrying only what RMC itself provides: validity (the "A"/"V"
// status field — confirmed live on dawn6g's Quectel to be a far more
// reliable fix-validity signal than AT+QGPSLOC=2's own intermittent
// "no fix" replies, 86/86 valid RMC over 60s while QGPSLOC alternated
// — see CLAUDE.md's gps/ package section), latitude/longitude, speed,
// and course. RMC has no altitude field at all — that only ever comes
// from a paired GGA sentence (parseNMEAGGA).
//
// Format: $--RMC,time,status,lat,N|S,lon,E|W,speed,course,date,...
func parseNMEARMC(line string) (Fix, bool) {
	line = strings.TrimSpace(line)
	if idx := strings.IndexByte(line, '*'); idx >= 0 {
		line = line[:idx] // strip checksum
	}
	// NMEA sentence IDs are "$" + 2-char talker ID + 3-char sentence
	// type (e.g. "$GPRMC", "$GNRMC") — line[3:6] is that type.
	if len(line) < 6 || line[0] != '$' || line[3:6] != "RMC" {
		return Fix{}, false
	}
	// Format: $--RMC,time,status,lat,N|S,lon,E|W,speed,course,date,...
	// — note the time field before status, unlike GGA's
	// time,lat,N|S,... (no status field at all); easy to miss, see the
	// comment on parseNMEARMC's doc above.
	fields := strings.Split(line, ",")
	if len(fields) < 9 {
		return Fix{}, false
	}
	status := fields[2]
	if status != "A" && status != "V" {
		return Fix{}, false
	}
	fix := Fix{
		Valid:      status == "A",
		Time:       time.Now(),
		SpeedKnots: math.NaN(),
		CourseDeg:  math.NaN(),
		Source:     "nmea",
	}
	if !fix.Valid {
		return fix, true
	}
	lat, ok := nmeaCoordToDecimal(fields[3], fields[4])
	if !ok {
		return Fix{}, false
	}
	lon, ok := nmeaCoordToDecimal(fields[5], fields[6])
	if !ok {
		return Fix{}, false
	}
	fix.Latitude = lat
	fix.Longitude = lon
	if speed, err := strconv.ParseFloat(fields[7], 64); err == nil {
		fix.SpeedKnots = speed
	}
	if course, err := strconv.ParseFloat(fields[8], 64); err == nil {
		fix.CourseDeg = course
	}
	return fix, true
}

// nmeaMaxFixAge bounds how long a combined fix may go without a fresh
// sentence before NMEAReader.Fix reports Valid=false rather than
// returning an increasingly stale "valid" reading forever — matters
// specifically because this reader is a continuous background stream,
// unlike the AT request/response path where every read is fresh by
// construction.
const nmeaMaxFixAge = 5 * time.Second

// nmeaReopenDelay is how long OpenNMEAReader's background loop waits
// before retrying after a read error (device unplugged, port busy,
// ...) — a transient hiccup is expected to recover, not a reason to
// give up on GPS for the rest of the process's life.
const nmeaReopenDelay = 3 * time.Second

// NMEAReader continuously reads NMEA sentences from a modem's
// dedicated NMEA output port (interface 01 on both SIMCom and Quectel
// — see CLAUDE.md's gps/ package section) and keeps the latest
// combined fix. AT still owns engine start/stop and XTRA (NMEA ports
// don't accept AT commands); this is a read-only fix source. RMC's
// "A"/"V" status is the authoritative Valid signal — GGA sentences
// only ever contribute Satellites/HDOP/Altitude (and lat/lon as a
// fallback when no RMC has been seen yet this run), never touching
// Valid themselves, since RMC's own validity marker is the one
// confirmed live to actually track a continuous lock correctly.
type NMEAReader struct {
	dev    string
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	fix      Fix
	lastSeen time.Time
}

// OpenNMEAReader starts reading dev in the background immediately;
// errors opening the port surface through the logged reopen loop, not
// a returned error, since a port that isn't ready yet at startup
// (modem still booting) is expected to become available shortly.
func OpenNMEAReader(ctx context.Context, dev string) *NMEAReader {
	ctx, cancel := context.WithCancel(ctx)
	r := &NMEAReader{dev: dev, cancel: cancel, done: make(chan struct{})}
	go r.run(ctx)
	return r
}

func (r *NMEAReader) run(ctx context.Context) {
	defer close(r.done)
	for {
		if ctx.Err() != nil {
			return
		}
		if err := r.readOnce(ctx); err != nil {
			log.Printf("gps: [%s] NMEA read error: %v (reopening in %s)", r.dev, err, nmeaReopenDelay)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(nmeaReopenDelay):
		}
	}
}

// nmeaReadDeadline bounds each individual Read so the loop below can
// check ctx.Done() regularly — confirmed live (dawn6g) that closing
// the file from another goroutine does not reliably unblock a Read
// already blocked on it from this one, the same way it can for a net
// connection; a first version relying on that hung on shutdown and
// had to be force-killed. SetReadDeadline on these ports is confirmed
// to work correctly (same mechanism drainPort already relies on in
// gps/at.go), so bounded reads are used throughout instead of a bare
// blocking bufio.Scanner.
const nmeaReadDeadline = 500 * time.Millisecond

func (r *NMEAReader) readOnce(ctx context.Context) error {
	// stty's baud argument fails on these virtual USB-serial ports —
	// see gps/at.go's atTransact for the same finding on the AT ports
	// of the same modems; dropped here for the identical reason.
	sttyCmd := exec.Command("stty", "-F", r.dev, "raw", "-echo")
	var sttyErr strings.Builder
	sttyCmd.Stderr = &sttyErr
	if err := sttyCmd.Run(); err != nil {
		log.Printf("gps: stty %s: %v: %s (continuing anyway)", r.dev, err, strings.TrimSpace(sttyErr.String()))
	}

	f, err := os.OpenFile(r.dev, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	var pending strings.Builder
	readBuf := make([]byte, 4096)
	for {
		if ctx.Err() != nil {
			return nil
		}
		_ = f.SetReadDeadline(time.Now().Add(nmeaReadDeadline))
		n, err := f.Read(readBuf)
		if n > 0 {
			pending.Write(readBuf[:n])
			r.consumeLines(&pending)
		}
		if err != nil {
			if os.IsTimeout(err) {
				continue // just the deadline firing with nothing to read
			}
			return err
		}
	}
}

// consumeLines pulls every complete \r- or \n-terminated line out of
// pending, handling each, and leaves any trailing partial line (split
// across two Read calls) in place for the next one.
func (r *NMEAReader) consumeLines(pending *strings.Builder) {
	for {
		s := pending.String()
		idx := strings.IndexAny(s, "\r\n")
		if idx < 0 {
			return
		}
		line := s[:idx]
		pending.Reset()
		pending.WriteString(s[idx+1:])
		if line != "" {
			r.handleLine(line)
		}
	}
}

// handleLine updates the combined fix from one NMEA sentence. GGA
// never touches Valid (see the type's doc comment) — only RMC does.
func (r *NMEAReader) handleLine(line string) {
	if gga, ok := parseNMEAGGA(line); ok {
		r.mu.Lock()
		r.lastSeen = time.Now()
		r.fix.Satellites = gga.Satellites
		r.fix.HDOP = gga.HDOP
		r.fix.Source = "nmea"
		if gga.Valid {
			r.fix.Altitude = gga.Altitude
			if r.fix.Latitude == 0 && r.fix.Longitude == 0 {
				// No RMC seen yet this run — GGA's own position is
				// the only one available so far.
				r.fix.Latitude = gga.Latitude
				r.fix.Longitude = gga.Longitude
			}
		}
		r.mu.Unlock()
		return
	}
	if rmc, ok := parseNMEARMC(line); ok {
		r.mu.Lock()
		r.lastSeen = time.Now()
		r.fix.Valid = rmc.Valid
		r.fix.Time = rmc.Time
		r.fix.Source = "nmea"
		if rmc.Valid {
			r.fix.Latitude = rmc.Latitude
			r.fix.Longitude = rmc.Longitude
			r.fix.SpeedKnots = rmc.SpeedKnots
			r.fix.CourseDeg = rmc.CourseDeg
		}
		r.mu.Unlock()
	}
}

// Fix returns the latest combined fix, or a Fix with Valid=false if
// nothing has been read in the last nmeaMaxFixAge — a dead/unplugged
// port must not keep reporting an increasingly stale "valid" fix.
func (r *NMEAReader) Fix() Fix {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.lastSeen) > nmeaMaxFixAge {
		return Fix{Time: time.Now(), Source: "nmea"}
	}
	return r.fix
}

func (r *NMEAReader) Close() error {
	r.cancel()
	<-r.done
	return nil
}
