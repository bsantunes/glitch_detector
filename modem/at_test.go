package modem

import (
	"math"
	"testing"
)

func TestParseIDMMPortIgnore(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name: "marked ignored",
			// Real udevadm property-output shape, as seen on aaeon for
			// a port a 99-dawn-gps.rules-style udev rule has reserved.
			output: "DEVPATH=/devices/pci0000:00/0000:00:14.0/usb2/2-3/2-3:1.3/ttyUSB3/tty/ttyUSB3\nID_VENDOR_ID=1e0e\nID_MODEL_ID=9001\nID_USB_INTERFACE_NUM=03\nID_MM_PORT_IGNORE=1\n",
			want:   true,
		},
		{
			name:   "not marked",
			output: "DEVPATH=/devices/pci0000:00/0000:00:14.0/usb2/2-3/2-3:1.2/ttyUSB2/tty/ttyUSB2\nID_VENDOR_ID=1e0e\nID_MODEL_ID=9001\nID_USB_INTERFACE_NUM=02\n",
			want:   false,
		},
		{
			name:   "empty output (e.g. udevadm error swallowed upstream)",
			output: "",
			want:   false,
		},
		{
			name:   "property present but not exactly \"1\" - must not fuzzy-match",
			output: "ID_MM_PORT_IGNORE=0\n",
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseIDMMPortIgnore(tt.output); got != tt.want {
				t.Errorf("parseIDMMPortIgnore(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

func TestParseATI(t *testing.T) {
	cases := []struct {
		name             string
		resp             string
		wantManufacturer string
		wantModel        string
	}{
		{
			name:             "labeled fields",
			resp:             "ATI\r\nManufacturer: SIMCOM INCORPORATED\r\nModel: SIM8202G-M2\r\nOK\r\n",
			wantManufacturer: "SIMCOM INCORPORATED",
			wantModel:        "SIM8202G-M2",
		},
		{
			name:             "unlabeled two-line format",
			resp:             "ATI\r\nQuectel\r\nEC25\r\nOK\r\n",
			wantManufacturer: "Quectel",
			wantModel:        "EC25",
		},
		{
			// Real captured output from a live Quectel RM520N-GL
			// (6global/dawn6g, no SIM present): it spams unsolicited
			// "+CPIN: NOT INSERTED" notifications before actually
			// answering ATI. Without anchoring on the command echo,
			// the positional fallback mistakes that noise for the
			// manufacturer/model themselves.
			name:             "unsolicited notifications before the echo",
			resp:             "\r\n+CPIN: NOT INSERTED\r\n\r\n+CPIN: NOT INSERTED\r\n\r\nATI\r\nQuectel\r\nRM520N-GL\r\nRevision: RM520NGLAAR03A04M4G\r\n\r\nOK\r\n\r\n+CPIN: NOT INSERTED\r\n",
			wantManufacturer: "Quectel",
			wantModel:        "RM520N-GL",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manufacturer, model := parseATI(tc.resp)
			if manufacturer != tc.wantManufacturer {
				t.Errorf("manufacturer = %q, want %q", manufacturer, tc.wantManufacturer)
			}
			if model != tc.wantModel {
				t.Errorf("model = %q, want %q", model, tc.wantModel)
			}
		})
	}
}

func TestParseCESQ(t *testing.T) {
	cases := []struct {
		name string
		resp string
		want Signal
	}{
		{
			name: "valid rsrp/rsrq",
			// rxlev=99(n/a), ber=99(n/a), rscp=255(n/a), ecno=255(n/a), rsrq=30, rsrp=110
			resp: "+CESQ: 99,99,255,255,30,110\r\nOK\r\n",
			want: Signal{Technology: "lte", RSRP: -140 + 110, RSRQ: -19.5 + 30*0.5, RSSI: math.NaN(), SINR: math.NaN()},
		},
		{
			name: "all sentinels means no data",
			resp: "+CESQ: 99,99,255,255,255,255\r\nOK\r\n",
			want: NaNSignal(),
		},
		{
			name: "unparseable response",
			resp: "ERROR\r\n",
			want: NaNSignal(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCESQ(tc.resp)
			if !signalsEqual(got, tc.want) {
				t.Errorf("parseCESQ() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
