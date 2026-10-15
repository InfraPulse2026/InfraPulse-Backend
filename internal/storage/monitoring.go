package storage

import (
	"context"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
)

func (s *Store) UpdateMonitoring(ctx context.Context, agentID, deviceID string, p monitoring.Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	result, err := tx.Exec(ctx, `INSERT INTO monitoring_policies(agent_id,device_id,telemetry_interval_seconds,inventory_interval_seconds,topology_interval_seconds)
	SELECT $1,$2,$3,$4,$5 WHERE EXISTS(SELECT 1 FROM devices WHERE agent_id=$1 AND device_id=$2)
	ON CONFLICT(agent_id,device_id) DO UPDATE SET telemetry_interval_seconds=EXCLUDED.telemetry_interval_seconds,
	inventory_interval_seconds=EXCLUDED.inventory_interval_seconds,topology_interval_seconds=EXCLUDED.topology_interval_seconds,updated_at=now(),origin='manual',managed_revision=NULL`, agentID, deviceID, p.TelemetryIntervalSeconds, p.InventoryIntervalSeconds, p.TopologyIntervalSeconds)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err = monitoringAccessTx(ctx, tx, "device:"+agentID+":"+deviceID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
