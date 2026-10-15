package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// An external test harness restarts only the disposable PostgreSQL instance
// after the barrier appears. This test never launches or stops a database.
func TestDatabaseProcessRestartPreservesReplayAndPoolRecovery(t *testing.T) {
	barrier := os.Getenv("INFRAPULSE_RESTART_BARRIER")
	if barrier == "" {
		t.Skip("requires isolated PostgreSQL restart harness and INFRAPULSE_RESTART_BARRIER")
	}
	if !filepath.IsAbs(barrier) {
		t.Fatal("restart barrier must be absolute")
	}
	s := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Second)
	// The fixture's original admin connection is also broken by a real restart.
	// Reconnect for cleanup; target only this exact fixture-owned schema.
	schema := s.pool.Config().ConnConfig.RuntimeParams["search_path"]
	if !strings.HasPrefix(schema, "infrapulse_test_") {
		t.Fatal("unexpected restart fixture schema")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := pgx.Connect(cleanup, os.Getenv("TEST_DATABASE_URL"))
		if err != nil {
			t.Errorf("restart schema cleanup connection: %v", err)
			return
		}
		defer conn.Close(context.Background())
		if _, err = conn.Exec(cleanup, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Errorf("restart schema cleanup: %v", err)
		}
	})
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	original := sampleEnvelope(now)
	if err := s.Ingest(ctx, original); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(barrier, []byte("ready\n"), 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(barrier)
	deadline := time.Now().Add(3 * time.Minute)
	disconnected := false
	recovered := false
	for time.Now().Before(deadline) {
		bounded, cancel := context.WithTimeout(ctx, time.Second)
		err := s.Ping(bounded)
		cancel()
		if err != nil {
			disconnected = true
		} else if disconnected {
			recovered = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !disconnected || !recovered {
		t.Fatal("database disconnect/reconnect not observed; harness must restart the isolated instance")
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("migration after database restart", err)
	}
	if err := s.Ingest(ctx, original); err != nil {
		t.Fatal("replay after database restart", err)
	}
	messageCounts(t, s, original.MessageId, 1, 1)
	next := sampleEnvelope(now.Add(time.Second))
	next.MessageId = "after-database-restart"
	next.Sequence = 3
	if err := s.Ingest(ctx, next); err != nil {
		t.Fatal("new write after database restart", err)
	}
	items, err := s.Metrics(ctx, "agent-1", "shared-router", 10)
	if err != nil || len(items) != 2 {
		t.Fatal("history after database restart", items, err)
	}
}
