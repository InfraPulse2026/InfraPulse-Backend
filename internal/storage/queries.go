package storage

import (
	"context"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"time"
)

func (s *Store) Agents(ctx context.Context) ([]Agent, error) {
	rows, err := s.pool.Query(ctx, `SELECT agent_id,session_id,name,version,protocol_version,
	connected AND last_seen>$1,last_seen,COALESCE(heartbeat,'null'::jsonb) FROM agents ORDER BY agent_id LIMIT 1000`, time.Now().Add(-s.timeout))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Agent, 0)
	for rows.Next() {
		var a Agent
		if err = rows.Scan(&a.AgentID, &a.SessionID, &a.Name, &a.Version, &a.ProtocolVersion, &a.Connected, &a.LastSeen, &a.Heartbeat); err != nil {
			return nil, err
		}
		a.LastSeen = a.LastSeen.UTC()
		items = append(items, a)
	}
	return items, rows.Err()
}

func (s *Store) Devices(ctx context.Context, agentID, id string, limit, offset int) ([]Device, error) {
	rows, err := s.pool.Query(ctx, `SELECT d.agent_id,d.device_id,d.name,d.hostname,d.vendor,d.model,d.type,d.management_address,d.source,
	d.online,a.connected AND a.last_seen>$3,d.last_seen,d.received_at,d.inventory,
	COALESCE(p.telemetry_interval_seconds,$6),COALESCE(p.inventory_interval_seconds,$7),COALESCE(p.topology_interval_seconds,$8),
	p.device_id IS NOT NULL,t.observed_at,t.received_at
	FROM devices d JOIN agents a USING(agent_id)
	LEFT JOIN monitoring_policies p ON p.agent_id=d.agent_id AND p.device_id=d.device_id
	LEFT JOIN LATERAL (SELECT observed_at,received_at FROM telemetry WHERE agent_id=d.agent_id AND device_id=d.device_id ORDER BY observed_at DESC,id DESC LIMIT 1) t ON true
	WHERE ($1='' OR d.agent_id=$1) AND ($2='' OR d.device_id=$2) ORDER BY d.agent_id,d.device_id LIMIT $4 OFFSET $5`, agentID, id, time.Now().Add(-s.timeout), limit, offset, s.defaults.TelemetryIntervalSeconds, s.defaults.InventoryIntervalSeconds, s.defaults.TopologyIntervalSeconds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Device, 0)
	for rows.Next() {
		var d Device
		var policy monitoring.Policy
		var override bool
		var observed, received *time.Time
		if err = rows.Scan(&d.AgentID, &d.DeviceID, &d.Name, &d.Hostname, &d.Vendor, &d.Model, &d.Type, &d.ManagementAddress, &d.Source, &d.Online, &d.AgentConnected, &d.LastSeen, &d.ReceivedAt, &d.Inventory,
			&policy.TelemetryIntervalSeconds, &policy.InventoryIntervalSeconds, &policy.TopologyIntervalSeconds, &override, &observed, &received); err != nil {
			return nil, err
		}
		d.LastSeen = d.LastSeen.UTC()
		d.ReceivedAt = d.ReceivedAt.UTC()
		origin := "backend_default"
		if override {
			origin = "device_override"
		}
		d.Monitoring = monitoring.Describe(policy, origin)
		d.TelemetryFreshness = monitoring.Evaluate(time.Now(), observed, received, policy.TelemetryIntervalSeconds)
		items = append(items, d)
	}
	return items, rows.Err()
}

func (s *Store) Device(ctx context.Context, agentID, id string) (Device, error) {
	items, err := s.Devices(ctx, agentID, id, 2, 0)
	if err != nil {
		return Device{}, err
	}
	if len(items) == 0 {
		return Device{}, ErrNotFound
	}
	if len(items) > 1 {
		return Device{}, ErrAmbiguous
	}
	return items[0], nil
}

func (s *Store) Metrics(ctx context.Context, agentID, id string, limit int) ([]Telemetry, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,agent_id,device_id,observed_at,collected_at,received_at,source,cpu_usage_percent,
	memory_total_bytes::text,memory_used_bytes::text,payload FROM telemetry WHERE agent_id=$1 AND device_id=$2 ORDER BY observed_at DESC,id DESC LIMIT $3`, agentID, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Telemetry, 0)
	for rows.Next() {
		var t Telemetry
		if err = rows.Scan(&t.ID, &t.AgentID, &t.DeviceID, &t.ObservedAt, &t.CollectedAt, &t.ReceivedAt, &t.Source, &t.CPUUsagePercent, &t.MemoryTotalBytes, &t.MemoryUsedBytes, &t.Sample); err != nil {
			return nil, err
		}
		t.ObservedAt = t.ObservedAt.UTC()
		t.CollectedAt = t.CollectedAt.UTC()
		t.ReceivedAt = t.ReceivedAt.UTC()
		items = append(items, t)
	}
	return items, rows.Err()
}
