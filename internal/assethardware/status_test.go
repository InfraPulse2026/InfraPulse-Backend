package assethardware

import "testing"

func TestVendorStatusNeverGuesses(t *testing.T) {
	for _, tc := range []struct{ health, rollup, kind, want string }{
		{"OK", "", "health", "ok"}, {"ok", "Warning", "health", "warning"},
		{"critical", "warning", "component", "critical"}, {"unknown", "ok", "health", "unknown"},
		{"", "", "inventory", "unknown"}, {"ok", "", "metric", "unknown"},
		{"failed", "", "health", "unknown"}, {"unknown", "critical", "health", "critical"},
	} {
		if got := Status(Observation{Health: tc.health, HealthRollup: tc.rollup, Kind: tc.kind, PowerState: "Off"}); got != tc.want {
			t.Fatalf("%+v: %s", tc, got)
		}
	}
}
