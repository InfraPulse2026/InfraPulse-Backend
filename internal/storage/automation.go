package storage

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	au "github.com/InfraPulse2026/InfraPulse-Backend/internal/automation"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/jackc/pgx/v5"
	"time"
)

// Called once, before serving requests. Decryptability is checked even when disabled.
func (s *Store) ConfigureAutomation(ctx context.Context, enabled bool, vault *datasources.Vault) error {
	rows, err := s.pool.Query(ctx, `SELECT id,ciphertext FROM automation_snapshots`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var cipher []byte
		if err = rows.Scan(&id, &cipher); err != nil {
			return err
		}
		plain, e := vault.Open("automation-snapshot:"+id, cipher)
		clear(plain)
		if e != nil {
			return errors.New("restore original AUTOMATION_ENCRYPTION_KEY to read existing snapshots")
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if enabled && vault == nil {
		return au.ErrUnavailable
	}
	s.automationEnabled = enabled
	s.automationVault = vault
	return nil
}
func (s *Store) CheckAutomationSchema(ctx context.Context) error {
	var ok bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=34)`).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return errors.New("start backend to apply migration 034 before Automation Worker")
	}
	return nil
}

// Lock the actual identity/session for the mutation, not the role cached in HTTP.
func automationActorTx(ctx context.Context, tx pgx.Tx, p access.Principal, adminOnly bool) (string, error) {
	var role string
	err := tx.QueryRow(ctx, `SELECT u.role FROM access_users u JOIN access_sessions ses ON ses.user_id=u.id WHERE u.id=$1 AND ses.token_hash=$2 AND NOT u.disabled AND ses.expires_at>now() AND u.role IN ('admin','operator') FOR SHARE OF u,ses`, p.User.ID, p.SessionHash).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", access.ErrForbidden
	}
	if err != nil {
		return "", err
	}
	if adminOnly && role != "admin" {
		return "", access.ErrForbidden
	}
	return role, nil
}
func automationAudit(ctx context.Context, tx pgx.Tx, actor, action, outcome, resource string, meta access.Meta) error {
	_, err := tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,$2,$3,$4,$5,$6)`, actor, action, outcome, resource, meta.RequestID, meta.Peer)
	return err
}
func (s *Store) AutomationTarget(ctx context.Context, id string) (au.Target, error) {
	var v au.Target
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT asset_id,revision,definition,updated_at FROM automation_targets WHERE asset_id=$1`, id).Scan(&v.AssetID, &v.Revision, &raw, &v.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	err = json.Unmarshal(raw, &v.TargetInput)
	return v, err
}
func (s *Store) PutAutomationTarget(ctx context.Context, id string, in au.TargetInput) (au.Target, error) {
	var out au.Target
	if !s.automationEnabled {
		return out, au.ErrUnavailable
	}
	if !au.Identifier(id) || in.Validate() != nil {
		return out, au.ErrInvalid
	}
	p, ok := access.Current(ctx)
	if !ok {
		return out, access.ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	if _, err = automationActorTx(ctx, tx, p, true); err != nil {
		return out, err
	}
	var revision int64
	var lifecycle, scope string
	if err = tx.QueryRow(ctx, `SELECT revision,lifecycle_status,scope FROM assets WHERE id=$1 FOR UPDATE`, id).Scan(&revision, &lifecycle, &scope); errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	} else if err != nil {
		return out, err
	}
	if revision != in.AssetRevision || lifecycle != "observed" || scope != "" {
		return out, au.ErrConflict
	}
	var current int64
	err = tx.QueryRow(ctx, `SELECT revision FROM automation_targets WHERE asset_id=$1 FOR UPDATE`, id).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if current != in.ExpectedRevision || current == 9223372036854775807 {
		return out, au.ErrConflict
	}
	raw, _ := json.Marshal(in)
	out = au.Target{TargetInput: in, AssetID: id, Revision: current + 1}
	if err = tx.QueryRow(ctx, `INSERT INTO automation_targets(asset_id,revision,definition) VALUES($1,$2,$3) ON CONFLICT(asset_id) DO UPDATE SET revision=excluded.revision,definition=excluded.definition,updated_at=now() RETURNING updated_at`, id, out.Revision, raw).Scan(&out.UpdatedAt); err != nil {
		return out, err
	}
	if err = automationAudit(ctx, tx, p.User.ID, "automation.target.update", "success", id, access.CurrentMeta(ctx)); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

const automationJobColumns = `id,asset_id,target_revision,template_id,actor_id,state,error_code,timeout_seconds,reason,created_at,started_at,finished_at,COALESCE(snapshot_id,'')`

func readAutomationJob(row interface{ Scan(...any) error }) (au.Job, error) {
	var j au.Job
	err := row.Scan(&j.ID, &j.AssetID, &j.TargetRevision, &j.TemplateID, &j.ActorID, &j.State, &j.ErrorCode, &j.TimeoutSeconds, &j.Reason, &j.CreatedAt, &j.StartedAt, &j.FinishedAt, &j.SnapshotID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return j, err
}
func automationEvent(ctx context.Context, tx pgx.Tx, id, code string) error {
	_, err := tx.Exec(ctx, `INSERT INTO automation_events(job_id,code) VALUES($1,$2)`, id, code)
	return err
}
func (s *Store) CreateAutomation(ctx context.Context, assetID string, in au.Input, key string) (au.Job, bool, error) {
	var out au.Job
	if !s.automationEnabled {
		return out, false, au.ErrUnavailable
	}
	if !au.Identifier(assetID) || in.Validate() != nil || commands.ValidateKey(key) != nil {
		return out, false, au.ErrInvalid
	}
	p, ok := access.Current(ctx)
	if !ok {
		return out, false, access.ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback(context.Background())
	if _, err = automationActorTx(ctx, tx, p, false); err != nil {
		return out, false, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,73194003))`, p.User.ID+":"+key); err != nil {
		return out, false, err
	}
	raw, _ := json.Marshal(struct {
		Asset string
		Input au.Input
	}{assetID, in})
	fingerprint := au.Digest(string(raw))
	var oldHash string
	err = tx.QueryRow(ctx, `SELECT request_hash FROM automation_jobs WHERE actor_id=$1 AND idempotency_key=$2`, p.User.ID, key).Scan(&oldHash)
	if err == nil {
		if oldHash != fingerprint {
			return out, false, au.ErrConflict
		}
		out, err = readAutomationJob(tx.QueryRow(ctx, `SELECT `+automationJobColumns+` FROM automation_jobs WHERE actor_id=$1 AND idempotency_key=$2`, p.User.ID, key))
		if err != nil {
			return out, false, err
		}
		return out, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, false, err
	}
	// Bound total outstanding work, including across operators and Assets.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194004)`); err != nil {
		return out, false, err
	}
	var outstanding int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM automation_jobs WHERE state IN ('queued','running')`).Scan(&outstanding); err != nil {
		return out, false, err
	}
	if outstanding >= 100 {
		return out, false, au.ErrUnavailable
	}
	var target au.Target
	var definition []byte
	var assetRevision int64
	var lifecycle, scope string
	if err = tx.QueryRow(ctx, `SELECT revision,lifecycle_status,scope FROM assets WHERE id=$1 FOR UPDATE`, assetID).Scan(&assetRevision, &lifecycle, &scope); errors.Is(err, pgx.ErrNoRows) {
		return out, false, ErrNotFound
	} else if err != nil {
		return out, false, err
	}
	if err = tx.QueryRow(ctx, `SELECT revision,definition FROM automation_targets WHERE asset_id=$1 FOR UPDATE`, assetID).Scan(&target.Revision, &definition); errors.Is(err, pgx.ErrNoRows) {
		return out, false, ErrNotFound
	} else if err != nil {
		return out, false, err
	}
	if err = json.Unmarshal(definition, &target.TargetInput); err != nil {
		return out, false, err
	}
	target.AssetID = assetID
	if target.Revision != in.TargetRevision || target.AssetRevision != assetRevision || !target.Enabled || scope != "" || lifecycle != "observed" {
		return out, false, au.ErrConflict
	}
	var busy bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM automation_jobs WHERE asset_id=$1 AND state IN ('queued','running'))`, assetID).Scan(&busy); err != nil {
		return out, false, err
	}
	if busy {
		return out, false, au.ErrConflict
	}
	var available bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM automation_workers WHERE available AND last_seen>now()-interval '20 seconds' AND templates ? $1)`, target.TemplateID).Scan(&available); err != nil {
		return out, false, err
	}
	if !available {
		return out, false, au.ErrUnavailable
	}
	id, err := access.NewID()
	if err != nil {
		return out, false, err
	}
	targetJSON, _ := json.Marshal(target)
	out, err = readAutomationJob(tx.QueryRow(ctx, `INSERT INTO automation_jobs(id,asset_id,target_revision,template_id,target,actor_id,session_hash,idempotency_key,request_hash,state,timeout_seconds,reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'queued',$10,$11) RETURNING `+automationJobColumns, id, assetID, target.Revision, target.TemplateID, targetJSON, p.User.ID, p.SessionHash, key, fingerprint, in.TimeoutSeconds, in.Reason))
	if err != nil {
		return out, false, err
	}
	if err = automationEvent(ctx, tx, id, "queued"); err != nil {
		return out, false, err
	}
	if err = automationAudit(ctx, tx, p.User.ID, "automation.job.create", "success", id, access.CurrentMeta(ctx)); err != nil {
		return out, false, err
	}
	return out, false, tx.Commit(ctx)
}
func (s *Store) AutomationJob(ctx context.Context, id string) (au.Job, error) {
	return readAutomationJob(s.pool.QueryRow(ctx, `SELECT `+automationJobColumns+` FROM automation_jobs WHERE id=$1`, id))
}
func (s *Store) AutomationJobs(ctx context.Context, asset string, limit, offset int) ([]au.Job, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+automationJobColumns+` FROM automation_jobs WHERE asset_id=$1 ORDER BY created_at DESC,id DESC LIMIT $2 OFFSET $3`, asset, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []au.Job{}
	for rows.Next() {
		j, e := readAutomationJob(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (s *Store) AutomationEvents(ctx context.Context, id string, limit, offset int) ([]au.Event, error) {
	if _, err := s.AutomationJob(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id,job_id,code,at FROM automation_events WHERE job_id=$1 ORDER BY id LIMIT $2 OFFSET $3`, id, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []au.Event{}
	for rows.Next() {
		var v au.Event
		if err = rows.Scan(&v.ID, &v.JobID, &v.Code, &v.At); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) CancelAutomation(ctx context.Context, id string) (au.Job, error) {
	p, ok := access.Current(ctx)
	if !ok {
		return au.Job{}, access.ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return au.Job{}, err
	}
	defer tx.Rollback(context.Background())
	role, err := automationActorTx(ctx, tx, p, false)
	if err != nil {
		return au.Job{}, err
	}
	out, err := readAutomationJob(tx.QueryRow(ctx, `SELECT `+automationJobColumns+` FROM automation_jobs WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return out, err
	}
	if role != "admin" && out.ActorID != p.User.ID {
		return out, access.ErrForbidden
	}
	if out.State == "queued" || out.State == "running" {
		changed, e := tx.Exec(ctx, `UPDATE automation_jobs SET cancel_requested=true,state=CASE WHEN state='queued' THEN 'cancelled' ELSE state END,finished_at=CASE WHEN state='queued' THEN now() ELSE finished_at END WHERE id=$1 AND NOT cancel_requested`, id)
		if e != nil {
			return out, e
		}
		if changed.RowsAffected() > 0 {
			if err = automationEvent(ctx, tx, id, "cancel_requested"); err != nil {
				return out, err
			}
			if err = automationAudit(ctx, tx, p.User.ID, "automation.job.cancel", "success", id, access.CurrentMeta(ctx)); err != nil {
				return out, err
			}
			if out.State == "queued" {
				if err = s.finishConfigurationRemediationTx(ctx, tx, id, "cancelled", "cancelled", nil); err != nil {
					return out, err
				}
			}
		}
	}
	out, err = readAutomationJob(tx.QueryRow(ctx, `SELECT `+automationJobColumns+` FROM automation_jobs WHERE id=$1`, id))
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
func (s *Store) AutomationWorkers(ctx context.Context) ([]au.WorkerStatus, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,available AND last_seen>now()-interval '20 seconds',versions,templates,last_seen,error_code FROM automation_workers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []au.WorkerStatus{}
	for rows.Next() {
		var v au.WorkerStatus
		var versions, templates []byte
		if err = rows.Scan(&v.ID, &v.Available, &versions, &templates, &v.LastSeen, &v.ErrorCode); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(versions, &v.Versions); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(templates, &v.Templates); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) AutomationWorker(ctx context.Context, v au.WorkerStatus) error {
	if !au.Identifier(v.ID) || len(v.Templates) > 3 || len(v.Versions) > 8 {
		return au.ErrInvalid
	}
	for _, id := range v.Templates {
		if _, ok := au.Lookup(id); !ok {
			return au.ErrInvalid
		}
	}
	if v.Versions == nil {
		v.Versions = map[string]string{}
	}
	if v.Templates == nil {
		v.Templates = []string{}
	}
	versions, _ := json.Marshal(v.Versions)
	templates, _ := json.Marshal(v.Templates)
	_, err := s.pool.Exec(ctx, `INSERT INTO automation_workers(id,available,versions,templates,error_code) VALUES($1,$2,$3,$4,$5) ON CONFLICT(id) DO UPDATE SET available=excluded.available,versions=excluded.versions,templates=excluded.templates,error_code=excluded.error_code,last_seen=now()`, v.ID, v.Available, versions, templates, safeAutomationError(v.ErrorCode))
	return err
}

const automationRunValid = `SELECT EXISTS(SELECT 1 FROM automation_jobs j JOIN assets a ON a.id=j.asset_id JOIN automation_targets t ON t.asset_id=j.asset_id JOIN access_users u ON u.id=j.actor_id JOIN access_sessions ses ON ses.user_id=u.id AND ses.token_hash=j.session_hash WHERE j.id=$1 AND j.state='running' AND j.claim_token=$2 AND NOT j.cancel_requested AND j.deadline>now() AND t.revision=j.target_revision AND (t.definition->>'enabled')::boolean AND a.revision=(t.definition->>'asset_revision')::bigint AND a.lifecycle_status='observed' AND a.scope='' AND NOT u.disabled AND u.role IN ('admin','operator') AND ses.expires_at>now())`

func (s *Store) ClaimAutomation(ctx context.Context, worker string) (au.Job, error) {
	var out au.Job
	if !s.automationEnabled {
		return out, au.ErrUnavailable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	// An orphan is terminal; running jobs are never blindly replayed after a crash.
	rows, err := tx.Query(ctx, `UPDATE automation_jobs SET state='failed',error_code='worker_lost',finished_at=now(),claim_token=NULL WHERE state='running' AND deadline<now()-interval '30 seconds' RETURNING id`)
	if err != nil {
		return out, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	for _, id := range ids {
		if err = automationEvent(ctx, tx, id, "worker_lost"); err != nil {
			return out, err
		}
		if err = s.finishConfigurationRemediationTx(ctx, tx, id, "failed", "worker_lost", nil); err != nil {
			return out, err
		}
	}
	var raw []byte
	out, err = readAutomationJob(tx.QueryRow(ctx, `SELECT `+automationJobColumns+` FROM automation_jobs WHERE state='queued' AND template_id IN (SELECT jsonb_array_elements_text(templates) FROM automation_workers WHERE id=$1 AND available AND last_seen>now()-interval '20 seconds') ORDER BY created_at,id FOR UPDATE SKIP LOCKED LIMIT 1`, worker))
	if errors.Is(err, ErrNotFound) {
		return out, tx.Commit(ctx)
	}
	if err != nil {
		return out, err
	}
	token, err := access.NewID()
	if err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `UPDATE automation_jobs SET state='running',worker_id=$2,claim_token=$3,started_at=now(),deadline=now()+make_interval(secs=>timeout_seconds) WHERE id=$1`, out.ID, worker, token); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `UPDATE configuration_remediations SET revision=revision+1,state='running' WHERE automation_job_id=$1 AND state='queued'`, out.ID); err != nil {
		return out, err
	}
	out.ClaimToken = token
	var valid bool
	if err = tx.QueryRow(ctx, automationRunValid, out.ID, token).Scan(&valid); err != nil {
		return out, err
	}
	if valid {
		valid, err = s.remediationRunValidTx(ctx, tx, out.ID)
		if err != nil {
			return out, err
		}
	}
	if !valid {
		if _, err = tx.Exec(ctx, `UPDATE automation_jobs SET state='failed',error_code='authorization_or_target_changed',claim_token=NULL,finished_at=now() WHERE id=$1`, out.ID); err != nil {
			return out, err
		}
		if err = automationEvent(ctx, tx, out.ID, "authorization_or_target_changed"); err != nil {
			return out, err
		}
		if err = s.finishConfigurationRemediationTx(ctx, tx, out.ID, "failed", "authorization_or_target_changed", nil); err != nil {
			return out, err
		}
		return au.Job{}, tx.Commit(ctx)
	}
	if err = tx.QueryRow(ctx, `SELECT target FROM automation_jobs WHERE id=$1`, out.ID).Scan(&raw); err != nil {
		return out, err
	}
	if err = json.Unmarshal(raw, &out.Target); err != nil {
		return out, err
	}
	out.State = "running"
	if err = automationEvent(ctx, tx, out.ID, "running"); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
func (s *Store) AutomationRunning(ctx context.Context, j au.Job) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	var valid bool
	err = tx.QueryRow(ctx, automationRunValid, j.ID, j.ClaimToken).Scan(&valid)
	if err == nil && valid {
		valid, err = s.remediationRunValidTx(ctx, tx, j.ID)
	}
	if err != nil {
		return false, err
	}
	return valid, tx.Commit(ctx)
}
func safeAutomationError(code string) string {
	switch code {
	case "", "runner_unavailable", "runner_failed", "credential_unavailable", "invalid_result", "worker_lost", "authorization_or_target_changed", "cancelled", "timed_out", "shutdown", "destination_denied":
		return code
	default:
		return "runner_failed"
	}
}
func (s *Store) FinishAutomation(ctx context.Context, j au.Job, state string, result au.Result) error {
	if state != "succeeded" && state != "failed" && state != "cancelled" && state != "timed_out" {
		return au.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var current, token string
	var actualAsset, actualTemplate, actualActor string
	var cancelled bool
	var deadline time.Time
	if err = tx.QueryRow(ctx, `SELECT state,COALESCE(claim_token,''),cancel_requested,deadline,asset_id,template_id,actor_id FROM automation_jobs WHERE id=$1 FOR UPDATE`, j.ID).Scan(&current, &token, &cancelled, &deadline, &actualAsset, &actualTemplate, &actualActor); err != nil {
		return err
	}
	if current != "running" || token != j.ClaimToken || j.AssetID != actualAsset || j.TemplateID != actualTemplate || j.ActorID != actualActor {
		return au.ErrConflict
	}
	// Pin Asset/target and identity against concurrent merge, retarget or revocation.
	if _, err = tx.Exec(ctx, `SELECT a.id FROM assets a JOIN automation_targets t ON t.asset_id=a.id WHERE a.id=$1 FOR SHARE OF a,t`, j.AssetID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT u.id FROM access_users u JOIN access_sessions ses ON ses.user_id=u.id JOIN automation_jobs j ON j.actor_id=u.id AND j.session_hash=ses.token_hash WHERE j.id=$1 FOR SHARE OF u,ses`, j.ID); err != nil {
		return err
	}
	code := safeAutomationError(result.ErrorCode)
	var valid bool
	if err = tx.QueryRow(ctx, automationRunValid, j.ID, token).Scan(&valid); err != nil {
		return err
	}
	if valid {
		valid, err = s.remediationRunValidTx(ctx, tx, j.ID)
		if err != nil {
			return err
		}
	}
	if cancelled {
		state = "cancelled"
		code = "cancelled"
	} else if !time.Now().Before(deadline) {
		state = "timed_out"
		code = "timed_out"
	} else if !valid {
		state = "failed"
		code = "authorization_or_target_changed"
	}
	var snapshotID *string
	if state == "succeeded" {
		if result.Validate() != nil {
			state = "failed"
			code = "invalid_result"
		} else {
			id, e := access.NewID()
			if e != nil {
				return e
			}
			snapshotID = &id
			payload, _ := json.Marshal(result)
			cipher, e := s.automationVault.Seal("automation-snapshot:"+id, string(payload))
			clear(payload)
			if e != nil {
				return e
			}
			if _, err = tx.Exec(ctx, `INSERT INTO automation_snapshots(id,asset_id,job_id,template_id,digest,bytes,ciphertext) VALUES($1,$2,$3,$4,$5,$6,$7)`, id, j.AssetID, j.ID, j.TemplateID, au.Digest(result.Configuration), len(result.Configuration), cipher); err != nil {
				return err
			}
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE automation_jobs SET state=$2,error_code=$3,finished_at=now(),claim_token=NULL,snapshot_id=$4 WHERE id=$1`, j.ID, state, code, snapshotID); err != nil {
		return err
	}
	if err = automationEvent(ctx, tx, j.ID, state); err != nil {
		return err
	}
	if snapshotID != nil && state == "succeeded" {
		if err = s.projectConfigurationDrift(ctx, tx, j, *snapshotID); err != nil {
			return err
		}
	}
	if err = s.finishConfigurationRemediationTx(ctx, tx, j.ID, state, code, snapshotID); err != nil {
		return err
	}
	if err = automationAudit(ctx, tx, j.ActorID, "automation.job.finish", state, j.ID, access.Meta{}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) AutomationSnapshots(ctx context.Context, asset string, limit, offset int) ([]au.Snapshot, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,asset_id,job_id,template_id,digest,bytes,observed_at FROM automation_snapshots WHERE asset_id=$1 ORDER BY observed_at DESC,id DESC LIMIT $2 OFFSET $3`, asset, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []au.Snapshot{}
	for rows.Next() {
		var v au.Snapshot
		if err = rows.Scan(&v.ID, &v.AssetID, &v.JobID, &v.TemplateID, &v.Digest, &v.Bytes, &v.ObservedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) AutomationSnapshotContent(ctx context.Context, id string) (au.Result, error) {
	var out au.Result
	p, ok := access.Current(ctx)
	if !ok {
		return out, access.ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	if _, err = automationActorTx(ctx, tx, p, true); err != nil {
		return out, err
	}
	var cipher []byte
	if err = tx.QueryRow(ctx, `SELECT ciphertext FROM automation_snapshots WHERE id=$1`, id).Scan(&cipher); errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	} else if err != nil {
		return out, err
	}
	plain, err := s.automationVault.Open("automation-snapshot:"+id, cipher)
	if err != nil {
		return out, errors.New("cannot decrypt automation snapshot")
	}
	defer clear(plain)
	if err = json.Unmarshal(plain, &out); err != nil {
		return out, err
	}
	if err = automationAudit(ctx, tx, p.User.ID, "automation.snapshot.read", "success", id, access.CurrentMeta(ctx)); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
