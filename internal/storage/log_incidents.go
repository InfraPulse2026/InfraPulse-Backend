package storage

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"github.com/jackc/pgx/v5"
	"time"
)

func (s *Store) ProcessLogSignals(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73512014)`); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT log_id FROM log_signal_pending ORDER BY log_id LIMIT 25 FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return 0, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err = s.applyLogEvent(ctx, tx, id); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM log_signal_pending WHERE log_id=$1`, id); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit(ctx)
}

func (s *Store) applyLogEvent(ctx context.Context, tx pgx.Tx, id int64) error {
	var done bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM log_incident_records WHERE log_id=$1)`, id).Scan(&done); err != nil {
		return err
	}
	if done {
		return nil
	}
	r := logbook.Record{}
	var binding, scope string
	var enabled, currentScope bool
	var auditSuccess *bool
	err := tx.QueryRow(ctx, `SELECT l.id,l.agent_id,l.device_id,l.source,l.source_id,l.process,l.severity,l.event_at,l.observed_at,l.received_at,l.time_basis,l.observation_metadata,b.id,ls.record_scope,ds.enabled,COALESCE(ls.record_scope='' OR ls.record_scope=ca.session_id,false),ar.success,CASE WHEN octet_length(l.message)<=8192 THEN l.message ELSE '' END,l.truncated`+assetLogJoin+`LEFT JOIN audit_records ar ON ar.log_id=l.id JOIN assets live ON live.id=b.asset_id LEFT JOIN agents ca ON ca.agent_id=l.agent_id WHERE l.id=$1 AND live.lifecycle_status='observed' AND live.resolution_status<>'conflict'`, id).Scan(&r.ID, &r.AgentID, &r.DeviceID, &r.Source, &r.SourceID, &r.Process, &r.Severity, &r.EventAt, &r.ObservedAt, &r.ReceivedAt, &r.TimeBasis, &r.Observation, &binding, &scope, &enabled, &currentScope, &auditSuccess, &r.Message, &r.Truncated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if auditSuccess != nil {
		r.Audit = &logbook.Audit{Success: auditSuccess}
	}
	severity, quality := health.LogEventSeverity(time.Now().UTC(), r)
	if severity == "" || !enabled || !currentScope {
		return nil
	}
	key, window := health.LogWindowKey(binding, scope, r)
	prior, err := readIncident(tx.QueryRow(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE resource_kind='log' AND resource_id=$1 AND resource_context=$2 AND state<>'resolved' FOR UPDATE`, key, binding))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	count := int64(1)
	observed := r.EventAt
	points := []health.ResourcePoint{}
	if err == nil {
		if prior.Severity == "critical" {
			severity = "critical"
		}
		if prior.LastObservedAt.After(observed) {
			observed = prior.LastObservedAt
		}
		if prior.Signal.Log != nil {
			count = prior.Signal.Log.EventCount + 1
			if prior.Signal.Log.TimeQuality != "exact" {
				quality = "estimated"
			}
		}
		points = append(points, prior.Signal.ResourceEvidence...)
	}
	points = append(points, health.ResourcePoint{RecordID: r.ID, RecordKind: "log", ObservedAt: r.EventAt, State: "reported_event"})
	if len(points) > 20 {
		points = append(points[:1], points[len(points)-19:]...)
	}
	patternID := ""
	if rule := health.LogEventRule(r); rule == "log.kernel_oom_kill" || rule == "log.ssh_password_failure" {
		patternID = rule
	}
	a := health.Assessment{RuleID: health.LogEventRule(r), RuleVersion: "1", Status: severity, Severity: severity, Reason: "reported_log_event_requires_review", ObservedAt: &observed, RecoveryEligible: false, Evidence: []health.Point{}, ResourceEvidence: points, Log: &health.LogEvidence{PatternID: patternID, AuditSuccess: auditSuccess, RecordID: r.ID, BindingID: binding, SourceIDHash: health.LogIdentityHash(r.SourceID), ProcessHash: health.LogIdentityHash(r.Process), Process: logProcessLabel(r.Process), Severity: r.Severity, EventAt: r.EventAt, ReceivedAt: r.ReceivedAt, WindowStart: window, EventCount: count, TimeQuality: quality, Lifecycle: "requires_review"}}
	encoded, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if err = applyResourceIncident(ctx, tx, r.AgentID, r.DeviceID, r.Source, scope, "log", key, binding, a, encoded); err != nil {
		return err
	}
	var incidentID string
	if err = tx.QueryRow(ctx, `SELECT id FROM incidents WHERE resource_kind='log' AND resource_id=$1 AND resource_context=$2 AND state<>'resolved'`, key, binding).Scan(&incidentID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO log_incident_records(log_id,incident_id,binding_id) VALUES($1,$2,$3)`, r.ID, incidentID, binding); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO incident_evidence(incident_id,kind,observed_at,assessment) VALUES($1,'reported_log_event',$2,$3)`, incidentID, r.EventAt, encoded)
	return err
}

