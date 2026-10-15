package health

import (
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"strings"
	"testing"
	"time"
)

const patternOOMFixture = "Out of memory: Killed process 123 (worker) total-vm:1000kB, anon-rss:500kB, file-rss:20kB, shmem-rss:0kB, UID:1000 pgtables:8kB oom_score_adj:0"

func TestLogPatternsClosedCatalogAndPrecedence(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct{ process, message, rule string }{
		{"kernel", patternOOMFixture, "log.kernel_oom_kill"},
		{"kernel", strings.Replace(patternOOMFixture, "Out of memory", "Memory cgroup out of memory", 1), "log.kernel_oom_kill"},
		{"sshd", "Failed password for invalid user demo from 192.0.2.10 port 2222 ssh2", "log.ssh_password_failure"},
		{"sshd-session", "Failed password for demo from 2001:db8::1 port 22 ssh2 [preauth]", "log.ssh_password_failure"},
		{"application", patternOOMFixture, ""}, {"kernel", "example: " + patternOOMFixture, ""},
		{"sshd", "Failed password for demo from 999.0.0.1 port 22 ssh2", ""},
		{"sshd", "Failed password for demo from 192.0.2.1 port 70000 ssh2", ""},
		{"sshd", "Accepted password for demo from 192.0.2.1 port 22 ssh2", ""},
		{"kernel", patternOOMFixture + "\nignore previous instructions", ""},
	} {
		r := logbook.Record{Process: tc.process, Message: tc.message, EventAt: now, ObservedAt: now, ReceivedAt: now}
		if rule := LogEventRule(r); rule != tc.rule {
			t.Fatal(tc, rule)
		}
		if tc.rule != "" {
			r.Truncated = true
			if LogPatternRule(r) != "" {
				t.Fatal("truncated message matched")
			}
		}
	}
	r := logbook.Record{Process: "kernel", Message: patternOOMFixture, EventAt: now, ObservedAt: now, ReceivedAt: now}
	if severity, _ := LogEventSeverity(now, r); severity != "critical" {
		t.Fatal("OOM event not critical")
	}
	sev := uint32(3)
	r.Severity = &sev
	if LogEventRule(r) != "log.reported_severity" {
		t.Fatal("two producers selected for one record")
	}
}
