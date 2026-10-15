package storage

import (
	"encoding/json"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/analysisai"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"strings"
	"testing"
	"time"
)

func TestAIMultiSourcePreviewLocalReferencesAndConfirmedJob(t *testing.T) {
	s, ctx, _, bindings, _ := assetDecisionFixture(t)
	agent := *bindings[0].LegacyAgentID
	now := time.Now().UTC().Add(-time.Second)
	log := assetLogsEnvelope(agent, "session-1", "ai-log", "system", "syslog", now)
	severity := uint32(3)
	log.GetLogs().Records[0].Severity = &severity
	if err := s.Ingest(ctx, log); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a := health.Assessment{RuleID: "cpu.sustained", RuleVersion: "1", Status: "critical", Severity: "critical", ObservedAt: &now, Evidence: []health.Point{}}
	raw, _ := json.Marshal(a)
	if err = applyIncident(ctx, tx, agent, "system", "linux", "", a, raw); err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	var caseID string
	if err = s.pool.QueryRow(ctx, `SELECT id FROM correlated_incidents WHERE state='open'`).Scan(&caseID); err != nil {
		t.Fatal(err)
	}
	cfg := analysisai.Config{Enabled: true, Model: "fixture-model", APIKey: "not-real", Timeout: time.Second}
	preview, err := s.CreateCorrelatedPreview(ctx, caseID, cfg)
	if err != nil || preview.CorrelatedIncidentID == nil || *preview.CorrelatedIncidentID != caseID || len(preview.Payload.Facts) != 2 || len(preview.References) < 2 {
		t.Fatal(preview, err)
	}
	outgoing, _ := json.Marshal(preview.Request)
	for _, private := range []string{agent, caseID, "private-message", "not-real"} {
		if strings.Contains(string(outgoing), private) {
			t.Fatal("local proof leaked", string(outgoing))
		}
	}
	body := analysisai.Create{PreviewID: preview.ID, RequestHash: preview.Hash, Confirmed: true}
	if _, _, err = s.CreateCorrelatedJob(ctx, "foreign-case", body, "cross-case", cfg); !errors.Is(err, analysisai.ErrConflict) {
		t.Fatal("cross-case preview accepted", err)
	}
	job, replay, err := s.CreateCorrelatedJob(ctx, caseID, body, "confirmed-case", cfg)
	if err != nil || replay || job.CorrelatedIncidentID == nil || *job.CorrelatedIncidentID != caseID || job.PromptVersion != analysisai.PromptVersion {
		t.Fatal(job, err)
	}
	jobs, err := s.CorrelatedAnalysisJobs(ctx, caseID, 100)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatal(jobs, err)
	}
}
