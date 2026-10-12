package storage

import (
	"context"
	"encoding/json"
	"errors"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	cc "github.com/InfraPulse2026/InfraPulse-Backend/internal/collectorconfig"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
	"math"
	"strconv"
	"strings"
	"time"
)

func (s *Store) ConfigureCollectorConfig(enabled bool) { s.collectorConfigEnabled = enabled }

const configRevisionColumns = `collector_id,revision,digest,snapshot,site,actor_id,reason,created_at`

func readConfigRevision(row interface{ Scan(...any) error }) (cc.Revision, error) {
	var v cc.Revision
	err := row.Scan(&v.CollectorID, &v.Revision, &v.Digest, &v.Snapshot, &v.Site, &v.ActorID, &v.Reason, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return v, err
}
func (s *Store) CollectorConfig(ctx context.Context, id string) (cc.State, error) {
	out := cc.State{CollectorID: id, Enabled: s.collectorConfigEnabled}
	err := s.pool.QueryRow(ctx, `SELECT a.connected AND a.last_seen>now()-make_interval(secs=>$2),a.session_id,
 COALESCE((a.hello->>'managed_config_v1')::boolean,false),COALESCE((a.hello->>'remote_management_enabled')::boolean,false),
 COALESCE(c.applied_revision,0),COALESCE(c.applied_digest,''),c.applied_at,COALESCE(c.verified_session_id,''),COALESCE(c.status,'none'),COALESCE(c.status_revision,0),COALESCE(c.error_code,''),COALESCE(c.highest_reported_revision,0)::text
 FROM agents a LEFT JOIN collector_config_state c ON c.collector_id=a.agent_id WHERE a.agent_id=$1`, id, s.timeout.Seconds()).Scan(&out.Connected, &out.SessionID, &out.ManagedConfigV1, &out.RemoteManagementEnabled, &out.AppliedRevision, &out.AppliedDigest, &out.AppliedAt, &out.VerifiedSessionID, &out.Status, &out.StatusRevision, &out.ErrorCode, &out.HighestReportedRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	revision, err := readConfigRevision(s.pool.QueryRow(ctx, `SELECT `+configRevisionColumns+` FROM collector_config_revisions WHERE collector_id=$1 ORDER BY revision DESC LIMIT 1`, id))
	if errors.Is(err, ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Desired = &revision
	out.InSync = out.Connected && out.ManagedConfigV1 && out.RemoteManagementEnabled && out.VerifiedSessionID == out.SessionID && out.AppliedRevision == revision.Revision && out.AppliedDigest == revision.Digest && out.Status == "applied" && out.StatusRevision == revision.Revision
	return out, nil
}
func (s *Store) CollectorConfigHistory(ctx context.Context, id string, limit, offset int) ([]cc.Revision, error) {
	if !cc.Identifier(id) || limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, cc.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT `+configRevisionColumns+` FROM collector_config_revisions WHERE collector_id=$1 ORDER BY revision DESC LIMIT $2 OFFSET $3`, id, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []cc.Revision{}
	for rows.Next() {
		v, e := readConfigRevision(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) CollectorConfigEvents(ctx context.Context, id string, limit, offset int) ([]cc.Event, error) {
	if !cc.Identifier(id) || limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, cc.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT id,collector_id,session_id,revision::text,digest,status,correlated,error_code,created_at FROM collector_config_events WHERE collector_id=$1 ORDER BY id DESC LIMIT $2 OFFSET $3`, id, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []cc.Event{}
	for rows.Next() {
		var v cc.Event
		if err = rows.Scan(&v.ID, &v.CollectorID, &v.SessionID, &v.Revision, &v.Digest, &v.Status, &v.Correlated, &v.ErrorCode, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) PutCollectorConfig(ctx context.Context, id string, in cc.Input, key string) (cc.Revision, bool, error) {
	var out cc.Revision
	if !s.collectorConfigEnabled {
		return out, false, cc.ErrDisabled
	}
	if !cc.Identifier(id) || in.ExpectedRevision < 0 || !in.Confirmed || len(strings.TrimSpace(in.Reason)) < 3 || len(in.Reason) > 512 || commands.ValidateKey(key) != nil || len(in.Targets) == 0 || len(in.Targets) > cc.MaxBytes || !cc.ValidText(in.Site, 128) {
		return out, false, cc.ErrInvalid
	}
	snapshot := &pb.DesiredConfigSnapshot{CollectorId: id}
	raw, _ := json.Marshal(map[string]json.RawMessage{"targets": in.Targets})
	if err := protojson.Unmarshal(raw, snapshot); err != nil {
		return out, false, cc.ErrInvalid
	}
	snapshot.CollectorId = id
	if len(snapshot.Targets) == 0 && !in.AllowEmpty {
		return out, false, cc.ErrInvalid
	}
	if in.Site != "" {
		for _, target := range snapshot.Targets {
			if target == nil {
				return out, false, cc.ErrInvalid
			}
			if target.Location == nil {
				target.Location = &pb.PhysicalLocation{}
			}
			if target.Location.Site == "" {
				target.Location.Site = in.Site
			}
		}
	}
	snapshot, err := cc.Normalize(snapshot)
	if err != nil || cc.Validate(snapshot) != nil {
		return out, false, cc.ErrInvalid
	}
	digest, err := cc.Digest(snapshot)
	if err != nil {
		return out, false, err
	}
	// Hash includes operation/path, optimistic revision and confirmation, in
	// addition to normalized target content. A key cannot be reused elsewhere.
	hash := decisionRequestHash("collector_config:"+id, struct {
		Digest       string
		Expected     int64
		Reason, Site string
		AllowEmpty   bool
	}{digest, in.ExpectedRevision, in.Reason, in.Site, in.AllowEmpty})
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback(context.Background())
	actor, ok := access.Current(ctx)
	if !ok {
		return out, false, access.ErrForbidden
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194002)`); err != nil {
		return out, false, err
	}
	if err = accessAdminTx(ctx, tx, actor); err != nil {
		return out, false, err
	}
	var previousHash string
	err = tx.QueryRow(ctx, `SELECT request_hash FROM collector_config_revisions WHERE actor_id=$1 AND idempotency_key=$2`, actor.User.ID, key).Scan(&previousHash)
	if err == nil {
		if previousHash != hash {
			return out, false, cc.ErrConflict
		}
		out, err = readConfigRevision(tx.QueryRow(ctx, `SELECT `+configRevisionColumns+` FROM collector_config_revisions WHERE actor_id=$1 AND idempotency_key=$2`, actor.User.ID, key))
		if err != nil {
			return out, false, err
		}
		return out, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, false, err
	}
	var capable bool
	err = tx.QueryRow(ctx, `SELECT COALESCE((hello->>'managed_config_v1')::boolean,false) AND COALESCE((hello->>'remote_management_enabled')::boolean,false) AND COALESCE((hello->>'application_ack')::boolean,false) FROM agents WHERE agent_id=$1 FOR UPDATE`, id).Scan(&capable)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, false, ErrNotFound
	}
	if err != nil {
		return out, false, err
	}
	if !capable {
		return out, false, cc.ErrConflict
	}
	if _, err = tx.Exec(ctx, `INSERT INTO collector_config_state(collector_id) VALUES($1) ON CONFLICT DO NOTHING`, id); err != nil {
		return out, false, err
	}
	var current int64
	var highest string
	if err = tx.QueryRow(ctx, `SELECT desired_revision,highest_reported_revision::text FROM collector_config_state WHERE collector_id=$1 FOR UPDATE`, id).Scan(&current, &highest); err != nil {
		return out, false, err
	}
	seen, err := strconv.ParseUint(highest, 10, 64)
	if err != nil || seen >= math.MaxInt64 || current == math.MaxInt64 || current != in.ExpectedRevision {
		return out, false, cc.ErrConflict
	}
	next := current + 1
	if uint64(next) <= seen {
		next = int64(seen) + 1
	}
	snapshot.Revision = uint64(next)
	snapshot.ConfigurationDigest = digest
	snapshot.IssuedAt = timestamppb.Now()
	payload, err := wireJSON(snapshot)
	if err != nil {
		return out, false, err
	}
	reason := logbook.SanitizeText(in.Reason)
	out, err = readConfigRevision(tx.QueryRow(ctx, `INSERT INTO collector_config_revisions(collector_id,revision,digest,snapshot,site,actor_id,reason,idempotency_key,request_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+configRevisionColumns, id, next, digest, payload, in.Site, actor.User.ID, reason, key, hash))
	if err != nil {
		return out, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE collector_config_state SET desired_revision=$2,status='pending',status_revision=$2,error_code='' WHERE collector_id=$1`, id, next); err != nil {
		return out, false, err
	}
	if err = accessAuditTx(ctx, tx, actor.User.ID, "", "collector.config.update", "success", access.CurrentMeta(ctx)); err != nil {
		return out, false, err
	}
	return out, false, tx.Commit(ctx)
}
func (s *Store) PendingCollectorConfigs(ctx context.Context, limit int) ([]cc.Delivery, error) {
	out := []cc.Delivery{}
	if !s.collectorConfigEnabled {
		return out, nil
	}
	if limit < 1 || limit > 16 {
		return nil, cc.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT r.collector_id,a.session_id,r.snapshot FROM collector_config_state c JOIN agents a ON a.agent_id=c.collector_id JOIN collector_config_revisions r ON r.collector_id=c.collector_id AND r.revision=c.desired_revision LEFT JOIN collector_config_deliveries d ON d.collector_id=r.collector_id AND d.revision=r.revision AND d.session_id=a.session_id
 WHERE a.connected AND a.last_seen>now()-make_interval(secs=>$2) AND COALESCE((a.hello->>'managed_config_v1')::boolean,false) AND COALESCE((a.hello->>'remote_management_enabled')::boolean,false)
 AND NOT (c.verified_session_id=a.session_id AND c.status_revision=r.revision AND c.status IN ('applied','rejected','failed'))
 AND (d.collector_id IS NULL OR (d.status NOT IN ('applied','rejected','failed') AND d.last_attempt_at<now()-interval '10 seconds'))
 ORDER BY COALESCE(d.last_attempt_at,'epoch'::timestamptz),r.created_at,r.collector_id LIMIT $1`, limit, s.timeout.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d cc.Delivery
		var b []byte
		if err = rows.Scan(&d.CollectorID, &d.SessionID, &b); err != nil {
			return nil, err
		}
		d.Snapshot = &pb.DesiredConfigSnapshot{}
		if err = protojson.Unmarshal(b, d.Snapshot); err != nil {
			return nil, err
		}
		d.Snapshot.SessionId = d.SessionID
		d.Snapshot.IssuedAt = timestamppb.Now()
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *Store) PrepareCollectorConfig(ctx context.Context, d cc.Delivery) (bool, error) {
	if !s.collectorConfigEnabled || d.Snapshot == nil {
		return false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	var current bool
	err = tx.QueryRow(ctx, `SELECT session_id=$2 AND connected AND last_seen>now()-make_interval(secs=>$3) FROM agents WHERE agent_id=$1 FOR UPDATE`, d.CollectorID, d.SessionID, s.timeout.Seconds()).Scan(&current)
	if err != nil {
		return false, err
	}
	if !current {
		return false, nil
	}
	var ready bool
	err = tx.QueryRow(ctx, `SELECT c.desired_revision=$2 AND r.digest=$3 AND NOT (c.verified_session_id=$4 AND c.status_revision=$2 AND c.status IN ('applied','rejected','failed')) FROM collector_config_state c JOIN collector_config_revisions r ON r.collector_id=c.collector_id AND r.revision=c.desired_revision WHERE c.collector_id=$1 FOR UPDATE OF c`, d.CollectorID, d.Snapshot.Revision, d.Snapshot.ConfigurationDigest, d.SessionID).Scan(&ready)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !ready {
		return false, nil
	}
	result, err := tx.Exec(ctx, `INSERT INTO collector_config_deliveries(collector_id,revision,session_id) VALUES($1,$2,$3) ON CONFLICT(collector_id,revision,session_id) DO UPDATE SET attempts=collector_config_deliveries.attempts+1,last_attempt_at=now() WHERE collector_config_deliveries.status NOT IN ('applied','rejected','failed') AND collector_config_deliveries.last_attempt_at<now()-interval '10 seconds'`, d.CollectorID, d.Snapshot.Revision, d.SessionID)
	if err != nil {
		return false, err
	}
	if result.RowsAffected() == 0 {
		return false, nil
	}
	if _, err = tx.Exec(ctx, `INSERT INTO collector_config_events(collector_id,session_id,revision,digest,status,correlated) VALUES($1,$2,$3,$4,'dispatching',true)`, d.CollectorID, d.SessionID, strconv.FormatUint(d.Snapshot.Revision, 10), d.Snapshot.ConfigurationDigest); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
func configStatusName(v pb.ConfigStatus) string {
	switch v {
	case pb.ConfigStatus_CONFIG_STATUS_RECEIVED:
		return "received"
	case pb.ConfigStatus_CONFIG_STATUS_VALIDATING:
		return "validating"
	case pb.ConfigStatus_CONFIG_STATUS_APPLIED:
		return "applied"
	case pb.ConfigStatus_CONFIG_STATUS_REJECTED:
		return "rejected"
	case pb.ConfigStatus_CONFIG_STATUS_FAILED:
		return "failed"
	}
	return "unknown"
}

// Only fixed diagnostic codes are exposed in management REST. Arbitrary agent
// error text remains in the privileged archive and is never copied here.
func configErrorCode(v string) string {
	switch v {
	case "management_disabled", "restore_rejected", "applied_state_inactive", "credential_ref_not_allowed", "credential_ref_missing", "destination_not_allowed", "local_target_conflict", "revision_conflict", "stale_revision", "digest_mismatch", "invalid_config", "apply_failed", "persistence_failed", "apply_timeout":
		return v
	}
	if v != "" {
		return "collector_rejected_configuration"
	}
	return ""
}
func projectCollectorConfig(ctx context.Context, tx pgx.Tx, e *pb.AgentEnvelope) error {
	if e.GetHello() == nil && e.GetConfigStatus() == nil && (e.GetHeartbeat() == nil || e.GetHeartbeat().ManagedConfig == nil) {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO collector_config_state(collector_id) VALUES($1) ON CONFLICT DO NOTHING`, e.AgentId); err != nil {
		return err
	}
	var status *pb.ConfigApplyStatus
	fromHello := false
	runtimeProof := e.GetHello() != nil
	retainedHeartbeat := false
	if heartbeat := e.GetHeartbeat(); heartbeat != nil {
		if heartbeat.Timestamp != nil && heartbeat.Timestamp.CheckValid() == nil && !heartbeat.Timestamp.AsTime().After(time.Now().Add(30*time.Second)) {
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_sessions WHERE agent_id=$1 AND session_id=$2 AND started_at<=$3)`, e.AgentId, e.SessionId, heartbeat.Timestamp.AsTime()).Scan(&runtimeProof); err != nil {
				return err
			}
		}
		retainedHeartbeat = !runtimeProof
	}
	if h := e.GetHello(); h != nil {
		if _, err := tx.Exec(ctx, `UPDATE collector_config_state SET verified_session_id='' WHERE collector_id=$1`, e.AgentId); err != nil {
			return err
		}
		if applied := h.AppliedConfig; applied != nil {
			fromHello = true
			status = &pb.ConfigApplyStatus{CollectorId: e.AgentId, Revision: applied.Revision, ConfigurationDigest: applied.ConfigurationDigest, Status: applied.Status, AppliedAt: applied.AppliedAt, AppliedRevision: applied.Revision, AppliedDigest: applied.ConfigurationDigest}
		}
	} else if h := e.GetHeartbeat(); h != nil {
		status = h.ManagedConfig
	} else {
		status = e.GetConfigStatus()
	}
	if status == nil {
		return nil
	}
	if status.CollectorId != e.AgentId {
		return cc.ErrInvalid
	}
	revision := strconv.FormatUint(status.Revision, 10)
	appliedRevision := strconv.FormatUint(status.AppliedRevision, 10)
	if _, err := tx.Exec(ctx, `UPDATE collector_config_state SET highest_reported_revision=GREATEST(highest_reported_revision,$2::numeric,$3::numeric) WHERE collector_id=$1 AND (highest_reported_revision<$2::numeric OR highest_reported_revision<$3::numeric)`, e.AgentId, revision, appliedRevision); err != nil {
		return err
	}
	name := configStatusName(status.Status)
	code := configErrorCode(status.ErrorCode)
	var correlated bool
	if status.Revision <= math.MaxInt64 && status.Revision > 0 {
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM collector_config_revisions r WHERE r.collector_id=$1 AND r.revision=$2 AND r.digest=$3 AND ($4 OR EXISTS(SELECT 1 FROM collector_config_deliveries d WHERE d.collector_id=r.collector_id AND d.revision=r.revision AND d.session_id=$5)))`, e.AgentId, status.Revision, status.ConfigurationDigest, fromHello, e.SessionId).Scan(&correlated); err != nil {
			return err
		}
	}
	if name == "applied" && (status.AppliedRevision != status.Revision || status.AppliedDigest != status.ConfigurationDigest || status.AppliedAt == nil || status.AppliedAt.CheckValid() != nil || status.AppliedAt.AsTime().After(time.Now().Add(30*time.Second))) {
		correlated = false
	}
	if name == "unknown" || retainedHeartbeat {
		correlated = false
	}
	duplicateHeartbeat := false
	if e.GetHeartbeat() != nil {
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM (SELECT session_id,revision,digest,status,correlated,error_code FROM collector_config_events WHERE collector_id=$1 ORDER BY id DESC LIMIT 1) last_event WHERE session_id=$2 AND revision=$3::numeric AND digest=$4 AND status=$5 AND correlated=$6 AND error_code=$7)`, e.AgentId, e.SessionId, revision, status.ConfigurationDigest, name, correlated, code).Scan(&duplicateHeartbeat); err != nil {
			return err
		}
	}
	if !duplicateHeartbeat {
		if _, err := tx.Exec(ctx, `INSERT INTO collector_config_events(collector_id,session_id,revision,digest,status,correlated,error_code) VALUES($1,$2,$3::numeric,$4,$5,$6,$7)`, e.AgentId, e.SessionId, revision, status.ConfigurationDigest, name, correlated, code); err != nil {
			return err
		}
	}
	if !correlated {
		return nil
	}
	stateName := name
	if name == "applied" && !runtimeProof {
		stateName = "applied_reported"
	}
	// Terminal outcomes cannot be undone by replayed RECEIVED/VALIDATING messages.
	if _, err := tx.Exec(ctx, `UPDATE collector_config_deliveries SET status=$4 WHERE collector_id=$1 AND revision=$2 AND session_id=$3 AND status NOT IN ('applied','rejected','failed')`, e.AgentId, status.Revision, e.SessionId, name); err != nil {
		return err
	}
	var appliedAt *time.Time
	if status.AppliedAt != nil && status.AppliedAt.CheckValid() == nil {
		v := status.AppliedAt.AsTime()
		appliedAt = &v
	}
	_, err := tx.Exec(ctx, `UPDATE collector_config_state SET status=$4,status_revision=$2,error_code=$5,verified_session_id=$3,
 applied_revision=CASE WHEN $4='applied' THEN $2 ELSE applied_revision END,
 applied_digest=CASE WHEN $4='applied' THEN $6 ELSE applied_digest END,
 applied_at=CASE WHEN $4='applied' THEN $7 ELSE applied_at END
 WHERE collector_id=$1 AND desired_revision=$2 AND (verified_session_id<>$3 OR status_revision<>$2 OR status NOT IN ('applied','applied_reported','rejected','failed') OR ($8 AND $4 IN ('applied','rejected','failed')) OR (status=$4 AND $4='applied'))
 AND NOT (status='validating' AND $4='received' AND verified_session_id=$3)
 AND ((status_revision,status,verified_session_id,error_code) IS DISTINCT FROM ($2::bigint,$4::text,$3::text,$5::text) OR ($4='applied' AND (applied_revision<>$2 OR applied_digest<>$6)))`, e.AgentId, status.Revision, e.SessionId, stateName, code, status.ConfigurationDigest, appliedAt, runtimeProof)
	return err
}
