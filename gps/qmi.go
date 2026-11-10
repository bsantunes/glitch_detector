package gps

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// qmiSessionRetryDelay bounds how fast a dead --loc-follow-nmea process
// (QMI client hiccup, modem reset, the "Resource temporarily unavailable"
// error observed live during development) gets restarted — avoids a
// busy-loop of qmicli invocations if the modem is gone for good.
//
// qmiSessionMaxImmediateFailures bounds how many consecutive *immediate*
// failures (the process exits well under qmiSessionRetryDelay after
// starting) are tolerated before giving up entirely. This distinguishes
// a transient hiccup (worth retrying — e.g. the modem reset, or the
// "Resource temporarily unavailable" error seen live) from a permanent,
// per-modem incompatibility: confirmed live on two Quectel RM520N-GL
// modems (6global and dawn6g), whose QMI LOC service rejects NMEA event
// registration outright (`QMI protocol error (48): 'InvalidArgument'`,
// and `--loc-get-nmea-types` itself answers `'NotSupported'`) — every
// single attempt fails instantly, so retrying forever would just spam
// the log for a device that will never work. The SIMCom modem on the
// same boards has no such issue and streams real NMEA sentences fine.
// qmiSessionCloseGraceDelay bounds how long Close waits for a graceful
// shutdown (via --loc-stop ending the session server-side, which should
// make the running --loc-follow-nmea process exit on its own) before
// falling back to a force-kill. See Close's doc comment for why the
// graceful path matters.
const (
	qmiSessionRetryDelay           = 5 * time.Second
	qmiSessionMaxImmediateFailures = 3
	qmiSessionCloseGraceDelay      = 3 * time.Second
)

// QMISession owns a long-lived QMI LOC NMEA stream for one QMI modem
// device and caches the most recently parsed fix. Unlike every other
// backend in modem/ (and the ModemManager backend above), qmicli's LOC
// service has no one-shot "get current fix" command — only a start/
// stream/stop session (confirmed live: `qmicli --loc-follow-nmea` blocks
// and streams $GxGGA/$GxRMC lines indefinitely) — so a fix has to be
// read off a continuously-running stream rather than fetched fresh per
// call. This is deliberately scoped to gps-tool/gps-exporter's own
// process lifetime, not something modem/'s stateless backends do.
type QMISession struct {
	device string
	cancel context.CancelFunc
	done   chan struct{}

	mu  sync.Mutex
	fix Fix
	// stopped/lastErr: the session gave up permanently after repeated
	// immediate failures (see qmiSessionMaxImmediateFailures) — a
	// per-modem incompatibility, not a shutdown request.
	stopped bool
	lastErr error
	// stopping: Close was called — run's loop should exit after the
	// current followOnce call ends rather than retrying/restarting.
	stopping bool
}

// NewQMISession starts the LOC session (--loc-start) and a background
// goroutine that keeps --loc-follow-nmea running (restarting it if it
// dies) and updates the cached fix as sentences arrive. Call Close when
// done with it.
func NewQMISession(ctx context.Context, device string) (*QMISession, error) {
	if err := exec.CommandContext(ctx, "qmicli", "-d", device, "--loc-start").Run(); err != nil {
		return nil, fmt.Errorf("qmicli -d %s --loc-start: %w", device, err)
	}
	sessionCtx, cancel := context.WithCancel(context.Background())
	s := &QMISession{device: device, cancel: cancel, done: make(chan struct{})}
	go s.run(sessionCtx)
	return s, nil
}

func (s *QMISession) run(ctx context.Context) {
	defer close(s.done)
	immediateFailures := 0
	for {
		if ctx.Err() != nil || s.stopRequested() {
			return
		}
		start := time.Now()
		err := s.followOnce(ctx)
		if ctx.Err() != nil || s.stopRequested() {
			return
		}
		if err == nil {
			// followOnce only returns nil if the process exited cleanly
			// on its own, which --loc-follow-nmea never does in normal
			// operation — treat it the same as a failure.
			err = fmt.Errorf("qmicli --loc-follow-nmea exited unexpectedly")
		}
		if time.Since(start) < qmiSessionRetryDelay {
			immediateFailures++
		} else {
			immediateFailures = 0 // ran a while first — a transient hiccup, not a permanent gap
		}
		if immediateFailures >= qmiSessionMaxImmediateFailures {
			log.Printf("gps: giving up on %s after %d consecutive immediate failures (likely unsupported on this modem/firmware): %v",
				s.device, immediateFailures, err)
			s.mu.Lock()
			s.stopped, s.lastErr = true, err
			s.mu.Unlock()
			return
		}
		log.Printf("gps: qmicli -d %s --loc-follow-nmea: %v (retrying)", s.device, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(qmiSessionRetryDelay):
		}
	}
}

