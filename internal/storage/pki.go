package storage

import (
	"context"
	"errors"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/pki"
	"github.com/jackc/pgx/v5"
)

func pkiLock(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('pki:' || $1,0))`, id)
	return err
}
func pkiAdmin(ctx context.Context, tx pgx.Tx) (access.Principal, error) {
	actor, ok := access.Current(ctx)
	if !ok {
		return actor, access.ErrForbidden
	}
	return actor, accessAdminTx(ctx, tx, actor)
}
func pkiAuditTx(ctx context.Context, tx pgx.Tx, id, actor, action, resource string) error {
	var owner *string
	if actor != "" {
		owner = &actor
	}
	_, err := tx.Exec(ctx, `INSERT INTO pki_audit(agent_id,actor_id,action,resource) VALUES($1,$2,$3,$4)`, id, owner, action, resource)
	if err != nil || actor == "" {
		return err
	}
	meta := access.CurrentMeta(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,$2,'success',$3,$4,$5)`, actor, "pki_"+action, resource, meta.RequestID, meta.Peer)
	return err
}
func (s *Store) PKICreateToken(ctx context.Context, token pki.Token) error {
	if _, err := pki.AgentURI(token.AgentID); err != nil {
		return pki.ErrDenied
	}
	if len(token.Hash) != 32 || len(token.ID) != 32 || token.ExpiresAt.Sub(token.CreatedAt) < time.Minute || token.ExpiresAt.Sub(token.CreatedAt) > 24*time.Hour {
		return pki.ErrDenied
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	actor, err := pkiAdmin(ctx, tx)
	if err != nil {
		return err
	}
	if err = pkiLock(ctx, tx, token.AgentID); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM pki_bootstrap_tokens WHERE agent_id=$1 AND used_at IS NULL AND revoked_at IS NULL AND expires_at>now()`, token.AgentID).Scan(&count); err != nil {
		return err
	}
	if count >= 16 {
		return pki.ErrConflict
	}
	if _, err = tx.Exec(ctx, `INSERT INTO pki_bootstrap_tokens(id,agent_id,token_hash,created_by,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6)`, token.ID, token.AgentID, token.Hash, actor.User.ID, token.CreatedAt, token.ExpiresAt); err != nil {
		return err
	}
	if err = pkiAuditTx(ctx, tx, token.AgentID, actor.User.ID, "token_created", "token:"+token.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) PKITokens(ctx context.Context, id string, limit int) ([]pki.Token, error) {
	if limit < 1 || limit > 200 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT id,agent_id,created_at,expires_at,used_at,revoked_at FROM pki_bootstrap_tokens WHERE ($1='' OR agent_id=$1) ORDER BY created_at DESC,id LIMIT $2`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []pki.Token{}
	for rows.Next() {
		var v pki.Token
		if err = rows.Scan(&v.ID, &v.AgentID, &v.CreatedAt, &v.ExpiresAt, &v.UsedAt, &v.RevokedAt); err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}
func (s *Store) PKIRevokeToken(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	actor, err := pkiAdmin(ctx, tx)
	if err != nil {
		return err
	}
	var agent string
	if err = tx.QueryRow(ctx, `SELECT agent_id FROM pki_bootstrap_tokens WHERE id=$1`, id).Scan(&agent); errors.Is(err, pgx.ErrNoRows) {
		return pki.ErrNotFound
	} else if err != nil {
		return err
	}
	if err = pkiLock(ctx, tx, agent); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE pki_bootstrap_tokens SET revoked_at=now() WHERE id=$1 AND revoked_at IS NULL AND used_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pki.ErrConflict
	}
	if err = pkiAuditTx(ctx, tx, agent, actor.User.ID, "token_revoked", "token:"+id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) PKICommitIssue(ctx context.Context, id string, hash []byte, previous string, issued pki.Issued) error {
	if _, err := pki.AgentURI(id); err != nil {
		return pki.ErrDenied
	}
	if (previous == "" && len(hash) != 32) || (previous != "" && len(hash) != 0) || len(issued.Fingerprint) != 64 || !issued.NotAfter.After(time.Now()) {
		return pki.ErrDenied
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err = pkiLock(ctx, tx, id); err != nil {
		return err
	}
	action := "enrolled"
	var parent *string
	resource := ""
	if previous == "" {
		if err = tx.QueryRow(ctx, `UPDATE pki_bootstrap_tokens SET used_at=now() WHERE token_hash=$1 AND agent_id=$2 AND expires_at>now() AND used_at IS NULL AND revoked_at IS NULL RETURNING id`, hash, id).Scan(&resource); errors.Is(err, pgx.ErrNoRows) {
			return pki.ErrDenied
		} else if err != nil {
			return err
		}
	} else {
		var valid bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pki_certificates WHERE fingerprint=$1 AND agent_id=$2 AND revoked_at IS NULL AND not_before<=now() AND not_after>now())`, previous, id).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return pki.ErrDenied
		}
		parent = &previous
		action = "renewed"
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM pki_certificates WHERE agent_id=$1 AND revoked_at IS NULL AND not_after>now()`, id).Scan(&count); err != nil {
		return err
	}
	if count >= 16 {
		return pki.ErrConflict
	}
	if _, err = tx.Exec(ctx, `INSERT INTO pki_certificates(fingerprint,agent_id,serial,not_before,not_after,renewed_from) VALUES($1,$2,$3,$4,$5,$6)`, issued.Fingerprint, id, issued.Serial, issued.NotBefore, issued.NotAfter, parent); err != nil {
		return err
	}
	if err = pkiAuditTx(ctx, tx, id, "", action, "certificate:"+issued.Fingerprint); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) PKICertificateActive(ctx context.Context, id, fingerprint string) error {
	var active bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pki_certificates WHERE agent_id=$1 AND fingerprint=$2 AND revoked_at IS NULL AND not_before<=now() AND not_after>now())`, id, fingerprint).Scan(&active)
	if err != nil {
		return err
	}
	if !active {
		return pki.ErrDenied
	}
	return nil
}
func (s *Store) PKICertificates(ctx context.Context, id string, limit int) ([]pki.Certificate, error) {
	if limit < 1 || limit > 200 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT fingerprint,agent_id,serial,not_before,not_after,created_at,renewed_from,revoked_at FROM pki_certificates WHERE ($1='' OR agent_id=$1) ORDER BY created_at DESC,fingerprint LIMIT $2`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []pki.Certificate{}
	for rows.Next() {
		var v pki.Certificate
		if err = rows.Scan(&v.Fingerprint, &v.AgentID, &v.Serial, &v.NotBefore, &v.NotAfter, &v.CreatedAt, &v.RenewedFrom, &v.RevokedAt); err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}
func (s *Store) PKIRevokeCertificate(ctx context.Context, fingerprint string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.Background())
	actor, err := pkiAdmin(ctx, tx)
	if err != nil {
		return "", err
	}
	var agent string
	if err = tx.QueryRow(ctx, `SELECT agent_id FROM pki_certificates WHERE fingerprint=$1`, fingerprint).Scan(&agent); errors.Is(err, pgx.ErrNoRows) {
		return "", pki.ErrNotFound
	} else if err != nil {
		return "", err
	}
	if err = pkiLock(ctx, tx, agent); err != nil {
		return "", err
	}
	tag, err := tx.Exec(ctx, `UPDATE pki_certificates SET revoked_at=now() WHERE fingerprint=$1 AND revoked_at IS NULL`, fingerprint)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", pki.ErrConflict
	}
	// Revocation is per certificate. Other issued certificates retain validity.
	if err = pkiAuditTx(ctx, tx, agent, actor.User.ID, "certificate_revoked", "certificate:"+fingerprint); err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	return agent, nil
}
func (s *Store) PKIAudit(ctx context.Context, id string, limit int) ([]pki.Audit, error) {
	if limit < 1 || limit > 200 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT id,agent_id,actor_id,action,resource,at FROM pki_audit WHERE ($1='' OR agent_id=$1) ORDER BY id DESC LIMIT $2`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []pki.Audit{}
	for rows.Next() {
		var v pki.Audit
		if err = rows.Scan(&v.ID, &v.AgentID, &v.ActorID, &v.Action, &v.Resource, &v.At); err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}
