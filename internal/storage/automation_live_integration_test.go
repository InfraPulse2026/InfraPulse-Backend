package storage

import (
	"encoding/json"
	au "github.com/InfraPulse2026/InfraPulse-Backend/internal/automation"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Opt-in acceptance test: real Linux Ansible Runner + selected SSH device +
// encrypted snapshot in an isolated PostgreSQL schema. No canned executor.
func TestAutomationRealLinuxRunnerSnapshot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("real Ansible control node requires Linux")
	}
	path := os.Getenv("TEST_AUTOMATION_TARGET_FILE")
	if path == "" {
		t.Skip("set TEST_AUTOMATION_TARGET_FILE for explicit real-device read-only acceptance")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read reviewed test target")
	}
	var target au.TargetInput
	if json.Unmarshal(raw, &target) != nil {
		t.Fatal("invalid test target")
	}
	executor := au.Process{Python: os.Getenv("AUTOMATION_PYTHON"), CredentialsDir: os.Getenv("AUTOMATION_CREDENTIALS_DIR"), WorkDir: t.TempDir()}
	if !filepath.IsAbs(executor.Python) || !filepath.IsAbs(executor.CredentialsDir) {
		t.Fatal("provide absolute Automation Worker runtime paths")
	}
	for _, cidr := range strings.Split(os.Getenv("AUTOMATION_ALLOWED_CIDRS"), ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil || prefix.Bits() == 0 {
			t.Fatal("explicit test destination allowlist required")
		}
		executor.AllowedNetworks = append(executor.AllowedNetworks, prefix)
	}
	status := executor.Probe(t.Context())
	if !status.Available {
		t.Fatal("real Ansible runtime probe failed")
	}
	status.ID = "worker_live_test"
	s, ctx, _, old := automationFixture(t)
	target.ExpectedRevision = old.Revision
	target.AssetRevision = 1
	target.Enabled = true
	target.Reason = "explicit real-device read-only acceptance"
	configured, err := s.PutAutomationTarget(ctx, old.AssetID, target)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AutomationWorker(ctx, status); err != nil {
		t.Fatal(err)
	}
	job, _, err := s.CreateAutomation(ctx, configured.AssetID, au.Input{TargetRevision: configured.Revision, TimeoutSeconds: 60, Reason: "read actual running configuration"}, "real-device-read")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimAutomation(ctx, status.ID)
	if err != nil || claimed.ID != job.ID {
		t.Fatal("job not claimed")
	}
	result := executor.Execute(ctx, claimed)
	if result.Validate() != nil {
		t.Fatal("real read-only Ansible job failed; inspect credentials, trusted host key and connectivity")
	}
	if err = s.FinishAutomation(ctx, claimed, "succeeded", result); err != nil {
		t.Fatal(err)
	}
	snapshots, err := s.AutomationSnapshots(ctx, configured.AssetID, 50, 0)
	if err != nil || len(snapshots) != 1 || snapshots[0].JobID != job.ID || snapshots[0].Digest != au.Digest(result.Configuration) {
		t.Fatal("actual device snapshot was not persisted")
	}
	stored, err := s.AutomationSnapshotContent(ctx, snapshots[0].ID)
	if err != nil || stored.Configuration != result.Configuration {
		t.Fatal("snapshot does not match actual runner result")
	}
}