// followOnce runs a single --loc-follow-nmea process until it exits (or
// ctx is canceled), updating s.fix as valid sentences arrive.
func (s *QMISession) followOnce(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "qmicli", "-d", s.device, "--loc-follow-nmea")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if fix, ok := parseNMEAGGA(scanner.Text()); ok {
			s.mu.Lock()
			s.fix = fix
			s.mu.Unlock()
		}
	}
	return cmd.Wait()
}

// Fix returns the most recently parsed fix (zero-value/Valid=false if
// none has arrived yet), or an error if the session gave up permanently
// (see qmiSessionMaxImmediateFailures) — callers should stop polling a
// session once this returns an error rather than retrying forever.
func (s *QMISession) Fix() (Fix, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return Fix{}, fmt.Errorf("gps session for %s stopped after repeated failures: %w", s.device, s.lastErr)
	}
	return s.fix, nil
}

// stopRequested reports whether Close has been called.
func (s *QMISession) stopRequested() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

// Close stops the QMI LOC session gracefully: it tells run's loop not
// to restart --loc-follow-nmea, then runs --loc-stop to end the
// session server-side — which should make the currently running
// --loc-follow-nmea process notice its stream ended and exit on its
// own — before falling back to a force-kill (via context cancellation)
// only if that doesn't happen within qmiSessionCloseGraceDelay.
//
// Getting this order backwards (force-kill first, --loc-stop after, as
// this code used to do) caused a real production incident: killing
// --loc-follow-nmea abruptly doesn't let the QMI client release
// cleanly, and that client-ID state lives at the kernel-driver/modem-
// firmware level — shared across every process talking to that device
// node, not scoped to this one. Doing that against 6global's Quectel
// modem left its QMI control endpoint hung for every client, breaking
// production modem-exporter telemetry for that modem until a reboot.
func (s *QMISession) Close() {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()

	_ = exec.Command("qmicli", "-d", s.device, "--loc-stop").Run()

	select {
	case <-s.done:
	case <-time.After(qmiSessionCloseGraceDelay):
		s.cancel() // last resort — didn't exit gracefully in time
		<-s.done
	}
	s.cancel() // no-op if already canceled; always release the context
}

// parseNMEAGGA parses an NMEA "GGA" sentence (any talker ID — $GPGGA,
// $GNGGA, $GBGGA, ...) into a Fix. Returns ok=false for any other
// sentence type, or a GGA sentence with an unparseable/empty fix-quality
// field.
//
// Format: $--GGA,time,lat,N|S,lon,E|W,quality,numSats,hdop,altitude,M,...
// quality 0 means no fix — observed live (no antenna connected):
// $GPGGA,,,,,,0,,,,,,,,*66
func parseNMEAGGA(line string) (Fix, bool) {
	line = strings.TrimSpace(line)
	if idx := strings.IndexByte(line, '*'); idx >= 0 {
		line = line[:idx] // strip checksum
	}
	// NMEA sentence IDs are "$" + 2-char talker ID + 3-char sentence type
	// (e.g. "$GPGGA", "$GNGGA", "$GBGGA") — line[3:6] is that type.
	if len(line) < 6 || line[0] != '$' || line[3:6] != "GGA" {
		return Fix{}, false
	}
	fields := strings.Split(line, ",")
	if len(fields) < 10 {
		return Fix{}, false
	}
	quality, err := strconv.Atoi(fields[6])
	if err != nil || quality == 0 {
		return Fix{Time: time.Now()}, true // valid GGA sentence, just no fix
	}
	lat, ok := nmeaCoordToDecimal(fields[2], fields[3])
	if !ok {
		return Fix{}, false
	}
	lon, ok := nmeaCoordToDecimal(fields[4], fields[5])
	if !ok {
		return Fix{}, false
	}
	alt, _ := strconv.ParseFloat(fields[9], 64)
	return Fix{Latitude: lat, Longitude: lon, Altitude: alt, Valid: true, Time: time.Now()}, true
}

// nmeaCoordToDecimal converts NMEA's ddmm.mmmm (latitude) or
// dddmm.mmmm (longitude) format plus a N/S or E/W hemisphere letter into
// signed decimal degrees.
func nmeaCoordToDecimal(value, hemisphere string) (float64, bool) {
	if value == "" || hemisphere == "" {
		return 0, false
	}
	raw, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	degrees := float64(int(raw / 100))
	minutes := raw - degrees*100
	decimal := degrees + minutes/60
	if hemisphere == "S" || hemisphere == "W" {
		decimal = -decimal
	}
	return decimal, true
}
