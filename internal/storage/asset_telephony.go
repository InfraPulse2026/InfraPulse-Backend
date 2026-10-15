package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assetvoip"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/telephony"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const assetVoIPColumns = `v.id,b.asset_id,v.binding_id,v.source_instance_id,b.source_instance_id,v.agent_id,v.target_id,v.address,v.transport,v.source,v.scope,v.service_kind,v.name,v.enabled,v.revision,v.reviewed_sample_id,v.reviewed_at,v.reviewed_by,v.created_at,v.updated_at`
const assetVoIPJoin = ` FROM asset_voip_associations v JOIN asset_source_bindings b ON b.id=v.binding_id`

func assetVoIPError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var p *pgconn.PgError
	if errors.As(err, &p) && (p.Code == "23505" || p.Code == "23514" || p.Code == "23503") {
		return assetvoip.ErrConflict
	}
	return err
}

func readAssetVoIP(row interface{ Scan(...any) error }) (assetvoip.Association, error) {
	var v assetvoip.Association
	err := row.Scan(&v.ID, &v.AssetID, &v.BindingID, &v.SourceInstanceID, &v.InventorySourceInstanceID, &v.AgentID, &v.TargetID, &v.Address, &v.Transport, &v.Source, &v.Scope, &v.ServiceKind, &v.Name, &v.Enabled, &v.Revision, &v.ReviewedSampleID, &v.ReviewedAt, &v.ReviewedBy, &v.CreatedAt, &v.UpdatedAt)
	v.ReviewedAt, v.CreatedAt, v.UpdatedAt = v.ReviewedAt.UTC(), v.CreatedAt.UTC(), v.UpdatedAt.UTC()
	return v, assetVoIPError(err)
}

// VoIP scope is determined by the received source, not by the supplied Asset ID.
// Real observations survive Collector reconnects. Simulated observations are
// isolated by their archived session, so the legacy target's global latest
// pointer must never be used for an Asset association.
const assetVoIPScopeSQL = `asset_voip_scope(m.payload,m.session_id,t.target_id,t.source)=$3`

func assetVoIPSampleTx(ctx context.Context, tx pgx.Tx, agent, target, scope string, clockIssue bool) (*telephony.Sample, error) {
	var raw []byte
	order := ` ORDER BY t.observed_at DESC,t.received_at DESC,t.id DESC`
	if clockIssue {
		order = ` ORDER BY t.received_at DESC,t.id DESC`
	}
	err := tx.QueryRow(ctx, `SELECT to_jsonb(t) FROM telephony_samples t JOIN agent_messages m ON m.agent_id=t.agent_id AND m.message_id=t.message_id WHERE t.agent_id=$1 AND t.target_id=$2 AND `+assetVoIPScopeSQL+` AND t.clock_skew=$4`+order+` LIMIT 1`, agent, target, scope, clockIssue).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sample telephony.Sample
	if err = json.Unmarshal(raw, &sample); err != nil {
		return nil, err
	}
	sample.ObservedAt, sample.ReceivedAt = sample.ObservedAt.UTC(), sample.ReceivedAt.UTC()
	return &sample, nil
}

func sameVoIPEndpoint(v assetvoip.Association, sample *telephony.Sample) bool {
	return sample != nil && sample.Address == v.Address && sample.Transport == v.Transport && sample.Source == v.Source
}

func priorAssetVoIPOperation(ctx context.Context, tx pgx.Tx, actor, key, hash string) (assetvoip.Association, bool, error) {
	var out assetvoip.Association
	var oldHash string
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT request_hash,response FROM asset_voip_operations WHERE actor_id=$1 AND idempotency_key=$2`, actor, key).Scan(&oldHash, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, false, nil
	}
	if err != nil {
		return out, false, err
	}
	if oldHash != hash {
		return out, false, assetvoip.ErrConflict
	}
	err = json.Unmarshal(raw, &out)
	return out, true, err
}

func saveAssetVoIPOperation(ctx context.Context, tx pgx.Tx, actor access.Principal, key, hash, id string, before *assetvoip.Association, after assetvoip.Association, reason string) error {
	b, err := json.Marshal(before)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(after)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO asset_voip_events(association_id,actor_id,before_value,after_value,reason) VALUES($1,$2,$3,$4,$5)`, id, actor.User.ID, b, raw, logbook.SanitizeText(reason)); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO asset_voip_operations(actor_id,idempotency_key,request_hash,response) VALUES($1,$2,$3,$4)`, actor.User.ID, key, hash, raw); err != nil {
		return err
	}
	meta := access.CurrentMeta(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,'asset_voip_saved','success',$2,$3,$4)`, actor.User.ID, id, meta.RequestID, meta.Peer)
	return err
}

