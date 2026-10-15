package storage

import (
	"context"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/topology"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
)

func topologyScope(o topology.Observation, t *pb.TopologyUpdate, session string) string {
	if o.Source == "simulated" || t.Source == "simulated" || o.Neighbor.GetObservation().GetSource().GetProvenance() == "simulated" || t.GetObservation().GetSource().GetProvenance() == "simulated" {
		return session
	}
	return ""
}

const globalTopologyUpsert = `WITH old AS (
 SELECT * FROM global_topology_latest WHERE source_instance_id=$1 AND scope=$2 AND observation_id=$3
), eligible AS (
 SELECT NOT EXISTS(SELECT 1 FROM old) OR EXISTS(SELECT 1 FROM old WHERE
 ($9<=now()+interval '30 seconds' AND (observed_at>now()+interval '30 seconds' OR $9>=observed_at)) OR
 ($9>now()+interval '30 seconds' AND observed_at>now()+interval '30 seconds' AND $9>=observed_at)) AS ok
), catchup_expiry AS (
 INSERT INTO global_topology_events(version_id,kind,occurred_at)
 SELECT version_id,'expired',expires_at FROM old WHERE NOT expiry_logged AND expires_at<=now() AND $11>now() AND (SELECT ok FROM eligible) RETURNING id
), new_version AS (
 INSERT INTO global_topology_versions(source_instance_id,scope,observation_id,agent_id,device_id,message_id,source,protocol,observed_at,received_at,expires_at,fingerprint,payload)
 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13 FROM eligible WHERE ok AND (SELECT count(*) FROM catchup_expiry)>=0 AND
 (NOT EXISTS(SELECT 1 FROM old) OR EXISTS(SELECT 1 FROM old WHERE fingerprint<>$12 OR ((expires_at<$9 OR expires_at<=now() OR expiry_logged) AND $11>now()))) RETURNING id
), new_event AS (
 INSERT INTO global_topology_events(version_id,kind,occurred_at)
 SELECT id,CASE WHEN $14 THEN 'imported' WHEN NOT EXISTS(SELECT 1 FROM old) THEN 'observed'
 WHEN EXISTS(SELECT 1 FROM old WHERE (expires_at<$9 OR expires_at<=now() OR expiry_logged) AND $11>now()) THEN 'revived' ELSE 'changed' END,$9 FROM new_version
), chosen AS (
 SELECT id FROM new_version UNION ALL SELECT version_id FROM old WHERE NOT EXISTS(SELECT 1 FROM new_version) AND (SELECT ok FROM eligible)
)
INSERT INTO global_topology_latest(source_instance_id,scope,observation_id,version_id,agent_id,device_id,message_id,source,protocol,observed_at,received_at,expires_at,fingerprint,payload)
SELECT $1,$2,$3,id,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13 FROM chosen
ON CONFLICT(source_instance_id,scope,observation_id) DO UPDATE SET version_id=EXCLUDED.version_id,message_id=EXCLUDED.message_id,observed_at=EXCLUDED.observed_at,received_at=EXCLUDED.received_at,expires_at=EXCLUDED.expires_at,fingerprint=EXCLUDED.fingerprint,payload=EXCLUDED.payload,expiry_logged=global_topology_latest.expiry_logged AND EXCLUDED.expires_at<=now()`

