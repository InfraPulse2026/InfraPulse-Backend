package storage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
)

func sourceQueueFixture(t *testing.T) (*Store, context.Context, datasources.Source) {
	t.Helper()
	s := integrationStore(t)
	_, admin := adminFixture(t, s)
	ctx := access.WithPrincipal(t.Context(), admin)
	noop := func(context.Context, datasources.Request) (datasources.Result, error) {
		return datasources.Result{Cursor: json.RawMessage(`{"position":1}`)}, nil
	}
	r, err := datasources.NewRegistry(datasources.Operation{Kind: "ZABBIX", Name: "sync", ReadOnly: true, Run: noop}, datasources.Operation{Kind: "ZABBIX", Name: "test", ReadOnly: true, Run: noop})
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureDataSourceOperations(r)
	v, err := s.SaveDataSource(ctx, "", datasources.Input{Type: "ZABBIX", Name: "fixture", Endpoint: "https://fixture.invalid", Enabled: true, SyncIntervalSeconds: 3})
	if err != nil {
		t.Fatal(err)
	}
	return s, ctx, v
}

func claimSourceJob(t *testing.T, s *Store, ctx context.Context) *datasources.Request {
	t.Helper()
	request, err := s.ClaimDataSourceJob(ctx)
	if err != nil || request == nil {
		t.Fatal("claim", request, err)
	}
	return request
}
func readySourceRetry(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `UPDATE data_source_jobs SET next_attempt_at=now()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
}

func TestDataSourceQueueIdempotencyRetryCursorAndFence(t *testing.T) {
	s, ctx, v := sourceQueueFixture(t)
	j, replay, err := s.QueueDataSourceJob(ctx, v.ID, "sync", "manual-sync-1")
	if err != nil || replay {
		t.Fatal(j, replay, err)
	}
	prior, replay, err := s.QueueDataSourceJob(ctx, v.ID, "sync", "manual-sync-1")
	if err != nil || !replay || prior.ID != j.ID {
		t.Fatal("idempotency", prior, replay, err)
	}
	if _, _, err = s.QueueDataSourceJob(ctx, v.ID, "test", "other-job"); err != datasources.ErrBusy {
		t.Fatal("source overlap", err)
	}
	r := claimSourceJob(t, s, ctx)
	if another, e := s.ClaimDataSourceJob(ctx); e != nil || another != nil {
		t.Fatal("duplicate worker claim", e)
	}
	if err = s.FinishDataSourceJob(ctx, *r, datasources.Result{Cursor: json.RawMessage(`{"position":999}`)}, datasources.Failure{Code: "connection_failed", Retryable: true}); err != nil {
		t.Fatal(err)
	}
	var cursor string
	if err = s.pool.QueryRow(ctx, `SELECT sync_cursor::text FROM data_sources WHERE id=$1`, v.ID).Scan(&cursor); err != nil || cursor != "{}" {
		t.Fatal("failed attempt advanced cursor", cursor, err)
	}
	readySourceRetry(t, s, j.ID)
	retry := claimSourceJob(t, s, ctx)
	if retry.Job.Attempts != 2 || retry.Job.LeaseToken == r.Job.LeaseToken {
		t.Fatal("retry lost attempt fence")
	}
	if err = s.FinishDataSourceJob(ctx, *r, datasources.Result{}, nil); err != datasources.ErrLease {
		t.Fatal("stale attempt committed", err)
	}
	if err = s.FinishDataSourceJob(ctx, *retry, datasources.Result{Cursor: json.RawMessage(`{"position":2}`)}, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishDataSourceJob(ctx, *retry, datasources.Result{}, nil); err != datasources.ErrLease {
		t.Fatal("duplicate completion accepted", err)
	}
	v, err = s.DataSource(ctx, v.ID)
	if err != nil || v.LastSuccessfulSync == nil {
		t.Fatal("success not recorded", v, err)
	}
	j, err = s.DataSourceJob(ctx, v.ID, j.ID)
	if err != nil || j.State != "succeeded" || j.Attempts != 2 {
		t.Fatal(j, err)
	}
	events, err := s.DataSourceJobEvents(ctx, v.ID, j.ID)
	if err != nil || len(events) != 5 {
		t.Fatal("attempt history", events, err)
	}
	if _, err = s.DataSourceJob(ctx, "another-source", j.ID); err != datasources.ErrNotFound {
		t.Fatal("history crossed source boundary", err)
	}
}

func TestDataSourceQueueConfigurationRevocationAndUnsupported(t *testing.T) {
	s, ctx, v := sourceQueueFixture(t)
	_, _, err := s.QueueDataSourceJob(ctx, v.ID, "test", "old-config")
	if err != nil {
		t.Fatal(err)
	}
	r := claimSourceJob(t, s, ctx)
	_, err = s.SaveDataSource(ctx, v.ID, datasources.Input{Type: v.Type, Name: "updated", Endpoint: v.Endpoint, Enabled: true, SyncIntervalSeconds: 10, Revision: v.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishDataSourceJob(ctx, *r, datasources.Result{Cursor: json.RawMessage(`{"old":true}`)}, nil); err != nil {
		t.Fatal(err)
	}
	j, err := s.DataSourceJob(ctx, v.ID, r.Job.ID)
	if err != nil || j.State != "cancelled" || j.Code != "configuration_changed" {
		t.Fatal("obsolete completion applied", j, err)
	}
	v, err = s.DataSource(ctx, v.ID)
	if err != nil || v.ConnectionStatus != "unverified" {
		t.Fatal("old result overwrote new status", v, err)
	}
	collector, err := s.SaveDataSource(ctx, "", datasources.Input{Type: "COLLECTOR", Name: "collector", ConnectionRef: "agent-1", Enabled: true, SyncIntervalSeconds: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.QueueDataSourceJob(ctx, collector.ID, "sync", "unsupported"); err != datasources.ErrUnsupported {
		t.Fatal("Collector sync invented", err)
	}
	job, _, err := s.QueueDataSourceJob(ctx, v.ID, "sync", "revoked-owner")
	if err != nil {
		t.Fatal(err)
	}
	actor, _ := access.Current(ctx)
	if err = s.AccessLogout(ctx, actor, false, access.Meta{}); err != nil {
		t.Fatal(err)
	}
	if request, e := s.ClaimDataSourceJob(ctx); e != nil || request != nil {
		t.Fatal("revoked queued job executed", e)
	}
	job, err = s.DataSourceJob(ctx, v.ID, job.ID)
	if err != nil || job.Code != "actor_revoked" || job.State != "cancelled" {
		t.Fatal(job, err)
	}
}

func TestDataSourceScheduleConcurrentAndExpiredLease(t *testing.T) {
	s, ctx, v := sourceQueueFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.ScheduleDataSources(ctx) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := s.DataSourceJobs(ctx, v.ID, 50, 0)
	if err != nil || len(jobs) != 1 || jobs[0].Trigger != "schedule" {
		t.Fatal("duplicate schedule", jobs, err)
	}
	r := claimSourceJob(t, s, ctx)
	if _, err = s.pool.Exec(ctx, `UPDATE data_source_jobs SET lease_until=now()-interval '1 second' WHERE id=$1`, r.Job.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishDataSourceJob(ctx, *r, datasources.Result{}, nil); err != datasources.ErrLease {
		t.Fatal("expired lease completed", err)
	}
	if recovered, e := s.ClaimDataSourceJob(ctx); e != nil || recovered != nil {
		t.Fatal("expired task not recovered", e)
	}
	readySourceRetry(t, s, r.Job.ID)
	next := claimSourceJob(t, s, ctx)
	if next.Job.Attempts != 2 || next.Job.LeaseToken == r.Job.LeaseToken {
		t.Fatal("restart replay not fenced")
	}
	if err = s.FinishDataSourceJob(ctx, *next, datasources.Result{}, errors.New("private-credential-response")); err != nil {
		t.Fatal(err)
	}
	v, err = s.DataSource(ctx, v.ID)
	if err != nil || v.LastError != "adapter_error" {
		t.Fatal("raw error leaked", v.LastError, err)
	}
	var delayed bool
	if err = s.pool.QueryRow(ctx, `SELECT next_sync_at>now()+interval '20 seconds' FROM data_sources WHERE id=$1`, v.ID).Scan(&delayed); err != nil || !delayed {
		t.Fatal("failed source has no backoff", err)
	}
}

func TestDataSourceWorkerCollectorAndShutdown(t *testing.T) {
	s := integrationStore(t)
	_, admin := adminFixture(t, s)
	ctx := access.WithPrincipal(t.Context(), admin)
	r := s.CollectorOperations()
	s.ConfigureDataSourceOperations(r)
	v, err := s.SaveDataSource(ctx, "", datasources.Input{Type: "COLLECTOR", Name: "collector", ConnectionRef: "agent-1", Enabled: true, SyncIntervalSeconds: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Ingest(ctx, registration("agent-1", time.Now())); err != nil {
		t.Fatal(err)
	}
	j, _, err := s.QueueDataSourceJob(ctx, v.ID, "test", "collector-live-test")
	if err != nil {
		t.Fatal(err)
	}
	worker, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		datasources.Run(worker, s, r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	t.Cleanup(func() {
		stop()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("source workers did not stop")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, e := s.DataSourceJob(ctx, v.ID, j.ID)
		if e != nil {
			t.Fatal(e)
		}
		if job.State == "succeeded" {
			v, e = s.DataSource(ctx, v.ID)
			if e != nil || v.LastSuccessfulSync != nil {
				t.Fatal("Collector test invented sync time", e)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("worker did not execute Collector test")
}

func TestDataSourceQueueExpiredLeaseRetryLimitAndMigration(t *testing.T) {
	s, ctx, v := sourceQueueFixture(t)
	// Simulate an already populated B2.1 database, only inside this disposable schema.
	if _, err := s.pool.Exec(ctx, `DROP TABLE data_source_job_events; DROP TABLE data_source_jobs; ALTER TABLE data_sources DROP COLUMN next_sync_at,DROP COLUMN sync_cursor,DROP COLUMN sync_failures; DELETE FROM schema_migrations WHERE version=19`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("upgrade 019", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("repeat 019", err)
	}
	if preserved, err := s.DataSource(ctx, v.ID); err != nil || preserved.Revision != v.Revision || preserved.Name != v.Name {
		t.Fatal("upgrade lost source", preserved, err)
	}
	j, _, err := s.QueueDataSourceJob(ctx, v.ID, "sync", "repeated-crash")
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		r := claimSourceJob(t, s, ctx)
		if r.Job.Attempts != attempt {
			t.Fatal("attempt mismatch", r.Job.Attempts)
		}
		if _, err = s.pool.Exec(ctx, `UPDATE data_source_jobs SET lease_until=now()-interval '1 second' WHERE id=$1`, j.ID); err != nil {
			t.Fatal(err)
		}
		if recovery, e := s.ClaimDataSourceJob(ctx); e != nil || recovery != nil {
			t.Fatal("crash recovery", e)
		}
		if err = s.FinishDataSourceJob(ctx, *r, datasources.Result{Cursor: json.RawMessage(`{"stale":true}`)}, nil); err != datasources.ErrLease {
			t.Fatal("crashed worker committed", err)
		}
		if attempt < 3 {
			readySourceRetry(t, s, j.ID)
		}
	}
	j, err = s.DataSourceJob(ctx, v.ID, j.ID)
	if err != nil || j.State != "failed" || j.Attempts != 3 || j.Code != "interrupted" {
		t.Fatal("unbounded retry", j, err)
	}
	var cursor string
	var delayed bool
	if err = s.pool.QueryRow(ctx, `SELECT sync_cursor::text,next_sync_at>now()+interval '20 seconds' FROM data_sources WHERE id=$1`, v.ID).Scan(&cursor, &delayed); err != nil || cursor != "{}" || !delayed {
		t.Fatal("crash advanced cursor or no source backoff", cursor, delayed, err)
	}
}

func TestDataSourceQueueTimeoutAndDisabledCancellation(t *testing.T) {
	s, ctx, v := sourceQueueFixture(t)
	j, _, err := s.QueueDataSourceJob(ctx, v.ID, "sync", "timeout-limit")
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		r := claimSourceJob(t, s, ctx)
		if err = s.FinishDataSourceJob(ctx, *r, datasources.Result{}, context.DeadlineExceeded); err != nil {
			t.Fatal(err)
		}
		if attempt < 3 {
			readySourceRetry(t, s, j.ID)
		}
	}
	j, err = s.DataSourceJob(ctx, v.ID, j.ID)
	if err != nil || j.State != "failed" || j.Code != "timeout" {
		t.Fatal(j, err)
	}
	j, _, err = s.QueueDataSourceJob(ctx, v.ID, "test", "disabled-after-queue")
	if err != nil {
		t.Fatal(err)
	}
	v, err = s.SaveDataSource(ctx, v.ID, datasources.Input{Type: v.Type, Name: v.Name, Endpoint: v.Endpoint, Enabled: false, SyncIntervalSeconds: 3, Revision: v.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if request, e := s.ClaimDataSourceJob(ctx); e != nil || request != nil {
		t.Fatal("disabled job dispatched", e)
	}
	j, err = s.DataSourceJob(ctx, v.ID, j.ID)
	if err != nil || j.State != "cancelled" || j.Code != "source_disabled" {
		t.Fatal(j, err)
	}
	if _, _, err = s.QueueDataSourceJob(ctx, v.ID, "sync", "new-disabled-task"); err != datasources.ErrDisabled {
		t.Fatal("disabled source queued", err)
	}
}
