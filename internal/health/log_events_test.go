package health

import (
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"testing"
	"time"
)

func TestLogSeverityTimeAndWindowIsolation(t *testing.T) {
	now := time.Now().UTC()
	sev := uint32(3)
	r := logbook.Record{Severity: &sev, EventAt: now, ObservedAt: now, ReceivedAt: now, TimeBasis: "observed_at", Source: "syslog", SourceID: "listener", Process: "kernel"}
	if status, q := LogEventSeverity(now, r); status != "warning" || q != "estimated" {
		t.Fatal(status, q)
	}
	if status, _ := LogEventSeverity(now.Add(10*time.Minute), r); status != "warning" {
		t.Fatal("durable event lost due to delayed worker")
	}
	sev = 0
	if status, _ := LogEventSeverity(now, r); status != "critical" {
		t.Fatal("zero severity lost")
	}
	sev = 4
	if status, _ := LogEventSeverity(now, r); status != "" {
		t.Fatal("warning syslog became incident")
	}
	sev = 3
	r.EventAt = now.Add(-301 * time.Second)
	if status, _ := LogEventSeverity(now, r); status != "" {
		t.Fatal("old original timestamp refreshed")
	}
	r.EventAt = now
	r.Observation = []byte(`{"time":{"clock_quality":"unsynced"}}`)
	if status, _ := LogEventSeverity(now, r); status != "" {
		t.Fatal("bad clock accepted")
	}
	r.Observation = []byte(`{"time":{"clock_quality":"synced"}}`)
	if _, q := LogEventSeverity(now, r); q != "exact" {
		t.Fatal(q)
	}
	k, _ := LogWindowKey("binding", "", r)
	foreign, _ := LogWindowKey("other-binding", "", r)
	sim, _ := LogWindowKey("binding", "simulation", r)
	if k == foreign || k == sim {
		t.Fatal("source/scope key collapsed")
	}
	r.Process = "other"
	other, _ := LogWindowKey("binding", "", r)
	if k == other {
		t.Fatal("different process mixed")
	}
}