// SaveAssetVoIP is an explicit human decision, using the same lock order as
// Asset merge/split and location edits. associationID empty creates a record;
// an update may correct its physical binding. Real SIP can be linked to a
// reviewed Asset inventoried by a different source (for example Zabbix).
// Simulation always stays within the probing Collector and exact session.
func (s *Store) SaveAssetVoIP(ctx context.Context, assetID, associationID string, in assetvoip.Input, key string) (assetvoip.Association, bool, error) {
	var out assetvoip.Association
	if !assetvoip.ValidInput(in) || commands.ValidateKey(key) != nil || assetID == "" || len(assetID) > 128 || len(associationID) > 128 || (associationID == "" && in.ExpectedRevision != 0) || (associationID != "" && in.ExpectedRevision < 1) {
		return out, false, assetvoip.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback(context.Background())
	actor, err := assetDecisionLock(ctx, tx)
	if err != nil {
		return out, false, err
	}
	hash := decisionRequestHash("voip:"+assetID+":"+associationID, in)
	if previous, exists, e := priorAssetVoIPOperation(ctx, tx, actor.User.ID, key, hash); e != nil || exists {
		return previous, exists, e
	}
	a, err := readAsset(tx.QueryRow(ctx, `SELECT `+assetColumns+` FROM assets a WHERE id=$1 FOR UPDATE`, assetID))
	if err != nil {
		return out, false, err
	}
	if a.LifecycleStatus != "observed" || a.Revision != in.ExpectedAssetRevision {
		return out, false, assetvoip.ErrConflict
	}
	var sourceID, inventorySourceID string
	var scope, agent string
	var sourceEnabled bool
	err = tx.QueryRow(ctx, `SELECT b.source_instance_id,b.scope,COALESCE(b.legacy_agent_id,'') FROM asset_source_bindings b WHERE b.id=$1 AND b.asset_id=$2`, in.BindingID, assetID).Scan(&inventorySourceID, &scope, &agent)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, false, assetvoip.ErrConflict
	}
	if err != nil {
		return out, false, err
	}
	if err = tx.QueryRow(ctx, `SELECT id,enabled FROM data_sources WHERE type='COLLECTOR' AND connection_ref=$1`, in.AgentID).Scan(&sourceID, &sourceEnabled); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return out, false, assetvoip.ErrConflict
		}
		return out, false, err
	}
	if scope != in.Scope || scope == "" && a.Scope != "" || scope != "" && (agent != in.AgentID || inventorySourceID != sourceID) {
		return out, false, assetvoip.ErrConflict
	}
	var before *assetvoip.Association
	var reviewedID int64
	if associationID != "" {
		old, e := readAssetVoIP(tx.QueryRow(ctx, `SELECT `+assetVoIPColumns+assetVoIPJoin+` WHERE v.id=$1 FOR UPDATE OF v`, associationID))
		if e != nil {
			return out, false, e
		}
		if old.Revision != in.ExpectedRevision || old.AgentID != in.AgentID || old.TargetID != in.TargetID || old.Scope != in.Scope || old.SourceInstanceID != sourceID {
			return out, false, assetvoip.ErrConflict
		}
		before = &old
		reviewedID = old.ReviewedSampleID
		// Disabling does not require a fresh endpoint, but must preserve the
		// reviewed identity: it cannot invent an unobserved new destination.
		if !in.Enabled && (old.Address != in.Address || old.Transport != in.Transport || old.Source != in.Source) {
			return out, false, assetvoip.ErrConflict
		}
	}
	if in.Enabled || before == nil {
		var currentSession string
		if err = tx.QueryRow(ctx, `SELECT session_id FROM agents WHERE agent_id=$1`, in.AgentID).Scan(&currentSession); err != nil {
			return out, false, assetVoIPError(err)
		}
		if !sourceEnabled || scope != "" && scope != currentSession {
			return out, false, assetvoip.ErrConflict
		}
		latest, e := assetVoIPSampleTx(ctx, tx, in.AgentID, in.TargetID, scope, false)
		if e != nil {
			return out, false, e
		}
		if latest == nil || latest.Address != in.Address || latest.Transport != in.Transport || latest.Source != in.Source {
			return out, false, assetvoip.ErrConflict
		}
		reviewedID = latest.ID
	}
	if associationID == "" {
		associationID, err = assetRegistryID("voip_")
		if err != nil {
			return out, false, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO asset_voip_associations(id,binding_id,source_instance_id,agent_id,target_id,address,transport,source,scope,service_kind,name,enabled,reviewed_sample_id,reviewed_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, associationID, in.BindingID, sourceID, in.AgentID, in.TargetID, in.Address, in.Transport, in.Source, scope, in.ServiceKind, in.Name, in.Enabled, reviewedID, actor.User.ID)
	} else {
		_, err = tx.Exec(ctx, `UPDATE asset_voip_associations SET binding_id=$2,address=$3,transport=$4,source=$5,service_kind=$6,name=$7,enabled=$8,reviewed_sample_id=$9,reviewed_by=$10,reviewed_at=now(),revision=revision+1,updated_at=now() WHERE id=$1`, associationID, in.BindingID, in.Address, in.Transport, in.Source, in.ServiceKind, in.Name, in.Enabled, reviewedID, actor.User.ID)
	}
	if err != nil {
		return out, false, assetVoIPError(err)
	}
	out, err = readAssetVoIP(tx.QueryRow(ctx, `SELECT `+assetVoIPColumns+assetVoIPJoin+` WHERE v.id=$1`, associationID))
	if err != nil {
		return out, false, err
	}
	if err = saveAssetVoIPOperation(ctx, tx, actor, key, hash, associationID, before, out, in.Reason); err != nil {
		return out, false, err
	}
	return out, false, tx.Commit(ctx)
}

