package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/encoding/protojson"
)

const commandColumns = `id,agent_id,device_id,interface_id,operation,expected_admin_up,timeout_seconds,idempotency_key,actor_id,state,reason,created_at,updated_at,deadline_at,session_id,dispatched_at,result,rollback_of_command_id,rollback_snapshot_id`

func readCommand(row interface{ Scan(...any) error }) (commands.Record, error) {
	var c commands.Record
	var result []byte
	err := row.Scan(&c.ID, &c.AgentID, &c.DeviceID, &c.InterfaceID, &c.Operation, &c.ExpectedAdminUp, &c.TimeoutSeconds, &c.IdempotencyKey, &c.ActorID, &c.State, &c.Reason, &c.CreatedAt, &c.UpdatedAt, &c.DeadlineAt, &c.SessionID, &c.DispatchedAt, &result, &c.RollbackOfCommandID, &c.RollbackSnapshotID)
	if err != nil {
		return c, err
	}
	c.CreatedAt, c.UpdatedAt, c.DeadlineAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC(), c.DeadlineAt.UTC()
	if c.DispatchedAt != nil {
		v := c.DispatchedAt.UTC()
		c.DispatchedAt = &v
	}
	if len(result) > 0 {
		err = json.Unmarshal(result, &c.Result)
	}
	return c, err
}
func commandEvent(ctx context.Context, tx pgx.Tx, id, state, reason string) error {
	_, err := tx.Exec(ctx, `INSERT INTO command_events(command_id,state,reason)VALUES($1,$2,$3)`, id, state, reason)
	return err
}
func commandActor(ctx context.Context, tx pgx.Tx, id string, hash []byte) error {
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM access_users u JOIN access_sessions s ON s.user_id=u.id WHERE u.id=$1 AND s.token_hash=$2 AND s.expires_at>now() AND NOT u.disabled AND u.role IN ('operator','admin'))`, id, hash).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return access.ErrForbidden
	}
	return nil
}
func (s *Store) ConfigureCommands(policy commands.Policy) { s.commandPolicy = policy }
func (s *Store) CommandPolicy() commands.Policy           { return s.commandPolicy }
func (s *Store) CreateCommand(ctx context.Context, r commands.Request, key string) (commands.Record, bool, error) {
	c := commands.Record{}
	if err := commands.ValidateRequest(r); err != nil {
		return c, false, err
	}
	if err := commands.ValidateKey(key); err != nil {
		return c, false, err
	}
	actor, ok := access.Current(ctx)
	if !ok {
		return c, false, access.ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return c, false, err
	}
	defer tx.Rollback(context.Background())
	if err = commandActor(ctx, tx, actor.User.ID, actor.SessionHash); err != nil {
		return c, false, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,913))`, actor.User.ID+":"+key); err != nil {
		return c, false, err
	}
	var original []byte
	err = tx.QueryRow(ctx, `SELECT fingerprint FROM controller_commands WHERE actor_id=$1 AND idempotency_key=$2`, actor.User.ID, key).Scan(&original)
	if err == nil {
		if !bytes.Equal(original, commands.FingerprintRequest(r)) {
			return c, false, commands.ErrConflict
		}
		c, err = readCommand(tx.QueryRow(ctx, `SELECT `+commandColumns+` FROM controller_commands WHERE actor_id=$1 AND idempotency_key=$2`, actor.User.ID, key))
		if err != nil {
			return c, false, err
		}
		return c, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return c, false, err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	session, canonical, err := s.commandPreflight(ctx, tx, r, now)
	if err != nil {
		return c, false, err
	}
	id, err := access.NewID()
	if err != nil {
		return c, false, err
	}
	id = strings.Replace(id, "usr_", "cmd_", 1)
	resolved := r
	resolved.InterfaceID = canonical
	wire := commands.Build(id, id, resolved, now) // Agent key uses globally unique command ID, not actor-scoped client key.
	wireJSON, err := protojson.Marshal(wire)
	if err != nil {
		return c, false, err
	}
	requestJSON, _ := json.Marshal(r)
	c, err = readCommand(tx.QueryRow(ctx, `INSERT INTO controller_commands(id,agent_id,device_id,interface_id,operation,expected_admin_up,timeout_seconds,idempotency_key,actor_id,actor_session_hash,fingerprint,state,created_at,updated_at,deadline_at,session_id,request,wire_payload)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'queued',$12,$12,$13,$14,$15,$16) RETURNING `+commandColumns, id, r.AgentID, r.DeviceID, canonical, r.Operation, *r.ExpectedAdminUp, r.TimeoutSeconds, key, actor.User.ID, actor.SessionHash, commands.FingerprintRequest(r), now, now.Add(time.Duration(r.TimeoutSeconds)*time.Second), session, requestJSON, wireJSON))
	if err != nil {
		var p *pgconn.PgError
		if errors.As(err, &p) && p.Code == "23505" {
			return c, false, commands.ErrBusy
		}
		return c, false, err
	}
	if err = commandEvent(ctx, tx, id, "queued", "operator_requested"); err != nil {
		return c, false, err
	}
	meta := access.CurrentMeta(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer)VALUES($1,'command_created','success',$2,$3,$4)`, actor.User.ID, "command:"+id, meta.RequestID, meta.Peer); err != nil {
		return c, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return c, false, err
	}
	return c, false, nil
}
func (s *Store) Command(ctx context.Context, id string) (commands.Record, error) {
	c, err := readCommand(s.pool.QueryRow(ctx, `SELECT `+commandColumns+` FROM controller_commands WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		err = commands.ErrNotFound
	}
	return c, err
}
func (s *Store) Commands(ctx context.Context, f commands.Filter) ([]commands.Record, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+commandColumns+` FROM controller_commands WHERE ($1='' OR agent_id=$1) AND ($2='' OR device_id=$2) AND ($3='' OR state=$3) ORDER BY created_at DESC,id LIMIT $4 OFFSET $5`, f.AgentID, f.DeviceID, f.State, f.Limit, f.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []commands.Record{}
	for rows.Next() {
		c, err := readCommand(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) CommandEvents(ctx context.Context, id string) ([]commands.Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,command_id,state,reason,to_char(at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') FROM command_events WHERE command_id=$1 ORDER BY id LIMIT 1000`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []commands.Event{}
	for rows.Next() {
		var e commands.Event
		if err = rows.Scan(&e.ID, &e.CommandID, &e.State, &e.Reason, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) QueuedCommands(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM controller_commands WHERE state='queued' ORDER BY created_at LIMIT 25`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (s *Store) ClaimCommand(ctx context.Context, id string) (commands.Record, *pb.Command, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return commands.Record{}, nil, err
	}
	defer tx.Rollback(context.Background())
	c, err := readCommand(tx.QueryRow(ctx, `SELECT `+commandColumns+` FROM controller_commands WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return c, nil, err
	}
	if c.State != "queued" {
		return c, nil, nil
	}
	var hash, requestJSON, wireJSON []byte
	if err = tx.QueryRow(ctx, `SELECT actor_session_hash,request,wire_payload FROM controller_commands WHERE id=$1`, id).Scan(&hash, &requestJSON, &wireJSON); err != nil {
		return c, nil, err
	}
	state, reason := "dispatching", "send_started"
	var r commands.Request
	if err = json.Unmarshal(requestJSON, &r); err != nil {
		return c, nil, err
	}
	if !c.DeadlineAt.After(time.Now()) {
		state, reason = "expired", "deadline_before_send"
	} else {
		actorErr := commandActor(ctx, tx, c.ActorID, hash)
		if actorErr != nil && !errors.Is(actorErr, access.ErrForbidden) {
			return c, nil, actorErr
		}
		if actorErr != nil {
			state, reason = "rejected", "actor_access_revoked"
		} else {
			if err = rollbackClaim(ctx, tx, c); err != nil && !errors.Is(err, commands.ErrSnapshot) {
				return c, nil, err
			}
			if err != nil {
				state, reason = "rejected", "rollback_snapshot_changed"
			}
			session, canonical, e := s.commandPreflight(ctx, tx, r, time.Now())
			if e != nil && !errors.Is(e, commands.ErrPrecondition) && !errors.Is(e, commands.ErrProtected) && !errors.Is(e, commands.ErrDisabled) {
				return c, nil, e
			}
			if e != nil || session != c.SessionID || canonical != c.InterfaceID {
				state, reason = "rejected", "preflight_changed_before_send"
			}
		}
	}
	c, err = readCommand(tx.QueryRow(ctx, `UPDATE controller_commands SET state=$2,reason=$3,updated_at=now(),dispatched_at=CASE WHEN $2='dispatching' THEN now() ELSE NULL END WHERE id=$1 RETURNING `+commandColumns, id, state, reason))
	if err != nil {
		return c, nil, err
	}
	if err = commandEvent(ctx, tx, id, state, reason); err != nil {
		return c, nil, err
	}
	if err = releaseUnsentRollbacks(ctx, tx); err != nil {
		return c, nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return c, nil, err
	}
	if state != "dispatching" {
		return c, nil, nil
	}
	wire := &pb.Command{}
	if err = protojson.Unmarshal(wireJSON, wire); err != nil {
		return c, nil, err
	}
	return c, wire, nil
}
func (s *Store) FinishCommandSend(ctx context.Context, id string, attempted bool, sendErr error) error {
	state, reason := "sent", "awaiting_agent_result"
	if sendErr != nil {
		if attempted {
			state, reason = "uncertain", "send_outcome_unknown"
		} else {
			state, reason = "rejected", "not_sent_to_agent"
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	result, err := tx.Exec(ctx, `UPDATE controller_commands SET state=$2,reason=$3,updated_at=now() WHERE id=$1 AND state='dispatching'`, id, state, reason)
	if err != nil {
		return err
	}
	if result.RowsAffected() > 0 {
		if err = commandEvent(ctx, tx, id, state, reason); err != nil {
			return err
		}
	}
	if err = releaseUnsentRollbacks(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) RecoverCommands(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	_, err = tx.Exec(ctx, `WITH changed AS(UPDATE controller_commands SET state=CASE WHEN state='queued' THEN 'rejected' ELSE 'uncertain' END,reason='controller_restarted',updated_at=now() WHERE state IN ('queued','dispatching','sent','accepted','running') RETURNING id,state,reason) INSERT INTO command_events(command_id,state,reason)SELECT id,state,reason FROM changed`)
	if err != nil {
		return err
	}
	if err = releaseUnsentRollbacks(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) ExpireCommands(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	_, err = tx.Exec(ctx, `WITH changed AS(UPDATE controller_commands SET state=CASE WHEN state='queued' THEN 'expired' ELSE 'uncertain' END,reason=CASE WHEN state='queued' THEN 'deadline_before_send' ELSE 'agent_result_missing' END,updated_at=now() WHERE state='queued' AND deadline_at<now() OR state IN ('dispatching','sent','accepted','running') AND deadline_at+interval '5 seconds'<now() RETURNING id,state,reason) INSERT INTO command_events(command_id,state,reason)SELECT id,state,reason FROM changed`)
	if err != nil {
		return err
	}
	if err = releaseUnsentRollbacks(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
