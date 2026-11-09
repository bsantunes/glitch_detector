package modem

import (
	"math"
	"testing"
)

// The "real capture" tests below use exact output captured live from
// `ubus call qmi <method>` on the WaveOS/Supermicro box, with both
// modems unregistered/offline. The "synthetic" tests cover the
// populated-leg shape based on reading
// /lib/wavesys/lib/lua/uqmi/{quectel,uqmi-ubus}.lua source — NOT
// confirmed against a live registered modem; validate against real
// output once one actually registers.

func TestModemsFromUbusList(t *testing.T) {
	raw := []byte(`{
		"lte0": {
			"esim": "0", "revision": "V1.0.01", "device": "/dev/qmi0",
			"state": "Connected", "model": "SIMCOM_SIM8262E-M2",
			"timestamp": 1792780765, "manufacturer": "SIMCOM INCORPORATED",
			"alias": "mwan0", "connectivity": "Offline"
		},
		"lte1": {
			"esim": "1", "revision": "RM520NGLAAR03A04M4G", "device": "/dev/qmi1",
			"state": "Connected", "model": "RM520N-GL",
			"timestamp": 1792780765, "manufacturer": "Quectel",
			"alias": "mwan1", "connectivity": "Offline"
		}
	}`)

	modems, err := modemsFromUbusList(raw)
	if err != nil {
		t.Fatalf("modemsFromUbusList() error = %v", err)
	}
	if len(modems) != 2 {
		t.Fatalf("modemsFromUbusList() = %d modems, want 2", len(modems))
	}

	byID := map[string]Modem{}
	for _, m := range modems {
		byID[m.ID] = m
	}

	lte0, ok := byID["lte0"]
	if !ok || lte0.Manufacturer != "SIMCOM INCORPORATED" || lte0.Model != "SIMCOM_SIM8262E-M2" || lte0.Port != "lte0" || lte0.Method != MethodUbus {
		t.Errorf("lte0 = %+v, unexpected", lte0)
	}
	lte1, ok := byID["lte1"]
	if !ok || lte1.Manufacturer != "Quectel" || lte1.Model != "RM520N-GL" || lte1.Port != "lte1" || lte1.Method != MethodUbus {
		t.Errorf("lte1 = %+v, unexpected", lte1)
	}
}

func TestSignalsFromUbusMetrics_RealCapture_Unregistered(t *testing.T) {
	raw := []byte(`{
		"lte0": {
			"sensors": {"voltage": "3.331", "temperature": "52"},
			"timestamp": 1792780765,
			"context": {"roaming": 0, "network": 0, "simready": 0},
			"labels": {"protocol": "simcom", "mode": "UNKNOWN", "iccid": "", "imei": "863746060086646"}
		},
		"lte1": {
			"sensors": {"voltage": "3.334", "temperature": "50"},
			"timestamp": 1792780765,
			"context": {"state": "SEARCH", "roaming": 0, "network": 0, "simready": 0},
			"labels": {"protocol": "quectel", "slot": "1", "mode": "servingcell", "iccid": "", "imei": "867922071133935"}
		}
	}`)

	for _, id := range []string{"lte0", "lte1"} {
		t.Run(id, func(t *testing.T) {
			got, err := signalsFromUbusMetrics(raw, id)
			if err != nil {
				t.Fatalf("signalsFromUbusMetrics(%q) error = %v", id, err)
			}
			if len(got) != 0 {
				t.Errorf("signalsFromUbusMetrics(%q) = %+v, want none (no RAT context key present)", id, got)
			}
		})
	}
}

func TestSignalsFromUbusMetrics_Synthetic_LTE(t *testing.T) {
	// Synthetic: context.LTE populated with quoted-numeric-string
	// values, matching quectel.lua's tostring()-based encoding.
	raw := []byte(`{
		"lte1": {
			"context": {
				"roaming": 0, "network": 1, "simready": 1,
				"LTE": {"rsrp": "-88", "rsrq": "-12", "rssi": "-58", "sinr": "14.6"}
			}
		}
	}`)

	want := []Signal{{Technology: "lte", Role: "anchor", RSRP: -88, RSRQ: -12, RSSI: -58, SINR: 14.6}}
	got, err := signalsFromUbusMetrics(raw, "lte1")
	if err != nil {
		t.Fatalf("signalsFromUbusMetrics() error = %v", err)
	}
	if !signalSlicesEqual(got, want) {
		t.Errorf("signalsFromUbusMetrics() = %+v, want %+v", got, want)
	}
}

