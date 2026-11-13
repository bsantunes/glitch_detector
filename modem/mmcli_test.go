package modem

import (
	"math"
	"testing"
)

func TestParseKeyValue(t *testing.T) {
	input := "modem.generic.manufacturer : QUALCOMM INCORPORATED\n" +
		"modem.generic.model         : SIM8202G-M2\n" +
		"\n" +
		"malformed line without separator\n" +
		"modem.generic.primary-port  : wwan0\n"

	got := parseKeyValue(input)

	want := map[string]string{
		"modem.generic.manufacturer": "QUALCOMM INCORPORATED",
		"modem.generic.model":        "SIM8202G-M2",
		"modem.generic.primary-port": "wwan0",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("parseKeyValue()[%q] = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("parseKeyValue() returned %d keys, want %d (got: %v)", len(got), len(want), got)
	}
}

func TestModemFromKeyValue(t *testing.T) {
	info := map[string]string{
		"modem.generic.manufacturer": "QUALCOMM INCORPORATED",
		"modem.generic.model":        "SIM8202G-M2",
		"modem.generic.primary-port": "wwan0",
	}
	m := modemFromKeyValue("0", info)

	if m.ID != "0" || m.Manufacturer != "QUALCOMM INCORPORATED" || m.Model != "SIM8202G-M2" || m.Port != "wwan0" || m.Method != MethodModemManager {
		t.Errorf("modemFromKeyValue() = %+v, unexpected", m)
	}
}

func TestSignalsFromKeyValue(t *testing.T) {
	cases := []struct {
		name string
		info map[string]string
		want []Signal
	}{
		{
			name: "5g present, no lte -> single anchor leg",
			info: map[string]string{
				"modem.signal.5g.rsrp": "-95.00",
				"modem.signal.5g.rsrq": "-11.00",
				"modem.signal.5g.rssi": "-70.00",
				"modem.signal.5g.snr":  "12.50",
			},
			want: []Signal{{Technology: "5g", Role: "anchor", RSRP: -95, RSRQ: -11, RSSI: -70, SINR: 12.5}},
		},
		{
			name: "5g sentinel, falls back to lte",
			info: map[string]string{
				"modem.signal.5g.rsrp":  "-32768",
				"modem.signal.lte.rsrp": "-102.00",
				"modem.signal.lte.rsrq": "-9.00",
				"modem.signal.lte.rssi": "-68.00",
			},
			want: []Signal{{Technology: "lte", Role: "anchor", RSRP: -102, RSRQ: -9, RSSI: -68, SINR: math.NaN()}},
		},
		{
			name: "EN-DC: both lte and 5g active -> lte anchor + 5g scg (synthetic, no real capture yet)",
			info: map[string]string{
				"modem.signal.lte.rsrp": "-102.00",
				"modem.signal.lte.rsrq": "-9.00",
				"modem.signal.lte.rssi": "-68.00",
				"modem.signal.5g.rsrp":  "-95.00",
				"modem.signal.5g.rsrq":  "-11.00",
				"modem.signal.5g.snr":   "12.50",
			},
			want: []Signal{
				{Technology: "lte", Role: "anchor", RSRP: -102, RSRQ: -9, RSSI: -68, SINR: math.NaN()},
				{Technology: "5g", Role: "scg", RSRP: -95, RSRQ: -11, RSSI: math.NaN(), SINR: 12.5},
			},
		},
		{
			name: "umts maps rscp/ecio onto rsrp/rsrq",
			info: map[string]string{
				"modem.signal.umts.rscp": "-85.00",
				"modem.signal.umts.ecio": "-6.00",
				"modem.signal.umts.rssi": "-75.00",
			},
			want: []Signal{{Technology: "umts", Role: "anchor", RSRP: -85, RSRQ: -6, RSSI: -75, SINR: math.NaN()}},
		},
		{
			name: "gsm reports rssi only",
			info: map[string]string{
				"modem.signal.gsm.rssi": "-80.00",
			},
			want: []Signal{{Technology: "gsm", Role: "anchor", RSRP: math.NaN(), RSRQ: math.NaN(), RSSI: -80, SINR: math.NaN()}},
		},
		{
			name: "no data anywhere",
			info: map[string]string{},
			want: nil,
		},
		{
			name: "empty strings and -- treated as no data",
			info: map[string]string{
				"modem.signal.5g.rsrp": "--",
				"modem.signal.5g.rssi": "",
			},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := signalsFromKeyValue(tc.info)
			if !signalSlicesEqual(got, tc.want) {
				t.Errorf("signalsFromKeyValue() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// signalsEqual compares Signal values treating NaN == NaN (unlike ==).
func signalsEqual(a, b Signal) bool {
	eq := func(x, y float64) bool {
		return (math.IsNaN(x) && math.IsNaN(y)) || x == y
	}
	return a.Technology == b.Technology && a.Role == b.Role &&
		eq(a.RSRP, b.RSRP) && eq(a.RSRQ, b.RSRQ) && eq(a.RSSI, b.RSSI) && eq(a.SINR, b.SINR)
}

// signalSlicesEqual compares []Signal order-sensitively via signalsEqual.
func signalSlicesEqual(a, b []Signal) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !signalsEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// TestCellInfoFrom3GPPKeyValue uses the exact fields captured live from
// `mmcli -m 0 --output-keyvalue` on the gateway board.
func TestCellInfoFrom3GPPKeyValue(t *testing.T) {
	info := map[string]string{
		"modem.3gpp.operator-code":        "26806",
		"modem.3gpp.operator-name":        "MEO",
		"modem.3gpp.registration-state":   "home",
		"modem.3gpp.packet-service-state": "attached",
	}
	want := CellInfo{PLMN: "26806", OperatorName: "MEO", RegistrationState: "home", PacketServiceState: "attached"}
	got := cellInfoFrom3GPPKeyValue(info)
	if got != want {
		t.Errorf("cellInfoFrom3GPPKeyValue() = %+v, want %+v", got, want)
	}
}

// TestApplyLocationKeyValue uses the exact fields captured live from
// `sudo mmcli -m 0 --location-get --output-keyvalue` on the gateway board.
// Hex TAC/CellID are normalized to decimal (0xBEBE=48830, 0x40615=263701)
// so CellInfo is consistent across backends — uqmi (used on WaveOS)
// reports the same tracking area code as decimal 48830 natively, which
// cross-checks the hex-to-decimal conversion here.
func TestApplyLocationKeyValue(t *testing.T) {
	loc := map[string]string{
		"modem.location.3gpp.mcc": "268",
		"modem.location.3gpp.mnc": "06",
		"modem.location.3gpp.lac": "0000",
		"modem.location.3gpp.tac": "00BEBE",
		"modem.location.3gpp.cid": "00040615",
	}
	ci := CellInfo{PLMN: "26806", OperatorName: "MEO"}
	applyLocationKeyValue(&ci, loc)

	if ci.TAC != "48830" {
		t.Errorf("TAC = %q, want 48830", ci.TAC)
	}
	if ci.CellID != "263701" {
		t.Errorf("CellID = %q, want 263701", ci.CellID)
	}
	// Fields set before the call must survive untouched.
	if ci.PLMN != "26806" || ci.OperatorName != "MEO" {
		t.Errorf("applyLocationKeyValue clobbered unrelated fields: %+v", ci)
	}
}

func TestApplyLocationKeyValue_NoData(t *testing.T) {
	ci := CellInfo{}
	applyLocationKeyValue(&ci, map[string]string{
		"modem.location.3gpp.tac": "--",
		"modem.location.3gpp.cid": "--",
	})
	if ci.TAC != "" || ci.CellID != "" {
		t.Errorf("applyLocationKeyValue() = %+v, want empty strings for \"--\" sentinel", ci)
	}
}
