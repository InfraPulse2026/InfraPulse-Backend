package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/externalmonitor"
	"github.com/jackc/pgx/v5"
)

// ConfigureDataSourceOperations must be called before workers and HTTP start.
func (s *Store) ConfigureDataSourceOperations(r *datasources.Registry) { s.sourceOperations = r }

func (s *Store) CollectorOperations() *datasources.Registry {
	r, _ := datasources.NewRegistry(datasources.Operation{Kind: "COLLECTOR", Name: "test", ReadOnly: true, Run: func(ctx context.Context, request datasources.Request) (datasources.Result, error) {
		v, err := s.DataSource(ctx, request.Source.ID)
		if err != nil {
			return datasources.Result{}, err
		}
		if v.ConnectionStatus != "connected" {
			return datasources.Result{}, datasources.Failure{Code: "collector_disconnected"}
		}
		return datasources.Result{}, nil
	}})
	return r
}

const sourceJobColumns = `id,source_id,operation,trigger,state,source_revision,attempts,code,created_at,started_at,finished_at,next_attempt_at,actor_id,lease_token`

func readSourceJob(row interface{ Scan(...any) error }) (datasources.Job, error) {
	var j datasources.Job
	var lease *string
	err := row.Scan(&j.ID, &j.SourceID, &j.Operation, &j.Trigger, &j.State, &j.SourceRevision, &j.Attempts, &j.Code, &j.CreatedAt, &j.StartedAt, &j.FinishedAt, &j.NextAttemptAt, &j.ActorID, &lease)
	if errors.Is(err, pgx.ErrNoRows) {
		return j, datasources.ErrNotFound
	}
	if lease != nil {
		j.LeaseToken = *lease
	}
	return j, err
}
func sourceJobID() (string, error) {
	id, err := access.NewID()
	return "job_" + strings.TrimPrefix(id, "usr_"), err
}
func sourceJobEvent(ctx context.Context, tx pgx.Tx, j datasources.Job, state, code string) error {
	_, err := tx.Exec(ctx, `INSERT INTO data_source_job_events(job_id,attempt,state,code) VALUES($1,$2,$3,$4)`, j.ID, j.Attempts, state, code)
	return err
}

