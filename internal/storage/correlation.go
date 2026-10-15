package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/correlation"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/topology"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
)

func correlationCategory(v health.Incident) string {
	if v.ResourceKind == "topology" {
		return "topology"
	}
	if v.ResourceKind == "log" {
		return "log"
	}
	if v.ResourceKind == "external" {
		return "external"
	}
	if v.ResourceKind == "sip" || strings.HasPrefix(v.RuleID, "sip.") {
		return "telephony"
	}
	if v.ResourceKind == "agent" || strings.HasPrefix(v.RuleID, "collector.") {
		return "collector"
	}
	if strings.HasPrefix(v.RuleID, "interface.") {
		return "network"
	}
	if strings.Contains(v.RuleID, "hardware") || strings.Contains(v.RuleID, "temperature") || strings.Contains(v.RuleID, "power") {
		return "hardware"
	}
	if strings.Contains(v.RuleID, "drift") || strings.Contains(v.RuleID, "configuration") {
		return "configuration"
	}
	if strings.Contains(v.RuleID, "unavailable") || strings.Contains(v.RuleID, "down") {
		return "availability"
	}
	if strings.Contains(v.RuleID, "cpu") || strings.Contains(v.RuleID, "ram") || strings.Contains(v.RuleID, "latency") || strings.Contains(v.RuleID, "utilization") {
		return "performance"
	}
	return "unknown"
}

func signalID(incidentID string) string {
	h := sha256.Sum256([]byte("incident:" + incidentID))
	return "sig_" + hex.EncodeToString(h[:16])
}

