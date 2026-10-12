package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	au "github.com/InfraPulse2026/InfraPulse-Backend/internal/automation"
	cd "github.com/InfraPulse2026/InfraPulse-Backend/internal/configdrift"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"strings"
	"sync"
	"testing"
)

const driftApproved = "hostname sw\ninterface GigabitEthernet0/12\n description Voice\n switchport mode access\n switchport access vlan 20\n no shutdown\n!\nend\n"

func createDriftSnapshot(t *testing.T, s *Store, ctx context.Context, target au.Target, config, key string) au.Job {
	t.Helper()
	if err := s.AutomationWorker(ctx, au.WorkerStatus{ID: "worker_one", Available: true, Templates: []string{target.TemplateID}}); err != nil {
		t.Fatal(err)
	}
	job, _, err := s.CreateAutomation(ctx, target.AssetID, automationInput(target), key)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimAutomation(ctx, "worker_one")
	if err != nil || claimed.ID != job.ID {
		t.Fatal("claim", err)
	}
	if err = s.FinishAutomation(ctx, claimed, "succeeded", au.Result{Configuration: config, Facts: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	job, err = s.AutomationJob(ctx, job.ID)
	if err != nil || job.State != "succeeded" || job.SnapshotID == "" {
		t.Fatal(job, err)
	}
	return job
}
func approveDriftBaseline(t *testing.T, s *Store, ctx context.Context, target au.Target, snapshot au.Job) cd.Baseline {
	t.Helper()
	v, _, err := s.PutConfigurationBaseline(ctx, target.AssetID, cd.BaselineInput{ExpectedRevision: 0, AssetRevision: target.AssetRevision, SnapshotID: snapshot.SnapshotID, Confirmed: true, Reason: "reviewed known-good switch configuration"}, "baseline-one")
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestConfigurationDriftApprovedBaselineAutomaticVLANAndRecovery(t *testing.T) {
	s, ctx, _, target := automationFixture(t)
	missing, err := s.ConfigurationDrift(ctx, target.AssetID)
	if err != nil || missing.Status != "unknown" || missing.Reason != "baseline_missing" {
		t.Fatal(missing, err)
	}
	expected := createDriftSnapshot(t, s, ctx, target, driftApproved, "expected")
	baseline := approveDriftBaseline(t, s, ctx, target, expected)
	state, err := s.ConfigurationDrift(ctx, target.AssetID)
	if err != nil || state.Status != "compliant" || !state.Fresh {
		t.Fatal(state, err)
	}
	actual := createDriftSnapshot(t, s, ctx, target, strings.Replace(driftApproved, "vlan 20", "vlan 30", 1), "actual")
	state, err = s.ConfigurationDrift(ctx, target.AssetID)
	if err != nil || state.Status != "drift" || state.Latest.ActualSnapshotID != actual.SnapshotID || state.Latest.Proposal == nil || state.Latest.Proposal.ExecutionAvailable {
		t.Fatal(state, err)
	}
	// Re-comparing older evidence never replaces the latest actual snapshot.
	old, err := s.CompareConfiguration(ctx, target.AssetID, cd.CompareInput{BaselineRevision: baseline.Revision, SnapshotID: expected.SnapshotID, Reason: "review historical evidence"})
	if err != nil || old.Status != "compliant" {
		t.Fatal(old, err)
	}
	state, err = s.ConfigurationDrift(ctx, target.AssetID)
	if err != nil || state.Status != "drift" || state.Latest.ActualSnapshotID != actual.SnapshotID {
		t.Fatal("historical result regressed current state", state, err)
	}
	recovered := createDriftSnapshot(t, s, ctx, target, driftApproved, "recollected-after-manual-recovery")
	state, err = s.ConfigurationDrift(ctx, target.AssetID)
	if err != nil || state.Status != "compliant" || state.Latest.ActualSnapshotID != recovered.SnapshotID {
		t.Fatal(state, err)
	}
	history, err := s.ConfigurationDriftHistory(ctx, target.AssetID, 50, 0)
	if err != nil || len(history) != 3 {
		t.Fatal(history, err)
	}
	var leaks int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM configuration_drift_results WHERE comparison::text LIKE '%description Voice%'`).Scan(&leaks); err != nil || leaks != 0 {
		t.Fatal("raw text in diff", err)
	}
}
func TestConfigurationDriftBaselineCASHistoryAndCrossAssetIsolation(t *testing.T) {
	s, ctx, _, target := automationFixture(t)
	snapshot := createDriftSnapshot(t, s, ctx, target, driftApproved, "expected")
	baseline := approveDriftBaseline(t, s, ctx, target, snapshot)
	input := cd.BaselineInput{ExpectedRevision: 0, AssetRevision: 1, SnapshotID: snapshot.SnapshotID, Confirmed: true, Reason: "reviewed known-good switch configuration"}
	replay, yes, err := s.PutConfigurationBaseline(ctx, target.AssetID, input, "baseline-one")
	if err != nil || !yes || replay.Revision != baseline.Revision {
		t.Fatal("replay", err)
	}
	input.Reason = "different reason"
	if _, _, err = s.PutConfigurationBaseline(ctx, target.AssetID, input, "baseline-one"); !errors.Is(err, cd.ErrConflict) {
		t.Fatal("key conflict", err)
	}
	if _, _, err = s.PutConfigurationBaseline(ctx, target.AssetID, input, "new-key"); !errors.Is(err, cd.ErrConflict) {
		t.Fatal("lost update", err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO assets(id,kind,canonical_name) VALUES('asset_other','network_device','other'); INSERT INTO automation_targets(asset_id,revision,definition) SELECT 'asset_other',revision,definition FROM automation_targets WHERE asset_id='asset_ansible'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.PutConfigurationBaseline(ctx, "asset_other", input, "other-key"); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-Asset snapshot accepted", err)
	}
	input.ExpectedRevision = baseline.Revision
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	conflicts := 0
	for _, key := range []string{"approve-two-a", "approve-two-b"} {
		wg.Go(func() {
			_, _, err := s.PutConfigurationBaseline(ctx, target.AssetID, input, key)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				success++
			} else if errors.Is(err, cd.ErrConflict) {
				conflicts++
			} else {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if success != 1 || conflicts != 1 {
		t.Fatal(success, conflicts)
	}
	history, err := s.ConfigurationBaselineHistory(ctx, target.AssetID, 50, 0)
	if err != nil || len(history) != 2 {
		t.Fatal(history, err)
	}
}
func TestConfigurationDriftStaleRetargetedAndChangedAssetAreUnknown(t *testing.T) {
	for _, scenario := range []string{"stale", "disabled", "retargeted", "merged", "changed_asset_revision"} {
		t.Run(scenario, func(t *testing.T) {
			s, ctx, _, target := automationFixture(t)
			snapshot := createDriftSnapshot(t, s, ctx, target, driftApproved, "expected")
			approveDriftBaseline(t, s, ctx, target, snapshot)
			var err error
			switch scenario {
			case "stale":
				_, err = s.pool.Exec(ctx, `UPDATE configuration_drift_results SET actual_observed_at=now()-interval '1 hour'`)
			case "disabled":
				in := target.TargetInput
				in.ExpectedRevision = target.Revision
				in.Enabled = false
				_, err = s.PutAutomationTarget(ctx, target.AssetID, in)
			case "retargeted":
				in := target.TargetInput
				in.ExpectedRevision = target.Revision
				in.Host = "192.0.2.24"
				_, err = s.PutAutomationTarget(ctx, target.AssetID, in)
			case "merged":
				_, err = s.pool.Exec(ctx, `UPDATE assets SET lifecycle_status='merged',revision=revision+1 WHERE id=$1`, target.AssetID)
			case "changed_asset_revision":
				_, err = s.pool.Exec(ctx, `UPDATE assets SET revision=revision+1 WHERE id=$1`, target.AssetID)
			}
			if err != nil {
				t.Fatal(err)
			}
			state, err := s.ConfigurationDrift(ctx, target.AssetID)
			if err != nil || state.Status != "unknown" || state.Latest == nil || state.Latest.Status != "compliant" {
				t.Fatal("stale observation claimed current compliance", state, err)
			}
		})
	}
}
func TestConfigurationDriftSecretRedactionAndUnsupportedSnapshot(t *testing.T) {
	s, ctx, _, target := automationFixture(t)
	expected := createDriftSnapshot(t, s, ctx, target, driftApproved+"username ops secret SENSITIVE_OLD\n", "expected")
	approveDriftBaseline(t, s, ctx, target, expected)
	createDriftSnapshot(t, s, ctx, target, driftApproved+"username ops secret SENSITIVE_NEW\n", "actual")
	state, err := s.ConfigurationDrift(ctx, target.AssetID)
	raw, _ := json.Marshal(state)
	if err != nil || state.Status != "drift" || strings.Contains(string(raw), "SENSITIVE_") {
		t.Fatal("configuration leaked through shared API", string(raw), err)
	}
	createDriftSnapshot(t, s, ctx, target, "interface Ethernet1\n shutdown\ninterface Ethernet1\n no shutdown\n", "unsupported")
	state, err = s.ConfigurationDrift(ctx, target.AssetID)
	if err != nil || state.Status != "unknown" || state.Latest.ErrorCode != "normalization_unsupported" {
		t.Fatal(state, err)
	}
}

func TestConfigurationDriftLiveRolesConfirmationAndRevision(t *testing.T) {
	s, ctx, admin, target := automationFixture(t)
	snapshot := createDriftSnapshot(t, s, ctx, target, driftApproved, "expected")
	in := cd.BaselineInput{AssetRevision: 1, SnapshotID: snapshot.SnapshotID, Confirmed: false, Reason: "reviewed baseline"}
	if _, _, err := s.PutConfigurationBaseline(ctx, target.AssetID, in, "unconfirmed"); !errors.Is(err, cd.ErrInvalid) {
		t.Fatal("unconfirmed baseline approved", err)
	}
	in.Confirmed = true
	in.AssetRevision = 2
	if _, _, err := s.PutConfigurationBaseline(ctx, target.AssetID, in, "stale-asset"); !errors.Is(err, cd.ErrConflict) {
		t.Fatal("stale Asset approved", err)
	}
	baseline := approveDriftBaseline(t, s, ctx, target, snapshot)
	if _, err := s.pool.Exec(ctx, `UPDATE access_users SET role='operator' WHERE id=$1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	in.AssetRevision = 1
	in.ExpectedRevision = baseline.Revision
	if _, _, err := s.PutConfigurationBaseline(ctx, target.AssetID, in, "operator-approve"); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("cached admin bypasses actual operator role", err)
	}
	compare := cd.CompareInput{BaselineRevision: baseline.Revision, SnapshotID: snapshot.SnapshotID, Reason: "operator comparison"}
	if _, err := s.CompareConfiguration(ctx, target.AssetID, compare); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE access_users SET role='viewer' WHERE id=$1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompareConfiguration(ctx, target.AssetID, compare); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("viewer wrote comparison", err)
	}
}

func TestConfigurationDriftVoIPContextIsRelatedNotProvenImpact(t *testing.T) {
	s, ctx, items, _, input := assetVoIPFixture(t)
	association, _, err := s.SaveAssetVoIP(ctx, items[0].ID, "", input, "reviewed-voice-service")
	if err != nil {
		t.Fatal(err)
	}
	vault, _ := datasources.NewVault(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{31}, 32)))
	if err = s.ConfigureAutomation(ctx, true, vault); err != nil {
		t.Fatal(err)
	}
	_, _, _, example := automationFixture(t)
	definition := example.TargetInput
	definition.ExpectedRevision = 0
	definition.AssetRevision = items[0].Revision
	target, err := s.PutAutomationTarget(ctx, items[0].ID, definition)
	if err != nil {
		t.Fatal(err)
	}
	expected := createDriftSnapshot(t, s, ctx, target, driftApproved, "expected")
	approveDriftBaseline(t, s, ctx, target, expected)
	createDriftSnapshot(t, s, ctx, target, strings.Replace(driftApproved, "vlan 20", "vlan 30", 1), "changed-vlan")
	state, err := s.ConfigurationDrift(ctx, target.AssetID)
	if err != nil || state.Status != "drift" || len(state.Latest.RelatedServices) != 1 {
		t.Fatal(state, err)
	}
	service := state.Latest.RelatedServices[0]
	if service.AssociationID != association.ID || service.Relation != "reviewed_asset_association_not_proven_port_impact" {
		t.Fatal("claims unsupported VoIP impact", service)
	}
	raw, _ := json.Marshal(state)
	if strings.Contains(string(raw), input.Address) || strings.Contains(string(raw), input.Name) {
		t.Fatal("endpoint or arbitrary service name leaked into diff")
	}
}
