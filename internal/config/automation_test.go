package config

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestAutomationRequiresAuthenticationAndStableEncryptionKey(t *testing.T) {
	t.Setenv("AUTH_MODE", "required")
	t.Setenv("AUTOMATION_ENABLED", "true")
	t.Setenv("AUTOMATION_ENCRYPTION_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("enabled automation accepted without encryption")
	}
	t.Setenv("AUTOMATION_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{23}, 32)))
	cfg, err := Load()
	if err != nil || !cfg.AutomationEnabled || cfg.AutomationVault == nil {
		t.Fatal(err)
	}
	t.Setenv("AUTH_MODE", "disabled")
	if _, err = Load(); err == nil {
		t.Fatal("automation accepted without REST authentication")
	}
	t.Setenv("AUTH_MODE", "required")
	t.Setenv("AUTOMATION_ENCRYPTION_KEY", "invalid")
	if _, err = Load(); err == nil {
		t.Fatal("bad encryption key accepted")
	}
	t.Setenv("AUTOMATION_ENCRYPTION_KEY", "")
	t.Setenv("AUTOMATION_ENABLED", "false")
	cfg, err = Load()
	if err != nil || cfg.AutomationEnabled {
		t.Fatal(err)
	}
}