func equalCorrelationJSON(a, b []byte) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func signalAssets(ctx context.Context, tx pgx.Tx, v health.Incident) ([]string, error) {
	if v.ResourceKind == "configuration" {
		var id string
		err := tx.QueryRow(ctx, `SELECT id FROM assets WHERE id=$1 AND lifecycle_status='observed' AND resolution_status<>'conflict' AND scope=''`, v.ResourceID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return []string{}, nil
		}
		if err != nil {
			return nil, err
		}
		return []string{id}, nil
	}
	if v.ResourceKind == "sip" {
		rows, err := tx.Query(ctx, `SELECT b.asset_id,a.address,a.transport FROM asset_voip_associations a JOIN asset_source_bindings b ON b.id=a.binding_id JOIN assets p ON p.id=b.asset_id WHERE a.agent_id=$1 AND a.target_id=$2 AND a.scope=$3 AND a.source=$4 AND a.enabled AND p.lifecycle_status='observed'`, v.AgentID, v.ResourceID, v.Scope, v.Source)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		ids := []string{}
		for rows.Next() {
			var id, address, transport string
			if err = rows.Scan(&id, &address, &transport); err != nil {
				return nil, err
			}
			if health.SIPContext(address, transport) == v.ResourceContext {
				ids = append(ids, id)
			}
		}
		return ids, rows.Err()
	}
	query := `SELECT DISTINCT asset_id FROM (
 SELECT b.asset_id FROM asset_source_bindings b WHERE $1<>'external' AND $2<>'sip' AND b.legacy_agent_id=$3 AND b.legacy_device_id=$4 AND b.scope=$5 AND ($2 NOT IN ('hardware','log','topology') OR b.id=$9)
 UNION ALL
 SELECT b.asset_id FROM asset_voip_associations a JOIN asset_source_bindings b ON b.id=a.binding_id WHERE $2='sip' AND a.agent_id=$3 AND a.target_id=$6 AND a.scope=$5 AND a.enabled
 UNION ALL
 SELECT b.asset_id FROM external_signal_hosts h JOIN asset_source_bindings b ON b.source_instance_id=h.source_id AND b.external_type='host' AND b.external_id=h.host_id AND b.scope='' WHERE $1='external' AND h.source_id=$7 AND h.event_id=$8
 ) candidates JOIN assets live ON live.id=candidates.asset_id WHERE live.lifecycle_status='observed' AND live.resolution_status<>'conflict' ORDER BY asset_id`
	sourceID, eventID := "", ""
	if v.SourceInstanceID != nil {
		sourceID = *v.SourceInstanceID
	}
	if v.ExternalEventID != nil {
		eventID = *v.ExternalEventID
	}
	rows, err := tx.Query(ctx, query, v.ResourceKind, v.ResourceKind, v.AgentID, v.DeviceID, v.Scope, v.ResourceID, sourceID, eventID, v.ResourceContext)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func readNormalizedSignal(row interface{ Scan(...any) error }) (correlation.Signal, error) {
	v := correlation.Signal{AssetIDs: []string{}, ServiceIDs: []string{}, SiteIDs: []string{}}
	var raw []byte
	err := row.Scan(&v.ID, &v.IncidentID, &v.Category, &v.Status, &v.Severity, &v.SourceKind, &v.SourceInstanceID, &v.Scope, &v.ObservedAt, &v.ReceivedAt, &v.TimeQuality, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	if err == nil {
		err = json.Unmarshal(raw, &v.Payload)
	}
	return v, err
}

func signalDimensions(ctx context.Context, tx pgx.Tx, v *correlation.Signal) error {
	v.AssetIDs, v.ServiceIDs, v.SiteIDs = []string{}, []string{}, []string{}
	rows, err := tx.Query(ctx, `SELECT asset_id FROM normalized_signal_assets WHERE signal_id=$1 ORDER BY role,asset_id`, v.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		v.AssetIDs = append(v.AssetIDs, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(v.AssetIDs) == 0 {
		v.AssetIDs, v.ServiceIDs, v.SiteIDs = []string{}, []string{}, []string{}
		return nil
	}
	rows, err = tx.Query(ctx, `SELECT DISTINCT s.id FROM correlation_services s JOIN correlation_service_assets a ON a.service_id=s.id WHERE s.enabled AND a.asset_id=ANY($1::text[]) ORDER BY s.id`, v.AssetIDs)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		v.ServiceIDs = append(v.ServiceIDs, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	rows, err = tx.Query(ctx, `WITH RECURSIVE p(asset_id,id,kind,parent_id) AS (
 SELECT ap.asset_id,l.id,l.kind,l.parent_id FROM asset_placements ap JOIN physical_locations l ON l.id=ap.location_id WHERE ap.asset_id=ANY($1::text[])
 UNION ALL SELECT p.asset_id,l.id,l.kind,l.parent_id FROM p JOIN physical_locations l ON l.id=p.parent_id
 ) SELECT DISTINCT id FROM p WHERE kind='site' ORDER BY id`, v.AssetIDs)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		v.SiteIDs = append(v.SiteIDs, id)
	}
	err = rows.Err()
	rows.Close()
	return err
}

func loadNormalizedSignal(ctx context.Context, tx pgx.Tx, id string) (correlation.Signal, error) {
	v, err := readNormalizedSignal(tx.QueryRow(ctx, `SELECT id,incident_id,category,status,severity,source_kind,source_instance_id,scope,observed_at,received_at,time_quality,payload FROM normalized_signals WHERE id=$1`, id))
	if err != nil {
		return v, err
	}
	err = signalDimensions(ctx, tx, &v)
	return v, err
}

func (s *Store) projectIncidentCorrelation(ctx context.Context, tx pgx.Tx, incidentID string) error {
	if incidentID == "" {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "correlation:"+incidentID); err != nil {
		return err
	}
	incident, err := readIncident(tx.QueryRow(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE id=$1`, incidentID))
	if err != nil {
		return err
	}
	current := []health.Incident{incident}
	if err = s.incidentFreshnessTx(ctx, tx, current, time.Now().UTC()); err != nil {
		return err
	}
	incident = current[0]
	assets, err := signalAssets(ctx, tx, incident)
	if err != nil {
		return err
	}
	status := "active"
	if incident.State == "resolved" {
		status = "resolved"
	}
	quality := "exact"
	if incident.Signal.Topology != nil {
		quality = "estimated"
		if status == "resolved" && incident.Signal.Topology.ReviewedAt != nil {
			incident.LastObservedAt = *incident.Signal.Topology.ReviewedAt
		}
	}
	if incident.Signal.Configuration != nil {
		quality = "estimated"
	}
	if incident.Signal.Log != nil {
		quality = incident.Signal.Log.TimeQuality
		if status == "resolved" && incident.Signal.Log.ReviewedAt != nil {
			incident.LastObservedAt = *incident.Signal.Log.ReviewedAt
		}
	}
	if incident.Signal.Hardware != nil && incident.Signal.Hardware.ClockQuality != "synced" {
		quality = "estimated"
	}
	if incident.Signal.External != nil {
		incident.LastObservedAt = incident.Signal.External.ObservedAt
		if status == "resolved" && incident.Signal.External.RecoveredAt != nil {
			incident.LastObservedAt = *incident.Signal.External.RecoveredAt
		}
	}
	if incident.SourceInstanceID == nil && incident.AgentID != "" {
		var sourceID string
		sourceErr := tx.QueryRow(ctx, `SELECT id FROM data_sources WHERE type='COLLECTOR' AND connection_ref=$1`, incident.AgentID).Scan(&sourceID)
		if sourceErr == nil {
			incident.SourceInstanceID = &sourceID
		} else if !errors.Is(sourceErr, pgx.ErrNoRows) {
			return sourceErr
		}
	}
	if incident.LastObservedAt.IsZero() {
		quality = "received_only"
		incident.LastObservedAt = incident.UpdatedAt
	}
	if incident.LastObservedAt.After(incident.UpdatedAt.Add(30 * time.Second)) {
		quality = "skewed"
	}
	payload, err := json.Marshal(incident)
	if err != nil {
		return err
	}
	id := signalID(incident.ID)
	var oldStatus, oldSeverity string
	var oldObserved, oldReceived time.Time
	var oldPayload []byte
	oldErr := tx.QueryRow(ctx, `SELECT status,severity,observed_at,received_at,payload FROM normalized_signals WHERE id=$1 FOR UPDATE`, id).Scan(&oldStatus, &oldSeverity, &oldObserved, &oldReceived, &oldPayload)
	changed := errors.Is(oldErr, pgx.ErrNoRows) || oldStatus != status || oldSeverity != incident.Severity || !oldObserved.Equal(incident.LastObservedAt) || !oldReceived.Equal(incident.UpdatedAt) || !equalCorrelationJSON(oldPayload, payload)
	if oldErr != nil && !errors.Is(oldErr, pgx.ErrNoRows) {
		return oldErr
	}
	if _, err = tx.Exec(ctx, `INSERT INTO normalized_signals(id,incident_id,category,status,severity,source_kind,source_instance_id,scope,observed_at,received_at,time_quality,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
 ON CONFLICT(id) DO UPDATE SET category=EXCLUDED.category,status=EXCLUDED.status,severity=EXCLUDED.severity,source_kind=EXCLUDED.source_kind,source_instance_id=EXCLUDED.source_instance_id,scope=EXCLUDED.scope,observed_at=EXCLUDED.observed_at,received_at=EXCLUDED.received_at,time_quality=EXCLUDED.time_quality,payload=EXCLUDED.payload,updated_at=CASE WHEN normalized_signals.payload IS DISTINCT FROM EXCLUDED.payload OR normalized_signals.status<>EXCLUDED.status OR normalized_signals.severity<>EXCLUDED.severity THEN now() ELSE normalized_signals.updated_at END`, id, incident.ID, correlationCategory(incident), status, incident.Severity, incident.Source, incident.SourceInstanceID, incident.Scope, incident.LastObservedAt, incident.UpdatedAt, quality, payload); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM normalized_signal_assets WHERE signal_id=$1`, id); err != nil {
		return err
	}
	for i, assetID := range assets {
		role := "affected"
		if i == 0 {
			role = "primary"
		}
		evidence, _ := json.Marshal(map[string]any{"incident_id": incident.ID, "mapping": "existing_asset_source_binding"})
		if _, err = tx.Exec(ctx, `INSERT INTO normalized_signal_assets(signal_id,asset_id,role,evidence) VALUES($1,$2,$3,$4)`, id, assetID, role, evidence); err != nil {
			return err
		}
	}
	memberCase := ""
	err = tx.QueryRow(ctx, `SELECT correlated_incident_id FROM correlated_incident_signals WHERE signal_id=$1`, id).Scan(&memberCase)
	if err == nil {
		if changed {
			kind := "signal_updated"
			if status == "resolved" && oldStatus != "resolved" {
				kind = "signal_resolved"
			}
			if err = insertCorrelationTimeline(ctx, tx, memberCase, &id, kind, incident.LastObservedAt, quality, incident.RuleID+" "+status); err != nil {
				return err
			}
		}
		if !changed {
			return nil
		}
		return refreshCorrelatedIncident(ctx, tx, memberCase)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	signal, err := loadNormalizedSignal(ctx, tx, id)
	if err != nil {
		return err
	}
	if status == "resolved" {
		return createCorrelatedIncident(ctx, tx, signal)
	}
	return s.attachCorrelatedSignal(ctx, tx, signal)
}

func createCorrelatedIncident(ctx context.Context, tx pgx.Tx, signal correlation.Signal) error {
	rawID, err := access.NewID()
	if err != nil {
		return err
	}
	id := "corr_" + rawID
	var primary *string
	if len(signal.AssetIDs) > 0 {
		primary = &signal.AssetIDs[0]
	}
	title := correlationTitle(ctx, tx, primary, signal.Category, false)
	state := "open"
	var resolved *time.Time
	if signal.Status == "resolved" {
		state = "resolved"
		at := signal.ObservedAt
		resolved = &at
	}
	if _, err = tx.Exec(ctx, `INSERT INTO correlated_incidents(id,state,severity,title,primary_asset_id,confidence,confidence_score,time_confidence,opened_at,resolved_at) VALUES($1,$2,$3,$4,$5,'low',0,$6,$7,$8)`, id, state, signal.Severity, title, primary, signalTimeConfidence(signal), signal.ObservedAt, resolved); err != nil {
		return err
	}
	evidence, _ := json.Marshal([]correlation.Evidence{{Kind: "existing_incident", Score: 0, Details: map[string]any{"seed": true}}})
	if _, err = tx.Exec(ctx, `INSERT INTO correlated_incident_signals(correlated_incident_id,signal_id,relation,score,evidence) VALUES($1,$2,'seed',0,$3)`, id, signal.ID, evidence); err != nil {
		return err
	}
	if err = insertCorrelationTimeline(ctx, tx, id, nil, "incident_opened", signal.ObservedAt, signal.TimeQuality, title); err != nil {
		return err
	}
	return insertCorrelationTimeline(ctx, tx, id, &signal.ID, "signal_added", signal.ObservedAt, signal.TimeQuality, signal.Category+" signal")
}

func signalTimeConfidence(v correlation.Signal) string {
	switch v.TimeQuality {
	case "exact":
		return "high"
	case "estimated":
		return "medium"
	default:
		return "low"
	}
}

func (s *Store) attachCorrelatedSignal(ctx context.Context, tx pgx.Tx, signal correlation.Signal) error {
	rows, err := tx.Query(ctx, `SELECT DISTINCT c.id FROM correlated_incidents c JOIN correlated_incident_signals m ON m.correlated_incident_id=c.id JOIN normalized_signals s ON s.id=m.signal_id WHERE c.state='open' AND s.status='active' AND s.observed_at BETWEEN $1 AND $2 ORDER BY c.id LIMIT 200`, signal.ObservedAt.Add(-15*time.Minute), signal.ObservedAt.Add(15*time.Minute))
	if err != nil {
		return err
	}
	caseIDs := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		caseIDs = append(caseIDs, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	links, err := correlationTopology(ctx, tx, signal.Scope)
	if err != nil {
		return err
	}
	bestCase, bestSignal := "", ""
	best := correlation.Decision{}
	for _, caseID := range caseIDs {
		members, e := caseSignals(ctx, tx, caseID)
		if e != nil {
			return e
		}
		if len(members) >= 256 {
			continue
		}
		for _, member := range members {
			var current health.Incident
			raw, _ := json.Marshal(member.Payload)
			if err = json.Unmarshal(raw, &current); err != nil {
				return err
			}
			currentMembers := []health.Incident{current}
			if err = s.incidentFreshnessTx(ctx, tx, currentMembers, time.Now().UTC()); err != nil {
				return err
			}
			member.Payload["evidence_status"] = currentMembers[0].EvidenceStatus
			topologyEvidence := []map[string]any{}
			for _, left := range signal.AssetIDs {
				for _, right := range member.AssetIDs {
					topologyEvidence = append(topologyEvidence, links[left+"\x00"+right]...)
				}
			}
			neighbor := len(topologyEvidence) > 0
			if signal.Payload["evidence_status"] != "fresh" || member.Payload["evidence_status"] != "fresh" {
				continue
			}
			decision := correlation.Score(signal, member, neighbor)
			for i := range decision.Evidence {
				if decision.Evidence[i].Kind == "topology_neighbor" {
					decision.Evidence[i].Details["observations"] = topologyEvidence
				}
			}
			if decision.Eligible && (bestCase == "" || decision.Score > best.Score || decision.Score == best.Score && caseID < bestCase) {
				bestCase, bestSignal, best = caseID, member.ID, decision
			}
		}
	}
	if bestCase == "" {
		return createCorrelatedIncident(ctx, tx, signal)
	}
	raw, _ := json.Marshal(best.Evidence)
	if _, err = tx.Exec(ctx, `INSERT INTO correlated_incident_signals(correlated_incident_id,signal_id,relation,score,evidence) VALUES($1,$2,$3,$4,$5)`, bestCase, signal.ID, best.Relation, best.Score, raw); err != nil {
		return err
	}
	for _, e := range best.Evidence {
		if e.Kind == "source_diversity" && e.Score == 0 {
			continue
		}
		details, _ := json.Marshal(e.Details)
		left, right := bestSignal, signal.ID
		if left > right {
			left, right = right, left
		}
		if _, err = tx.Exec(ctx, `INSERT INTO correlation_evidence(correlated_incident_id,left_signal_id,right_signal_id,kind,score,details) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, bestCase, left, right, e.Kind, e.Score, details); err != nil {
			return err
		}
	}
	if err = insertCorrelationTimeline(ctx, tx, bestCase, &signal.ID, "signal_added", signal.ObservedAt, signal.TimeQuality, best.Relation); err != nil {
		return err
	}
	return refreshCorrelatedIncident(ctx, tx, bestCase)
}

func caseSignals(ctx context.Context, tx pgx.Tx, caseID string) ([]correlation.Signal, error) {
	rows, err := tx.Query(ctx, `SELECT s.id,s.incident_id,s.category,s.status,s.severity,s.source_kind,s.source_instance_id,s.scope,s.observed_at,s.received_at,s.time_quality,s.payload FROM correlated_incident_signals m JOIN normalized_signals s ON s.id=m.signal_id WHERE m.correlated_incident_id=$1 ORDER BY s.observed_at,s.id`, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []correlation.Signal{}
	for rows.Next() {
		v, e := readNormalizedSignal(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for i := range out {
		if err = signalDimensions(ctx, tx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func correlationTopology(ctx context.Context, tx pgx.Tx, scope string) (map[string][]map[string]any, error) {
	bindings, truncated, err := globalTopologyBindings(ctx, tx, "all")
	if err != nil || truncated {
		return nil, err
	}
	now := time.Now().UTC()
	rows, err := tx.Query(ctx, `SELECT source_instance_id,scope,observation_id,version_id,agent_id,device_id,message_id,source,protocol,observed_at,received_at,expires_at,payload FROM global_topology_latest WHERE expires_at>$1 AND observed_at<=$2 AND scope=$3 ORDER BY source_instance_id,scope,observation_id LIMIT $4`, now, now.Add(30*time.Second), scope, MaxGlobalTopologyObservations+1)
	if err != nil {
		return nil, err
	}
	obs := []topology.GlobalObservation{}
	for rows.Next() {
		var o topology.GlobalObservation
		var raw []byte
		if err = rows.Scan(&o.SourceID, &o.Scope, &o.ID, &o.VersionID, &o.AgentID, &o.DeviceID, &o.MessageID, &o.Source, &o.Protocol, &o.ObservedAt, &o.ReceivedAt, &o.ExpiresAt, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		o.Neighbor = &pb.Neighbor{}
		if err = protojson.Unmarshal(raw, o.Neighbor); err != nil {
			rows.Close()
			return nil, err
		}
		obs = append(obs, o)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(obs) > MaxGlobalTopologyObservations {
		return nil, nil
	}
	graph := topology.BuildGlobal(now, bindings, obs, topology.GlobalFilter{Scope: "all", Limit: MaxGlobalTopologyObservations}, false, false)
	out := map[string][]map[string]any{}
	for _, edge := range graph.Edges {
		if edge.Status != "active" || edge.Confidence != "high" {
			continue
		}
		details := []map[string]any{}
		for _, e := range edge.Evidence {
			if e.Scope == scope && e.Resolution.Confidence == "high" && e.Status == "active" {
				details = append(details, map[string]any{"edge_id": edge.ID, "source_instance_id": e.SourceID, "version_id": e.VersionID, "observation_id": e.ObservationID, "observed_at": e.ObservedAt, "expires_at": e.ExpiresAt})
			}
		}
		if len(details) > 0 {
			out[edge.From+"\x00"+edge.To] = details
			out[edge.To+"\x00"+edge.From] = details
		}
	}
	return out, nil
}

func correlationTitle(ctx context.Context, tx pgx.Tx, assetID *string, category string, multi bool) string {
	name := "infrastructure"
	if assetID != nil {
		_ = tx.QueryRow(ctx, `SELECT canonical_name FROM assets WHERE id=$1`, *assetID).Scan(&name)
	}
	prefix := "Incident"
	if multi {
		prefix = "Multi-source incident"
	}
	title := []rune(logbook.SanitizeText(prefix + ": " + name + " (" + category + ")"))
	if len(title) > 256 {
		title = title[:256]
	}
	return string(title)
}

func refreshCorrelatedIncident(ctx context.Context, tx pgx.Tx, id string) error {
	var total, active, sources, maxScore int
	var severity, timeConfidence, category string
	var primary *string
	err := tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE s.status='active'),count(DISTINCT (s.source_kind,COALESCE(s.source_instance_id,''))),COALESCE(max(m.score),0),CASE WHEN bool_or(s.severity='critical') THEN 'critical' ELSE 'warning' END,CASE WHEN bool_or(s.time_quality IN ('skewed','received_only')) THEN 'low' WHEN bool_or(s.time_quality='estimated') THEN 'medium' ELSE 'high' END,(array_agg(s.category ORDER BY s.observed_at,s.id))[1] FROM correlated_incident_signals m JOIN normalized_signals s ON s.id=m.signal_id WHERE m.correlated_incident_id=$1`, id).Scan(&total, &active, &sources, &maxScore, &severity, &timeConfidence, &category)
	if err != nil {
		return err
	}
	_ = tx.QueryRow(ctx, `SELECT a.asset_id FROM normalized_signal_assets a JOIN correlated_incident_signals m ON m.signal_id=a.signal_id WHERE m.correlated_incident_id=$1 GROUP BY a.asset_id ORDER BY count(*) DESC,a.asset_id LIMIT 1`, id).Scan(&primary)
	state := "open"
	var resolved *time.Time
	if active == 0 {
		state = "resolved"
		at := time.Now().UTC()
		resolved = &at
	}
	confidence := "low"
	if maxScore >= 60 {
		confidence = "medium"
	}
	if maxScore >= 80 && timeConfidence == "high" {
		confidence = "high"
	}
	if timeConfidence == "low" {
		confidence = "low"
	}
	title := correlationTitle(ctx, tx, primary, category, total > 1 || sources > 1)
	var oldState string
	if err = tx.QueryRow(ctx, `SELECT state FROM correlated_incidents WHERE id=$1 FOR UPDATE`, id).Scan(&oldState); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE correlated_incidents SET state=$2,severity=$3,title=$4,primary_asset_id=$5,confidence=$6,confidence_score=$7,time_confidence=$8,revision=revision+1,updated_at=now(),resolved_at=CASE WHEN $2='resolved' THEN COALESCE(resolved_at,$9) ELSE NULL END WHERE id=$1`, id, state, severity, title, primary, confidence, maxScore, timeConfidence, resolved)
	if err != nil {
		return err
	}
	if oldState != "resolved" && state == "resolved" {
		return insertCorrelationTimeline(ctx, tx, id, nil, "incident_resolved", *resolved, "estimated", "all member signals resolved")
	}
	return nil
}

func insertCorrelationTimeline(ctx context.Context, tx pgx.Tx, caseID string, signalID *string, kind string, occurred time.Time, quality, summary string) error {
	sid := ""
	if signalID != nil {
		sid = *signalID
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{caseID, sid, kind, occurred.UTC().Format(time.RFC3339Nano), summary}, "\x00")))
	_, err := tx.Exec(ctx, `INSERT INTO correlation_timeline(correlated_incident_id,signal_id,event_kind,occurred_at,time_quality,summary,fingerprint) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(fingerprint) DO NOTHING`, caseID, signalID, kind, occurred, quality, logbook.SanitizeText(summary), hex.EncodeToString(sum[:]))
	return err
}

// RebuildCorrelation safely projects legacy incidents after migration 35. It is
// idempotent and never deletes or rewrites legacy incident/evidence history.
func (s *Store) RebuildCorrelation(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	actor, err := assetDecisionLock(ctx, tx)
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73512012)`); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT id FROM incidents ORDER BY opened_at,id LIMIT 10001`)
	if err != nil {
		return 0, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if len(ids) > 10000 {
		return 0, correlation.ErrConflict
	}
	for _, id := range ids {
		if err = queueIncidentCorrelation(ctx, tx, id); err != nil {
			return 0, err
		}
	}
	meta := access.CurrentMeta(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,'correlation_rebuild_queued','success','correlation',$2,$3)`, actor.User.ID, meta.RequestID, meta.Peer); err != nil {
		return 0, err
	}
	return len(ids), tx.Commit(ctx)
}

func readCorrelatedIncident(row interface{ Scan(...any) error }) (correlation.Incident, error) {
	var v correlation.Incident
	err := row.Scan(&v.ID, &v.State, &v.Severity, &v.Title, &v.PrimaryAssetID, &v.Confidence, &v.ConfidenceScore, &v.TimeConfidence, &v.Revision, &v.OpenedAt, &v.UpdatedAt, &v.ResolvedAt, &v.SignalCount, &v.ActiveSignalCount, &v.SourceCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	return v, err
}

const correlatedIncidentColumns = `c.id,c.state,c.severity,c.title,c.primary_asset_id,c.confidence,c.confidence_score,c.time_confidence,c.revision,c.opened_at,c.updated_at,c.resolved_at,(SELECT count(*) FROM correlated_incident_signals m WHERE m.correlated_incident_id=c.id),(SELECT count(*) FROM correlated_incident_signals m JOIN normalized_signals s ON s.id=m.signal_id WHERE m.correlated_incident_id=c.id AND s.status='active'),(SELECT count(DISTINCT (s.source_kind,COALESCE(s.source_instance_id,''))) FROM correlated_incident_signals m JOIN normalized_signals s ON s.id=m.signal_id WHERE m.correlated_incident_id=c.id)`

func (s *Store) CorrelatedIncidents(ctx context.Context, f correlation.Filter) ([]correlation.Incident, error) {
	if f.Limit < 1 || f.Limit > 200 || f.Offset < 0 || f.Offset > 100000 || len(f.AssetID) > 128 || len(f.ServiceID) > 128 || f.State != "" && f.State != "open" && f.State != "resolved" || f.Severity != "" && f.Severity != "warning" && f.Severity != "critical" {
		return nil, correlation.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT `+correlatedIncidentColumns+` FROM correlated_incidents c WHERE ($1='' OR c.state=$1) AND ($2='' OR c.severity=$2) AND ($3='' OR EXISTS(SELECT 1 FROM correlated_incident_signals m JOIN normalized_signal_assets a ON a.signal_id=m.signal_id WHERE m.correlated_incident_id=c.id AND a.asset_id=$3)) AND ($4='' OR EXISTS(SELECT 1 FROM correlated_incident_signals m JOIN normalized_signal_assets a ON a.signal_id=m.signal_id JOIN correlation_service_assets sa ON sa.asset_id=a.asset_id WHERE m.correlated_incident_id=c.id AND sa.service_id=$4)) ORDER BY c.updated_at DESC,c.id LIMIT $5 OFFSET $6`, f.State, f.Severity, f.AssetID, f.ServiceID, f.Limit, f.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []correlation.Incident{}
	for rows.Next() {
		v, e := readCorrelatedIncident(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) CorrelatedIncident(ctx context.Context, id string) (correlation.Case, error) {
	out := correlation.Case{Signals: []correlation.Membership{}, Evidence: []correlation.EvidenceRecord{}, Timeline: []correlation.TimelineEvent{}, Limitations: []string{"correlation is deterministic context, not proof of root cause", "service and physical location relations require reviewed mappings", "only fresh high-confidence physical topology is used", "low-quality timestamps reduce confidence", "collector loss is not treated as downstream device failure"}}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	out.Incident, err = readCorrelatedIncident(tx.QueryRow(ctx, `SELECT `+correlatedIncidentColumns+` FROM correlated_incidents c WHERE c.id=$1`, id))
	if err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT s.id,s.incident_id,s.category,s.status,s.severity,s.source_kind,s.source_instance_id,s.scope,s.observed_at,s.received_at,s.time_quality,s.payload,m.relation,m.score,m.evidence,m.added_at FROM correlated_incident_signals m JOIN normalized_signals s ON s.id=m.signal_id WHERE m.correlated_incident_id=$1 ORDER BY s.observed_at,s.id`, id)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var m correlation.Membership
		var rawPayload, rawEvidence []byte
		if err = rows.Scan(&m.Signal.ID, &m.Signal.IncidentID, &m.Signal.Category, &m.Signal.Status, &m.Signal.Severity, &m.Signal.SourceKind, &m.Signal.SourceInstanceID, &m.Signal.Scope, &m.Signal.ObservedAt, &m.Signal.ReceivedAt, &m.Signal.TimeQuality, &rawPayload, &m.Relation, &m.Score, &rawEvidence, &m.AddedAt); err != nil {
			rows.Close()
			return out, err
		}
		if err = json.Unmarshal(rawPayload, &m.Signal.Payload); err != nil {
			rows.Close()
			return out, err
		}
		if err = json.Unmarshal(rawEvidence, &m.Evidence); err != nil {
			rows.Close()
			return out, err
		}
		out.Signals = append(out.Signals, m)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	for i := range out.Signals {
		if err = signalDimensions(ctx, tx, &out.Signals[i].Signal); err != nil {
			return out, err
		}
	}
	rows, err = tx.Query(ctx, `SELECT id,left_signal_id,right_signal_id,kind,score,details,created_at FROM correlation_evidence WHERE correlated_incident_id=$1 ORDER BY id DESC LIMIT 1000`, id)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var e correlation.EvidenceRecord
		var raw []byte
		if err = rows.Scan(&e.ID, &e.LeftSignalID, &e.RightSignalID, &e.Kind, &e.Score, &raw, &e.CreatedAt); err != nil {
			rows.Close()
			return out, err
		}
		if err = json.Unmarshal(raw, &e.Details); err != nil {
			rows.Close()
			return out, err
		}
		out.Evidence = append(out.Evidence, e)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	rows, err = tx.Query(ctx, `SELECT id,signal_id,event_kind,occurred_at,recorded_at,time_quality,summary FROM correlation_timeline WHERE correlated_incident_id=$1 ORDER BY occurred_at DESC,id DESC LIMIT 500`, id)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var e correlation.TimelineEvent
		if err = rows.Scan(&e.ID, &e.SignalID, &e.EventKind, &e.OccurredAt, &e.RecordedAt, &e.TimeQuality, &e.Summary); err != nil {
			rows.Close()
			return out, err
		}
		out.Timeline = append(out.Timeline, e)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	out.Truncated = len(out.Evidence) >= 1000 || len(out.Timeline) >= 500
	sort.Slice(out.Timeline, func(i, j int) bool {
		if out.Timeline[i].OccurredAt.Equal(out.Timeline[j].OccurredAt) {
			return out.Timeline[i].ID < out.Timeline[j].ID
		}
		return out.Timeline[i].OccurredAt.Before(out.Timeline[j].OccurredAt)
	})
	return out, tx.Commit(ctx)
}

func readCorrelationService(row interface{ Scan(...any) error }) (correlation.Service, error) {
	var v correlation.Service
	err := row.Scan(&v.ID, &v.Name, &v.Kind, &v.Enabled, &v.Revision, &v.CreatedBy, &v.CreatedAt, &v.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	return v, err
}

const correlationServiceColumns = `id,name,kind,enabled,revision,created_by,created_at,updated_at`

func serviceAssets(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, id string) ([]correlation.AssetRole, error) {
	rows, err := q.Query(ctx, `SELECT asset_id,role FROM correlation_service_assets WHERE service_id=$1 ORDER BY asset_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []correlation.AssetRole{}
	for rows.Next() {
		var a correlation.AssetRole
		if err = rows.Scan(&a.AssetID, &a.Role); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Store) CorrelationServices(ctx context.Context, limit, offset int) ([]correlation.Service, error) {
	if limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, correlation.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT `+correlationServiceColumns+` FROM correlation_services ORDER BY lower(name),id LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []correlation.Service{}
	for rows.Next() {
		v, e := readCorrelationService(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for i := range out {
		out[i].Assets, err = serviceAssets(ctx, s.pool, out[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func (s *Store) CorrelationService(ctx context.Context, id string) (correlation.Service, error) {
	v, err := readCorrelationService(s.pool.QueryRow(ctx, `SELECT `+correlationServiceColumns+` FROM correlation_services WHERE id=$1`, id))
	if err != nil {
		return v, err
	}
	v.Assets, err = serviceAssets(ctx, s.pool, id)
	return v, err
}

func (s *Store) SaveCorrelationService(ctx context.Context, id string, in correlation.ServiceInput, key string) (correlation.Service, bool, error) {
	var out correlation.Service
	create := id == ""
	if err := in.Validate(create); err != nil || commands.ValidateKey(key) != nil {
		return out, false, correlation.ErrInvalid
	}
	in.Assets = append([]correlation.AssetRole(nil), in.Assets...)
	sort.Slice(in.Assets, func(i, j int) bool { return in.Assets[i].AssetID < in.Assets[j].AssetID })
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback(context.Background())
	actor, err := assetDecisionLock(ctx, tx)
	if err != nil {
		return out, false, err
	}
	hash := decisionRequestHash("correlation-service:"+id, in)
	var oldHash string
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,response FROM correlation_operations WHERE actor_id=$1 AND idempotency_key=$2`, actor.User.ID, key).Scan(&oldHash, &raw)
	if err == nil {
		if oldHash != hash {
			return out, false, correlation.ErrConflict
		}
		return out, true, json.Unmarshal(raw, &out)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, false, err
	}
	var before *correlation.Service
	if create {
		rawID, e := access.NewID()
		if e != nil {
			return out, false, e
		}
		id = "svc_" + rawID
	} else {
		old, e := readCorrelationService(tx.QueryRow(ctx, `SELECT `+correlationServiceColumns+` FROM correlation_services WHERE id=$1 FOR UPDATE`, id))
		if e != nil {
			return out, false, e
		}
		if old.Revision != in.ExpectedRevision {
			return out, false, correlation.ErrConflict
		}
		old.Assets, e = serviceAssets(ctx, tx, id)
		if e != nil {
			return out, false, e
		}
		before = &old
	}
	for _, a := range in.Assets {
		var ok bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM assets WHERE id=$1 AND lifecycle_status='observed')`, a.AssetID).Scan(&ok); err != nil {
			return out, false, err
		}
		if !ok {
			return out, false, correlation.ErrConflict
		}
	}
	assetIDs := []string{}
	for _, a := range in.Assets {
		assetIDs = append(assetIDs, a.AssetID)
	}
	var namespaces int
	if err = tx.QueryRow(ctx, `SELECT count(DISTINCT scope) FROM assets WHERE id=ANY($1::text[])`, assetIDs).Scan(&namespaces); err != nil {
		return out, false, err
	}
	if namespaces != 1 {
		return out, false, correlation.ErrConflict
	}
	if create {
		out, err = readCorrelationService(tx.QueryRow(ctx, `INSERT INTO correlation_services(id,name,kind,enabled,created_by) VALUES($1,$2,$3,$4,$5) RETURNING `+correlationServiceColumns, id, in.Name, in.Kind, in.Enabled, actor.User.ID))
	} else {
		out, err = readCorrelationService(tx.QueryRow(ctx, `UPDATE correlation_services SET name=$2,kind=$3,enabled=$4,revision=revision+1,updated_at=now() WHERE id=$1 RETURNING `+correlationServiceColumns, id, in.Name, in.Kind, in.Enabled))
	}
	if err != nil {
		return out, false, correlation.ErrConflict
	}
	if _, err = tx.Exec(ctx, `DELETE FROM correlation_service_assets WHERE service_id=$1`, id); err != nil {
		return out, false, err
	}
	sort.Slice(in.Assets, func(i, j int) bool { return in.Assets[i].AssetID < in.Assets[j].AssetID })
	for _, a := range in.Assets {
		if _, err = tx.Exec(ctx, `INSERT INTO correlation_service_assets(service_id,asset_id,role) VALUES($1,$2,$3)`, id, a.AssetID, a.Role); err != nil {
			return out, false, err
		}
	}
	out.Assets = append([]correlation.AssetRole{}, in.Assets...)
	b, _ := json.Marshal(before)
	a, _ := json.Marshal(out)
	if _, err = tx.Exec(ctx, `INSERT INTO correlation_service_events(service_id,actor_id,before_value,after_value,reason) VALUES($1,$2,$3,$4,$5)`, id, actor.User.ID, b, a, logbook.SanitizeText(in.Reason)); err != nil {
		return out, false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO correlation_operations(actor_id,idempotency_key,request_hash,response) VALUES($1,$2,$3,$4)`, actor.User.ID, key, hash, a); err != nil {
		return out, false, err
	}
	meta := access.CurrentMeta(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,'correlation_service_saved','success',$2,$3,$4)`, actor.User.ID, id, meta.RequestID, meta.Peer); err != nil {
		return out, false, err
	}
	return out, false, tx.Commit(ctx)
}

func (s *Store) CorrelationSummary(ctx context.Context) (map[string]any, error) {
	var open, resolved, multi, signals int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE state='open'),count(*) FILTER(WHERE state='resolved'),count(*) FILTER(WHERE (SELECT count(DISTINCT (s.source_kind,COALESCE(s.source_instance_id,''))) FROM correlated_incident_signals m JOIN normalized_signals s ON s.id=m.signal_id WHERE m.correlated_incident_id=c.id)>1),(SELECT count(*) FROM normalized_signals) FROM correlated_incidents c`).Scan(&open, &resolved, &multi, &signals)
	return map[string]any{"open": open, "resolved": resolved, "multi_source": multi, "normalized_signals": signals, "correlation_window_seconds": int(correlation.Window.Seconds()), "minimum_score": correlation.Threshold}, err
}
