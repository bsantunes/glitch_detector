package modem

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// usbPortSpec is a parsed "usb:<vid>:<pid>:<ifnum>" port specifier —
// see ResolvePort's doc comment for why this exists.
type usbPortSpec struct {
	vendorID  string // lowercase hex, e.g. "2c7c"
	productID string // lowercase hex, e.g. "0801"
	ifNum     int
}

// parseUSBPortSpec parses "usb:<vid>:<pid>:<ifnum>" — vid/pid as hex
// (case-insensitive, with or without leading zeros), ifnum as a plain
// decimal integer. sysfs's own bInterfaceNumber is always exactly 2
// digits, zero-padded, but the spec itself takes a plain int so both
// "usb:2c7c:0801:3" and "usb:2c7c:0801:03" work.
func parseUSBPortSpec(spec string) (usbPortSpec, error) {
	parts := strings.Split(spec, ":")
	if len(parts) != 4 || parts[0] != "usb" {
		return usbPortSpec{}, fmt.Errorf("malformed usb port spec %q, want usb:<vid>:<pid>:<ifnum>", spec)
	}
	ifNum, err := strconv.Atoi(parts[3])
	if err != nil {
		return usbPortSpec{}, fmt.Errorf("malformed usb port spec %q: interface number %q is not an integer", spec, parts[3])
	}
	return usbPortSpec{
		vendorID:  strings.ToLower(parts[1]),
		productID: strings.ToLower(parts[2]),
		ifNum:     ifNum,
	}, nil
}

// ResolvePort resolves a -port argument that may be either a plain
// device path (e.g. "/dev/ttyUSB3", "/dev/dawn-gps") or a
// "usb:<vid>:<pid>:<ifnum>" specifier (e.g. "usb:2c7c:0801:03") into a
// real device path by walking sysfs. The usb: form exists because
// ttyUSB numbers on a board with more than one modem can shift across
// a reboot (confirmed live: aaeon's 99-dawn-gps.rules udev rule already
// pins by USB vendor/product/interface for the identical reason) — this
// gives -port the same stability without a udev rule needing to be
// installed first, which matters on boards like dawn6g/6global (two
// modems, no such rule yet). A plain device path is returned
// unchanged, including one that doesn't exist yet — ResolvePort never
// checks that a non-usb: path is real, only that a usb: spec resolves
// to something that currently is.
func ResolvePort(portArg string) (string, error) {
	if !strings.HasPrefix(portArg, "usb:") {
		return portArg, nil
	}
	spec, err := parseUSBPortSpec(portArg)
	if err != nil {
		return "", err
	}
	dev, err := findUSBTTY(spec)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", portArg, err)
	}
	return dev, nil
}

// findUSBTTY walks /sys/class/tty for a ttyUSB*/ttyACM* device whose
// USB interface number and parent USB device's vendor/product ID match
// spec.
func findUSBTTY(spec usbPortSpec) (string, error) {
	var candidates []string
	for _, pattern := range []string{"/sys/class/tty/ttyUSB*", "/sys/class/tty/ttyACM*"} {
		matches, _ := filepath.Glob(pattern)
		candidates = append(candidates, matches...)
	}
	for _, ttyPath := range candidates {
		// EvalSymlinks on .../ttyUSBn/device resolves all the way down
		// to the tty's own sysfs node (e.g.
		// .../usb2/2-3/2-3:1.3/ttyUSB10) — confirmed live on dawn6g,
		// not the interface directory one level up. matchesUSBSpec
		// takes that full node path and walks up from there itself.
		devNode, err := filepath.EvalSymlinks(filepath.Join(ttyPath, "device"))
		if err != nil {
			continue
		}
		if matchesUSBSpec(devNode, spec) {
			return "/dev/" + filepath.Base(ttyPath), nil
		}
	}
	return "", fmt.Errorf("no tty device found for usb %s:%s interface %d", spec.vendorID, spec.productID, spec.ifNum)
}

// matchesUSBSpec reports whether devNode — the resolved sysfs path of
// a tty's own device node (e.g. .../usb2/2-3/2-3:1.3/ttyUSB10) —
// matches spec. bInterfaceNumber lives one level up, in the USB
// interface directory (.../2-3/2-3:1.3); idVendor/idProduct live one
// level above that, in the USB device directory (.../2-3) — confirmed
// against dawn6g's real sysfs layout for its Quectel (2c7c:0801,
// interface 03, under
// /sys/devices/.../usb2/2-3/2-3:1.3/ttyUSB10). Pure filesystem reads,
// no globbing — split out from findUSBTTY specifically so it's
// unit-testable against a fake sysfs tree built with t.TempDir(),
// without real hardware.
func matchesUSBSpec(devNode string, spec usbPortSpec) bool {
	ifaceDir := filepath.Dir(devNode)
	ifNumRaw, err := os.ReadFile(filepath.Join(ifaceDir, "bInterfaceNumber"))
	if err != nil {
		return false
	}
	ifNum, err := strconv.Atoi(strings.TrimSpace(string(ifNumRaw)))
	if err != nil || ifNum != spec.ifNum {
		return false
	}

	devDir := filepath.Dir(ifaceDir)
	vendor, err := os.ReadFile(filepath.Join(devDir, "idVendor"))
	if err != nil {
		return false
	}
	product, err := os.ReadFile(filepath.Join(devDir, "idProduct"))
	if err != nil {
		return false
	}
	return strings.ToLower(strings.TrimSpace(string(vendor))) == spec.vendorID &&
		strings.ToLower(strings.TrimSpace(string(product))) == spec.productID
}