// Review closes only a reported log event. It never asserts physical recovery,
// executes commands or resolves other members of a multi-source incident.
func (s *Store) ReviewLogIncident(ctx context.Context, id string, input health.LogReview) (health.Incident, error) {
	var empty health.Incident
	if err := input.Validate(); err != nil {
		return empty, err
	}
	actor, ok := access.Current(ctx)
	if !ok || !access.Allowed(actor.User.Role, access.IncidentWrite) {
		return empty, access.ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73512014)`); err != nil {
		return empty, err
	}
	if err = commandActor(ctx, tx, actor.User.ID, actor.SessionHash); err != nil {
		return empty, err
	}
	v, err := readIncident(tx.QueryRow(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return empty, err
	}
	if v.ResourceKind != "log" || v.Signal.Log == nil || v.Signal.Log.EventCount != input.ExpectedEventCount {
		return empty, health.ErrConflict
	}
	if v.State == "resolved" {
		if v.Signal.Log.ReviewedAt == nil {
			return empty, health.ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return empty, err
		}
		return s.Incident(ctx, id)
	}
	// Ingest already locks this row before persisting an envelope. Hold it through
	// review so a concurrent committed record is either seen as pending or belongs
	// to the next occurrence after this review, never silently closed unseen.
	var agent string
	if err = tx.QueryRow(ctx, `SELECT agent_id FROM agents WHERE agent_id=$1 FOR UPDATE`, v.AgentID).Scan(&agent); err != nil {
		return empty, err
	}
	var pending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM log_records l JOIN log_signal_pending pending ON pending.log_id=l.id `+assetLogScopeJoin+` WHERE l.agent_id=$1 AND l.device_id=$2 AND l.source=$3 AND encode(sha256(convert_to(l.source_id,'UTF8')),'hex')=$4 AND encode(sha256(convert_to(l.process,'UTF8')),'hex')=$5 AND ls.record_scope=$6 AND l.event_at>=$7 AND l.event_at<$8)`, v.AgentID, v.DeviceID, v.Source, v.Signal.Log.SourceIDHash, v.Signal.Log.ProcessHash, v.Scope, v.Signal.Log.WindowStart, v.Signal.Log.WindowStart.Add(5*time.Minute)).Scan(&pending); err != nil {
		return empty, err
	}
	if pending {
		return empty, health.ErrConflict
	}
	now := time.Now().UTC()
	v.Signal.Log.Lifecycle = "reviewed_event"
	v.Signal.Log.ReviewedAt = &now
	v.Signal.Log.ReviewedBy = actor.User.ID
	v.Signal.Log.ReviewReason = logbook.SanitizeText(input.Reason)
	v.Signal.Reason = "operator_reviewed_log_event"
	encoded, err := json.Marshal(v.Signal)
	if err != nil {
		return empty, err
	}
	if _, err = tx.Exec(ctx, `UPDATE incidents SET state='resolved',resolved_at=now(),updated_at=now(),signal=$2 WHERE id=$1`, id, encoded); err != nil {
		return empty, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO incident_transitions(incident_id,state,reason,actor_id) VALUES($1,'resolved','operator_reviewed_log_event',$2)`, id, actor.User.ID); err != nil {
		return empty, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO incident_evidence(incident_id,kind,observed_at,assessment) VALUES($1,'log_event_review',$2,$3)`, id, now, encoded); err != nil {
		return empty, err
	}
	meta := access.CurrentMeta(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,'log_event_reviewed','success',$2,$3,$4)`, actor.User.ID, "incident:"+id, meta.RequestID, meta.Peer); err != nil {
		return empty, err
	}
	if err = queueIncidentCorrelation(ctx, tx, id); err != nil {
		return empty, err
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, err
	}
	return s.Incident(ctx, id)
}

func (s *Store) logIncidentFreshness(ctx context.Context, q freshnessQueryer, items []health.Incident, now time.Time) error {
	ids := []string{}
	for _, v := range items {
		if v.ResourceKind == "log" && v.State != "resolved" {
			ids = append(ids, v.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `SELECT i.id,COALESCE(b.id,''),COALESCE(ds.enabled,false),COALESCE(ca.connected AND ca.last_seen>($2::timestamptz-($3::double precision*interval '1 second')) AND (i.scope='' OR i.scope=ca.session_id),false),COALESCE(live.lifecycle_status='observed' AND live.resolution_status<>'conflict',false) FROM incidents i LEFT JOIN asset_source_bindings b ON b.id=i.resource_context LEFT JOIN data_sources ds ON ds.id=b.source_instance_id LEFT JOIN agents ca ON ca.agent_id=i.agent_id LEFT JOIN assets live ON live.id=b.asset_id WHERE i.id=ANY($1::text[])`, ids, now, s.timeout.Seconds())
	if err != nil {
		return err
	}
	states := map[string]string{}
	for rows.Next() {
		var id, binding string
		var enabled, connected, live bool
		if err = rows.Scan(&id, &binding, &enabled, &connected, &live); err != nil {
			rows.Close()
			return err
		}
		status := "fresh"
		if binding == "" || !live {
			status = "missing"
		} else if !enabled || !connected {
			status = "source_unavailable"
		}
		states[id] = status
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for idx := range items {
		v := &items[idx]
		if v.ResourceKind != "log" || v.State == "resolved" {
			continue
		}
		v.EvidenceStatus = states[v.ID]
		if v.EvidenceStatus == "" {
			v.EvidenceStatus = "missing"
		}
		if v.EvidenceStatus == "fresh" && now.Sub(v.LastObservedAt) > 300*time.Second {
			v.EvidenceStatus = "stale"
		}
	}
	return nil
}

func (s *Store) LogIncidentRecords(ctx context.Context, id string, limit, offset int) ([]health.LogIncidentRecord, error) {
	if limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, errors.New("invalid pagination")
	}
	v, err := s.Incident(ctx, id)
	if err != nil {
		return nil, err
	}
	if v.ResourceKind != "log" {
		return nil, health.ErrConflict
	}
	rows, err := s.pool.Query(ctx, `SELECT e.log_id,e.binding_id,l.event_at,l.severity FROM log_incident_records e JOIN log_records l ON l.id=e.log_id WHERE e.incident_id=$1 ORDER BY e.log_id LIMIT $2 OFFSET $3`, id, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []health.LogIncidentRecord{}
	for rows.Next() {
		var v health.LogIncidentRecord
		if err = rows.Scan(&v.LogID, &v.BindingID, &v.EventAt, &v.Severity); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func logProcessLabel(raw string) string {
	value := []rune(logbook.SanitizeText(raw))
	if len(value) > 128 {
		value = value[:128]
	}
	return string(value)
}
