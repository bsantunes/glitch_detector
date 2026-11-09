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

func TestParseQMISignalsOutput(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want []Signal
	}{
		{
			name: "LTE signal info, no 5G section at all",
			out: "[/dev/cdc-wdm0] Successfully got signal info\n" +
				"LTE:\n" +
				"\tRSSI: '-85 dBm'\n" +
				"\tRSRQ: '-10 dB'\n" +
				"\tRSRP: '-100 dBm'\n" +
				"\tSNR: '10.0 dB'\n",
			want: []Signal{{Technology: "lte", Role: "anchor", RSSI: -85, RSRQ: -10, RSRP: -100, SINR: 10.0}},
		},
		{
			name: "EN-DC: both LTE and 5G sections carry real values -> lte anchor + 5g scg (synthetic, no real capture yet)",
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
			want: []Signal{
				{Technology: "lte", Role: "anchor", RSSI: -90, RSRQ: -12, RSRP: -105, SINR: 8.0},
				{Technology: "5g", Role: "scg", RSSI: math.NaN(), RSRQ: -9, RSRP: -95, SINR: 15.0},
			},
		},
		{
			name: "no recognizable lines",
			out:  "[/dev/cdc-wdm0] Successfully got signal info\n",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseQMISignalsOutput(tc.out)
			if !signalSlicesEqual(got, tc.want) {
				t.Errorf("parseQMISignalsOutput() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestParseQMISignalsOutput_RealCapture uses output captured live from
// a SIM8202G-M2 on the gateway board (qmicli --nas-get-signal-info), with
// an inactive 5G section reporting "n/a" values that must be filtered
// out rather than reported as a bogus all-NaN scg leg.
func TestParseQMISignalsOutput_RealCapture(t *testing.T) {
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

	want := []Signal{{Technology: "lte", Role: "anchor", RSSI: -57, RSRQ: -10, RSRP: -86, SINR: 14.2}}
	got := parseQMISignalsOutput(out)
	if !signalSlicesEqual(got, want) {
		t.Errorf("parseQMISignalsOutput() = %+v, want %+v", got, want)
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

	// Role isn't set here — getSignalsQMI assigns it ("anchor") after
	// calling this pure parser, since this format never reports more
	// than one leg at a time.
	want := Signal{Technology: "lte", RSSI: -57, RSRQ: -10, RSRP: math.NaN(), SINR: 14.2}
	got := parseQMISignalStrengthOutput(out)
	if !signalsEqual(got, want) {
		t.Errorf("parseQMISignalStrengthOutput() = %+v, want %+v", got, want)
	}
}

// TestParseQMISignalStrengthOutput_NoServingCell uses output captured
// live from a Quectel RM520N-GL (WaveOS/Supermicro box) in power-save/
// not-registered state: qmicli reports "Network 'none'" instead of a
// real technology, and there's no RSRQ/SNR section at all — RSSI is
// still a real reading (-128 dBm), everything else stays NaN.
func TestParseQMISignalStrengthOutput_NoServingCell(t *testing.T) {
	out := "[/dev/cdc-wdm1] Successfully got signal strength\n" +
		"Current:\n" +
		"\tNetwork 'none': '-128 dBm'\n" +
		"RSSI:\n" +
		"\tNetwork 'none': '-128 dBm'\n" +
		"ECIO:\n" +
		"\tNetwork 'none': '-2.5 dBm'\n" +
		"IO: '-106 dBm'\n" +
		"SINR (8): '9.0 dB'\n"

	want := Signal{Technology: "", RSSI: -128, RSRQ: math.NaN(), RSRP: math.NaN(), SINR: math.NaN()}
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

// The two tests below use `qmicli --nas-get-serving-system`/
// `--nas-get-system-info` output captured live from the SIMCom modem on
// the WaveOS/Supermicro box.

func TestCellInfoFromQMIServingSystem(t *testing.T) {
	out := "[/dev/cdc-wdm0] Successfully got serving system:\n" +
		"\tRegistration state: 'not-registered-searching'\n" +
		"\tCS: 'detached'\n" +
		"\tPS: 'detached'\n" +
		"\tSelected network: '3gpp'\n" +
		"\tRadio interfaces: '1'\n" +
		"\t\t[0]: 'lte'\n" +
		"\tRoaming status: 'on'\n" +
		"\tData service capabilities: '0'\n" +
		"\tCurrent PLMN:\n" +
		"\t\tMCC: '268'\n" +
		"\t\tMNC: '6'\n" +
		"\t\tDescription: ''\n" +
		"\tRoaming indicators: '1'\n" +
		"\t\t[0]: 'on' (lte)\n" +
		"\tDetailed status:\n" +
		"\t\tStatus: 'limited'\n" +
		"\t\tCapability: 'cs-ps'\n" +
		"\tFull operator code info:\n" +
		"\t\tMCC: '268'\n" +
		"\t\tMNC: '6'\n" +
		"\t\tMNC with PCS digit: 'no'\n"

	ci := cellInfoFromQMIServingSystem(out)
	if ci.RegistrationState != "not-registered-searching" {
		t.Errorf("RegistrationState = %q, want not-registered-searching", ci.RegistrationState)
	}
	if ci.PLMN != "26806" {
		t.Errorf("PLMN = %q, want 26806", ci.PLMN)
	}
}

func TestApplyQMISystemInfo(t *testing.T) {
	out := "[/dev/cdc-wdm0] Successfully got system info:\n" +
		"\tWCDMA service:\n" +
		"\t\tStatus: 'none'\n" +
		"\t\tTrue Status: 'none'\n" +
		"\tLTE service:\n" +
		"\t\tStatus: 'limited'\n" +
		"\t\tTrue Status: 'none'\n" +
		"\t\tDomain: 'none'\n" +
		"\t\tService capability: 'cs-ps'\n" +
		"\t\tRoaming status: 'on'\n" +
		"\t\tForbidden: 'no'\n" +
		"\t\tCell ID: '263731'\n" +
		"\t\tMCC: '268'\n" +
		"\t\tMNC: '06'\n" +
		"\t\tTracking Area Code: '48830'\n" +
		"\t5G SA service:\n" +
		"\t\tStatus: 'none'\n" +
		"\t\tTrue Status: 'none'\n" +
		"\tSIM reject info: 'unavailable'\n"

	var ci CellInfo
	applyQMISystemInfo(&ci, out)

	if ci.TAC != "48830" {
		t.Errorf("TAC = %q, want 48830", ci.TAC)
	}
	// qmicli already reports a single composite Cell ID directly
	// (matches the enodeb_id<<8|cell_id the uqmi backend has to compute
	// itself from separate fields for the same real cell).
	if ci.CellID != "263731" {
		t.Errorf("CellID = %q, want 263731", ci.CellID)
	}
}

func TestApplyQMISystemInfo_NoActiveRAT(t *testing.T) {
	out := "[/dev/cdc-wdm1] Successfully got system info:\n" +
		"\tWCDMA service:\n" +
		"\t\tStatus: 'power save'\n" +
		"\tLTE service:\n" +
		"\t\tStatus: 'power save'\n" +
		"\t5G SA service:\n" +
		"\t\tStatus: 'none'\n"

	var ci CellInfo
	applyQMISystemInfo(&ci, out)
	if ci.TAC != "" || ci.CellID != "" {
		t.Errorf("applyQMISystemInfo() = %+v, want empty TAC/CellID when no section has data", ci)
	}
}