func (s *Store) QueueDataSourceJob(ctx context.Context, id, operation, key string) (datasources.Job, bool, error) {
	var empty datasources.Job
	if (operation != "test" && operation != "sync") || commands.ValidateKey(key) != nil {
		return empty, false, datasources.ErrInvalid
	}
	actor, ok := access.Current(ctx)
	if !ok {
		return empty, false, access.ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return empty, false, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194002)`); err != nil {
		return empty, false, err
	}
	if err = accessAdminTx(ctx, tx, actor); err != nil {
		return empty, false, err
	}
	v, err := readSource(tx.QueryRow(ctx, `SELECT `+sourceColumns+` FROM data_sources WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return empty, false, err
	}
	prior, e := readSourceJob(tx.QueryRow(ctx, `SELECT `+sourceJobColumns+` FROM data_source_jobs WHERE source_id=$1 AND operation=$2 AND idempotency_key=$3`, id, operation, key))
	if e == nil {
		return prior, true, tx.Commit(ctx)
	}
	if e != datasources.ErrNotFound {
		return empty, false, e
	}
	if !v.Enabled {
		return empty, false, datasources.ErrDisabled
	}
	if _, ok = s.sourceOperations.Lookup(v.Type, operation); !ok {
		return empty, false, datasources.ErrUnsupported
	}
	var busy bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM data_source_jobs WHERE source_id=$1 AND state IN ('queued','running','retry_wait'))`, id).Scan(&busy); err != nil {
		return empty, false, err
	}
	if busy {
		return empty, false, datasources.ErrBusy
	}
	jobID, err := sourceJobID()
	if err != nil {
		return empty, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO data_source_jobs(id,source_id,operation,trigger,state,source_revision,idempotency_key,actor_id,actor_session_hash) VALUES($1,$2,$3,'manual','queued',$4,$5,$6,$7)`, jobID, id, operation, v.Revision, key, actor.User.ID, actor.SessionHash)
	if err != nil {
		return empty, false, err
	}
	j, err := readSourceJob(tx.QueryRow(ctx, `SELECT `+sourceJobColumns+` FROM data_source_jobs WHERE id=$1`, jobID))
	if err != nil {
		return empty, false, err
	}
	if err = sourceJobEvent(ctx, tx, j, "queued", ""); err != nil {
		return empty, false, err
	}
	meta := access.CurrentMeta(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,'data_source_job_queued','success',$2,$3,$4)`, actor.User.ID, jobID, meta.RequestID, meta.Peer)
	if err != nil {
		return empty, false, err
	}
	return j, false, tx.Commit(ctx)
}

func (s *Store) DataSourceJobs(ctx context.Context, id string, limit, offset int) ([]datasources.Job, error) {
	if limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, datasources.ErrInvalid
	}
	if _, err := s.DataSource(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+sourceJobColumns+` FROM data_source_jobs WHERE source_id=$1 ORDER BY created_at DESC,id LIMIT $2 OFFSET $3`, id, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []datasources.Job{}
	for rows.Next() {
		j, e := readSourceJob(rows)
		if e != nil {
			return nil, e
		}
		items = append(items, j)
	}
	return items, rows.Err()
}
func (s *Store) DataSourceJob(ctx context.Context, id, jobID string) (datasources.Job, error) {
	return readSourceJob(s.pool.QueryRow(ctx, `SELECT `+sourceJobColumns+` FROM data_source_jobs WHERE source_id=$1 AND id=$2`, id, jobID))
}
func (s *Store) DataSourceJobEvents(ctx context.Context, id, jobID string) ([]datasources.Event, error) {
	if _, err := s.DataSourceJob(ctx, id, jobID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id,job_id,attempt,state,code,at FROM data_source_job_events WHERE job_id=$1 ORDER BY id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []datasources.Event{}
	for rows.Next() {
		var e datasources.Event
		if err = rows.Scan(&e.ID, &e.JobID, &e.Attempt, &e.State, &e.Code, &e.At); err != nil {
			return nil, err
		}
		items = append(items, e)
	}
	return items, rows.Err()
}

func (s *Store) ScheduleDataSources(ctx context.Context) error {
	kinds := s.sourceOperations.SyncKinds()
	if len(kinds) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	rows, err := tx.Query(ctx, `SELECT id,revision FROM data_sources d WHERE enabled AND type=ANY($1::text[]) AND next_sync_at<=now() AND NOT EXISTS(SELECT 1 FROM data_source_jobs j WHERE j.source_id=d.id AND j.state IN ('queued','running','retry_wait')) ORDER BY next_sync_at,id LIMIT 50 FOR UPDATE SKIP LOCKED`, kinds)
	if err != nil {
		return err
	}
	type due struct {
		id       string
		revision int64
	}
	items := []due{}
	for rows.Next() {
		var d due
		if err = rows.Scan(&d.id, &d.revision); err != nil {
			rows.Close()
			return err
		}
		items = append(items, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, d := range items {
		jobID, e := sourceJobID()
		if e != nil {
			return e
		}
		if _, err = tx.Exec(ctx, `INSERT INTO data_source_jobs(id,source_id,operation,trigger,state,source_revision) VALUES($1,$2,'sync','schedule','queued',$3)`, jobID, d.id, d.revision); err != nil {
			return err
		}
		if err = sourceJobEvent(ctx, tx, datasources.Job{ID: jobID}, "queued", ""); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE data_sources SET next_sync_at=now()+make_interval(secs=>sync_interval_seconds) WHERE id=$1`, d.id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) ClaimDataSourceJob(ctx context.Context) (*datasources.Request, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194002)`); err != nil {
		return nil, err
	}
	var sourceID string
	err = tx.QueryRow(ctx, `SELECT d.id FROM data_sources d WHERE EXISTS(SELECT 1 FROM data_source_jobs j WHERE j.source_id=d.id AND j.state IN ('queued','retry_wait','running') AND (NOT d.enabled OR j.source_revision<>d.revision OR (j.state<>'running' AND j.next_attempt_at<=now()) OR (j.state='running' AND j.lease_until<=now()))) ORDER BY d.next_sync_at,d.id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&sourceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	v, err := readSource(tx.QueryRow(ctx, `SELECT `+sourceColumns+` FROM data_sources WHERE id=$1`, sourceID))
	if err != nil {
		return nil, err
	}
	j, err := readSourceJob(tx.QueryRow(ctx, `SELECT `+sourceJobColumns+` FROM data_source_jobs WHERE source_id=$1 AND state IN ('queued','retry_wait','running') FOR UPDATE`, sourceID))
	if err != nil {
		return nil, err
	}
	cancelCode := ""
	if !v.Enabled {
		cancelCode = "source_disabled"
	} else if v.Revision != j.SourceRevision {
		cancelCode = "configuration_changed"
	} else if _, ok := s.sourceOperations.Lookup(v.Type, j.Operation); !ok {
		cancelCode = "adapter_unavailable"
	}
	if cancelCode == "" && j.Trigger == "manual" {
		var session []byte
		if err = tx.QueryRow(ctx, `SELECT actor_session_hash FROM data_source_jobs WHERE id=$1`, j.ID).Scan(&session); err != nil {
			return nil, err
		}
		actor := access.Principal{User: access.User{ID: *j.ActorID}, SessionHash: session}
		if err = accessAdminTx(ctx, tx, actor); errors.Is(err, access.ErrForbidden) {
			cancelCode = "actor_revoked"
		} else if err != nil {
			return nil, err
		}
	}
	if cancelCode != "" {
		if _, err = tx.Exec(ctx, `UPDATE data_source_jobs SET state='cancelled',code=$2,finished_at=now(),lease_token=NULL,lease_until=NULL WHERE id=$1`, j.ID, cancelCode); err != nil {
			return nil, err
		}
		if err = sourceJobEvent(ctx, tx, j, "cancelled", cancelCode); err != nil {
			return nil, err
		}
		return nil, tx.Commit(ctx)
	}
	if j.State == "running" {
		state := "retry_wait"
		if j.Attempts >= 3 {
			state = "failed"
		}
		if _, err = tx.Exec(ctx, `UPDATE data_source_jobs SET state=$2,code='interrupted',lease_token=NULL,lease_until=NULL,next_attempt_at=now()+interval '2 seconds',finished_at=CASE WHEN $2='failed' THEN now() ELSE NULL END WHERE id=$1`, j.ID, state); err != nil {
			return nil, err
		}
		if err = sourceJobEvent(ctx, tx, j, state, "interrupted"); err != nil {
			return nil, err
		}
		if state == "failed" {
			if _, err = tx.Exec(ctx, `UPDATE data_sources SET connection_status='error',last_error='interrupted' WHERE id=$1`, sourceID); err != nil {
				return nil, err
			}
			if j.Operation == "sync" {
				if err = sourceSyncFailure(ctx, tx, sourceID); err != nil {
					return nil, err
				}
			}
		}
		return nil, tx.Commit(ctx)
	}
	var cursor json.RawMessage
	var ciphertext []byte
	if err = tx.QueryRow(ctx, `SELECT sync_cursor,credential_ciphertext FROM data_sources WHERE id=$1`, sourceID).Scan(&cursor, &ciphertext); err != nil {
		return nil, err
	}
	var secret []byte
	if ciphertext != nil {
		secret, err = s.sourceVault.Open(sourceID, ciphertext)
		if err != nil {
			return nil, err
		}
	}
	lease, err := sourceJobID()
	if err != nil {
		clear(secret)
		return nil, err
	}
	_, err = tx.Exec(ctx, `UPDATE data_source_jobs SET state='running',code='',attempts=attempts+1,started_at=COALESCE(started_at,now()),lease_token=$2,lease_until=now()+make_interval(secs=>$3) WHERE id=$1`, j.ID, lease, v.Configuration.TimeoutSeconds+15)
	if err != nil {
		clear(secret)
		return nil, err
	}
	j, err = readSourceJob(tx.QueryRow(ctx, `SELECT `+sourceJobColumns+` FROM data_source_jobs WHERE id=$1`, j.ID))
	if err != nil {
		clear(secret)
		return nil, err
	}
	if err = sourceJobEvent(ctx, tx, j, "running", ""); err != nil {
		clear(secret)
		return nil, err
	}
	pending, err := s.pendingExternalSignals(ctx, tx, v, cursor)
	if err != nil {
		clear(secret)
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		clear(secret)
		return nil, err
	}
	return &datasources.Request{Job: j, Source: v, Credential: secret, Cursor: cursor, PendingSignals: pending}, nil
}