func globalTopologyArgs(source, scope, message string, o topology.Observation, importing bool) ([]any, error) {
	raw, err := wireJSON(o.Neighbor)
	return []any{source, scope, o.ID, o.AgentID, o.DeviceID, message, o.Source, o.Protocol, o.ObservedAt, o.ReceivedAt, o.ExpiresAt, topology.BusinessFingerprint(o.Neighbor), raw, importing}, err
}
func storeGlobalTopology(ctx context.Context, tx pgx.Tx, source, scope, message string, o topology.Observation, importing bool) error {
	args, err := globalTopologyArgs(source, scope, message, o, importing)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, globalTopologyUpsert, args...)
	return err
}
func projectGlobalTopology(ctx context.Context, tx pgx.Tx, e *pb.AgentEnvelope, observations []topology.Observation) error {
	if len(observations) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194040)`); err != nil {
		return err
	}
	source, err := collectorAssetSource(ctx, tx, e.AgentId)
	if err != nil {
		return err
	}
	for start := 0; start < len(observations); start += 256 {
		batch := &pgx.Batch{}
		for _, o := range observations[start:min(start+256, len(observations))] {
			args, e2 := globalTopologyArgs(source, topologyScope(o, e.GetTopology(), e.SessionId), e.MessageId, o, false)
			if e2 != nil {
				return e2
			}
			batch.Queue(globalTopologyUpsert, args...)
		}
		if err = tx.SendBatch(ctx, batch).Close(); err != nil {
			return err
		}
	}
	return nil
}

// Seed only already accepted current observations. Older snapshots remain in
// the immutable legacy archive; importing does not fabricate prior transitions.
func seedGlobalTopology(ctx context.Context, tx pgx.Tx) error {
	lastAgent, lastID := "", ""
	sources := map[string]string{}
	for {
		rows, err := tx.Query(ctx, `SELECT o.agent_id,o.edge_id,o.device_id,o.message_id,o.source,o.protocol,o.observed_at,o.received_at,o.expires_at,o.payload,m.session_id,COALESCE(m.payload->'topology'->>'source',''),COALESCE(m.payload->'topology'->'observation'->'source'->>'provenance','') FROM topology_observations o JOIN agent_messages m USING(agent_id,message_id) WHERE (o.agent_id,o.edge_id)>($1,$2) ORDER BY o.agent_id,o.edge_id LIMIT 256`, lastAgent, lastID)
		if err != nil {
			return err
		}
		type record struct {
			o              topology.Observation
			message, scope string
		}
		page := []record{}
		for rows.Next() {
			var r record
			var raw []byte
			var session, source, provenance string
			if err = rows.Scan(&r.o.AgentID, &r.o.ID, &r.o.DeviceID, &r.message, &r.o.Source, &r.o.Protocol, &r.o.ObservedAt, &r.o.ReceivedAt, &r.o.ExpiresAt, &raw, &session, &source, &provenance); err != nil {
				rows.Close()
				return err
			}
			r.o.Neighbor = &pb.Neighbor{}
			if err = protojson.Unmarshal(raw, r.o.Neighbor); err != nil {
				rows.Close()
				return err
			}
			if r.o.Source == "simulated" || source == "simulated" || provenance == "simulated" || r.o.Neighbor.GetObservation().GetSource().GetProvenance() == "simulated" {
				r.scope = session
			}
			page = append(page, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, r := range page {
			source := sources[r.o.AgentID]
			if source == "" {
				source, err = collectorAssetSource(ctx, tx, r.o.AgentID)
				if err != nil {
					return err
				}
				sources[r.o.AgentID] = source
			}
			if err = storeGlobalTopology(ctx, tx, source, r.scope, r.message, r.o, true); err != nil {
				return err
			}
			lastAgent, lastID = r.o.AgentID, r.o.ID
		}
	}
}

func (s *Store) ExpireGlobalTopology(ctx context.Context, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194040)`); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `WITH due AS (SELECT source_instance_id,scope,observation_id,version_id,expires_at FROM global_topology_latest WHERE NOT expiry_logged AND expires_at<=$1 ORDER BY expires_at,source_instance_id,scope,observation_id LIMIT 500 FOR UPDATE), mark AS (UPDATE global_topology_latest l SET expiry_logged=true FROM due d WHERE l.source_instance_id=d.source_instance_id AND l.scope=d.scope AND l.observation_id=d.observation_id RETURNING d.version_id,d.expires_at) INSERT INTO global_topology_events(version_id,kind,occurred_at) SELECT version_id,'expired',expires_at FROM mark`, now)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
