package config

import (
	"testing"
	"time"
)

func TestAssetResolutionConfiguration(t *testing.T) {
	t.Setenv("ASSET_AUTO_RESOLUTION_ENABLED", "false")
	t.Setenv("ASSET_RESOLUTION_MAX_AGE", "24h")
	c, err := Load()
	if err != nil || c.AssetAutoResolution || c.AssetResolutionMaxAge != 24*time.Hour {
		t.Fatal(err)
	}
	t.Setenv("ASSET_AUTO_RESOLUTION_ENABLED", "true")
	c, err = Load()
	if err != nil || !c.AssetAutoResolution {
		t.Fatal(err)
	}
	t.Setenv("ASSET_RESOLUTION_MAX_AGE", "0s")
	if _, err = Load(); err == nil {
		t.Fatal("unbounded age accepted")
	}
}
