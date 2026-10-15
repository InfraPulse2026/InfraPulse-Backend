package reports

import (
	"testing"
	"time"
)

func TestV2ScopeAndClosedUTCPeriods(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f, err := (V2Filter{From: now.Add(-time.Hour), To: now}).Normalize(now)
	if err != nil || f.Scope != "real" {
		t.Fatal(f, err)
	}
	invalid := f
	invalid.SiteID = "site' OR true"
	if _, err = invalid.Normalize(now); err == nil {
		t.Fatal("invalid scope id accepted")
	}
	from, to, next := ClosedPeriod(now, "weekly")
	if to.Weekday() != time.Monday || !to.Before(now) || to.Sub(from) != 7*24*time.Hour || next.Sub(to) != 7*24*time.Hour {
		t.Fatal(from, to, next)
	}
	from, to, next = ClosedPeriod(now, "daily")
	if to.Hour() != 0 || to.Sub(from) != 24*time.Hour || next.Sub(to) != 24*time.Hour {
		t.Fatal(from, to, next)
	}
}
