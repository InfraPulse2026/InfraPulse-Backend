package storage

import (
	"encoding/json"
	cd "github.com/InfraPulse2026/InfraPulse-Backend/internal/configdrift"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"strings"
	"testing"
	"time"
)

func TestConfigurationIncidentDurableLatestReplayRecoveryAndOccurrence(t *testing.T) {
	s, ctx, _, target := automationFixture(t)
	expected := createDriftSnapshot(t, s, ctx, target, driftApproved, "expected")
	baseline := approveDriftBaseline(t, s, ctx, target, expected)
	changed := createDriftSnapshot(t, s, ctx, target, strings.Replace(driftApproved, "vlan 20", "vlan 30", 1), "changed")
	if _, err := s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	list := func() []health.Incident {
		t.Helper()
		v, err := s.Incidents(ctx, health.Filter{ResourceKind: "configuration"})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	incidents := list()
	if len(incidents) != 1 || incidents[0].ResourceID != target.AssetID || incidents[0].RuleID != "configuration.approved_drift" || incidents[0].AgentID != "" || incidents[0].Signal.Configuration.SnapshotID != changed.SnapshotID || incidents[0].EvidenceStatus != "fresh" {
		t.Fatal(incidents)
	}
	first := incidents[0]
	raw, _ := json.Marshal(first)
	if strings.Contains(string(raw), "description Voice") || strings.Contains(string(raw), "switch_readonly") || strings.Contains(string(raw), "192.0.2.20") {
		t.Fatal("configuration/target leaked", string(raw))
	}
	var category, quality, asset string
	if err := s.pool.QueryRow(ctx, `SELECT n.category,n.time_quality,a.asset_id FROM normalized_signals n JOIN normalized_signal_assets a ON a.signal_id=n.id WHERE n.incident_id=$1`, first.ID).Scan(&category, &quality, &asset); err != nil || category != "configuration" || quality != "estimated" || asset != target.AssetID {
		t.Fatal(category, quality, asset, err)
	}
	if _, err := s.CompareConfiguration(ctx, target.AssetID, cd.CompareInput{BaselineRevision: baseline.Revision, SnapshotID: expected.SnapshotID, Reason: "historical comparison"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	if list()[0].State != "open" || list()[0].Signal.Configuration.SnapshotID != changed.SnapshotID {
		t.Fatal("historical compliant recovered latest drift")
	}
	var evidence int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM incident_evidence WHERE incident_id=$1`, first.ID).Scan(&evidence); err != nil || evidence != 1 {
		t.Fatal("replay duplicated evidence", evidence, err)
	}
	recovered := createDriftSnapshot(t, s, ctx, target, driftApproved, "recovered")
	if _, err := s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	incidents = list()
	if len(incidents) != 1 || incidents[0].State != "resolved" || incidents[0].Signal.Configuration.SnapshotID != recovered.SnapshotID || !incidents[0].Signal.RecoveryEligible {
		t.Fatal("explicit compliance did not recover", incidents)
	}
	createDriftSnapshot(t, s, ctx, target, strings.Replace(driftApproved, "vlan 20", "vlan 40", 1), "changed-again")
	if _, err := s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	incidents = list()
	if len(incidents) != 2 {
		t.Fatal("new occurrence missing", incidents)
	}
	var open health.Incident
	for _, v := range incidents {
		if v.State == "open" {
			open = v
		}
	}
	if open.Occurrence != 2 {
		t.Fatal(open)
	}
	stale := []health.Incident{open}
	if err := s.configurationIncidentFreshness(ctx, s.pool, stale, time.Now().Add(16*time.Minute)); err != nil || stale[0].State != "open" || stale[0].EvidenceStatus != "stale" {
		t.Fatal("silence recovered drift", stale, err)
	}
}

func TestConfigurationIncidentUnknownBaselineChangeAndTargetDisable(t *testing.T) {
	for _, scenario := range []string{"unknown", "baseline", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			s, ctx, _, target := automationFixture(t)
			expected := createDriftSnapshot(t, s, ctx, target, driftApproved, "expected")
			approveDriftBaseline(t, s, ctx, target, expected)
			changed := createDriftSnapshot(t, s, ctx, target, strings.Replace(driftApproved, "vlan 20", "vlan 30", 1), "changed")
			if _, err := s.ProcessCorrelation(ctx); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "unknown":
				createDriftSnapshot(t, s, ctx, target, "interface Ethernet1\n shutdown\ninterface Ethernet1\n no shutdown\n", "unsupported")
			case "baseline":
				_, _, err := s.PutConfigurationBaseline(ctx, target.AssetID, cd.BaselineInput{ExpectedRevision: 1, AssetRevision: target.AssetRevision, SnapshotID: changed.SnapshotID, Confirmed: true, Reason: "approved a new baseline"}, "new-baseline")
				if err != nil {
					t.Fatal(err)
				}
			case "disabled":
				input := target.TargetInput
				input.Enabled = false
				input.ExpectedRevision = target.Revision
				if _, err := s.PutAutomationTarget(ctx, target.AssetID, input); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ProcessCorrelation(ctx); err != nil {
				t.Fatal(err)
			}
			v, err := s.Incidents(ctx, health.Filter{ResourceKind: "configuration"})
			if err != nil || len(v) != 1 || v[0].State != "open" || v[0].EvidenceStatus != "stale" {
				t.Fatal("uncertain evidence reported recovery", v, err)
			}
		})
	}
}

func TestConfigurationIncidentWorkerRollbackPreservesComparison(t *testing.T) {
	s, ctx, _, target := automationFixture(t)
	expected := createDriftSnapshot(t, s, ctx, target, driftApproved, "expected")
	approveDriftBaseline(t, s, ctx, target, expected)
	createDriftSnapshot(t, s, ctx, target, strings.Replace(driftApproved, "vlan 20", "vlan 30", 1), "changed")
	if _, err := s.pool.Exec(ctx, `ALTER TABLE configuration_signal_state ADD CONSTRAINT reject_signal CHECK(result_id<0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessConfigurationSignals(ctx); err == nil {
		t.Fatal("expected worker failure")
	}
	var pending, incidents, results int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM configuration_signal_pending),(SELECT count(*) FROM incidents WHERE resource_kind='configuration'),(SELECT count(*) FROM configuration_drift_results)`).Scan(&pending, &incidents, &results); err != nil || pending != 1 || incidents != 0 || results != 2 {
		t.Fatal(pending, incidents, results, err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE configuration_signal_state DROP CONSTRAINT reject_signal`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
}
