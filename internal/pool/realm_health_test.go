package pool

import "testing"

func TestRealmHealthMatchesCounts(t *testing.T) {
	p := realmPool(t)
	h := p.RealmHealth("cn")
	total, healthy, cooling, disabled, full := p.CountsDetailedForRealm("cn")
	if h.Total != total || h.Healthy != healthy || h.Cooling != cooling || h.Disabled != disabled || h.InFlightFull != full {
		t.Fatalf("realm health %+v != counts %d %d %d %d %d", h, total, healthy, cooling, disabled, full)
	}
	if h.Healthy != 2 {
		t.Fatalf("cn healthy=%d want 2", h.Healthy)
	}
}
