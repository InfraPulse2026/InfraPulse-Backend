package storage

import (
	"context"
	"encoding/json"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
)

func (s *Store) healthTime() time.Time {
	if s.healthClock != nil {
		return s.healthClock().UTC()
	}
	return time.Now().UTC()
}
func resourceLock(ctx context.Context, tx pgx.Tx, agent, device, source, scope, kind, resource string) error {
	raw, _ := json.Marshal([]string{agent, device, source, scope, kind, resource})
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "resource-health:"+string(raw))
	return err
}
func (s *Store) projectResourceHealth(ctx context.Context, tx pgx.Tx, e *pb.AgentEnvelope) error {
	if e.GetTelemetry() != nil {
		return s.projectInterfaceHealth(ctx, tx, e)
	}
	if e.GetTelephony() != nil {
		return s.projectSIPHealth(ctx, tx, e)
	}
	return nil
}
func saveResourceHealth(ctx context.Context, tx pgx.Tx, agent, device, source, scope, kind, resource, contextID string, a health.Assessment) error {
	if a.ObservedAt == nil {
		return nil
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	var deviceID, targetID *string
	if kind == "sip" {
		targetID = &resource
	} else if kind != "agent" {
		deviceID = &device
	}
	tag, err := tx.Exec(ctx, `INSERT INTO resource_health_observations(agent_id,device_id,target_id,source,scope,resource_kind,resource_id,resource_context,rule_id,observed_at,assessment)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT(agent_id,resource_kind,resource_id,resource_context,source,scope,rule_id,device_key)
 DO UPDATE SET observed_at=EXCLUDED.observed_at,assessment=EXCLUDED.assessment,evaluated_at=now() WHERE EXCLUDED.observed_at>resource_health_observations.observed_at`, agent, deviceID, targetID, source, scope, kind, resource, contextID, a.RuleID, *a.ObservedAt, raw)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return applyResourceIncident(ctx, tx, agent, device, source, scope, kind, resource, contextID, a, raw)
}
func (s *Store) projectInterfaceHealth(ctx context.Context, tx pgx.Tx, e *pb.AgentEnvelope) error {
	seen := map[string]bool{}
	for _, sample := range e.GetTelemetry().Samples {
		scope := ""
		if sample.Source == "simulated" {
			scope = e.SessionId
		}
		raw, _ := json.Marshal([]string{sample.DeviceId, sample.Source, scope})
		key := string(raw)
		if seen[key] {
			continue
		}
		seen[key] = true
		var interval int
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT telemetry_interval_seconds FROM monitoring_policies WHERE agent_id=$1 AND device_id=$2),$3)`, e.AgentId, sample.DeviceId, s.defaults.TelemetryIntervalSeconds).Scan(&interval); err != nil {
			return err
		}
		now := s.healthTime()
		window := time.Duration(max(1200, 12*interval)) * time.Second
		rows, err := tx.Query(ctx, `SELECT t.id,t.observed_at,t.received_at,t.payload FROM telemetry t JOIN agent_messages m ON m.agent_id=t.agent_id AND m.message_id=t.message_id WHERE t.agent_id=$1 AND t.device_id=$2 AND t.source=$3 AND ($4='' OR m.session_id=$4) AND t.observed_at BETWEEN $5 AND $6 AND t.observed_at<=t.received_at+interval '30 seconds' ORDER BY t.observed_at DESC,t.id DESC LIMIT 120`, e.AgentId, sample.DeviceId, sample.Source, scope, now.Add(-window), now)
		if err != nil {
			return err
		}
		type item struct {
			base    health.ResourceSample
			payload *pb.TelemetrySample
		}
		history := []item{}
		for rows.Next() {
			var v item
			v.payload = &pb.TelemetrySample{}
			var encoded []byte
			if err = rows.Scan(&v.base.ID, &v.base.ObservedAt, &v.base.ReceivedAt, &encoded); err != nil {
				rows.Close()
				return err
			}
			if err = protojson.Unmarshal(encoded, v.payload); err != nil {
				rows.Close()
				return err
			}
			v.base.BootID = v.payload.BootId
			history = append(history, v)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(history) == 0 {
			continue
		}
		for _, iface := range history[0].payload.Interfaces {
			if iface == nil || iface.Id == "" {
				continue
			}
			if err = resourceLock(ctx, tx, e.AgentId, sample.DeviceId, sample.Source, scope, "interface", iface.Id); err != nil {
				return err
			}
			points := make([]health.ResourceSample, 0, len(history))
			for _, item := range history {
				v := item.base
				for _, candidate := range item.payload.Interfaces {
					if candidate != nil && candidate.Id == iface.Id {
						v.Interface = candidate
						break
					}
				}
				points = append(points, v)
			}
			for _, a := range health.InterfaceRules(now, points, interval) {
				if err = saveResourceHealth(ctx, tx, e.AgentId, sample.DeviceId, sample.Source, scope, "interface", iface.Id, "", a); err != nil {
					return err
				}
			}
			if err = s.projectInterfaceFlap(ctx, tx, e.AgentId, sample.DeviceId, sample.Source, scope, e.SessionId, iface.Id, now, interval, points); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *Store) projectSIPHealth(ctx context.Context, tx pgx.Tx, e *pb.AgentEnvelope) error {
	for _, target := range e.GetTelephony().Targets {
		scope := ""
		if target.Source == "simulated" || e.GetTelephony().Source == "simulated" || target.GetObservation().GetSource().GetProvenance() == "simulated" || e.GetTelephony().GetObservation().GetSource().GetProvenance() == "simulated" {
			scope = e.SessionId
		}
		if err := resourceLock(ctx, tx, e.AgentId, "", target.Source, scope, "sip", target.TargetId); err != nil {
			return err
		}
		var interval int
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT expected_interval_seconds FROM telephony_policies WHERE agent_id=$1 AND target_id=$2),$3)`, e.AgentId, target.TargetId, s.telephonyDefault()).Scan(&interval); err != nil {
			return err
		}
		now := s.healthTime()
		window := time.Duration(max(1200, 12*interval)) * time.Second
		rows, err := tx.Query(ctx, `SELECT t.id,t.observed_at,t.received_at,t.address,t.transport,t.reachable,t.response_code,t.latency_ms,t.state,t.error_category FROM telephony_samples t JOIN agent_messages m ON m.agent_id=t.agent_id AND m.message_id=t.message_id WHERE t.agent_id=$1 AND t.target_id=$2 AND t.source=$3 AND asset_voip_scope(m.payload,m.session_id,t.target_id,t.source)=$4 AND NOT t.clock_skew AND t.observed_at BETWEEN $5 AND $6 ORDER BY t.observed_at DESC,t.id DESC LIMIT 120`, e.AgentId, target.TargetId, target.Source, scope, now.Add(-window), now)
		if err != nil {
			return err
		}
		points := []health.ResourceSample{}
		for rows.Next() {
			var v health.ResourceSample
			h := &pb.TelephonyHealth{}
			if err = rows.Scan(&v.ID, &v.ObservedAt, &v.ReceivedAt, &h.Address, &h.Transport, &h.Reachable, &h.ResponseCode, &h.LatencyMs, &h.State, &h.ErrorCategory); err != nil {
				rows.Close()
				return err
			}
			v.Identity = health.SIPContext(h.Address, h.Transport)
			v.SIP = h
			points = append(points, v)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(points) == 0 {
			continue
		}
		contextID := points[0].Identity
		for _, a := range health.SIPRules(now, points, interval) {
			if err = saveResourceHealth(ctx, tx, e.AgentId, "", target.Source, scope, "sip", target.TargetId, contextID, a); err != nil {
				return err
			}
		}
	}
	return nil
}
