package storage

import (
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"testing"
	"time"
)

func TestAuditFailureNoSeverityReplayEvidenceReviewAndSuccess(t *testing.T) {
	s, ctx, _, bindings, _ := assetDecisionFixture(t)
	agent := *bindings[0].LegacyAgentID
	now := time.Now().UTC().Add(-time.Second)
	e := assetLogsEnvelope(agent, "session-1", "audit-failure", "system", "linux-file", now)
	r := e.GetLogs().Records[0]
	r.Id = "failed-without-severity"
	r.Severity = nil
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	v, err := s.Incidents(ctx, health.Filter{ResourceKind: "log", AgentID: agent})
	if err != nil || len(v) != 1 {
		t.Fatal(v, err)
	}
	first := v[0]
	if first.RuleID != "audit.reported_failure" || first.Severity != "warning" || first.Signal.Log.Severity != nil || first.Signal.Log.AuditSuccess == nil || *first.Signal.Log.AuditSuccess {
		t.Fatal("fabricated severity/outcome", first)
	}
	proof, err := s.LogIncidentRecords(ctx, first.ID, 100, 0)
	if err != nil || len(proof) != 1 || proof[0].Severity != nil {
		t.Fatal("nullable proof", proof, err)
	}
	if err = s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	yes := true
	good := assetLogsEnvelope(agent, "session-1", "audit-success", "system", "linux-file", now)
	good.GetLogs().Records[0].Id = "successful"
	good.GetLogs().Records[0].Audit.Success = &yes
	if err = s.Ingest(ctx, good); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	v, err = s.Incidents(ctx, health.Filter{ResourceKind: "log", AgentID: agent})
	if err != nil || len(v) != 1 || v[0].Signal.Log.EventCount != 1 || v[0].State != "open" {
		t.Fatal("success/replay recovered failure", v, err)
	}
	unknown := assetLogsEnvelope(agent, "session-1", "audit-unknown", "system", "linux-file", now)
	unknown.GetLogs().Records[0].Id = "unknown-outcome"
	unknown.GetLogs().Records[0].Audit.Success = nil
	if err = s.Ingest(ctx, unknown); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	second := assetLogsEnvelope(agent, "session-1", "audit-second", "system", "linux-file", now)
	second.GetLogs().Records[0].Id = "second-failure"
	sev := uint32(6)
	second.GetLogs().Records[0].Severity = &sev
	if err = s.Ingest(ctx, second); err != nil {
		t.Fatal(err)
	}
	review := health.LogReview{Confirmed: true, ExpectedEventCount: 1, Reason: "Checked failed audit operation"}
	if _, err = s.ReviewLogIncident(ctx, first.ID, review); !errors.Is(err, health.ErrConflict) {
		t.Fatal("pending audit closed unseen", err)
	}
	if _, err = s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	proof, err = s.LogIncidentRecords(ctx, first.ID, 100, 0)
	if err != nil || len(proof) != 2 || proof[1].Severity == nil || *proof[1].Severity != 6 {
		t.Fatal("original severity changed", proof, err)
	}
	review.ExpectedEventCount = 2
	reviewed, err := s.ReviewLogIncident(ctx, first.ID, review)
	if err != nil || reviewed.State != "resolved" || reviewed.Signal.RecoveryEligible {
		t.Fatal(reviewed, err)
	}
}
