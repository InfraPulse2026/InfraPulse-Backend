package storage

import (
	"context"
	"encoding/json"
	"errors"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/topology"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"time"
)

type topologySignalSnapshot struct {
	id                           int64
	agent, device, source, scope string
	observed, received           time.Time
	payload                      *pb.TopologyUpdate
}

func readTopologySignal(ctx context.Context, tx pgx.Tx, id int64) (topologySignalSnapshot, error) {
	var v topologySignalSnapshot
	var raw []byte
	var session string
	err := tx.QueryRow(ctx, `SELECT t.id,t.agent_id,t.device_id,t.source,t.observed_at,t.received_at,t.payload,m.session_id FROM topology_snapshots t JOIN agent_messages m USING(agent_id,message_id) WHERE t.id=$1`, id).Scan(&v.id, &v.agent, &v.device, &v.source, &v.observed, &v.received, &raw, &session)
	if err != nil {
		return v, err
	}
	// Accepted oversized snapshots remain available to the graph/archive, but do
	// not become incident baselines. Keep worker memory bounded before decoding.
	if len(raw) > 1<<20 {
		return v, nil
	}
	v.payload = &pb.TopologyUpdate{}
	if err = protojson.Unmarshal(raw, v.payload); err != nil {
		return v, err
	}
	v.scope = topologyScope(topology.Observation{Source: v.source, Neighbor: &pb.Neighbor{}}, v.payload, session)
	for _, n := range v.payload.Observations {
		scope := topologyScope(topology.Observation{Source: n.Source, Neighbor: n}, v.payload, session)
		if scope != v.scope {
			v.payload = nil
			break
		}
	}
	return v, nil
}

func (s *Store) ProcessTopologySignals(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73512016)`); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT p.snapshot_id FROM topology_signal_pending p JOIN topology_snapshots t ON t.id=p.snapshot_id ORDER BY t.observed_at,t.id LIMIT 25 FOR UPDATE OF p SKIP LOCKED`)
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
		if err = s.applyTopologySnapshot(ctx, tx, id); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM topology_signal_pending WHERE snapshot_id=$1`, id); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit(ctx)
}

func (s *Store) applyTopologySnapshot(ctx context.Context, tx pgx.Tx, id int64) error {
	v, err := readTopologySignal(ctx, tx, id)
	if err != nil {
		return err
	}
	peers, ok := topology.PeerSnapshot(v.payload, v.received)
	if !ok {
		return nil
	}
	if v.observed.After(time.Now().Add(30 * time.Second)) {
		return nil
	}
	var binding string
	// Exact source-owned device binding, never discovery hostname matching.
	err = tx.QueryRow(ctx, `SELECT b.id FROM asset_source_bindings b JOIN data_sources d ON d.id=b.source_instance_id JOIN assets a ON a.id=b.asset_id JOIN agents ca ON ca.agent_id=b.legacy_agent_id WHERE d.type='COLLECTOR' AND d.connection_ref=$1 AND b.legacy_agent_id=$1 AND b.legacy_device_id=$2 AND d.enabled AND b.external_type='device' AND b.external_id=$2 AND b.scope=$3 AND a.lifecycle_status='observed' AND a.resolution_status<>'conflict' AND ($3='' OR ca.session_id=$3)`, v.agent, v.device, v.scope).Scan(&binding)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var oldID int64
	var oldObserved time.Time
	var oldBinding string
	err = tx.QueryRow(ctx, `SELECT snapshot_id,observed_at,binding_id FROM topology_signal_state WHERE agent_id=$1 AND device_id=$2 AND source=$3 AND scope=$4`, v.agent, v.device, v.source, v.scope).Scan(&oldID, &oldObserved, &oldBinding)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && !v.observed.After(oldObserved) {
		return nil
	}
	if err == nil && oldBinding == binding {
		previous, e := readTopologySignal(ctx, tx, oldID)
		if e != nil {
			return e
		}
		oldPeers, valid := topology.PeerSnapshot(previous.payload, previous.received)
		if valid {
			for port, after := range peers {
				before, exists := oldPeers[port]
				// A gap or expiry destroys continuity; rediscovery starts a baseline.
				if exists && before.PeerHash != after.PeerHash && before.ExpiresAt.After(after.ObservedAt) && after.ObservedAt.After(before.ObservedAt) {
					if e = s.applyTopologyPeerChange(ctx, tx, v, binding, port, oldID, before, after); e != nil {
						return e
					}
				}
			}
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO topology_signal_state(agent_id,device_id,source,scope,binding_id,snapshot_id,observed_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(agent_id,device_id,source,scope) DO UPDATE SET binding_id=excluded.binding_id,snapshot_id=excluded.snapshot_id,observed_at=excluded.observed_at`, v.agent, v.device, v.source, v.scope, binding, v.id, v.observed)
	return err
}

