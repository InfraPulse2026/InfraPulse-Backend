package reports

import (
	"testing"
	"time"
)

func TestNormalizeBoundariesAndStableIdentity(t *testing.T) {
	now := time.Now()
	base := Filter{From: now.Add(-time.Hour), To: now.Add(-time.Minute)}
	normal, err := base.Normalize(now)
	if err != nil || normal.TopLimit != 20 {
		t.Fatal("valid report rejected")
	}
	zone := time.FixedZone("offset", 3*3600)
	same := base
	same.From = same.From.In(zone)
	same.To = same.To.In(zone)
	same, _ = same.Normalize(now)
	if normal.Fingerprint() != same.Fingerprint() {
		t.Fatal("timezone changed request identity")
	}
	for _, bad := range []Filter{{From: now, To: now}, {From: now.Add(-32 * 24 * time.Hour), To: now}, {From: now.Add(-time.Minute), To: now.Add(time.Second)}, {From: base.From, To: base.To, TopLimit: 101}, {From: base.From, To: base.To, ResourceKind: "anything"}} {
		if _, err = bad.Normalize(now); err == nil {
			t.Fatal("invalid filter accepted")
		}
	}
}
