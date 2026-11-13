package modem

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func candidateATDevices() []string {
	var devs []string
	for _, pattern := range []string{"/dev/ttyUSB*", "/dev/ttyACM*"} {
		matches, _ := filepath.Glob(pattern)
		devs = append(devs, matches...)
	}
	return devs
}

// PortInUse reports whether another process already has dev open, by
// scanning /proc/*/fd for a symlink pointing at it — a device node can
// be opened concurrently by more than one process at the OS level, but
// doing so against a port another process is actively reading/writing
// interleaves garbage into both sides. Confirmed live and relevant on
// WaveOS specifically: its uqmi-ubus daemon holds its own AT ports open
// continuously (for PCI/EARFCN polling via AT+QENG) for as long as it
// runs, on a *different* ttyUSB number than the modem's QMI/AT-for-
// signal ports but discovered only by checking, not by any fixed
// interface-number rule (confirmed live to vary by firmware). Best-
// effort: a /proc read failure (different busybox build, permissions)
// is treated as "possibly in use" so callers degrade to skipping that
// port rather than risking a collision.
func PortInUse(dev string) bool {
	procDirs, err := os.ReadDir("/proc")
	if err != nil {
		return true
	}
	for _, d := range procDirs {
		if _, err := strconv.Atoi(d.Name()); err != nil {
			continue // not a PID directory
		}
		fdDir := filepath.Join("/proc", d.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue // process exited, or fd dir unreadable — not our concern
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err == nil && target == dev {
				return true
			}
		}
	}
	return false
}

// DetectAT scans serial candidate devices for one that answers "AT"
// with "OK" — the lowest common denominator supported by essentially
// every cellular modem. This is the last-resort fallback, used only
// when neither ModemManager nor QMI/MBIM tooling found a modem.
func DetectAT(ctx context.Context) ([]Modem, error) {
	var modems []Modem
	for _, dev := range candidateATDevices() {
		if PortInUse(dev) {
			continue
		}
		resp, err := sendAT(dev, "ATI", 2*time.Second)
		if err != nil {
			continue
		}
		manufacturer, model := parseATI(resp)
		modems = append(modems, Modem{
			ID:           dev,
			Manufacturer: manufacturer,
			Model:        model,
			Port:         dev,
			Method:       MethodAT,
		})
	}
	return modems, nil
}

// sendAT configures the serial port with `stty` (raw mode, no echo,
// 115200 8N1 — a safe common default for USB cellular modems) and
// writes a single AT command, returning whatever the modem replies
// within the given timeout.
func sendAT(dev, cmd string, timeout time.Duration) (string, error) {
	// stty commonly exits non-zero here ("unable to perform all requested
	// operations") even though it applies the settings that matter and the
	// port works fine afterward — so a failure is logged, not fatal.
	if err := exec.Command("stty", "-F", dev, "115200", "raw", "-echo").Run(); err != nil {
		log.Printf("modem: stty %s: %v (continuing anyway)", dev, err)
	}

	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := f.WriteString(cmd + "\r\n"); err != nil {
		return "", err
	}

	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 4096)
		var sb strings.Builder
		for {
			n, err := f.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
				if strings.Contains(sb.String(), "OK") || strings.Contains(sb.String(), "ERROR") {
					ch <- result{sb.String(), nil}
					return
				}
			}
			if err != nil {
				ch <- result{sb.String(), err}
				return
			}
		}
	}()

	select {
	case r := <-ch:
		if r.err != nil && r.s == "" {
			return "", r.err
		}
		if !strings.Contains(r.s, "OK") {
			return "", fmt.Errorf("no OK response from %s", dev)
		}
		return r.s, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("timeout waiting for response from %s", dev)
	}
}

var atiManufacturerRe = regexp.MustCompile(`(?i)manufacturer:\s*(.+)`)
var atiModelRe = regexp.MustCompile(`(?i)model:\s*(.+)`)

// parseATI extracts manufacturer/model from an ATI response. Vendors
// differ wildly in formatting; many just print the manufacturer name on
// the first non-empty line and the model on the second, with no
// "Manufacturer:"/"Model:" labels at all, so that's used as a fallback.
func parseATI(resp string) (manufacturer, model string) {
	lines := strings.Split(resp, "\n")
	// Anchor on the command echo ("ATI...") before applying the
	// positional fallback below — some modems spam unsolicited
	// notifications ahead of actually answering a command (confirmed
	// live: a Quectel RM520N-GL spamming "+CPIN: NOT INSERTED" with no
	// SIM present, arriving before its ATI reply), which the
	// "first/second non-empty line" heuristic would otherwise mistake
	// for the manufacturer/model themselves. Everything from the last
	// AT-prefixed line onward is this command's actual answer;
	// anything before it is stale/unrelated buffer content. No echo
	// found (echo disabled on some modems) just leaves lines as-is.
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "AT") {
			lines = lines[i:]
		}
	}

	var nonEmpty []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || l == "OK" || strings.HasPrefix(l, "AT") {
			continue
		}
		nonEmpty = append(nonEmpty, l)
	}
	for _, l := range nonEmpty {
		if m := atiManufacturerRe.FindStringSubmatch(l); m != nil {
			manufacturer = strings.TrimSpace(m[1])
		}
		if m := atiModelRe.FindStringSubmatch(l); m != nil {
			model = strings.TrimSpace(m[1])
		}
	}
	if manufacturer == "" && len(nonEmpty) > 0 {
		manufacturer = nonEmpty[0]
	}
	if model == "" && len(nonEmpty) > 1 {
		model = nonEmpty[1]
	}
	return manufacturer, model
}

var cesqRe = regexp.MustCompile(`\+CESQ:\s*(\d+),(\d+),(\d+),(\d+),(\d+),(\d+)`)

// getSignalAT uses AT+CESQ (3GPP TS 27.007 §8.69), the most widely
// implemented standard signal-quality query across GSM/UMTS/LTE modems,
// reporting RSRP/RSRQ on LTE-capable devices. Vendor-specific commands
// (e.g. Quectel AT+QCSQ, Sierra AT!GSTATUS) would give richer 5G data
// but aren't portable across manufacturers, so are left as a future
// extension point once we know which non-ModemManager modems we
// actually need to support.
func getSignalAT(ctx context.Context, m Modem) (Signal, error) {
	resp, err := sendAT(m.Port, "AT+CESQ", 2*time.Second)
	if err != nil {
		return NaNSignal(), err
	}
	return parseCESQ(resp), nil
}

// parseCESQ decodes the +CESQ response fields
// (rxlev,ber,rscp,ecno,rsrq,rsrp), converting the raw index values to
// dBm/dB and mapping "not known" sentinels (255) to NaN.
func parseCESQ(resp string) Signal {
	sig := NaNSignal()
	m := cesqRe.FindStringSubmatch(resp)
	if m == nil {
		return sig
	}
	vals := make([]int, 6)
	for i, s := range m[1:] {
		vals[i], _ = strconv.Atoi(s)
	}
	rsrq, rsrp := vals[4], vals[5]
	if rsrq != 255 {
		sig.RSRQ = -19.5 + float64(rsrq)*0.5
	}
	if rsrp != 255 {
		sig.RSRP = -140 + float64(rsrp)
		sig.Technology = "lte"
	}
	return sig
}