func (s *Store) applyTopologyPeerChange(ctx context.Context, tx pgx.Tx, v topologySignalSnapshot, binding, port string, oldID int64, before, after topology.PeerEndpoint) error {
	key := topology.PeerEventKey(binding, v.source, v.scope, port)
	prior, err := readIncident(tx.QueryRow(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE resource_kind='topology' AND resource_id=$1 AND resource_context=$2 AND state<>'resolved' FOR UPDATE`, key, binding))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	count := int64(1)
	if err == nil && prior.Signal.Topology != nil {
		count = prior.Signal.Topology.EventCount + 1
	}
	evidence := &health.TopologyEvidence{BindingID: binding, BeforeSnapshotID: oldID, AfterSnapshotID: v.id, LocalPortHash: port, BeforePeerHash: before.PeerHash, AfterPeerHash: after.PeerHash, Protocol: after.Protocol, EventCount: count, ExpiresAt: after.ExpiresAt, Lifecycle: "requires_review"}
	a := health.Assessment{RuleID: "topology.peer_changed", RuleVersion: "1", Status: "warning", Severity: "warning", Reason: "reported_neighbor_endpoint_changed", ObservedAt: &after.ObservedAt, RecoveryEligible: false, Evidence: []health.Point{}, Topology: evidence}
	encoded, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if err = applyResourceIncident(ctx, tx, v.agent, v.device, v.source, v.scope, "topology", key, binding, a, encoded); err != nil {
		return err
	}
	var incidentID string
	if err = tx.QueryRow(ctx, `SELECT id FROM incidents WHERE resource_kind='topology' AND resource_id=$1 AND resource_context=$2 AND state<>'resolved'`, key, binding).Scan(&incidentID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO topology_incident_records(incident_id,binding_id,before_snapshot_id,after_snapshot_id,local_port_hash,observed_at) VALUES($1,$2,$3,$4,$5,$6)`, incidentID, binding, oldID, v.id, port, after.ObservedAt)
	return err
}

func (s *Store) ReviewTopologyIncident(ctx context.Context, id string, input health.LogReview) (health.Incident, error) {
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
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73512016)`); err != nil {
		return empty, err
	}
	if err = commandActor(ctx, tx, actor.User.ID, actor.SessionHash); err != nil {
		return empty, err
	}
	v, err := readIncident(tx.QueryRow(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return empty, err
	}
	if v.ResourceKind != "topology" || v.Signal.Topology == nil || v.Signal.Topology.EventCount != input.ExpectedEventCount {
		return empty, health.ErrConflict
	}
	if v.State == "resolved" {
		if v.Signal.Topology.ReviewedAt == nil {
			return empty, health.ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return empty, err
		}
		return s.Incident(ctx, id)
	}
	var agent string
	if err = tx.QueryRow(ctx, `SELECT agent_id FROM agents WHERE agent_id=$1 FOR UPDATE`, v.AgentID).Scan(&agent); err != nil {
		return empty, err
	}
	var pending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM topology_signal_pending p JOIN topology_snapshots t ON t.id=p.snapshot_id WHERE t.agent_id=$1 AND t.device_id=$2 AND t.source=$3)`, v.AgentID, v.DeviceID, v.Source).Scan(&pending); err != nil {
		return empty, err
	}
	if pending {
		return empty, health.ErrConflict
	}
	now := time.Now().UTC()
	v.Signal.Topology.Lifecycle = "reviewed_event"
	v.Signal.Topology.ReviewedAt = &now
	v.Signal.Topology.ReviewedBy = actor.User.ID
	v.Signal.Topology.ReviewReason = logbook.SanitizeText(input.Reason)
	v.Signal.Reason = "operator_reviewed_topology_event"
	encoded, err := json.Marshal(v.Signal)
	if err != nil {
		return empty, err
	}
	if _, err = tx.Exec(ctx, `UPDATE incidents SET state='resolved',resolved_at=now(),updated_at=now(),signal=$2 WHERE id=$1`, id, encoded); err != nil {
		return empty, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO incident_transitions(incident_id,state,reason,actor_id) VALUES($1,'resolved','operator_reviewed_topology_event',$2)`, id, actor.User.ID); err != nil {
		return empty, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO incident_evidence(incident_id,kind,observed_at,assessment) VALUES($1,'topology_event_review',$2,$3)`, id, now, encoded); err != nil {
		return empty, err
	}
	meta := access.CurrentMeta(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,'topology_event_reviewed','success',$2,$3,$4)`, actor.User.ID, "incident:"+id, meta.RequestID, meta.Peer); err != nil {
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

func (s *Store) TopologyIncidentRecords(ctx context.Context, id string, limit, offset int) ([]health.TopologyIncidentRecord, error) {
	if limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, errors.New("invalid pagination")
	}
	v, err := s.Incident(ctx, id)
	if err != nil {
		return nil, err
	}
	if v.ResourceKind != "topology" {
		return nil, health.ErrConflict
	}
	rows, err := s.pool.Query(ctx, `SELECT id,before_snapshot_id,after_snapshot_id,binding_id,local_port_hash,observed_at FROM topology_incident_records WHERE incident_id=$1 ORDER BY id LIMIT $2 OFFSET $3`, id, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []health.TopologyIncidentRecord{}
	for rows.Next() {
		var v health.TopologyIncidentRecord
		if err = rows.Scan(&v.ID, &v.BeforeSnapshotID, &v.AfterSnapshotID, &v.BindingID, &v.LocalPortHash, &v.ObservedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) topologyIncidentFreshness(ctx context.Context, q freshnessQueryer, items []health.Incident, now time.Time) error {
	ids := []string{}
	for _, v := range items {
		if v.ResourceKind == "topology" && v.State != "resolved" {
			ids = append(ids, v.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `SELECT i.id,COALESCE(d.enabled AND a.lifecycle_status='observed' AND a.resolution_status<>'conflict' AND b.legacy_agent_id=i.agent_id AND b.legacy_device_id=i.device_id AND b.scope=i.scope,false),COALESCE(ca.connected AND ca.last_seen>$2::timestamptz-($3::double precision*interval '1 second') AND (i.scope='' OR ca.session_id=i.scope),false) FROM incidents i LEFT JOIN asset_source_bindings b ON b.id=i.resource_context LEFT JOIN data_sources d ON d.id=b.source_instance_id LEFT JOIN assets a ON a.id=b.asset_id LEFT JOIN agents ca ON ca.agent_id=i.agent_id WHERE i.id=ANY($1::text[])`, ids, now, s.timeout.Seconds())
	if err != nil {
		return err
	}
	statuses := map[string]string{}
	for rows.Next() {
		var id string
		var enabled, connected bool
		if err = rows.Scan(&id, &enabled, &connected); err != nil {
			rows.Close()
			return err
		}
		status := "fresh"
		if !enabled || !connected {
			status = "source_unavailable"
		}
		statuses[id] = status
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for i := range items {
		v := &items[i]
		if v.ResourceKind != "topology" || v.State == "resolved" {
			continue
		}
		v.EvidenceStatus = statuses[v.ID]
		if v.EvidenceStatus == "" {
			v.EvidenceStatus = "missing"
		}
		if v.EvidenceStatus == "fresh" && (v.Signal.Topology == nil || !v.Signal.Topology.ExpiresAt.After(now)) {
			v.EvidenceStatus = "stale"
		}
	}
	return nil
}
