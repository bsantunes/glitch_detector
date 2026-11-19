package modem

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseUSBPortSpec(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		want    usbPortSpec
		wantErr bool
	}{
		{
			name: "zero-padded interface number, lowercase ids",
			spec: "usb:2c7c:0801:03",
			want: usbPortSpec{vendorID: "2c7c", productID: "0801", ifNum: 3},
		},
		{
			name: "unpadded interface number, uppercase ids normalized",
			spec: "usb:1E0E:9001:3",
			want: usbPortSpec{vendorID: "1e0e", productID: "9001", ifNum: 3},
		},
		{name: "missing usb: prefix", spec: "2c7c:0801:03", wantErr: true},
		{name: "too few fields", spec: "usb:2c7c:03", wantErr: true},
		{name: "non-numeric interface", spec: "usb:2c7c:0801:gps", wantErr: true},
		{name: "plain device path is not a usb spec", spec: "/dev/ttyUSB3", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseUSBPortSpec(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseUSBPortSpec(%q): expected error, got %+v", tt.spec, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseUSBPortSpec(%q): unexpected error: %v", tt.spec, err)
			}
			if got != tt.want {
				t.Errorf("parseUSBPortSpec(%q) = %+v, want %+v", tt.spec, got, tt.want)
			}
		})
	}
}

func TestResolvePort_NonUSBSpecPassesThrough(t *testing.T) {
	for _, dev := range []string{"/dev/ttyUSB3", "/dev/dawn-gps", "/dev/ttyACM0"} {
		got, err := ResolvePort(dev)
		if err != nil {
			t.Fatalf("ResolvePort(%q): unexpected error: %v", dev, err)
		}
		if got != dev {
			t.Errorf("ResolvePort(%q) = %q, want unchanged", dev, got)
		}
	}
}

func TestResolvePort_MalformedUSBSpec(t *testing.T) {
	if _, err := ResolvePort("usb:2c7c:0801"); err == nil {
		t.Fatalf("expected error for malformed usb spec, got none")
	}
}

// writeFakeUSBInterface builds a minimal fake sysfs tree under root
// mimicking a real USB device+interface+tty-node triple (e.g.
// .../usb2/2-3/idVendor, .../2-3/2-3:1.3/bInterfaceNumber,
// .../2-3:1.3/ttyUSB10) and returns the tty device node's own path —
// exactly the shape EvalSymlinks resolves .../ttyUSBn/device to,
// confirmed live against dawn6g's real sysfs for its Quectel
// (2c7c:0801, interface 03, under
// /sys/devices/.../usb2/2-3/2-3:1.3/ttyUSB10).
func writeFakeUSBInterface(t *testing.T, root, vendorID, productID, ifNumStr string) string {
	t.Helper()
	devDir := filepath.Join(root, "usb2", "2-3")
	ifaceDir := filepath.Join(devDir, "2-3:1.3")
	devNode := filepath.Join(ifaceDir, "ttyUSB10")
	if err := os.MkdirAll(devNode, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile := func(path, content string) {
		if err := os.WriteFile(path, []byte(content+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(filepath.Join(devDir, "idVendor"), vendorID)
	writeFile(filepath.Join(devDir, "idProduct"), productID)
	writeFile(filepath.Join(ifaceDir, "bInterfaceNumber"), ifNumStr)
	return devNode
}

func TestMatchesUSBSpec(t *testing.T) {
	root := t.TempDir()
	devNode := writeFakeUSBInterface(t, root, "2c7c", "0801", "03")

	match := usbPortSpec{vendorID: "2c7c", productID: "0801", ifNum: 3}
	if !matchesUSBSpec(devNode, match) {
		t.Errorf("expected match for %+v against the fake interface, got false", match)
	}

	wrongVendor := usbPortSpec{vendorID: "1e0e", productID: "0801", ifNum: 3}
	if matchesUSBSpec(devNode, wrongVendor) {
		t.Errorf("expected no match for wrong vendor ID, got true")
	}

	wrongIface := usbPortSpec{vendorID: "2c7c", productID: "0801", ifNum: 2}
	if matchesUSBSpec(devNode, wrongIface) {
		t.Errorf("expected no match for wrong interface number, got true")
	}
}

func TestMatchesUSBSpec_MissingFiles(t *testing.T) {
	root := t.TempDir()
	emptyDir := filepath.Join(root, "empty")
	if err := os.MkdirAll(emptyDir, 0755); err != nil {
		t.Fatal(err)
	}
	if matchesUSBSpec(emptyDir, usbPortSpec{vendorID: "2c7c", productID: "0801", ifNum: 3}) {
		t.Errorf("expected no match against a directory with no sysfs attribute files")
	}
}