func (s *Store) assetVoIPServiceTx(ctx context.Context, tx pgx.Tx, v assetvoip.Association, now time.Time) (assetvoip.Service, error) {
	out := assetvoip.Service{Association: v, MappingStatus: "active", MappingReason: "reviewed_binding", Incidents: []health.Incident{}}
	var connected, sourceEnabled bool
	var session, bindingScope, bindingAgent, inventorySourceID, sourceType, connectionRef string
	var interval int
	var origin string
	err := tx.QueryRow(ctx, `SELECT a.connected AND a.last_seen>$2,a.session_id,COALESCE(p.expected_interval_seconds,$3),CASE WHEN p.target_id IS NULL THEN 'backend_default' WHEN p.origin='collector_applied' THEN 'collector_applied' ELSE 'target_override' END,s.enabled,b.scope,COALESCE(b.legacy_agent_id,''),b.source_instance_id,s.type,s.connection_ref FROM agents a JOIN asset_source_bindings b ON b.id=$4 JOIN data_sources s ON s.id=$6 LEFT JOIN telephony_policies p ON p.agent_id=a.agent_id AND p.target_id=$5 WHERE a.agent_id=$1`, v.AgentID, now.Add(-s.timeout), s.telephonyDefault(), v.BindingID, v.TargetID, v.SourceInstanceID).Scan(&connected, &session, &interval, &origin, &sourceEnabled, &bindingScope, &bindingAgent, &inventorySourceID, &sourceType, &connectionRef)
	if err != nil {
		return out, err
	}

	out.Monitoring = telephony.DescribeInterval(v.TargetID, interval, origin)
	latest, err := assetVoIPSampleTx(ctx, tx, v.AgentID, v.TargetID, v.Scope, false)
	if err != nil {
		return out, err
	}
	issue, err := assetVoIPSampleTx(ctx, tx, v.AgentID, v.TargetID, v.Scope, true)
	if err != nil {
		return out, err
	}
	if latest != nil {
		out.ObservedEndpoint = &assetvoip.Endpoint{Address: latest.Address, Transport: latest.Transport, Source: latest.Source, ObservedAt: latest.ObservedAt}
	}
	if sameVoIPEndpoint(v, latest) {
		out.Latest = latest
	}
	if sameVoIPEndpoint(v, issue) {
		out.ClockIssue = issue
	}
	switch {
	case !v.Enabled:
		out.MappingStatus, out.MappingReason = "disabled", "association_disabled"
	case bindingScope != v.Scope || sourceType != "COLLECTOR" || connectionRef != v.AgentID || v.Scope != "" && (bindingAgent != v.AgentID || inventorySourceID != v.SourceInstanceID):
		out.MappingStatus, out.MappingReason = "review_required", "binding_ownership_changed"
	case v.Scope != "" && v.Scope != session:
		out.MappingStatus, out.MappingReason = "session_expired", "simulation_session_changed"
	case !sourceEnabled:
		out.MappingStatus, out.MappingReason = "source_disabled", "collector_source_disabled"
	case latest == nil:
		out.MappingStatus, out.MappingReason = "no_observation", "no_qualified_observation"
	case !sameVoIPEndpoint(v, latest):
		out.MappingStatus, out.MappingReason = "review_required", "endpoint_identity_changed"
	}
	out.Health = telephony.Evaluate(now, out.Latest, out.ClockIssue, connected, interval)
	if out.MappingStatus != "active" {
		out.Latest, out.ClockIssue = nil, nil
		out.Health = telephony.Evaluate(now, nil, nil, connected, interval)
		out.Health.Status, out.Health.Reason = "stale", out.MappingReason
		if out.MappingStatus == "no_observation" {
			out.Health.Status = "unknown"
		}
	}
	// These are existing per-Collector SIP incidents for this exact reviewed
	// endpoint, not a new global incident or root-cause correlation claim.
	rows, err := tx.Query(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE agent_id=$1 AND resource_kind='sip' AND resource_id=$2 AND source=$3 AND scope=$4 AND resource_context=$5 ORDER BY opened_at DESC,id LIMIT 21`, v.AgentID, v.TargetID, v.Source, v.Scope, health.SIPContext(v.Address, v.Transport))
	if err != nil {
		return out, err
	}
	for rows.Next() {
		incident, e := readIncident(rows)
		if e != nil {
			rows.Close()
			return out, e
		}
		out.Incidents = append(out.Incidents, incident)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(out.Incidents) > 20 {
		out.Incidents, out.IncidentsTruncated = out.Incidents[:20], true
	}
	for i := range out.Incidents {
		incident := &out.Incidents[i]
		incident.EvidenceStatus = "fresh"
		at := incident.LastObservedAt
		if incident.Signal.ObservedAt != nil {
			at = *incident.Signal.ObservedAt
		}
		if !connected {
			incident.EvidenceStatus = "collector_unavailable"
		} else if out.MappingStatus != "active" || at.After(now.Add(30*time.Second)) || now.Sub(at) > time.Duration(monitoring.StaleAfterSeconds(interval))*time.Second {
			incident.EvidenceStatus = "stale"
		}
	}
	return out, nil
}

func (s *Store) AssetVoIP(ctx context.Context, assetID string, limit, offset int) (assetvoip.Card, error) {
	out := assetvoip.Card{AssetID: assetID, TopologyNodeID: assetID, GeneratedAt: time.Now().UTC(), Items: []assetvoip.Service{}, Limit: limit, Offset: offset}
	if !assetvoip.ValidPage(limit, offset) {
		return out, assetvoip.ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	a, err := readAsset(tx.QueryRow(ctx, `SELECT `+assetColumns+` FROM assets a WHERE id=$1`, assetID))
	if err != nil {
		return out, err
	}
	out.AssetName, out.Scope = a.CanonicalName, a.Scope
	if a.LifecycleStatus != "observed" {
		return out, assetvoip.ErrConflict
	}
	out.Placement, err = placementTx(ctx, tx, assetID)
	if err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT `+assetVoIPColumns+assetVoIPJoin+` WHERE b.asset_id=$1 ORDER BY v.id LIMIT $2 OFFSET $3`, assetID, limit, offset)
	if err != nil {
		return out, err
	}
	items := []assetvoip.Association{}
	for rows.Next() {
		v, e := readAssetVoIP(rows)
		if e != nil {
			rows.Close()
			return out, e
		}
		items = append(items, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	for _, v := range items {
		service, e := s.assetVoIPServiceTx(ctx, tx, v, out.GeneratedAt)
		if e != nil {
			return out, e
		}
		out.Items = append(out.Items, service)
	}
	return out, tx.Commit(ctx)
}

func assetVoIPOwnedTx(ctx context.Context, tx pgx.Tx, assetID, associationID string) (assetvoip.Association, error) {
	return readAssetVoIP(tx.QueryRow(ctx, `SELECT `+assetVoIPColumns+assetVoIPJoin+` JOIN assets a ON a.id=b.asset_id WHERE v.id=$1 AND b.asset_id=$2 AND a.lifecycle_status='observed'`, associationID, assetID))
}

// AssetVoIPHistory never broadens to all Collector targets, to another session,
// or to an older address/transport after the reviewed mapping has changed.
func (s *Store) AssetVoIPHistory(ctx context.Context, assetID, associationID string, in assetvoip.HistoryFilter) (assetvoip.HistoryPage, error) {
	out := assetvoip.HistoryPage{Items: []telephony.Sample{}, Limit: in.Limit}
	if !assetvoip.ValidHistory(in) {
		return out, assetvoip.ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	out.Association, err = assetVoIPOwnedTx(ctx, tx, assetID, associationID)
	if err != nil {
		return out, err
	}
	v := out.Association
	rows, err := tx.Query(ctx, `SELECT to_jsonb(t) FROM telephony_samples t JOIN agent_messages m ON m.agent_id=t.agent_id AND m.message_id=t.message_id WHERE t.agent_id=$1 AND t.target_id=$2 AND `+assetVoIPScopeSQL+` AND t.address=$4 AND t.transport=$5 AND t.source=$6 AND ($7::timestamptz IS NULL OR t.observed_at>=$7) AND ($8::timestamptz IS NULL OR t.observed_at<$8) AND ($9::bigint=0 OR t.id<$9) ORDER BY t.id DESC LIMIT $10`, v.AgentID, v.TargetID, v.Scope, v.Address, v.Transport, v.Source, in.From, in.To, in.BeforeID, in.Limit+1)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return out, err
		}
		var sample telephony.Sample
		if err = json.Unmarshal(raw, &sample); err != nil {
			rows.Close()
			return out, err
		}
		sample.ObservedAt, sample.ReceivedAt = sample.ObservedAt.UTC(), sample.ReceivedAt.UTC()
		out.Items = append(out.Items, sample)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(out.Items) > in.Limit {
		out.Items = out.Items[:in.Limit]
		v := out.Items[len(out.Items)-1].ID
		out.NextBeforeID = &v
	}
	return out, tx.Commit(ctx)
}

func (s *Store) AssetVoIPEvents(ctx context.Context, assetID, associationID string, limit, offset int) ([]assetvoip.Event, error) {
	out := []assetvoip.Event{}
	if !assetvoip.ValidPage(limit, offset) {
		return out, assetvoip.ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	if _, err = assetVoIPOwnedTx(ctx, tx, assetID, associationID); err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT id,association_id,actor_id,before_value,after_value,reason,created_at FROM asset_voip_events WHERE association_id=$1 ORDER BY id DESC LIMIT $2 OFFSET $3`, associationID, limit, offset)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var v assetvoip.Event
		var before, after []byte
		if err = rows.Scan(&v.ID, &v.AssociationID, &v.ActorID, &before, &after, &v.Reason, &v.CreatedAt); err != nil {
			rows.Close()
			return out, err
		}
		if err = json.Unmarshal(before, &v.Before); err == nil {
			err = json.Unmarshal(after, &v.After)
		}
		if err != nil {
			rows.Close()
			return out, err
		}
		v.CreatedAt = v.CreatedAt.UTC()
		out = append(out, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
