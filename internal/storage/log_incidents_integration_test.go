package storage

import (
	"encoding/json"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"strings"
	"testing"
	"time"
)

func TestLogIncidentGroupingReviewPendingReplayAndIndependentRecovery(t *testing.T) {
	s, ctx, _, bindings, _ := assetDecisionFixture(t)
	s.defaults.TelemetryIntervalSeconds = 30
	now := time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)
	agent := *bindings[0].LegacyAgentID
	emit := func(message, record string, sev uint32, device string) {
		t.Helper()
		e := assetLogsEnvelope(agent, "session-1", message, device, "syslog", now)
		r := e.GetLogs().Records[0]
		r.Id = record
		r.Severity = &sev
		if sev > 3 {
			r.Audit = nil
		}
		if err := s.Ingest(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	process := func() {
		t.Helper()
		if _, err := s.ProcessCorrelation(ctx); err != nil {
			t.Fatal(err)
		}
	}
	list := func(state string) []health.Incident {
		t.Helper()
		v, err := s.Incidents(ctx, health.Filter{ResourceKind: "log", AgentID: agent, State: state})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	emit("first", "log-1", 3, "system")
	process()
	logs := list("open")
	if len(logs) != 1 || logs[0].Signal.Log.EventCount != 1 || logs[0].Signal.Log.Lifecycle != "requires_review" {
		t.Fatal("reported event missing", logs)
	}
	id := logs[0].ID
	emit("second", "log-2", 0, "system")
	process()
	logs = list("open")
	if len(logs) != 1 || logs[0].Signal.Log.EventCount != 2 || logs[0].Severity != "critical" {
		t.Fatal("no grouping/escalation", logs)
	}
	emit("second", "log-2", 0, "system")
	process()
	if list("open")[0].Signal.Log.EventCount != 2 {
		t.Fatal("replay double-counted")
	}
	emit("unbound", "log-unknown", 0, "claimed-host-only")
	emit("informational", "log-info", 6, "system")
	process()
	if len(list("open")) != 1 {
		t.Fatal("unbound or informational logs created incident")
	}
	proof, err := s.LogIncidentRecords(ctx, id, 100, 0)
	if err != nil || len(proof) != 2 || proof[0].BindingID != bindings[0].ID {
		t.Fatal("evidence records", proof, err)
	}
	encoded, _ := json.Marshal(logs[0])
	if strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), `"raw"`) {
		t.Fatal("raw/secret copied to incident")
	}
	// A component problem in the same case remains open after log review.
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
	process()
	var caseID string
	if err = s.pool.QueryRow(ctx, `SELECT correlated_incident_id FROM correlated_incident_signals m JOIN normalized_signals n ON n.id=m.signal_id WHERE n.incident_id=$1`, id).Scan(&caseID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM correlated_incident_signals WHERE correlated_incident_id=$1`, caseID).Scan(&count); err != nil || count != 2 {
		t.Fatal("log/component correlation", count, err)
	}
	review := health.LogReview{Confirmed: true, ExpectedEventCount: 2, Reason: "Checked the reported event with operator"}
	emit("pending", "log-3", 3, "system")
	if _, err = s.ReviewLogIncident(ctx, id, review); !errors.Is(err, health.ErrConflict) {
		t.Fatal("pending new log closed unseen", err)
	}
	process()
	if _, err = s.ReviewLogIncident(ctx, id, review); !errors.Is(err, health.ErrConflict) {
		t.Fatal("stale count accepted", err)
	}
	review.ExpectedEventCount = 3
	v, err := s.ReviewLogIncident(ctx, id, review)
	if err != nil || v.State != "resolved" || v.Signal.RecoveryEligible || v.Signal.Log.Lifecycle != "reviewed_event" {
		t.Fatal("event review", v, err)
	}
	if _, err = s.ReviewLogIncident(ctx, id, review); err != nil {
		t.Fatal("review replay", err)
	}
	process()
	var state string
	if err = s.pool.QueryRow(ctx, `SELECT state FROM correlated_incidents WHERE id=$1`, caseID).Scan(&state); err != nil || state != "open" {
		t.Fatal("log review resolved component problem", state, err)
	}
	emit("after-review", "log-4", 3, "system")
	process()
	v = list("open")[0]
	if v.Occurrence != 2 || v.Signal.Log.EventCount != 1 {
		t.Fatal("new event reopened old reviewed records", v)
	}
	otherAgent := *bindings[1].LegacyAgentID
	e := assetLogsEnvelope(otherAgent, "session-1", "other-source", "system", "syslog", now)
	severity := uint32(3)
	e.GetLogs().Records[0].Severity = &severity
	if err = s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	process()
	foreign, err := s.Incidents(ctx, health.Filter{ResourceKind: "log", AgentID: otherAgent})
	if err != nil || len(foreign) != 1 || foreign[0].ResourceID == v.ResourceID {
		t.Fatal("other Collector mixed", foreign, err)
	}
	s.timeout = time.Hour
	stale := []health.Incident{v}
	if err = s.logIncidentFreshness(ctx, s.pool, stale, now.Add(6*time.Minute)); err != nil || stale[0].State != "open" || stale[0].EvidenceStatus != "stale" {
		t.Fatal("silence recovered event", stale, err)
	}
	s.timeout = 90 * time.Second
	var audits int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM access_audit WHERE action='log_event_reviewed'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("duplicate/missing review audit", audits, err)
	}
	actor, _ := access.Current(ctx)
	if _, err = s.pool.Exec(ctx, `UPDATE access_users SET role='viewer' WHERE id=$1`, actor.User.ID); err != nil {
		t.Fatal(err)
	}
	review.ExpectedEventCount = 1
	if _, err = s.ReviewLogIncident(ctx, v.ID, review); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("revoked role accepted", err)
	}
}

func TestLogIncidentWorkerFailurePreservesQueueAndOriginalRecord(t *testing.T) {
	s, ctx, _, bindings, _ := assetDecisionFixture(t)
	agent := *bindings[0].LegacyAgentID
	now := time.Now().UTC().Add(-time.Second)
	e := assetLogsEnvelope(agent, "session-1", "failure-log", "system", "syslog", now)
	sev := uint32(3)
	e.GetLogs().Records[0].Severity = &sev
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE agents SET connected=false WHERE agent_id=$1`, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE log_incident_records ADD CONSTRAINT fail_log_projection CHECK(log_id<0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessLogSignals(ctx); err == nil {
		t.Fatal("expected projection failure")
	}
	var pending, incidents, records int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM log_signal_pending),(SELECT count(*) FROM incidents WHERE resource_kind='log'),(SELECT count(*) FROM log_records)`).Scan(&pending, &incidents, &records); err != nil || pending != 1 || incidents != 0 || records != 1 {
		t.Fatal("partial/lost event", pending, incidents, records, err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE log_incident_records DROP CONSTRAINT fail_log_projection`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	v, err := s.Incidents(ctx, health.Filter{ResourceKind: "log"})
	if err != nil || len(v) != 1 || v[0].EvidenceStatus != "source_unavailable" {
		t.Fatal("retry lost incident", v, err)
	}
}
