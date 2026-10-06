package storage

import (
	"context"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
)

func (s *Store) UpdateMonitoring(ctx context.Context, agentID, deviceID string, p monitoring.Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	result, err := s.pool.Exec(ctx, `INSERT INTO monitoring_policies(agent_id,device_id,telemetry_interval_seconds,inventory_interval_seconds,topology_interval_seconds)
	SELECT $1,$2,$3,$4,$5 WHERE EXISTS(SELECT 1 FROM devices WHERE agent_id=$1 AND device_id=$2)
	ON CONFLICT(agent_id,device_id) DO UPDATE SET telemetry_interval_seconds=EXCLUDED.telemetry_interval_seconds,
	inventory_interval_seconds=EXCLUDED.inventory_interval_seconds,topology_interval_seconds=EXCLUDED.topology_interval_seconds,updated_at=now()`, agentID, deviceID, p.TelemetryIntervalSeconds, p.InventoryIntervalSeconds, p.TopologyIntervalSeconds)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
