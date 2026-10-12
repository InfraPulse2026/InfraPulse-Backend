package storage

import (
	"context"
	"encoding/json"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
)

func (s *Store) ResourceHealth(ctx context.Context, agent, device, kind, resource string) ([]health.Observation, error) {
	var interval int
	contextID := ""
	if kind == "agent" {
		agent = resource
		device = ""
		interval = 3
		var found bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agents WHERE agent_id=$1)`, agent).Scan(&found); err != nil {
			return nil, err
		}
		if !found {
			return nil, ErrNotFound
		}
	} else if kind == "sip" {
		target, err := s.TelephonyTarget(ctx, agent, resource)
		if err != nil {
			return nil, err
		}
		agent = target.AgentID
		interval = target.Monitoring.ExpectedIntervalSeconds
		var address, transport string
		if err = s.pool.QueryRow(ctx, `SELECT COALESCE(ts.address,''),COALESCE(ts.transport,'') FROM telephony_targets t LEFT JOIN telephony_samples ts ON ts.id=t.latest_sample_id WHERE t.agent_id=$1 AND t.target_id=$2`, agent, resource).Scan(&address, &transport); err != nil {
			return nil, err
		}
		contextID = health.SIPContext(address, transport)
	} else if kind == "interface" {
		d, err := s.Device(ctx, agent, device)
		if err != nil {
			return nil, err
		}
		agent = d.AgentID
		interval = d.Monitoring.TelemetryIntervalSeconds
	} else {
		return nil, ErrNotFound
	}
	var connected bool
	var session string
	if err := s.pool.QueryRow(ctx, `SELECT connected AND last_seen>$2,session_id FROM agents WHERE agent_id=$1`, agent, time.Now().Add(-s.timeout)).Scan(&connected, &session); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT agent_id,COALESCE(device_id,''),resource_kind,resource_id,resource_context,source,scope,assessment,evaluated_at FROM resource_health_observations WHERE agent_id=$1 AND resource_kind=$2 AND ($3='' OR device_id=$3) AND ($4='' OR resource_id=$4) ORDER BY evaluated_at DESC,resource_id,source,scope,rule_id LIMIT 600`, agent, kind, device, resource)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []health.Observation{}
	for rows.Next() {
		var v health.Observation
		var raw []byte
		if err = rows.Scan(&v.AgentID, &v.DeviceID, &v.ResourceKind, &v.ResourceID, &v.ResourceContext, &v.Source, &v.Scope, &raw, &v.EvaluatedAt); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &v.Assessment); err != nil {
			return nil, err
		}
		v.Current = (v.Scope == "" || v.Scope == session) && (kind != "sip" || v.ResourceContext == contextID)
		staleAfter := time.Duration(monitoring.StaleAfterSeconds(interval)) * time.Second
		if kind == "agent" {
			staleAfter = 60 * time.Second
		}
		if kind != "agent" && !connected || !v.Current || v.Assessment.ObservedAt == nil || time.Since(*v.Assessment.ObservedAt) > staleAfter {
			v.Assessment.Status = "stale"
			v.Assessment.Severity = "none"
			v.Assessment.RecoveryEligible = false
			v.Assessment.Reason = "observation_or_collector_stale"
		}
		items = append(items, v)
	}
	return items, rows.Err()
}
