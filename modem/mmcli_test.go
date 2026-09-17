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

func TestSignalFromKeyValue(t *testing.T) {
	cases := []struct {
		name string
		info map[string]string
		want Signal
	}{
		{
			name: "5g present, takes priority over lte",
			info: map[string]string{
				"modem.signal.5g.rsrp":  "-95.00",
				"modem.signal.5g.rsrq":  "-11.00",
				"modem.signal.5g.rssi":  "-70.00",
				"modem.signal.5g.snr":   "12.50",
				"modem.signal.lte.rsrp": "-100.00",
			},
			want: Signal{Technology: "5g", RSRP: -95, RSRQ: -11, RSSI: -70, SINR: 12.5},
		},
		{
			name: "5g sentinel, falls back to lte",
			info: map[string]string{
				"modem.signal.5g.rsrp":  "-32768",
				"modem.signal.lte.rsrp": "-102.00",
				"modem.signal.lte.rsrq": "-9.00",
				"modem.signal.lte.rssi": "-68.00",
			},
			want: Signal{Technology: "lte", RSRP: -102, RSRQ: -9, RSSI: -68, SINR: math.NaN()},
		},
		{
			name: "umts maps rscp/ecio onto rsrp/rsrq",
			info: map[string]string{
				"modem.signal.umts.rscp": "-85.00",
				"modem.signal.umts.ecio": "-6.00",
				"modem.signal.umts.rssi": "-75.00",
			},
			want: Signal{Technology: "umts", RSRP: -85, RSRQ: -6, RSSI: -75, SINR: math.NaN()},
		},
		{
			name: "gsm reports rssi only",
			info: map[string]string{
				"modem.signal.gsm.rssi": "-80.00",
			},
			want: Signal{Technology: "gsm", RSRP: math.NaN(), RSRQ: math.NaN(), RSSI: -80, SINR: math.NaN()},
		},
		{
			name: "no data anywhere",
			info: map[string]string{},
			want: NaNSignal(),
		},
		{
			name: "empty strings and -- treated as no data",
			info: map[string]string{
				"modem.signal.5g.rsrp": "--",
				"modem.signal.5g.rssi": "",
			},
			want: NaNSignal(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := signalFromKeyValue(tc.info)
			if !signalsEqual(got, tc.want) {
				t.Errorf("signalFromKeyValue() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// signalsEqual compares Signal values treating NaN == NaN (unlike ==).
func signalsEqual(a, b Signal) bool {
	eq := func(x, y float64) bool {
		return (math.IsNaN(x) && math.IsNaN(y)) || x == y
	}
	return a.Technology == b.Technology && eq(a.RSRP, b.RSRP) && eq(a.RSRQ, b.RSRQ) && eq(a.RSSI, b.RSSI) && eq(a.SINR, b.SINR)
}