func TestSignalsFromUbusMetrics_Synthetic_ENDC(t *testing.T) {
	// Synthetic: both context.LTE and context.NR5G populated (EN-DC) ->
	// lte anchor + nr scg, same convention as the mmcli/qmicli backends.
	raw := []byte(`{
		"lte1": {
			"context": {
				"LTE": {"rsrp": "-90", "rsrq": "-12", "rssi": "-60", "sinr": "8.0"},
				"NR5G": {"rsrp": "-95", "rsrq": "-9", "sinr": "15.0"}
			}
		}
	}`)

	want := []Signal{
		{Technology: "lte", Role: "anchor", RSRP: -90, RSRQ: -12, RSSI: -60, SINR: 8.0},
		{Technology: "5g", Role: "scg", RSRP: -95, RSRQ: -9, RSSI: math.NaN(), SINR: 15.0},
	}
	got, err := signalsFromUbusMetrics(raw, "lte1")
	if err != nil {
		t.Fatalf("signalsFromUbusMetrics() error = %v", err)
	}
	if !signalSlicesEqual(got, want) {
		t.Errorf("signalsFromUbusMetrics() = %+v, want %+v", got, want)
	}
}

func TestSignalsFromUbusMetrics_UnknownDevice(t *testing.T) {
	raw := []byte(`{"lte0": {"context": {}}}`)
	if _, err := signalsFromUbusMetrics(raw, "lte9"); err == nil {
		t.Errorf("signalsFromUbusMetrics() with unknown device = nil error, want an error")
	}
}

func TestCellInfoFromUbusLocation_RealCapture_Empty(t *testing.T) {
	// Real capture: "cell" is an empty Lua table encoded as a JSON
	// array, not an object, when there's no data yet.
	raw := []byte(`{
		"lte0": {"cell": [], "timestamp": 1792780765, "sim": {"mcc": "", "mnc": ""}},
		"lte1": {"cell": [], "timestamp": 1792780765, "sim": {"mcc": "", "mnc": ""}}
	}`)

	for _, id := range []string{"lte0", "lte1"} {
		t.Run(id, func(t *testing.T) {
			got := cellInfoFromUbusLocation(raw, id)
			if got != (CellInfo{}) {
				t.Errorf("cellInfoFromUbusLocation(%q) = %+v, want zero value", id, got)
			}
		})
	}
}

func TestCellInfoFromUbusLocation_Synthetic_Populated(t *testing.T) {
	// Synthetic: "cell" populated as a real object, based on reading
	// uqmi-ubus.lua's location.cell assignment (mcc/mnc/tac/cellid).
	raw := []byte(`{
		"lte1": {"cell": {"mcc": "268", "mnc": "6", "tac": "48830", "cellid": "263711"}}
	}`)

	got := cellInfoFromUbusLocation(raw, "lte1")
	want := CellInfo{PLMN: "26806", TAC: "48830", CellID: "263711"}
	if got != want {
		t.Errorf("cellInfoFromUbusLocation() = %+v, want %+v", got, want)
	}
}

func TestOperatorNameFromUbusMetrics(t *testing.T) {
	// Real capture: no "opername" key present at all while unregistered
	// (the daemon's labels table only had protocol/mode/iccid/imei) —
	// must degrade to "" rather than error.
	raw := []byte(`{
		"lte0": {"labels": {"protocol": "simcom", "mode": "UNKNOWN", "iccid": "", "imei": "863746060086646"}}
	}`)
	if got := operatorNameFromUbusMetrics(raw, "lte0"); got != "" {
		t.Errorf("operatorNameFromUbusMetrics() = %q, want \"\"", got)
	}

	raw2 := []byte(`{"lte1": {"labels": {"opername": "MEO"}}}`)
	if got := operatorNameFromUbusMetrics(raw2, "lte1"); got != "MEO" {
		t.Errorf("operatorNameFromUbusMetrics() = %q, want MEO", got)
	}
}
