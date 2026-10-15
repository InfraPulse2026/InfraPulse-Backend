package storage

import (
	"context"
	"testing"
)

func TestMigrationFailureRollbackRepairAndFutureSchemaFence(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	// Only this disposable schema is changed. Simulate a manually damaged
	// pre-045 database; the migrator itself commits all pending versions together.
	if _, err := s.pool.Exec(ctx, `DROP TABLE report_schedule_runs,report_schedule_events,report_schedules; DELETE FROM schema_migrations WHERE version=45; CREATE TABLE report_schedules(id text PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err == nil {
		t.Fatal("damaged migration unexpectedly succeeded")
	}
	var applied bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=45)`).Scan(&applied); err != nil || applied {
		t.Fatal("failed migration marker committed", err)
	}
	if _, err := s.pool.Exec(ctx, `DROP TABLE report_schedules`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("repair and retry", err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=45)`).Scan(&applied); err != nil || !applied {
		t.Fatal("repair not applied", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(46)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err == nil {
		t.Fatal("older binary accepted unknown schema")
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version=46`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("repeated migration", err)
	}
}
