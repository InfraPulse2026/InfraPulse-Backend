package storage

import (
	"context"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/availability"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"github.com/jackc/pgx/v5"
)

type availabilityQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}
type deviceKey struct{ agentID, deviceID string }

// Every public view shares the same selector and evaluator.
func (s *Store) availabilityEach(ctx context.Context, q availabilityQueryer, agentID, deviceID string, limit, offset int, now time.Time, consume func(string, string, availability.State)) error {
	rows, err := q.Query(ctx, `SELECT d.agent_id,d.device_id,a.connected AND a.last_seen>$3,
	COALESCE(p.telemetry_interval_seconds,$6),COALESCE(p.inventory_interval_seconds,$7),
	v.kind,v.online,v.observed_at,v.received_at,COALESCE(v.source,''),COALESCE(v.message_id,''),COALESCE(v.event_id,''),COALESCE(v.time_basis,''),
	k.kind,k.online,k.observed_at,k.received_at,COALESCE(k.source,''),COALESCE(k.message_id,''),COALESCE(k.event_id,''),COALESCE(k.time_basis,'')
	FROM devices d JOIN agents a USING(agent_id)
	LEFT JOIN monitoring_policies p ON p.agent_id=d.agent_id AND p.device_id=d.device_id
	LEFT JOIN device_availability_current v ON v.agent_id=d.agent_id AND v.device_id=d.device_id
	LEFT JOIN LATERAL(SELECT kind,online,observed_at,received_at,source,message_id,event_id,time_basis
	  FROM availability_sources WHERE agent_id=d.agent_id AND device_id=d.device_id AND clock_skew
	  ORDER BY received_at DESC,observed_at DESC LIMIT 1) k ON true
	WHERE ($1='' OR d.agent_id=$1) AND ($2='' OR d.device_id=$2)
	ORDER BY d.agent_id,d.device_id LIMIT NULLIF($4,0) OFFSET $5`,
		agentID, deviceID, now.Add(-s.timeout), limit, offset, s.defaults.TelemetryIntervalSeconds, s.defaults.InventoryIntervalSeconds)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var agent, device string
		var connected bool
		policy := monitoring.Defaults()
		var kind, clockKind *string
		var online, clockOnline *bool
		var observed, received, clockObserved, clockReceived *time.Time
		var source, message, event, basis, clockSource, clockMessage, clockEvent, clockBasis string
		if err = rows.Scan(&agent, &device, &connected, &policy.TelemetryIntervalSeconds, &policy.InventoryIntervalSeconds,
			&kind, &online, &observed, &received, &source, &message, &event, &basis,
			&clockKind, &clockOnline, &clockObserved, &clockReceived, &clockSource, &clockMessage, &clockEvent, &clockBasis); err != nil {
			return err
		}
		input := availability.Input{AgentConnected: connected, Policy: policy, AgentTimeout: s.timeout}
		if kind != nil {
			input.Latest = &availability.Observation{Kind: *kind, Online: *online, ObservedAt: *observed, ReceivedAt: *received, Source: source, MessageID: message, EventID: event, TimeBasis: basis}
		}
		if clockKind != nil {
			input.ClockIssue = &availability.Observation{Kind: *clockKind, Online: *clockOnline, ObservedAt: *clockObserved, ReceivedAt: *clockReceived, Source: clockSource, MessageID: clockMessage, EventID: clockEvent, TimeBasis: clockBasis}
		}
		consume(agent, device, availability.Evaluate(now, input))
	}
	return rows.Err()
}

func (s *Store) availabilityPage(ctx context.Context, q availabilityQueryer, agentID, deviceID string, limit, offset int, now time.Time) (map[deviceKey]availability.State, error) {
	items := map[deviceKey]availability.State{}
	err := s.availabilityEach(ctx, q, agentID, deviceID, limit, offset, now, func(agent, device string, state availability.State) { items[deviceKey{agent, device}] = state })
	return items, err
}

func (s *Store) AvailabilitySummary(ctx context.Context, agentID string) (availability.Summary, error) {
	now := time.Now().UTC()
	out := availability.Summary{GeneratedAt: now}
	err := s.availabilityEach(ctx, s.pool, agentID, "", 0, 0, now, func(_, _ string, state availability.State) {
		out.Total++
		switch state.Status {
		case "online":
			out.Online++
		case "offline":
			out.Offline++
		case "stale":
			out.Stale++
		default:
			out.Unknown++
		}
		if state.ClockSkew {
			out.ClockSkew++
		}
		if !state.AgentConnected {
			out.AgentDisconnected++
		}
	})
	return out, err
}
