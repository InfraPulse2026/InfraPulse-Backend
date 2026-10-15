package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"github.com/jackc/pgx/v5"
)

func (s *Store) HealthPolicy() health.Policy {
	if s.healthPolicy.CPUCritical == 0 {
		return health.Defaults()
	}
	return s.healthPolicy
}
func (s *Store) ConfigureHealth(policy health.Policy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	s.healthPolicy = policy
	return nil
}
func (s *Store) projectHealth(ctx context.Context, tx pgx.Tx, e *pb.AgentEnvelope) error {
	batch := e.GetTelemetry()
	if batch == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, sample := range batch.Samples {
		scope := ""
		if sample.Source == "simulated" {
			scope = e.SessionId
		}
		identity, _ := json.Marshal([]string{e.AgentId, sample.DeviceId, sample.Source, scope})
		key := string(identity)
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "health:"+key); err != nil {
			return err
		}
		interval := s.defaults.TelemetryIntervalSeconds
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT telemetry_interval_seconds FROM monitoring_policies WHERE agent_id=$1 AND device_id=$2),$3)`, e.AgentId, sample.DeviceId, interval).Scan(&interval); err != nil {
			return err
		}
		now := time.Now().UTC()
		if s.healthClock != nil {
			now = s.healthClock().UTC()
		}
		window := time.Duration(max(1200, 12*interval)) * time.Second
		rows, err := tx.Query(ctx, `SELECT t.id,t.observed_at,t.received_at,COALESCE(t.payload->>'boot_id',''),t.cpu_usage_percent,t.memory_total_bytes::text,t.memory_used_bytes::text
 FROM telemetry t JOIN agent_messages m ON m.agent_id=t.agent_id AND m.message_id=t.message_id
 WHERE t.agent_id=$1 AND t.device_id=$2 AND t.source=$3 AND ($4='' OR m.session_id=$4)
 AND t.observed_at>=$5 AND t.observed_at<=$6 AND t.observed_at<=t.received_at+interval '30 seconds'
 ORDER BY t.observed_at DESC,t.id DESC LIMIT 120`, e.AgentId, sample.DeviceId, sample.Source, scope, now.Add(-window), now)
		if err != nil {
			return err
		}
		samples := []health.Sample{}
		for rows.Next() {
			var v health.Sample
			var total, used *string
			if err = rows.Scan(&v.ID, &v.ObservedAt, &v.ReceivedAt, &v.BootID, &v.CPU, &total, &used); err != nil {
				rows.Close()
				return err
			}
			if total != nil && used != nil {
				a, e1 := strconv.ParseUint(*total, 10, 64)
				b, e2 := strconv.ParseUint(*used, 10, 64)
				if e1 == nil && e2 == nil && a > 0 && b <= a {
					percent := 100 * float64(b) / float64(a)
					v.RAM = &percent
				}
			}
			samples = append(samples, v)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, assessment := range health.Evaluate(now, samples, interval, s.HealthPolicy()) {
			if assessment.ObservedAt == nil {
				continue
			}
			encoded, err := json.Marshal(assessment)
			if err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `INSERT INTO health_observations(agent_id,device_id,source,scope,rule_id,observed_at,assessment) VALUES($1,$2,$3,$4,$5,$6,$7)
 ON CONFLICT(agent_id,device_id,source,scope,rule_id) DO UPDATE SET observed_at=EXCLUDED.observed_at,assessment=EXCLUDED.assessment,evaluated_at=now() WHERE EXCLUDED.observed_at>health_observations.observed_at`, e.AgentId, sample.DeviceId, sample.Source, scope, assessment.RuleID, *assessment.ObservedAt, encoded)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				continue
			}
			if err = applyIncident(ctx, tx, e.AgentId, sample.DeviceId, sample.Source, scope, assessment, encoded); err != nil {
				return err
			}
		}
	}
	return nil
}

func applyIncident(ctx context.Context, tx pgx.Tx, agent, device, source, scope string, a health.Assessment, encoded []byte) error {
	return applyResourceIncident(ctx, tx, agent, device, source, scope, "device", "", "", a, encoded)
}
func applyResourceIncident(ctx context.Context, tx pgx.Tx, agent, device, source, scope, kind, resource, contextID string, a health.Assessment, encoded []byte) error {
	keys := []string{agent, device, source, scope, a.RuleID}
	if kind != "device" {
		keys = append(keys, kind, resource, contextID)
	}
	identity, _ := json.Marshal(keys)
	hash := sha256.Sum256(identity)
	fingerprint := hex.EncodeToString(hash[:])
	var id, state, severity string
	err := tx.QueryRow(ctx, `SELECT id,state,severity FROM incidents WHERE fingerprint=$1 AND state<>'resolved' FOR UPDATE`, fingerprint).Scan(&id, &state, &severity)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	problem := a.Status == "warning" || a.Status == "critical"
	if !exists && !problem {
		return nil
	}
	evidence := ""
	transition := ""
	if !exists {
		b := make([]byte, 16)
		if _, err = rand.Read(b); err != nil {
			return err
		}
		id = "inc_" + hex.EncodeToString(b)
		var occurrence int
		if err = tx.QueryRow(ctx, `SELECT COALESCE(max(occurrence),0)+1 FROM incidents WHERE fingerprint=$1`, fingerprint).Scan(&occurrence); err != nil {
			return err
		}
		agentID := &agent
		var deviceID, targetID *string
		if kind == "configuration" {
			agentID = nil
		} else if kind == "sip" {
			targetID = &resource
		} else if kind != "agent" {
			deviceID = &device
		}
		if _, err = tx.Exec(ctx, `INSERT INTO incidents(id,fingerprint,agent_id,device_id,source,scope,rule_id,occurrence,state,severity,last_observed_at,signal,resource_kind,resource_id,resource_context,target_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'open',$9,$10,$11,$12,$13,$14,$15)`, id, fingerprint, agentID, deviceID, source, scope, a.RuleID, occurrence, a.Severity, *a.ObservedAt, encoded, kind, resource, contextID, targetID); err != nil {
			return err
		}
		evidence = "opened"
		state = "open"
		transition = a.Reason
	} else {
		if problem && a.Severity != severity {
			severity = a.Severity
			evidence = "severity_changed"
			transition = "severity_changed"
		}
		if a.RecoveryEligible && a.Status == "ok" {
			state = "resolved"
			evidence = "recovered"
			transition = "stable_recovery"
		}
		if _, err = tx.Exec(ctx, `UPDATE incidents SET state=$2,severity=$3,last_observed_at=$4,updated_at=now(),signal=$5,resolved_at=CASE WHEN $2='resolved' THEN now() ELSE resolved_at END WHERE id=$1`, id, state, severity, *a.ObservedAt, encoded); err != nil {
			return err
		}
	}
	if transition != "" {
		if _, err = tx.Exec(ctx, `INSERT INTO incident_transitions(incident_id,state,reason) VALUES($1,$2,$3)`, id, state, transition); err != nil {
			return err
		}
	}
	if evidence != "" {
		if _, err = tx.Exec(ctx, `INSERT INTO incident_evidence(incident_id,kind,observed_at,assessment) VALUES($1,$2,$3,$4)`, id, evidence, *a.ObservedAt, encoded); err != nil {
			return err
		}
	}
	return queueIncidentCorrelation(ctx, tx, id)
}

const incidentColumns = `id,COALESCE(agent_id,''),COALESCE(device_id,''),source,scope,rule_id,occurrence,state,severity,opened_at,last_observed_at,updated_at,acknowledged_at,acknowledged_by,resolved_at,signal,resource_kind,resource_id,resource_context,source_instance_id,external_event_id`

func readIncident(row interface{ Scan(...any) error }) (health.Incident, error) {
	var v health.Incident
	var raw []byte
	err := row.Scan(&v.ID, &v.AgentID, &v.DeviceID, &v.Source, &v.Scope, &v.RuleID, &v.Occurrence, &v.State, &v.Severity, &v.OpenedAt, &v.LastObservedAt, &v.UpdatedAt, &v.AcknowledgedAt, &v.AcknowledgedBy, &v.ResolvedAt, &raw, &v.ResourceKind, &v.ResourceID, &v.ResourceContext, &v.SourceInstanceID, &v.ExternalEventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	if err == nil {
		err = json.Unmarshal(raw, &v.Signal)
	}
	return v, err
}
func (s *Store) Incidents(ctx context.Context, f health.Filter) ([]health.Incident, error) {
	if f.Limit < 1 || f.Limit > 200 {
		f.Limit = 100
	}
	if f.Offset < 0 || f.Offset > 100000 {
		f.Offset = 0
	}
	rows, err := s.pool.Query(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE ($1='' OR agent_id=$1) AND ($2='' OR device_id=$2) AND ($3='' OR state=$3) AND ($4='' OR rule_id=$4) AND ($5='' OR source=$5) AND ($8='' OR resource_kind=$8) AND ($9='' OR resource_id=$9) ORDER BY opened_at DESC,id LIMIT $6 OFFSET $7`, f.AgentID, f.DeviceID, f.State, f.RuleID, f.Source, f.Limit, f.Offset, f.ResourceKind, f.ResourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []health.Incident{}
	for rows.Next() {
		v, e := readIncident(rows)
		if e != nil {
			return nil, e
		}
		items = append(items, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err = s.incidentFreshness(ctx, items); err != nil {
		return nil, err
	}
	return items, nil
}
func (s *Store) Incident(ctx context.Context, id string) (health.Incident, error) {
	v, err := readIncident(s.pool.QueryRow(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE id=$1`, id))
	if err != nil {
		return v, err
	}
	items := []health.Incident{v}
	err = s.incidentFreshness(ctx, items)
	return items[0], err
}

func (s *Store) incidentFreshness(ctx context.Context, items []health.Incident) error {
	return s.incidentFreshnessQuery(ctx, s.pool, items, time.Now())
}
func (s *Store) incidentFreshnessTx(ctx context.Context, tx pgx.Tx, items []health.Incident, now time.Time) error {
	return s.incidentFreshnessQuery(ctx, tx, items, now)
}

type freshnessQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func (s *Store) incidentFreshnessQuery(ctx context.Context, query freshnessQueryer, items []health.Incident, now time.Time) error {
	if len(items) == 0 {
		return nil
	}
	agents, devices, kinds, resources := []string{}, []string{}, []string{}, []string{}
	for _, item := range items {
		agents = append(agents, item.AgentID)
		devices = append(devices, item.DeviceID)
		kinds = append(kinds, item.ResourceKind)
		resources = append(resources, item.ResourceID)
	}
	rows, err := query.Query(ctx, `SELECT ids.agent_id,ids.device_id,ids.kind,ids.resource,a.connected AND a.last_seen>$5,a.session_id,CASE WHEN ids.kind='sip' THEN COALESCE(tp.expected_interval_seconds,$7) ELSE COALESCE(p.telemetry_interval_seconds,$6) END,COALESCE(ts.address,''),COALESCE(ts.transport,'') FROM unnest($1::text[],$2::text[],$3::text[],$4::text[]) AS ids(agent_id,device_id,kind,resource) JOIN agents a ON a.agent_id=ids.agent_id LEFT JOIN monitoring_policies p ON p.agent_id=ids.agent_id AND p.device_id=ids.device_id LEFT JOIN telephony_policies tp ON ids.kind='sip' AND tp.agent_id=ids.agent_id AND tp.target_id=ids.resource LEFT JOIN telephony_targets tt ON ids.kind='sip' AND tt.agent_id=ids.agent_id AND tt.target_id=ids.resource LEFT JOIN telephony_samples ts ON ts.id=tt.latest_sample_id`, agents, devices, kinds, resources, now.Add(-s.timeout), s.defaults.TelemetryIntervalSeconds, s.telephonyDefault())
	if err != nil {
		return err
	}
	defer rows.Close()
	type freshnessInput struct {
		connected bool
		session   string
		interval  int
		contextID string
	}
	data := map[string]freshnessInput{}
	key := func(agent, device, kind, resource string) string {
		b, _ := json.Marshal([]string{agent, device, kind, resource})
		return string(b)
	}
	for rows.Next() {
		var agent, device, kind, resource, address, transport string
		var v freshnessInput
		if err = rows.Scan(&agent, &device, &kind, &resource, &v.connected, &v.session, &v.interval, &address, &transport); err != nil {
			return err
		}
		if kind == "sip" {
			v.contextID = health.SIPContext(address, transport)
		}
		data[key(agent, device, kind, resource)] = v
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range items {
		v := &items[i]
		at := v.LastObservedAt
		if v.State == "resolved" {
			v.EvidenceStatus = "historical"
			continue
		}
		if v.ResourceKind == "external" || v.ResourceKind == "configuration" {
			continue
		}
		if v.ResourceKind == "agent" {
			v.EvidenceStatus = "fresh"
			if now.Sub(at) > 60*time.Second {
				v.EvidenceStatus = "stale"
			}
			continue
		}
		d, ok := data[key(v.AgentID, v.DeviceID, v.ResourceKind, v.ResourceID)]
		v.EvidenceStatus = "fresh"
		if !ok || !d.connected {
			v.EvidenceStatus = "collector_unavailable"
		} else if v.Scope != "" && v.Scope != d.session || v.ResourceKind == "sip" && v.ResourceContext != d.contextID || now.Sub(at) > time.Duration(monitoring.StaleAfterSeconds(d.interval))*time.Second {
			v.EvidenceStatus = "stale"
		}
	}
	rows.Close()
	if err = s.hardwareIncidentFreshness(ctx, query, items, now); err != nil {
		return err
	}
	if err = s.logIncidentFreshness(ctx, query, items, now); err != nil {
		return err
	}
	if err = s.configurationIncidentFreshness(ctx, query, items, now); err != nil {
		return err
	}
	if err = s.topologyIncidentFreshness(ctx, query, items, now); err != nil {
		return err
	}
	return s.externalIncidentFreshness(ctx, query, items, now)
}
func (s *Store) IncidentTransitions(ctx context.Context, id string) ([]health.Transition, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,state,reason,at,actor_id FROM incident_transitions WHERE incident_id=$1 ORDER BY id DESC LIMIT 200`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []health.Transition{}
	for rows.Next() {
		var v health.Transition
		if err = rows.Scan(&v.ID, &v.State, &v.Reason, &v.At, &v.ActorID); err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}
func (s *Store) IncidentEvidence(ctx context.Context, id string) ([]health.Evidence, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,kind,observed_at,at,assessment FROM incident_evidence WHERE incident_id=$1 ORDER BY id DESC LIMIT 200`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []health.Evidence{}
	for rows.Next() {
		var v health.Evidence
		if err = rows.Scan(&v.ID, &v.Kind, &v.ObservedAt, &v.At, &v.Assessment); err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}
func (s *Store) AcknowledgeIncident(ctx context.Context, id string) (health.Incident, error) {
	var empty health.Incident
	actor, ok := access.Current(ctx)
	if !ok {
		return empty, access.ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(context.Background())
	if err = commandActor(ctx, tx, actor.User.ID, actor.SessionHash); err != nil {
		return empty, err
	}
	v, err := readIncident(tx.QueryRow(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return empty, err
	}
	if v.State == "resolved" {
		return empty, health.ErrConflict
	}
	if v.State == "acknowledged" {
		if err = tx.Commit(ctx); err != nil {
			return empty, err
		}
		return s.Incident(ctx, id)
	}
	v, err = readIncident(tx.QueryRow(ctx, `UPDATE incidents SET state='acknowledged',acknowledged_at=now(),acknowledged_by=$2,updated_at=now() WHERE id=$1 RETURNING `+incidentColumns, id, actor.User.ID))
	if err != nil {
		return empty, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO incident_transitions(incident_id,state,reason,actor_id) VALUES($1,'acknowledged','operator_acknowledged',$2)`, id, actor.User.ID); err != nil {
		return empty, err
	}
	meta := access.CurrentMeta(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer)VALUES($1,'incident_acknowledged','success',$2,$3,$4)`, actor.User.ID, "incident:"+id, meta.RequestID, meta.Peer); err != nil {
		return empty, err
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, err
	}
	return s.Incident(ctx, id)
}
func (s *Store) DeviceHealth(ctx context.Context, agent, device string) ([]health.Observation, error) {
	d, err := s.Device(ctx, agent, device)
	if err != nil {
		return nil, err
	}
	var session string
	if err = s.pool.QueryRow(ctx, `SELECT session_id FROM agents WHERE agent_id=$1`, d.AgentID).Scan(&session); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT agent_id,device_id,source,scope,assessment,evaluated_at FROM health_observations WHERE agent_id=$1 AND device_id=$2 ORDER BY evaluated_at DESC,source,scope,rule_id LIMIT 600`, d.AgentID, d.DeviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []health.Observation{}
	for rows.Next() {
		var v health.Observation
		var raw []byte
		if err = rows.Scan(&v.AgentID, &v.DeviceID, &v.Source, &v.Scope, &raw, &v.EvaluatedAt); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &v.Assessment); err != nil {
			return nil, err
		}
		v.Current = v.Scope == "" || v.Scope == session
		v.ResourceKind = "device"
		if !d.AgentConnected || v.Scope != "" && v.Scope != session || v.Assessment.ObservedAt == nil || time.Since(*v.Assessment.ObservedAt) > time.Duration(monitoring.StaleAfterSeconds(d.Monitoring.TelemetryIntervalSeconds))*time.Second {
			v.Assessment.Status = "stale"
			v.Assessment.Severity = "none"
			v.Assessment.SecondsToCritical = nil
			v.Assessment.RecoveryEligible = false
			v.Assessment.Reason = "telemetry_or_collector_stale"
		}
		items = append(items, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	ports, err := s.ResourceHealth(ctx, d.AgentID, d.DeviceID, "interface", "")
	if err != nil {
		return nil, err
	}
	return append(items, ports...), nil
}
