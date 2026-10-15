package storage

import (
	"context"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/topology"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
)

const MaxTopologyInventory = 2000

func projectTopology(ctx context.Context, tx pgx.Tx, e *pb.AgentEnvelope) error {
	t := e.GetTopology()
	observations, err := topology.Prepare(e.AgentId, t, time.Now())
	if err != nil {
		return err
	}
	// An observation source may precede inventory. It does not prove health.
	_, err = tx.Exec(ctx, `INSERT INTO devices(agent_id,device_id,name,source,state_at,last_seen)
	VALUES($1,$2,$2,$3,$4,$4) ON CONFLICT(agent_id,device_id) DO NOTHING`, e.AgentId, t.DeviceId, t.Source, t.ObservedAt.AsTime())
	if err != nil {
		return err
	}
	raw, err := wireJSON(t)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO topology_snapshots(agent_id,message_id,device_id,source,observed_at,collected_at,completeness,ttl_seconds,expiry_assumption,observation_count,payload)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, e.AgentId, e.MessageId, t.DeviceId, t.Source, t.ObservedAt.AsTime(), at(t.CollectedAt, t.ObservedAt.AsTime()), t.Completeness.String(), t.TtlSeconds, t.ExpiryAssumption, len(t.Observations), raw)
	if err != nil {
		return err
	}
	const query = `INSERT INTO topology_observations(agent_id,edge_id,device_id,message_id,source,protocol,observed_at,expires_at,payload)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT(agent_id,edge_id) DO UPDATE SET
		message_id=EXCLUDED.message_id,source=EXCLUDED.source,protocol=EXCLUDED.protocol,
		observed_at=EXCLUDED.observed_at,expires_at=EXCLUDED.expires_at,received_at=now(),payload=EXCLUDED.payload
		WHERE
		(EXCLUDED.observed_at<=now()+interval '30 seconds' AND
		 (topology_observations.observed_at>now()+interval '30 seconds' OR EXCLUDED.observed_at>=topology_observations.observed_at))
		OR (EXCLUDED.observed_at>now()+interval '30 seconds' AND topology_observations.observed_at>now()+interval '30 seconds'
		 AND EXCLUDED.observed_at>=topology_observations.observed_at)`
	// Bounded batches avoid one database round trip per observed neighbor.
	for start := 0; start < len(observations); start += 256 {
		batch := &pgx.Batch{}
		for _, o := range observations[start:min(start+256, len(observations))] {
			raw, err := wireJSON(o.Neighbor)
			if err != nil {
				return err
			}
			batch.Queue(query,
				o.AgentID, o.ID, o.DeviceID, e.MessageId, o.Source, o.Protocol, o.ObservedAt, o.ExpiresAt, raw)
		}
		if err = tx.SendBatch(ctx, batch).Close(); err != nil {
			return err
		}
	}
	// Absence in a complete/partial/failed poll never rewrites collector TTL.
	if err = projectGlobalTopology(ctx, tx, e, observations); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO topology_signal_pending(snapshot_id) SELECT id FROM topology_snapshots WHERE agent_id=$1 AND message_id=$2 ON CONFLICT DO NOTHING`, e.AgentId, e.MessageId)
	return err
}

func (s *Store) Topology(ctx context.Context, agentID, deviceID string, includeExpired bool, limit, offset int) (topology.Graph, error) {
	now := time.Now().UTC()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return topology.Graph{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	devices, inventoryTruncated, err := topologyDevices(ctx, tx, agentID, now.Add(-s.timeout))
	if err != nil {
		return topology.Graph{}, err
	}
	states, err := s.availabilityPage(ctx, tx, agentID, "", MaxTopologyInventory+1, 0, now)
	if err != nil {
		return topology.Graph{}, err
	}
	for i := range devices {
		devices[i].Availability = states[deviceKey{devices[i].AgentID, devices[i].ID}]
		devices[i].StateReported = devices[i].Availability.LastReportedOnline != nil
		if devices[i].StateReported {
			devices[i].ReportedOnline = *devices[i].Availability.LastReportedOnline
		}
		devices[i].AgentConnected = devices[i].Availability.AgentConnected
	}
	observations, truncated, err := topologyObservations(ctx, tx, agentID, deviceID, includeExpired, now, limit, offset)
	if err != nil {
		return topology.Graph{}, err
	}
	collections, collectionTruncated, err := topologyCollections(ctx, tx, agentID, deviceID, now)
	if err != nil {
		return topology.Graph{}, err
	}
	g := topology.Build(now, devices, observations, collections, deviceID, includeExpired, inventoryTruncated)
	g.Truncated = truncated || inventoryTruncated || collectionTruncated || offset > 0
	if err = tx.Commit(ctx); err != nil {
		return topology.Graph{}, err
	}
	return g, nil
}

func topologyDevices(ctx context.Context, tx pgx.Tx, agentID string, cutoff time.Time) ([]topology.Device, bool, error) {
	rows, err := tx.Query(ctx, `SELECT d.agent_id,d.device_id,d.name,d.hostname,COALESCE(d.inventory->>'chassis_id',''),
	d.management_address,d.vendor,d.model,d.type,d.source,d.online,a.connected AND a.last_seen>$2,d.last_seen,
	(d.inventory_at IS NOT NULL OR EXISTS(SELECT 1 FROM telemetry t WHERE t.agent_id=d.agent_id AND t.device_id=d.device_id))
	FROM devices d JOIN agents a USING(agent_id) WHERE ($1='' OR d.agent_id=$1)
	ORDER BY d.agent_id,d.device_id LIMIT $3`, agentID, cutoff, MaxTopologyInventory+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := []topology.Device{}
	for rows.Next() {
		var d topology.Device
		if err = rows.Scan(&d.AgentID, &d.ID, &d.Name, &d.Hostname, &d.ChassisID, &d.ManagementAddress, &d.Vendor, &d.Model, &d.Type, &d.Source, &d.ReportedOnline, &d.AgentConnected, &d.LastSeen, &d.StateReported); err != nil {
			return nil, false, err
		}
		items = append(items, d)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(items) > MaxTopologyInventory
	if truncated {
		items = items[:MaxTopologyInventory]
	}
	return items, truncated, nil
}

func topologyObservations(ctx context.Context, tx pgx.Tx, agentID, deviceID string, includeExpired bool, now time.Time, limit, offset int) ([]topology.Observation, bool, error) {
	rows, err := tx.Query(ctx, `SELECT edge_id,agent_id,device_id,source,protocol,observed_at,received_at,expires_at,payload
	FROM topology_observations WHERE ($1='' OR agent_id=$1) AND ($2='' OR device_id=$2)
	AND ($3 OR (expires_at>$4 AND observed_at<=$5))
	ORDER BY agent_id,device_id,edge_id LIMIT $6 OFFSET $7`, agentID, deviceID, includeExpired, now, now.Add(30*time.Second), limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := []topology.Observation{}
	for rows.Next() {
		var o topology.Observation
		var raw []byte
		if err = rows.Scan(&o.ID, &o.AgentID, &o.DeviceID, &o.Source, &o.Protocol, &o.ObservedAt, &o.ReceivedAt, &o.ExpiresAt, &raw); err != nil {
			return nil, false, err
		}
		o.Neighbor = &pb.Neighbor{}
		if err = protojson.Unmarshal(raw, o.Neighbor); err != nil {
			return nil, false, err
		}
		items = append(items, o)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(items) > limit
	if truncated {
		items = items[:limit]
	}
	return items, truncated, nil
}

func topologyCollections(ctx context.Context, tx pgx.Tx, agentID, deviceID string, now time.Time) ([]topology.Collection, bool, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON(agent_id,device_id,source) agent_id,device_id,message_id,source,
	completeness,observed_at,collected_at,received_at,ttl_seconds,expiry_assumption,observation_count
	FROM topology_snapshots WHERE ($1='' OR agent_id=$1) AND ($2='' OR device_id=$2)
	ORDER BY agent_id,device_id,source,(observed_at>$4),observed_at DESC,id DESC LIMIT $3`, agentID, deviceID, MaxTopologyInventory+1, now.Add(30*time.Second))
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := []topology.Collection{}
	for rows.Next() {
		var c topology.Collection
		if err = rows.Scan(&c.AgentID, &c.DeviceID, &c.MessageID, &c.Source, &c.Completeness, &c.ObservedAt, &c.CollectedAt, &c.ReceivedAt, &c.TTLSeconds, &c.ExpiryAssumption, &c.ObservationCount); err != nil {
			return nil, false, err
		}
		c.ObservedAt, c.CollectedAt, c.ReceivedAt = c.ObservedAt.UTC(), c.CollectedAt.UTC(), c.ReceivedAt.UTC()
		c.ClockSkew = c.ObservedAt.After(now.Add(30 * time.Second))
		items = append(items, c)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(items) > MaxTopologyInventory
	if truncated {
		items = items[:MaxTopologyInventory]
	}
	return items, truncated, nil
}
