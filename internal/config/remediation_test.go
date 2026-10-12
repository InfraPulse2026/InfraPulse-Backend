package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rm "github.com/InfraPulse2026/InfraPulse-Backend/internal/remediation"
)

func TestRemediationDefaultDenyAndEnablement(t *testing.T) {
	t.Setenv("REMEDIATION_POLICY_FILE", "")
	t.Setenv("AUTOMATION_ENABLED", "false")
	policy, err := LoadRemediationPolicy()
	if err != nil || policy.Enabled {
		t.Fatal("unsafe remediation default", err)
	}
	policy = rm.Policy{Enabled: true, Version: "lab_v1", AllowedUsers: []string{"user_one"}, Rules: []rm.Rule{{AssetID: "asset_one", BindingID: "binding_one", InterfaceID: "port12", Interface: "GigabitEthernet0/12", Platform: rm.Platform, Role: "access", AllowedVLANs: []int{20}, ManagementVLANs: []int{1}}}}
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REMEDIATION_POLICY_FILE", path)
	if _, err = Load(); err == nil {
		t.Fatal("enabled remediation accepted without automation")
	}
	t.Setenv("AUTOMATION_ENABLED", "true")
	t.Setenv("AUTH_MODE", "required")
	t.Setenv("AUTOMATION_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{29}, 32)))
	cfg, err := Load()
	if err != nil || !cfg.Remediation.Enabled {
		t.Fatal(err)
	}
	t.Setenv("AUTH_MODE", "disabled")
	if _, err = Load(); err == nil {
		t.Fatal("enabled remediation accepted without authentication")
	}
}

func TestRemediationLocalPolicyClosedAndBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	t.Setenv("REMEDIATION_POLICY_FILE", path)
	for _, raw := range []string{`{"enabled":false,"extra_vars":{}}`, `{"enabled":false} {}`, `{"enabled":true}`, `{"enabled":false,"password":"DO_NOT_EXPOSE_SECRET"}`, strings.Repeat("x", rm.MaxPolicyFileBytes+1)} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadRemediationPolicy()
		if err == nil || strings.Contains(err.Error(), "DO_NOT_EXPOSE_SECRET") {
			t.Fatal("untrusted policy accepted or leaked", err)
		}
	}
	t.Setenv("REMEDIATION_POLICY_FILE", "relative/policy.json")
	if _, err := LoadRemediationPolicy(); err == nil {
		t.Fatal("relative deployment path accepted")
	}
	t.Setenv("REMEDIATION_POLICY_FILE", filepath.Join(t.TempDir(), "missing.json"))
	if _, err := LoadRemediationPolicy(); err == nil {
		t.Fatal("missing configured policy silently disabled")
	}
	if err := os.WriteFile(path, []byte(`{"enabled":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REMEDIATION_POLICY_FILE", path)
	policy, err := LoadRemediationPolicy()
	if err != nil || policy.Enabled {
		t.Fatal("explicit disabled policy", err)
	}
}

func TestIndependentWorkerUsesSameProtectedResources(t *testing.T) {
	for _, key := range []string{"COMMANDS_ENABLED", "COMMAND_ALLOW_DISABLE", "COMMAND_ALLOW_ROLLBACK", "COMMAND_PROTECTED_DEVICES", "COMMAND_PROTECTED_INTERFACES", "REMEDIATION_POLICY_FILE"} {
		t.Setenv(key, "")
	}
	t.Setenv("COMMAND_PROTECTED_DEVICES", "asset_one,device_one")
	t.Setenv("COMMAND_PROTECTED_INTERFACES", "GigabitEthernet0/1,port1")
	worker, err := LoadCommandPolicy()
	if err != nil {
		t.Fatal(err)
	}
	server, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	left, _ := json.Marshal(worker)
	right, _ := json.Marshal(server.Commands)
	if !bytes.Equal(left, right) || len(worker.ProtectedInterfaces) != 5 || len(worker.ProtectedDevices) != 2 {
		t.Fatal("server/worker protection differs")
	}
}
