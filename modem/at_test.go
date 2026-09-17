package modem

import (
	"math"
	"testing"
)

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
