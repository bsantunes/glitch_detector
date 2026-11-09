package modem

import (
	"math"
	"testing"
)

// The JSON samples below were captured live from `uqmi` against two
// different real modems on a WaveOS (OpenWrt) Supermicro box: a SIMCom
// 5G module (qmi0) and a Quectel RM520N-GL (qmi1, in power-save/
// not-registered state at capture time).

func TestSignalFromUQMIJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want Signal
	}{
		{
			name: "SIMCom, registered on LTE",
			raw:  `{"type":"lte","rssi":-98,"rsrq":-15,"rsrp":-122,"snr":2.000000}`,
			want: Signal{Technology: "lte", RSSI: -98, RSRQ: -15, RSRP: -122, SINR: 2},
		},
		{
			name: "Quectel, not registered -> bare string, not an object",
			raw:  `"Information unavailable"`,
			want: NaNSignal(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := signalFromUQMIJSON([]byte(tc.raw))
			if !signalsEqual(got, tc.want) {
				t.Errorf("signalFromUQMIJSON() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestApplyUQMIServingSystem(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want CellInfo
	}{
		{
			name: "SIMCom, searching, roaming",
			raw:  `{"registration":"searching","plmn_mcc":268,"plmn_mnc":6,"plmn_description":"","roaming":true}`,
			want: CellInfo{RegistrationState: "searching", PLMN: "26806"},
		},
		{
			name: "Quectel, not registered -> no plmn fields at all",
			raw:  `{"registration":"not_registered"}`,
			want: CellInfo{RegistrationState: "not_registered"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ci CellInfo
			applyUQMIServingSystem(&ci, []byte(tc.raw))
			if ci != tc.want {
				t.Errorf("applyUQMIServingSystem() = %+v, want %+v", ci, tc.want)
			}
		})
	}
}

func TestApplyUQMISystemInfo(t *testing.T) {
	// Real capture from the SIMCom modem: wcdma inactive ("none"), lte
	// carrying tracking_area_code/enodeb_id/cell_id.
	raw := []byte(`{
		"wcdma": {"service_status":"none","true_service_status":"none","preferred_data_path":false},
		"lte": {
			"service_status":"limited","true_service_status":"none","preferred_data_path":false,
			"domain":"none","service":"cs-ps","roaming_status":"on","forbidden":false,
			"mcc":"268","mnc":"06","tracking_area_code":48830,"enodeb_id":1030,"cell_id":51,
			"cell_access_status":"all calls","registration_restriction":0,"registration_domain":0,
			"5g_nsa_available":false,"dcnr_restriction":false
		}
	}`)

	var ci CellInfo
	applyUQMISystemInfo(&ci, raw)

	if ci.TAC != "48830" {
		t.Errorf("TAC = %q, want 48830", ci.TAC)
	}
	// eci = enodeb_id<<8 | cell_id = 1030*256 + 51 = 263731
	wantCellID := "263731"
	if ci.CellID != wantCellID {
		t.Errorf("CellID = %q, want %s", ci.CellID, wantCellID)
	}
}

func TestApplyUQMISystemInfo_NoActiveRAT(t *testing.T) {
	// Real capture from the Quectel modem: both sections present but
	// neither carries tracking_area_code (power-save, not registered).
	raw := []byte(`{
		"wcdma": {"service_status":"power save","true_service_status":"none","preferred_data_path":false},
		"lte": {"service_status":"power save","true_service_status":"none","preferred_data_path":false,"registration_restriction":0}
	}`)

	var ci CellInfo
	applyUQMISystemInfo(&ci, raw)

	if ci.TAC != "" || ci.CellID != "" {
		t.Errorf("applyUQMISystemInfo() = %+v, want empty TAC/CellID when no section has data", ci)
	}
}

func TestUQMISignalAndCellInfo_NaNDefaults(t *testing.T) {
	// Sanity check that an all-empty Signal still reports NaN, not zero,
	// for fields uqmi genuinely never sends.
	sig := signalFromUQMIJSON([]byte(`{"type":"lte"}`))
	if !math.IsNaN(sig.RSRP) || !math.IsNaN(sig.RSRQ) || !math.IsNaN(sig.RSSI) || !math.IsNaN(sig.SINR) {
		t.Errorf("signalFromUQMIJSON() = %+v, want all-NaN metrics with only \"type\" present", sig)
	}
}
