package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/analysisai"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func aiID(prefix string) (string, error) {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	return prefix + hex.EncodeToString(b), err
}
func (s *Store) CreatePreview(ctx context.Context, incident string, cfg analysisai.Config) (analysisai.Preview, error) {
	var p analysisai.Preview
	actor, ok := access.Current(ctx)
	if !ok {
		return p, access.ErrForbidden
	}
	payload, references, caseID, caseRevision, err := s.aiMultiSourcePayload(ctx, incident)
	if err != nil {
		return p, err
	}
	request, hash, err := analysisai.BuildRequest(cfg.Model, payload)
	if err != nil {
		return p, err
	}
	id, err := aiID("prv_")
	if err != nil {
		return p, err
	}
	now := time.Now().UTC()
	p = analysisai.Preview{CorrelatedIncidentID: caseID, CorrelationRevision: caseRevision, References: references, ID: id, IncidentID: incident, CreatedBy: actor.User.ID, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute), Destination: analysisai.Destination, Enabled: cfg.Enabled, PromptVersion: analysisai.PromptVersion, Hash: hash, Request: request, Payload: payload, Excluded: []string{"raw logs/audit text", "addresses and hostname", "real Agent/device/resource/incident IDs", "source labels and endpoint identifiers", "credentials and inventory", "absolute timestamps and raw topology evidence"}}
	encoded, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return p, err
	}
	defer tx.Rollback(context.Background())
	var permitted bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM access_users u JOIN access_sessions s ON s.user_id=u.id WHERE u.id=$1 AND s.token_hash=$2 AND NOT u.disabled AND s.expires_at>now())`, actor.User.ID, actor.SessionHash).Scan(&permitted); err != nil {
		return p, err
	}
	if !permitted {
		return p, access.ErrForbidden
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "ai-preview:"+actor.User.ID); err != nil {
		return p, err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM ai_previews WHERE created_by=$1 AND created_at>now()-interval '1 minute'`, actor.User.ID).Scan(&count); err != nil {
		return p, err
	}
	if count >= 10 {
		return p, analysisai.ErrConflict
	}
	if _, err = tx.Exec(ctx, `INSERT INTO ai_previews(id,incident_id,created_by,created_at,expires_at,request_hash,model,preview)VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, p.ID, p.IncidentID, p.CreatedBy, p.CreatedAt, p.ExpiresAt, p.Hash, cfg.Model, encoded); err != nil {
		return p, err
	}
	if err = aiAudit(ctx, tx, actor.User.ID, "preview_created", "preview:"+p.ID); err != nil {
		return p, err
	}
	return p, tx.Commit(ctx)
}
func aiAudit(ctx context.Context, tx pgx.Tx, actor, action, resource string) error {
	meta := access.CurrentMeta(ctx)
	_, err := tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer)VALUES($1,$2,'success',$3,$4,$5)`, actor, "ai_"+action, resource, meta.RequestID, meta.Peer)
	return err
}

const aiJobColumns = `id,incident_id,preview_id,created_by,created_at,updated_at,state,failure,model,request_hash,result,(SELECT preview->>'prompt_version' FROM ai_previews WHERE id=ai_analyses.preview_id),(SELECT preview->>'correlated_incident_id' FROM ai_previews WHERE id=ai_analyses.preview_id)`

