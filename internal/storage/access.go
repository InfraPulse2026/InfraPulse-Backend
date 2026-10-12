package storage

import (
	"context"
	"errors"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const accessUserColumns = `id,username,role,disabled,created_at,updated_at,last_login_at`

func readAccessUser(row interface{ Scan(...any) error }) (access.User, error) {
	var u access.User
	err := row.Scan(&u.ID, &u.Username, &u.Role, &u.Disabled, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt)
	u.CreatedAt, u.UpdatedAt = u.CreatedAt.UTC(), u.UpdatedAt.UTC()
	if u.LastLoginAt != nil {
		v := u.LastLoginAt.UTC()
		u.LastLoginAt = &v
	}
	return u, err
}
func accessAuditTx(ctx context.Context, tx pgx.Tx, actor, target, action, outcome string, meta access.Meta) error {
	var a, b *string
	if actor != "" {
		a = &actor
	}
	if target != "" {
		b = &target
	}
	_, err := tx.Exec(ctx, `INSERT INTO access_audit(actor_id,target_id,action,outcome,request_id,peer) VALUES($1,$2,$3,$4,$5,$6)`, a, b, action, outcome, meta.RequestID, meta.Peer)
	return err
}
func accessAdminTx(ctx context.Context, tx pgx.Tx, p access.Principal) error {
	var admin bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM access_users u JOIN access_sessions s ON s.user_id=u.id WHERE u.id=$1 AND u.role='admin' AND NOT u.disabled AND s.token_hash=$2 AND s.expires_at>now())`, p.User.ID, p.SessionHash).Scan(&admin)
	if err != nil {
		return err
	}
	if !admin {
		return access.ErrForbidden
	}
	return nil
}
func (s *Store) AccessLoginRecord(ctx context.Context, name string) (access.LoginRecord, error) {
	var r access.LoginRecord
	err := s.pool.QueryRow(ctx, `SELECT `+accessUserColumns+`,password_hash FROM access_users WHERE username=$1`, name).Scan(&r.ID, &r.Username, &r.Role, &r.Disabled, &r.CreatedAt, &r.UpdatedAt, &r.LastLoginAt, &r.PasswordHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, access.ErrCredentials
	}
	return r, err
}
func (s *Store) AccessStartSession(ctx context.Context, r access.LoginRecord, hash []byte, expires time.Time, meta access.Meta) (access.Principal, error) {
	out := access.Principal{}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	var currentHash string
	var disabled bool
	if err = tx.QueryRow(ctx, `SELECT password_hash,disabled FROM access_users WHERE id=$1 FOR UPDATE`, r.ID).Scan(&currentHash, &disabled); err != nil {
		return out, err
	}
	if disabled || currentHash != r.PasswordHash {
		return out, access.ErrCredentials
	}
	// Bound persisted sessions to 20 per user; discard expired/oldest credentials.
	if _, err = tx.Exec(ctx, `DELETE FROM access_sessions WHERE user_id=$1 AND (expires_at<=now() OR token_hash IN (SELECT token_hash FROM access_sessions WHERE user_id=$1 ORDER BY created_at DESC,token_hash OFFSET 19))`, r.ID); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO access_sessions(token_hash,user_id,expires_at) VALUES($1,$2,$3)`, hash, r.ID, expires); err != nil {
		return out, err
	}
	out.User, err = readAccessUser(tx.QueryRow(ctx, `UPDATE access_users SET last_login_at=now() WHERE id=$1 RETURNING `+accessUserColumns, r.ID))
	if err != nil {
		return out, err
	}
	if err = accessAuditTx(ctx, tx, r.ID, r.ID, "login", "success", meta); err != nil {
		return out, err
	}
	if err = tx.Commit(ctx); err != nil {
		return out, err
	}
	out.ExpiresAt, out.SessionHash = expires.UTC(), hash
	return out, nil
}
func (s *Store) AccessSession(ctx context.Context, hash []byte) (access.Principal, error) {
	var p access.Principal
	err := s.pool.QueryRow(ctx, `SELECT u.id,u.username,u.role,u.disabled,u.created_at,u.updated_at,u.last_login_at,s.expires_at
 FROM access_sessions s JOIN access_users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at>now() AND NOT u.disabled`, hash).Scan(&p.User.ID, &p.User.Username, &p.User.Role, &p.User.Disabled, &p.User.CreatedAt, &p.User.UpdatedAt, &p.User.LastLoginAt, &p.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, access.ErrCredentials
	}
	p.SessionHash = hash
	p.ExpiresAt = p.ExpiresAt.UTC()
	p.User.CreatedAt, p.User.UpdatedAt = p.User.CreatedAt.UTC(), p.User.UpdatedAt.UTC()
	if p.User.LastLoginAt != nil {
		v := p.User.LastLoginAt.UTC()
		p.User.LastLoginAt = &v
	}
	return p, err
}
func (s *Store) AccessLogout(ctx context.Context, p access.Principal, all bool, meta access.Meta) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if all {
		_, err = tx.Exec(ctx, `DELETE FROM access_sessions WHERE user_id=$1`, p.User.ID)
	} else {
		_, err = tx.Exec(ctx, `DELETE FROM access_sessions WHERE token_hash=$1 AND user_id=$2`, p.SessionHash, p.User.ID)
	}
	if err != nil {
		return err
	}
	action := "logout"
	if all {
		action = "logout_all"
	}
	if err = accessAuditTx(ctx, tx, p.User.ID, p.User.ID, action, "success", meta); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) AccessLoginFailure(ctx context.Context, meta access.Meta) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO access_audit(action,outcome,request_id,peer) VALUES('login','failure',$1,$2)`, meta.RequestID, meta.Peer)
	return err
}
func (s *Store) AccessUsers(ctx context.Context, limit, offset int) ([]access.User, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+accessUserColumns+` FROM access_users ORDER BY username LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []access.User{}
	for rows.Next() {
		u, err := readAccessUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
func (s *Store) AccessCreateUser(ctx context.Context, actor access.Principal, name, role, hash string, bootstrap bool, meta access.Meta) (access.User, error) {
	out := access.User{}
	name, err := access.Username(name)
	if err != nil {
		return out, err
	}
	if !access.ValidRole(role) {
		return out, errors.New("invalid role")
	}
	id, err := access.NewID()
	if err != nil {
		return out, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194002)`); err != nil {
		return out, err
	}
	actorID := actor.User.ID
	action := "user_created"
	if bootstrap {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM access_users WHERE role='admin' AND NOT disabled)`).Scan(&exists); err != nil {
			return out, err
		}
		if exists {
			return out, access.ErrBootstrap
		}
		if role != "admin" {
			return out, access.ErrForbidden
		}
		actorID = ""
		action = "admin_bootstrap"
	} else {
		if err = accessAdminTx(ctx, tx, actor); err != nil {
			return out, err
		}
	}
	out, err = readAccessUser(tx.QueryRow(ctx, `INSERT INTO access_users(id,username,role,password_hash) VALUES($1,$2,$3,$4) RETURNING `+accessUserColumns, id, name, role, hash))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return out, access.ErrConflict
		}
		return out, err
	}
	if err = accessAuditTx(ctx, tx, actorID, id, action, "success", meta); err != nil {
		return out, err
	}
	if err = tx.Commit(ctx); err != nil {
		return out, err
	}
	return out, nil
}
func (s *Store) AccessUpdateUser(ctx context.Context, actor access.Principal, id string, role *string, disabled *bool, meta access.Meta) (access.User, error) {
	out := access.User{}
	if role != nil && !access.ValidRole(*role) {
		return out, errors.New("invalid role")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194002)`); err != nil {
		return out, err
	}
	if err = accessAdminTx(ctx, tx, actor); err != nil {
		return out, err
	}
	out, err = readAccessUser(tx.QueryRow(ctx, `SELECT `+accessUserColumns+` FROM access_users WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return out, access.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	newRole, newDisabled := out.Role, out.Disabled
	if role != nil {
		newRole = *role
	}
	if disabled != nil {
		newDisabled = *disabled
	}
	if out.Role == "admin" && !out.Disabled && (newRole != "admin" || newDisabled) {
		var count int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM access_users WHERE role='admin' AND NOT disabled`).Scan(&count); err != nil {
			return out, err
		}
		if count <= 1 {
			return out, access.ErrLastAdmin
		}
	}
	if newRole == out.Role && newDisabled == out.Disabled {
		return out, tx.Commit(ctx)
	}
	out, err = readAccessUser(tx.QueryRow(ctx, `UPDATE access_users SET role=$2,disabled=$3,updated_at=now() WHERE id=$1 RETURNING `+accessUserColumns, id, newRole, newDisabled))
	if err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM access_sessions WHERE user_id=$1`, id); err != nil {
		return out, err
	}
	if err = accessAuditTx(ctx, tx, actor.User.ID, id, "user_access_changed", "success", meta); err != nil {
		return out, err
	}
	if err = tx.Commit(ctx); err != nil {
		return out, err
	}
	return out, nil
}
func (s *Store) AccessSetPassword(ctx context.Context, actor access.Principal, id, hash, oldHash string, meta access.Meta) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194002)`); err != nil {
		return err
	}
	if oldHash == "" {
		if err = accessAdminTx(ctx, tx, actor); err != nil {
			return err
		}
	} else {
		if id != actor.User.ID {
			return access.ErrForbidden
		}
		var valid bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM access_users u JOIN access_sessions s ON s.user_id=u.id WHERE u.id=$1 AND u.password_hash=$2 AND NOT u.disabled AND s.token_hash=$3 AND s.expires_at>now())`, id, oldHash, actor.SessionHash).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return access.ErrCredentials
		}
	}
	result, err := tx.Exec(ctx, `UPDATE access_users SET password_hash=$2,updated_at=now() WHERE id=$1`, id, hash)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return access.ErrNotFound
	}
	if _, err = tx.Exec(ctx, `DELETE FROM access_sessions WHERE user_id=$1`, id); err != nil {
		return err
	}
	if err = accessAuditTx(ctx, tx, actor.User.ID, id, "password_changed", "success", meta); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) AccessAudit(ctx context.Context, limit, offset int) ([]access.Audit, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,actor_id,target_id,action,outcome,resource,request_id,peer,at FROM access_audit ORDER BY id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []access.Audit{}
	for rows.Next() {
		var a access.Audit
		if err = rows.Scan(&a.ID, &a.ActorID, &a.TargetID, &a.Action, &a.Outcome, &a.Resource, &a.RequestID, &a.Peer, &a.At); err != nil {
			return nil, err
		}
		a.At = a.At.UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

func monitoringAccessTx(ctx context.Context, tx pgx.Tx, resource string) error {
	p, ok := access.Current(ctx)
	if !ok {
		return nil
	} // Trusted internal/test calls have no HTTP identity.
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM access_users u JOIN access_sessions s ON s.user_id=u.id WHERE u.id=$1 AND u.role IN ('operator','admin') AND NOT u.disabled AND s.token_hash=$2 AND s.expires_at>now())`, p.User.ID, p.SessionHash).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return access.ErrForbidden
	}
	meta := access.CurrentMeta(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,'monitoring_changed','success',$2,$3,$4)`, p.User.ID, resource, meta.RequestID, meta.Peer)
	return err
}
