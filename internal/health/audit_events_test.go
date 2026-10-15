package health

import (
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"testing"
	"time"
)

func TestAuditFailureRequiresExplicitOutcomeAndFreshReceipt(t *testing.T) {
	now := time.Now().UTC()
	failed, success := false, true
	r := logbook.Record{EventAt: now, ObservedAt: now, ReceivedAt: now, Audit: &logbook.Audit{Success: &failed}}
	if severity, _ := LogEventSeverity(now, r); severity != "warning" || LogEventRule(r) != "audit.reported_failure" {
		t.Fatal("explicit failure ignored")
	}
	auditKey, _ := LogWindowKey("binding", "", r)
	r.Audit.Success = &success
	if severity, _ := LogEventSeverity(now, r); severity != "" {
		t.Fatal("successful audit became incident")
	}
	r.Audit.Success = nil
	if severity, _ := LogEventSeverity(now, r); severity != "" {
		t.Fatal("missing outcome became failure")
	}
	r.Audit.Success = &failed
	sev := uint32(0)
	r.Severity = &sev
	severeKey, _ := LogWindowKey("binding", "", r)
	if auditKey == severeKey || LogEventRule(r) != "log.reported_severity" {
		t.Fatal("audit and severe rules mixed")
	}
	r.Severity = nil
	r.EventAt = now.Add(-6 * time.Minute)
	if severity, _ := LogEventSeverity(now, r); severity != "" {
		t.Fatal("historical failure refreshed")
	}
}
