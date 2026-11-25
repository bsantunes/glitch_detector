package main

import "testing"

func TestTwampTags_IfaceOnlyWithBindDev(t *testing.T) {
	tags := twampTags("6global", "10.30.30.1:862", "")
	if _, ok := tags["iface"]; ok || len(tags) != 2 {
		t.Errorf("unbound tags = %v, want only device/reflector (unchanged schema)", tags)
	}
	tags = twampTags("6global", "51.91.57.3:862", "lte1")
	if tags["iface"] != "lte1" || tags["device"] != "6global" || tags["reflector"] != "51.91.57.3:862" {
		t.Errorf("bound tags = %v, want device/reflector plus iface=lte1", tags)
	}
}
