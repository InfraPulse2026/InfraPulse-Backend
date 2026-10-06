package monitoring

import (
	"testing"
	"time"
)

func TestPolicyBoundaries(t *testing.T) {
	for _, seconds := range []int{3, 60, 36000, 86400} {
		p := Policy{seconds, seconds, seconds}
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []Policy{{2, 300, 300}, {3, 86401, 300}, {3, 300, 0}} {
		if p.Validate() == nil {
			t.Fatalf("invalid policy accepted: %v", p)
		}
	}
}

func TestFreshnessDependsOnCollectionInterval(t *testing.T) {
	now := time.Date(2026, 10, 5, 16, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		ago      time.Duration
		interval int
		status   string
	}{
		{"fast-fresh", 5 * time.Second, 3, "fresh"},
		{"fast-stale", 12 * time.Second, 3, "stale"},
		{"slow-fresh", 5 * time.Hour, 36000, "fresh"},
		{"slow-stale", 21 * time.Hour, 36000, "stale"},
		{"daily-fresh", 25 * time.Hour, 86400, "fresh"},
		{"daily-stale", 49 * time.Hour, 86400, "stale"},
		{"future", -time.Minute, 3, "clock_skew"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := now.Add(-tc.ago)
			f := Evaluate(now, &observed, &now, tc.interval)
			if f.Status != tc.status {
				t.Fatalf("got %+v", f)
			}
		})
	}
	if f := Evaluate(now, nil, nil, 3); f.Status != "no_data" || f.AgeSeconds != nil {
		t.Fatalf("bad missing data: %+v", f)
	}
}

func TestDailyPolicyExportsAgentCompatibleYAML(t *testing.T) {
	p := Policy{86400, 86400, 86400}
	s := Describe(p, "device_override")
	if s.RecommendedTopologyTTLSeconds != 86400 || s.LocalTelemetryYAML != "telemetry:\n  fast_interval: 86400s\n  inventory_interval: 86400s\n  topology_interval: 86400s\n  heartbeat_interval: 10s\n" {
		t.Fatalf("daily settings exceed Agent limits or change heartbeat: %+v", s)
	}
}