func (s *Store) FinishDataSourceJob(ctx context.Context, request datasources.Request, result datasources.Result, runErr error) error {
	code, retry := datasources.Outcome(runErr)
	if !validateAssetObservations(result.Observations) {
		code = "invalid_response"
		retry = false
		runErr = assets.ErrInvalid
		result.Observations = nil
	}
	if len(result.Cursor) > 0 {
		var object map[string]json.RawMessage
		if len(result.Cursor) > 16384 || json.Unmarshal(result.Cursor, &object) != nil || object == nil {
			code = "invalid_response"
			retry = false
			runErr = datasources.ErrInvalid
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if len(result.Observations) > 0 {
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194030)`); err != nil {
			return err
		}
	}
	var enabled bool
	var revision int64
	var sourceType string
	var sourceConfig []byte
	if err = tx.QueryRow(ctx, `SELECT enabled,revision,type,configuration FROM data_sources WHERE id=$1 FOR UPDATE`, request.Source.ID).Scan(&enabled, &revision, &sourceType, &sourceConfig); err != nil {
		return err
	}
	j, err := readSourceJob(tx.QueryRow(ctx, `SELECT `+sourceJobColumns+` FROM data_source_jobs WHERE id=$1 AND source_id=$2 FOR UPDATE`, request.Job.ID, request.Source.ID))
	if err != nil {
		return err
	}
	if j.State != "running" || j.LeaseToken != request.Job.LeaseToken {
		return datasources.ErrLease
	}
	var live bool
	if err = tx.QueryRow(ctx, `SELECT lease_until>now() FROM data_source_jobs WHERE id=$1`, j.ID).Scan(&live); err != nil {
		return err
	}
	if !live {
		return datasources.ErrLease
	}
	state := "succeeded"
	actualSource := request.Source
	actualSource.Type = sourceType
	actualSource.Revision = revision
	// Decode into a zero value: omitted DB fields must not retain an adapter's
	// mutable request configuration. Publication is governed by persisted policy.
	actualSource.Configuration = datasources.Settings{}
	if err = json.Unmarshal(sourceConfig, &actualSource.Configuration); err != nil {
		return err
	}
	if result.Monitoring != nil && (j.Operation != "sync" || !validateExternalMonitoring(actualSource, result.Monitoring)) {
		code = "invalid_response"
		retry = false
		runErr = externalmonitor.ErrInvalid
	}
	if result.Prometheus != nil && (j.Operation != "sync" || !validatePrometheus(actualSource, result.Prometheus, result.Observations)) {
		code = "invalid_response"
		retry = false
		runErr = externalmonitor.ErrInvalid
	}
	if len(result.Observations) > 0 && (j.Operation != "sync" || sourceType == "COLLECTOR") {
		code = "invalid_response"
		retry = false
		runErr = assets.ErrInvalid
	}
	if !enabled || revision != j.SourceRevision {
		state = "cancelled"
		code = "configuration_changed"
		retry = false
	} else if runErr != nil {
		state = "failed"
		if retry && j.Attempts < 3 {
			state = "retry_wait"
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE data_source_jobs SET state=$2,code=$3,lease_token=NULL,lease_until=NULL,next_attempt_at=now()+make_interval(secs=>$4),finished_at=CASE WHEN $2='retry_wait' THEN NULL ELSE now() END WHERE id=$1`, j.ID, state, code, 1<<j.Attempts); err != nil {
		return err
	}
	if state == "succeeded" && len(result.Observations) > 0 {
		if err = s.projectExternalAssets(ctx, tx, request.Source.ID, j.ID, result.Observations); err != nil {
			return err
		}
	}
	if state == "succeeded" {
		if err = s.projectExternalMonitoring(ctx, tx, actualSource, result.Monitoring); err != nil {
			return err
		}
		if err = s.projectPrometheus(ctx, tx, actualSource, result.Prometheus); err != nil {
			return err
		}
	}
	if err = sourceJobEvent(ctx, tx, j, state, code); err != nil {
		return err
	}
	if state != "cancelled" {
		status := "error"
		if state == "succeeded" {
			status = "connected"
		}
		if _, err = tx.Exec(ctx, `UPDATE data_sources SET connection_status=$2,last_error=CASE WHEN $3='ok' THEN '' ELSE $3 END WHERE id=$1`, request.Source.ID, status, code); err != nil {
			return err
		}
		if j.Operation == "sync" && state == "succeeded" {
			var cursor any
			if len(result.Cursor) > 0 {
				cursor = result.Cursor
			}
			if _, err = tx.Exec(ctx, `UPDATE data_sources SET sync_cursor=COALESCE($2::jsonb,sync_cursor),sync_failures=0,last_successful_sync=now(),next_sync_at=now()+make_interval(secs=>sync_interval_seconds) WHERE id=$1`, request.Source.ID, cursor); err != nil {
				return err
			}
		} else if j.Operation == "sync" && state == "failed" {
			if err = sourceSyncFailure(ctx, tx, request.Source.ID); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

func sourceSyncFailure(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `UPDATE data_sources SET sync_failures=LEAST(sync_failures+1,16),next_sync_at=now()+make_interval(secs=>GREATEST(sync_interval_seconds,LEAST(86400,30*power(2,LEAST(sync_failures,10))))) WHERE id=$1`, id)
	return err
}
