package storage

import (
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

func TestCollectorUpgradeEnrichesLegacyFileRecord(t *testing.T) {
	s := integrationStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ctx := t.Context()
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	old := logEnvelope("legacy-file", now)
	if err := s.Ingest(ctx, old); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the populated Stage9F schema inside this disposable schema.
	// Apply 017 twice to prove upgrade/data preservation/idempotent startup.
	if _, err := s.pool.Exec(ctx, `ALTER TABLE log_records DROP COLUMN syslog_metadata, DROP COLUMN observation_metadata, DROP COLUMN raw_payload_omitted; DELETE FROM schema_migrations WHERE version=17`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("Stage9F upgrade", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("repeat upgrade", err)
	}
	upgraded := proto.Clone(old).(*pb.AgentEnvelope)
	upgraded.MessageId = "upgraded-file"
	upgraded.GetLogs().Records[0].Observation = metadata(now, "auditd")
	if err := s.Ingest(ctx, upgraded); err != nil {
		t.Fatal(err)
	}
	page, err := s.Logs(ctx, logbook.Filter{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].Observation == nil {
		t.Fatalf("legacy enrichment: %+v %v", page, err)
	}
	if page.Items[0].MessageID != "legacy-file" {
		t.Fatal("original row overwritten")
	}
	conflict := proto.Clone(upgraded).(*pb.AgentEnvelope)
	conflict.MessageId = "upgrade-conflict"
	conflict.GetLogs().Records[0].Message = "different business contents"
	if err = s.Ingest(ctx, conflict); err != ErrLogConflict {
		t.Fatalf("content conflict suppressed: %v", err)
	}
}
