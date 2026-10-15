package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/encoding/protojson"
)

const snapshotColumns = `agent_id,snapshot_id,origin_command_id,device_id,interface_id,status,reason,initial_observed_admin_up,after_admin_up,previous_state_basis,recorded_at,expires_at,expiry_basis,rollback_command_id`

func readSnapshot(row interface{ Scan(...any) error }) (commands.Snapshot, error) {
	var s commands.Snapshot
	err := row.Scan(&s.AgentID, &s.SnapshotID, &s.OriginCommandID, &s.DeviceID, &s.InterfaceID, &s.Status, &s.Reason, &s.InitialObservedAdminUp, &s.AfterAdminUp, &s.PreviousStateBasis, &s.RecordedAt, &s.ExpiresAt, &s.ExpiryBasis, &s.RollbackCommandID)
	s.RecordedAt = s.RecordedAt.UTC()
	return s, err
}
func (s *Store) CommandSnapshot(ctx context.Context, id string) (commands.Snapshot, error) {
	snapshot, err := readSnapshot(s.pool.QueryRow(ctx, `SELECT `+snapshotColumns+` FROM command_snapshots WHERE origin_command_id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		err = commands.ErrSnapshot
	}
	return snapshot, err
}
func (s *Store) Snapshots(ctx context.Context, agent, device string, limit, offset int) ([]commands.Snapshot, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+snapshotColumns+` FROM command_snapshots WHERE ($1='' OR agent_id=$1)AND($2='' OR device_id=$2)ORDER BY recorded_at DESC,origin_command_id LIMIT $3 OFFSET $4`, agent, device, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []commands.Snapshot{}
	for rows.Next() {
		v, err := readSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) CreateRollback(ctx context.Context, original string, request commands.RollbackRequest, key string) (commands.Record, bool, error) {
	c := commands.Record{}
	if err := commands.ValidateRollback(request); err != nil {
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
	var stored []byte
	err = tx.QueryRow(ctx, `SELECT fingerprint FROM controller_commands WHERE actor_id=$1 AND idempotency_key=$2`, actor.User.ID, key).Scan(&stored)
	if err == nil {
		if !bytes.Equal(stored, commands.RollbackFingerprint(original, request)) {
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
	parent, err := readCommand(tx.QueryRow(ctx, `SELECT `+commandColumns+` FROM controller_commands WHERE id=$1 FOR UPDATE`, original))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, false, commands.ErrNotFound
	}
	if err != nil {
		return c, false, err
	}
	snapshot, err := readSnapshot(tx.QueryRow(ctx, `SELECT `+snapshotColumns+` FROM command_snapshots WHERE origin_command_id=$1 FOR UPDATE`, original))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, false, commands.ErrSnapshot
	}
	if err != nil {
		return c, false, err
	}
	if parent.State != "success" || parent.Result == nil || parent.Result.Uncertain || snapshot.Status != "candidate" || commands.ValidateSnapshotID(snapshot.SnapshotID) != nil || snapshot.AfterAdminUp == nil || *request.ExpectedAdminUp != *snapshot.AfterAdminUp || snapshot.SnapshotID != parent.Result.SnapshotID || snapshot.AgentID != parent.AgentID || snapshot.DeviceID != parent.DeviceID || snapshot.InterfaceID != parent.InterfaceID {
		return c, false, commands.ErrSnapshot
	}
	r := commands.Request{AgentID: parent.AgentID, DeviceID: parent.DeviceID, InterfaceID: parent.InterfaceID, Operation: "rollback", ExpectedAdminUp: request.ExpectedAdminUp, TimeoutSeconds: request.TimeoutSeconds, Confirmed: true}
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
	wireJSON, err := protojson.Marshal(commands.BuildRollback(id, parent.DeviceID, snapshot.SnapshotID, request, now))
	if err != nil {
		return c, false, err
	}
	requestJSON, _ := json.Marshal(r)
	c, err = readCommand(tx.QueryRow(ctx, `INSERT INTO controller_commands(id,agent_id,device_id,interface_id,operation,expected_admin_up,timeout_seconds,idempotency_key,actor_id,actor_session_hash,fingerprint,state,created_at,updated_at,deadline_at,session_id,request,wire_payload,rollback_of_command_id,rollback_snapshot_id)
 VALUES($1,$2,$3,$4,'rollback',$5,$6,$7,$8,$9,$10,'queued',$11,$11,$12,$13,$14,$15,$16,$17)RETURNING `+commandColumns, id, parent.AgentID, parent.DeviceID, canonical, *request.ExpectedAdminUp, request.TimeoutSeconds, key, actor.User.ID, actor.SessionHash, commands.RollbackFingerprint(original, request), now, now.Add(time.Duration(request.TimeoutSeconds)*time.Second), session, requestJSON, wireJSON, original, snapshot.SnapshotID))
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return c, false, commands.ErrBusy
		}
		return c, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE command_snapshots SET status='reserved',reason='rollback_queued',rollback_command_id=$3 WHERE agent_id=$1 AND snapshot_id=$2`, snapshot.AgentID, snapshot.SnapshotID, id); err != nil {
		return c, false, err
	}
	if err = commandEvent(ctx, tx, id, "queued", "operator_requested_rollback"); err != nil {
		return c, false, err
	}
	meta := access.CurrentMeta(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer)VALUES($1,'rollback_created','success',$2,$3,$4)`, actor.User.ID, "command:"+id, meta.RequestID, meta.Peer); err != nil {
		return c, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return c, false, err
	}
	return c, false, nil
}

func rollbackClaim(ctx context.Context, tx pgx.Tx, c commands.Record) error {
	if c.Operation != "rollback" {
		return nil
	}
	if c.RollbackSnapshotID == nil || c.RollbackOfCommandID == nil {
		return commands.ErrSnapshot
	}
	snapshot, err := readSnapshot(tx.QueryRow(ctx, `SELECT `+snapshotColumns+` FROM command_snapshots WHERE agent_id=$1 AND snapshot_id=$2 FOR UPDATE`, c.AgentID, *c.RollbackSnapshotID))
	if errors.Is(err, pgx.ErrNoRows) {
		return commands.ErrSnapshot
	}
	if err != nil {
		return err
	}
	if snapshot.OriginCommandID != *c.RollbackOfCommandID || snapshot.RollbackCommandID == nil || *snapshot.RollbackCommandID != c.ID || snapshot.Status != "reserved" || snapshot.DeviceID != c.DeviceID || snapshot.InterfaceID != c.InterfaceID {
		return commands.ErrSnapshot
	}
	return nil
}
func updateSnapshotResult(ctx context.Context, tx pgx.Tx, c commands.Record, result *commands.Result, state string) error {
	if result.SnapshotID != "" {
		if _, err := tx.Exec(ctx, `UPDATE command_snapshots SET status='superseded',reason='newer_agent_snapshot_reference' WHERE agent_id=$1 AND device_id=$2 AND interface_id=$3 AND snapshot_id<>$4 AND status IN ('candidate','unverified')`, c.AgentID, c.DeviceID, c.InterfaceID, result.SnapshotID); err != nil {
			return err
		}
		status := "unverified"
		var after *bool
		if state == "success" && result.Verification != nil {
			status = "candidate"
			after = result.Verification.ObservedAdminUp
		}
		_, err := tx.Exec(ctx, `INSERT INTO command_snapshots(agent_id,snapshot_id,origin_command_id,device_id,interface_id,status,initial_observed_admin_up,after_admin_up)
  VALUES($1,$2,$3,$4,$5,$6,$7,$8)ON CONFLICT(agent_id,snapshot_id)DO UPDATE SET status=CASE WHEN command_snapshots.status='unverified' AND EXCLUDED.status='candidate' THEN 'candidate' ELSE command_snapshots.status END,after_admin_up=EXCLUDED.after_admin_up WHERE command_snapshots.origin_command_id=EXCLUDED.origin_command_id`, c.AgentID, result.SnapshotID, c.ID, c.DeviceID, c.InterfaceID, status, c.ExpectedAdminUp, after)
		if err != nil {
			return err
		}
	}
	if c.Operation == "rollback" && c.RollbackSnapshotID != nil {
		status := "reserved"
		if state == "success" {
			status = "consumed"
		} else if state == "uncertain" {
			status = "uncertain"
		} else if state != "accepted" && state != "running" {
			status = "unavailable"
		}
		_, err := tx.Exec(ctx, `UPDATE command_snapshots SET status=$3,reason=$4 WHERE agent_id=$1 AND snapshot_id=$2 AND rollback_command_id=$5`, c.AgentID, *c.RollbackSnapshotID, status, "agent_result_"+state, c.ID)
		return err
	}
	return nil
}
func releaseUnsentRollbacks(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `UPDATE command_snapshots s SET status='candidate',reason='rollback_not_sent',rollback_command_id=NULL FROM controller_commands c WHERE s.rollback_command_id=c.id AND s.status='reserved' AND (c.state IN ('expired','rejected')AND c.dispatched_at IS NULL OR c.reason='not_sent_to_agent')`)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE command_snapshots s SET status='uncertain',reason='rollback_delivery_unknown' FROM controller_commands c WHERE s.rollback_command_id=c.id AND s.status='reserved' AND c.state='uncertain'`)
	return err
}
