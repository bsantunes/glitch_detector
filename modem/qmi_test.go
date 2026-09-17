package modem

import (
	"math"
	"testing"
)

// The qmicli/mbimcli sample output below is representative of the
// documented libqmi/libmbim text format, NOT captured from a real
// device — it needs cross-checking against actual tool output on the
// gateway board (format has drifted across library versions before).

func TestParseCLIKeyValue_QMICLI(t *testing.T) {
	out := "[/dev/cdc-wdm0] Device manufacturer retrieved:\n" +
		"\tManufacturer: 'QUALCOMM INCORPORATED'\n" +
		"[/dev/cdc-wdm0] Device model retrieved:\n" +
		"\tModel: 'SIM8202G-M2'\n"

	got := parseCLIKeyValue(out)
	if got["manufacturer"] != "QUALCOMM INCORPORATED" {
		t.Errorf("manufacturer = %q, want QUALCOMM INCORPORATED", got["manufacturer"])
	}
	if got["model"] != "SIM8202G-M2" {
		t.Errorf("model = %q, want SIM8202G-M2", got["model"])
	}
}

func TestParseCLIKeyValue_MBIMCLI(t *testing.T) {
	out := "[/dev/cdc-wdm0] Device capabilities retrieved:\n" +
		"\tDevice type: 'Remote'\n" +
		"\tCellular class: 'GSM'\n" +
		"\tDevice ID: '359999999999999'\n" +
		"\tFirmware info: '01.001.01.001'\n" +
		"\tHardware info: 'SIM8202G-M2'\n"

	got := parseCLIKeyValue(out)
	if got["deviceid"] != "359999999999999" {
		t.Errorf("deviceid = %q, want 359999999999999", got["deviceid"])
	}
	if got["hardwareinfo"] != "SIM8202G-M2" {
		t.Errorf("hardwareinfo = %q, want SIM8202G-M2", got["hardwareinfo"])
	}
}

func TestParseQMISignalOutput(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want Signal
	}{
		{
			name: "LTE signal info",
			out: "[/dev/cdc-wdm0] Successfully got signal info\n" +
				"LTE:\n" +
				"\tRSSI: '-85 dBm'\n" +
				"\tRSRQ: '-10 dB'\n" +
				"\tRSRP: '-100 dBm'\n" +
				"\tSNR: '10.0 dB'\n",
			want: Signal{Technology: "lte", RSSI: -85, RSRQ: -10, RSRP: -100, SINR: 10.0},
		},
		{
			name: "5G NR signal info, later section wins over earlier LTE",
			out: "[/dev/cdc-wdm0] Successfully got signal info\n" +
				"LTE:\n" +
				"\tRSSI: '-90 dBm'\n" +
				"\tRSRQ: '-12 dB'\n" +
				"\tRSRP: '-105 dBm'\n" +
				"\tSNR: '8.0 dB'\n" +
				"5G:\n" +
				"\tRSRP: '-95 dBm'\n" +
				"\tRSRQ: '-9 dB'\n" +
				"\tSNR: '15.0 dB'\n",
			want: Signal{Technology: "5g", RSSI: -90, RSRQ: -9, RSRP: -95, SINR: 15.0},
		},
		{
			name: "no recognizable lines",
			out:  "[/dev/cdc-wdm0] Successfully got signal info\n",
			want: NaNSignal(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseQMISignalOutput(tc.out)
			if !signalsEqual(got, tc.want) {
				t.Errorf("parseQMISignalOutput() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestParseQMISignalOutput_RealCapture uses output captured live from a
// SIM8202G-M2 on the gateway board (qmicli --nas-get-signal-info), with
// an inactive 5G section reporting "n/a" values that must be ignored.
func TestParseQMISignalOutput_RealCapture(t *testing.T) {
	out := "[/dev/cdc-wdm0] Successfully got signal info\n" +
		"LTE:\n" +
		"\tRSSI: '-57 dBm'\n" +
		"\tRSRQ: '-10 dB'\n" +
		"\tRSRP: '-86 dBm'\n" +
		"\tSNR: '14.2 dB'\n" +
		"5G:\n" +
		"\tRSRP: 'n/a'\n" +
		"\tSNR: 'n/a'\n" +
		"\tRSRQ: 'n/a'\n"

	want := Signal{Technology: "lte", RSSI: -57, RSRQ: -10, RSRP: -86, SINR: 14.2}
	got := parseQMISignalOutput(out)
	if !signalsEqual(got, want) {
		t.Errorf("parseQMISignalOutput() = %+v, want %+v", got, want)
	}
}

// TestParseQMISignalStrengthOutput uses output captured live from the
// same modem via the older `qmicli --nas-get-signal-strength` command.
func TestParseQMISignalStrengthOutput(t *testing.T) {
	out := "\n[/dev/cdc-wdm0] Successfully got signal strength\n" +
		"Current:\n" +
		"\tNetwork 'lte': '-57 dBm'\n" +
		"RSSI:\n" +
		"\tNetwork 'lte': '-57 dBm'\n" +
		"ECIO:\n" +
		"\tNetwork 'lte': '-2.5 dBm'\n" +
		"IO: '-106 dBm'\n" +
		"SINR (8): '9.0 dB'\n" +
		"RSRQ:\n" +
		"\tNetwork 'lte': '-10 dB'\n" +
		"SNR:\n" +
		"\tNetwork 'lte': '14.2 dB'\n"

	want := Signal{Technology: "lte", RSSI: -57, RSRQ: -10, RSRP: math.NaN(), SINR: 14.2}
	got := parseQMISignalStrengthOutput(out)
	if !signalsEqual(got, want) {
		t.Errorf("parseQMISignalStrengthOutput() = %+v, want %+v", got, want)
	}
}

func TestParseMBIMSignalOutput(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want float64 // expected RSSI; other fields are always NaN today
	}{
		{
			name: "dBm value present in parentheses",
			out:  "[/dev/cdc-wdm0] Signal state retrieved:\n\tRSSI: '24' (-63.00 dBm)\n",
			want: -63,
		},
		{
			name: "raw 0-31 scale only",
			out:  "[/dev/cdc-wdm0] Signal state retrieved:\n\tRSSI: '20'\n",
			want: -113 + 2*20,
		},
		{
			name: "99 means unknown",
			out:  "[/dev/cdc-wdm0] Signal state retrieved:\n\tRSSI: '99'\n",
			want: math.NaN(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseMBIMSignalOutput(tc.out)
			if !(math.IsNaN(got.RSSI) && math.IsNaN(tc.want)) && got.RSSI != tc.want {
				t.Errorf("parseMBIMSignalOutput().RSSI = %v, want %v", got.RSSI, tc.want)
			}
		})
	}
}
