package config

import (
	"testing"
	"time"
)

func TestConfigurationDriftFreshnessConfiguration(t *testing.T) {
	for _, valid := range []string{"1m", "15m", "24h"} {
		t.Setenv("CONFIGURATION_DRIFT_MAX_AGE", valid)
		cfg, err := Load()
		if err != nil || cfg.ConfigurationDriftMaxAge < time.Minute {
			t.Fatal(valid, err)
		}
	}
	for _, invalid := range []string{"30s", "25h", "0", "broken"} {
		t.Setenv("CONFIGURATION_DRIFT_MAX_AGE", invalid)
		if _, err := Load(); err == nil {
			t.Fatal("invalid drift freshness", invalid)
		}
	}
}
