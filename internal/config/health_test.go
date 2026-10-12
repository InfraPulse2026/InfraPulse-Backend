package config

import "testing"

func TestHealthThresholdConfiguration(t *testing.T) {
	for _, key := range []string{"HEALTH_CPU_WARNING", "HEALTH_CPU_CRITICAL", "HEALTH_CPU_RECOVERY", "HEALTH_RAM_WARNING", "HEALTH_RAM_CRITICAL", "HEALTH_RAM_RECOVERY", "HEALTH_FORECAST_HORIZON_SECONDS", "PKI_ENABLED"} {
		t.Setenv(key, "")
	}
	if c, err := Load(); err != nil || c.Health.CPUWarning != 80 {
		t.Fatalf("health defaults: %v", err)
	}
	for _, tc := range []struct{ key, value string }{{"HEALTH_CPU_WARNING", "NaN"}, {"HEALTH_CPU_WARNING", "101"}, {"HEALTH_CPU_CRITICAL", "70"}, {"HEALTH_RAM_RECOVERY", "80"}, {"HEALTH_FORECAST_HORIZON_SECONDS", "59"}, {"HEALTH_FORECAST_HORIZON_SECONDS", "3601"}} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatal("unsafe rule config accepted")
			}
		})
	}
	t.Setenv("HEALTH_CPU_WARNING", "75")
	if c, err := Load(); err != nil || c.Health.CPUWarning != 75 {
		t.Fatal("valid threshold rejected")
	}
}
