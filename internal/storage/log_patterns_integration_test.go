package storage

import (
	"encoding/json"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"strings"
	"testing"
	"time"
)

func TestLogPatternsNoSeverityEvidenceReplayReviewAndIsolation(t *testing.T) {
	s, ctx, _, bindings, _ := assetDecisionFixture(t)
	now := time.Now().UTC().Add(-time.Second)
	agent := *bindings[0].LegacyAgentID
	emit := func(message, process, text string) {
		t.Helper()
		e := assetLogsEnvelope(agent, "session-1", message, "system", "syslog", now)
		r := e.GetLogs().Records[0]
		r.Id = message
		r.Process = process
		r.Message = text
		r.Severity = nil
		r.Audit = nil
		if err := s.Ingest(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	oom := "Out of memory: Killed process 123 (private-worker) total-vm:1000kB, anon-rss:500kB, file-rss:20kB, shmem-rss:0kB, UID:1000 pgtables:8kB oom_score_adj:0"
	emit("oom", "kernel", oom)
	emit("ssh", "sshd", "Failed password for private-user from 192.0.2.10 port 2222 ssh2")
	emit("wrong-process", "application", oom)
	if _, err := s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	v, err := s.Incidents(ctx, health.Filter{ResourceKind: "log"})
	if err != nil || len(v) != 2 {
		t.Fatal(v, err)
	}
	var id string
	for _, incident := range v {
		if incident.Signal.Log.PatternID != incident.RuleID || incident.Signal.Log.Severity != nil {
			t.Fatal("fabricated original severity", incident)
		}
		if incident.RuleID == "log.kernel_oom_kill" {
			id = incident.ID
			if incident.Severity != "critical" {
				t.Fatal(incident)
			}
		}
		raw, _ := json.Marshal(incident)
		if strings.Contains(string(raw), "private-user") || strings.Contains(string(raw), "private-worker") || strings.Contains(string(raw), "192.0.2.10") {
			t.Fatal("text leaked", string(raw))
		}
	}
	emit("oom", "kernel", oom)
	if _, err = s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	proof, err := s.LogIncidentRecords(ctx, id, 100, 0)
	if err != nil || len(proof) != 1 {
		t.Fatal("pattern replay duplicated", proof, err)
	}
	reviewed, err := s.ReviewLogIncident(ctx, id, health.LogReview{Confirmed: true, ExpectedEventCount: 1, Reason: "Reviewed reported kernel event"})
	if err != nil || reviewed.State != "resolved" || reviewed.Signal.RecoveryEligible {
		t.Fatal(reviewed, err)
	}
}