func readAIJob(row interface{ Scan(...any) error }) (analysisai.Job, error) {
	var job analysisai.Job
	var raw []byte
	err := row.Scan(&job.ID, &job.IncidentID, &job.PreviewID, &job.CreatedBy, &job.CreatedAt, &job.UpdatedAt, &job.State, &job.Failure, &job.Model, &job.RequestHash, &raw, &job.PromptVersion, &job.CorrelatedIncidentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return job, analysisai.ErrNotFound
	}
	if err == nil && raw != nil {
		job.Result = &analysisai.Result{}
		err = json.Unmarshal(raw, job.Result)
	}
	return job, err
}
func (s *Store) CreateJob(ctx context.Context, incident string, body analysisai.Create, key string, cfg analysisai.Config) (analysisai.Job, bool, error) {
	var empty analysisai.Job
	if !cfg.Enabled {
		return empty, false, analysisai.ErrDisabled
	}
	actor, ok := access.Current(ctx)
	if !ok {
		return empty, false, access.ErrForbidden
	}
	if !body.Confirmed || commands.ValidateKey(key) != nil || len(body.RequestHash) != 64 {
		return empty, false, analysisai.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return empty, false, err
	}
	defer tx.Rollback(context.Background())
	if err = commandActor(ctx, tx, actor.User.ID, actor.SessionHash); err != nil {
		return empty, false, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "ai-job:"+actor.User.ID); err != nil {
		return empty, false, err
	}
	fingerprintBytes, _ := json.Marshal([]string{incident, body.PreviewID, body.RequestHash})
	fingerprint := string(fingerprintBytes)
	var previous string
	err = tx.QueryRow(ctx, `SELECT request_fingerprint FROM ai_analyses WHERE created_by=$1 AND idempotency_key=$2`, actor.User.ID, key).Scan(&previous)
	if err == nil {
		if previous != fingerprint {
			return empty, false, analysisai.ErrConflict
		}
		job, e := readAIJob(tx.QueryRow(ctx, `SELECT `+aiJobColumns+` FROM ai_analyses WHERE created_by=$1 AND idempotency_key=$2`, actor.User.ID, key))
		if e != nil {
			return empty, false, e
		}
		return job, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return empty, false, err
	}
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT preview FROM ai_previews WHERE id=$1 AND incident_id=$2 AND created_by=$3 AND expires_at>now() AND request_hash=$4 AND model=$5 FOR UPDATE`, body.PreviewID, incident, actor.User.ID, body.RequestHash, cfg.Model).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, false, analysisai.ErrConflict
	}
	if err != nil {
		return empty, false, err
	}
	var preview analysisai.Preview
	if err = json.Unmarshal(raw, &preview); err != nil {
		return empty, false, err
	}
	if preview.Destination != analysisai.Destination || preview.PromptVersion != analysisai.PromptVersion || !preview.Enabled {
		return empty, false, analysisai.ErrConflict
	}
	id, err := aiID("ana_")
	if err != nil {
		return empty, false, err
	}
	job, err := readAIJob(tx.QueryRow(ctx, `INSERT INTO ai_analyses(id,incident_id,preview_id,created_by,actor_session_hash,state,model,request_hash,idempotency_key,request_fingerprint)VALUES($1,$2,$3,$4,$5,'queued',$6,$7,$8,$9) RETURNING `+aiJobColumns, id, incident, preview.ID, actor.User.ID, actor.SessionHash, cfg.Model, preview.Hash, key, fingerprint))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return empty, false, analysisai.ErrConflict
		}
		return empty, false, err
	}
	if err = aiAudit(ctx, tx, actor.User.ID, "analysis_confirmed", "analysis:"+id); err != nil {
		return empty, false, err
	}
	return job, false, tx.Commit(ctx)
}
func (s *Store) Job(ctx context.Context, id string) (analysisai.Job, error) {
	return readAIJob(s.pool.QueryRow(ctx, `SELECT `+aiJobColumns+` FROM ai_analyses WHERE id=$1`, id))
}
func (s *Store) Jobs(ctx context.Context, incident string, limit int) ([]analysisai.Job, error) {
	if limit < 1 || limit > 100 {
		return nil, analysisai.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT `+aiJobColumns+` FROM ai_analyses WHERE incident_id=$1 ORDER BY created_at DESC,id LIMIT $2`, incident, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []analysisai.Job{}
	for rows.Next() {
		job, e := readAIJob(rows)
		if e != nil {
			return nil, e
		}
		items = append(items, job)
	}
	return items, rows.Err()
}
func (s *Store) ClaimJob(ctx context.Context, cfg analysisai.Config) (analysisai.Job, analysisai.Preview, error) {
	var job analysisai.Job
	var preview analysisai.Preview
	if !cfg.Enabled {
		return job, preview, analysisai.ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return job, preview, err
	}
	defer tx.Rollback(context.Background())
	var id, actor string
	var session []byte
	err = tx.QueryRow(ctx, `SELECT id,created_by,actor_session_hash FROM ai_analyses WHERE state='queued' ORDER BY created_at,id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id, &actor, &session)
	if errors.Is(err, pgx.ErrNoRows) {
		return job, preview, analysisai.ErrNotFound
	}
	if err != nil {
		return job, preview, err
	}
	failure := ""
	if e := commandActor(ctx, tx, actor, session); e != nil {
		if errors.Is(e, access.ErrForbidden) {
			failure = "actor_no_longer_authorized"
		} else {
			return job, preview, e
		}
	}
	var raw []byte
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT p.preview,p.expires_at FROM ai_previews p JOIN ai_analyses a ON a.preview_id=p.id WHERE a.id=$1`, id).Scan(&raw, &expires)
	if err != nil {
		return job, preview, err
	}
	if err = json.Unmarshal(raw, &preview); err != nil {
		return job, preview, err
	}
	if !expires.After(time.Now()) || preview.Request.Model != cfg.Model || preview.Destination != analysisai.Destination || preview.PromptVersion != analysisai.PromptVersion {
		failure = "preview_expired_or_config_changed"
	}
	if failure != "" {
		if _, err = tx.Exec(ctx, `UPDATE ai_analyses SET state='failed',failure=$2,updated_at=now() WHERE id=$1`, id, failure); err != nil {
			return job, preview, err
		}
		if err = tx.Commit(ctx); err != nil {
			return job, preview, err
		}
		return job, preview, analysisai.ErrNotFound
	}
	job, err = readAIJob(tx.QueryRow(ctx, `UPDATE ai_analyses SET state='running',updated_at=now() WHERE id=$1 RETURNING `+aiJobColumns, id))
	if err != nil {
		return job, preview, err
	}
	return job, preview, tx.Commit(ctx)
}
func (s *Store) FinishJob(ctx context.Context, id string, result *analysisai.Result, failure string, uncertain bool) error {
	if result != nil {
		var raw []byte
		if err := s.pool.QueryRow(ctx, `SELECT p.preview FROM ai_previews p JOIN ai_analyses a ON a.preview_id=p.id WHERE a.id=$1`, id).Scan(&raw); err != nil {
			return err
		}
		var preview analysisai.Preview
		if json.Unmarshal(raw, &preview) != nil {
			return analysisai.ErrInvalid
		}
		if err := analysisai.ValidateResult(result, preview.Payload); err != nil {
			return err
		}
	}
	state := "succeeded"
	if failure != "" {
		state = "failed"
	}
	if uncertain {
		state = "uncertain"
	}
	var encoded []byte
	var err error
	if result != nil {
		encoded, err = json.Marshal(result)
		if err != nil {
			return err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var actor string
	err = tx.QueryRow(ctx, `UPDATE ai_analyses SET state=$2,failure=$3,result=$4,updated_at=now() WHERE id=$1 AND state='running' RETURNING created_by`, id, state, failure, encoded).Scan(&actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return analysisai.ErrConflict
	}
	if err != nil {
		return err
	}
	if err = aiAudit(ctx, tx, actor, "analysis_"+state, "analysis:"+id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) RecoverJobs(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `UPDATE ai_analyses SET state=CASE WHEN state='running' THEN 'uncertain' ELSE 'failed' END,failure=CASE WHEN state='running' THEN 'server_restart_after_dispatch' ELSE 'server_restart_before_dispatch' END,updated_at=now() WHERE state IN ('queued','running')`)
	return err
}
