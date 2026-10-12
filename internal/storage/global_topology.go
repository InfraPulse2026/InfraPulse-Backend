package storage

import (
	"context"
	"encoding/json"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/topology"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"time"
)

const MaxGlobalTopologyBindings = 4000
const MaxGlobalTopologyObservations = 10000

func globalTopologyBindings(ctx context.Context, tx pgx.Tx, scope string) ([]topology.GlobalBinding, bool, error) {
	rows, err := tx.Query(ctx, `SELECT a.id,a.kind,a.canonical_name,a.scope,a.resolution_status,b.id,b.source_instance_id,s.type,s.enabled,b.scope,COALESCE(b.legacy_agent_id,''),b.external_id,COALESCE(b.latest_fact_id,0),b.inventory_at,b.inventory FROM asset_source_bindings b JOIN assets a ON a.id=b.asset_id JOIN data_sources s ON s.id=b.source_instance_id WHERE a.lifecycle_status='observed' AND ($1='all' OR ($1='real' AND b.scope='') OR ($1='simulated' AND b.scope<>'')) ORDER BY b.id LIMIT $2`, scope, MaxGlobalTopologyBindings+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []topology.GlobalBinding{}
	total := 0
	truncated := false
	for rows.Next() {
		var b topology.GlobalBinding
		var raw []byte
		if err = rows.Scan(&b.Asset.ID, &b.Asset.Kind, &b.Asset.CanonicalName, &b.Asset.Scope, &b.Asset.ResolutionStatus, &b.BindingID, &b.SourceID, &b.SourceType, &b.SourceEnabled, &b.Scope, &b.AgentID, &b.DeviceID, &b.FactID, &b.ObservedAt, &raw); err != nil {
			return nil, false, err
		}
		total += len(raw)
		if len(out) >= MaxGlobalTopologyBindings || total > 16<<20 {
			truncated = true
			break
		}
		b.Device = &pb.Device{}
		if err = protojson.Unmarshal(raw, b.Device); err != nil {
			return nil, false, err
		}
		out = append(out, b)
	}
	return out, truncated, rows.Err()
}
func (s *Store) GlobalTopology(ctx context.Context, f topology.GlobalFilter) (topology.GlobalGraph, error) {
	if f.Scope == "" {
		f.Scope = "all"
	}
	if f.Limit == 0 {
		f.Limit = 500
	}
	if (f.Scope != "all" && f.Scope != "real" && f.Scope != "simulated") || f.Limit < 1 || f.Limit > 2000 || f.Offset < 0 || f.Offset > 1000000 || len(f.SourceID) > 128 || len(f.AssetID) > 128 {
		return topology.GlobalGraph{}, topology.ErrGlobalInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return topology.GlobalGraph{}, err
	}
	defer tx.Rollback(context.Background())
	if f.AssetID != "" {
		a, e := readAsset(tx.QueryRow(ctx, `SELECT `+assetColumns+` FROM assets a WHERE id=$1`, f.AssetID))
		if e != nil {
			return topology.GlobalGraph{}, e
		}
		if a.LifecycleStatus != "observed" {
			return topology.GlobalGraph{}, assets.ErrConflict
		}
	}
	bindings, truncated, err := globalTopologyBindings(ctx, tx, f.Scope)
	if err != nil {
		return topology.GlobalGraph{}, err
	}
	now := time.Now().UTC()
	rows, err := tx.Query(ctx, `SELECT l.source_instance_id,l.scope,l.observation_id,l.version_id,l.agent_id,l.device_id,l.message_id,l.source,l.protocol,l.observed_at,l.received_at,l.expires_at,l.payload FROM global_topology_latest l WHERE ($1='' OR source_instance_id=$1) AND ($2='all' OR ($2='real' AND scope='') OR ($2='simulated' AND scope<>'')) AND ($3 OR (expires_at>$4 AND observed_at<=$5)) ORDER BY source_instance_id,scope,observation_id LIMIT $6`, f.SourceID, f.Scope, f.IncludeExpired, now, now.Add(30*time.Second), MaxGlobalTopologyObservations+1)
	if err != nil {
		return topology.GlobalGraph{}, err
	}
	observations := []topology.GlobalObservation{}
	bytes := 0
	obsTruncated := false
	for rows.Next() {
		var o topology.GlobalObservation
		var raw []byte
		if err = rows.Scan(&o.SourceID, &o.Scope, &o.ID, &o.VersionID, &o.AgentID, &o.DeviceID, &o.MessageID, &o.Source, &o.Protocol, &o.ObservedAt, &o.ReceivedAt, &o.ExpiresAt, &raw); err != nil {
			rows.Close()
			return topology.GlobalGraph{}, err
		}
		bytes += len(raw)
		if len(observations) >= MaxGlobalTopologyObservations || bytes > 16<<20 {
			obsTruncated = true
			break
		}
		o.Neighbor = &pb.Neighbor{}
		if err = protojson.Unmarshal(raw, o.Neighbor); err != nil {
			rows.Close()
			return topology.GlobalGraph{}, err
		}
		observations = append(observations, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return topology.GlobalGraph{}, err
	}
	graph := topology.BuildGlobal(now, bindings, observations, f, truncated, obsTruncated)
	if err = tx.Commit(ctx); err != nil {
		return graph, err
	}
	return graph, nil
}

type TopologyChange struct {
	ID                  int64           `json:"id"`
	VersionID           int64           `json:"version_id"`
	Kind                string          `json:"kind"`
	OccurredAt          time.Time       `json:"occurred_at"`
	RecordedAt          time.Time       `json:"recorded_at"`
	SourceID            string          `json:"source_instance_id"`
	Scope               string          `json:"scope"`
	AgentID             string          `json:"agent_id"`
	DeviceID            string          `json:"device_id"`
	ObservationID       string          `json:"observation_id"`
	Protocol            string          `json:"protocol"`
	CurrentLocalAssetID *string         `json:"current_local_asset_id"`
	VersionObservedAt   time.Time       `json:"version_observed_at"`
	VersionExpiresAt    time.Time       `json:"version_expires_at"`
	Neighbor            json.RawMessage `json:"neighbor"`
}
type TopologyChangePage struct {
	Items       []TopologyChange `json:"items"`
	NextAfterID int64            `json:"next_after_id"`
	HasMore     bool             `json:"has_more"`
	Limit       int              `json:"limit"`
}

func (s *Store) GlobalTopologyChanges(ctx context.Context, source, scope, asset string, after int64, limit int) (TopologyChangePage, error) {
	if scope == "" {
		scope = "all"
	}
	if limit == 0 {
		limit = 50
	}
	out := TopologyChangePage{Items: []TopologyChange{}, NextAfterID: after, Limit: limit}
	if (scope != "all" && scope != "real" && scope != "simulated") || after < 0 || limit < 1 || limit > 200 || len(source) > 128 || len(asset) > 128 {
		return out, topology.ErrGlobalInvalid
	}
	if asset != "" {
		if _, err := s.Asset(ctx, asset); err != nil {
			return out, err
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT e.id,e.version_id,e.kind,e.occurred_at,e.recorded_at,v.source_instance_id,v.scope,v.agent_id,v.device_id,v.observation_id,v.protocol,b.asset_id,v.observed_at,v.expires_at,v.payload FROM global_topology_events e JOIN global_topology_versions v ON v.id=e.version_id LEFT JOIN asset_source_bindings b ON b.source_instance_id=v.source_instance_id AND b.scope=v.scope AND b.external_type='device' AND b.external_id=v.device_id WHERE e.id>$1 AND ($2='' OR v.source_instance_id=$2) AND ($3='all' OR ($3='real' AND v.scope='') OR ($3='simulated' AND v.scope<>'')) AND ($4='' OR b.asset_id=$4) ORDER BY e.id LIMIT $5`, after, source, scope, asset, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	totalBytes := 0
	for rows.Next() {
		var v TopologyChange
		var raw []byte
		if err = rows.Scan(&v.ID, &v.VersionID, &v.Kind, &v.OccurredAt, &v.RecordedAt, &v.SourceID, &v.Scope, &v.AgentID, &v.DeviceID, &v.ObservationID, &v.Protocol, &v.CurrentLocalAssetID, &v.VersionObservedAt, &v.VersionExpiresAt, &raw); err != nil {
			return out, err
		}
		if len(out.Items) >= limit {
			out.HasMore = true
			break
		}
		totalBytes += len(raw)
		if totalBytes > 8<<20 {
			if len(out.Items) == 0 {
				return out, topology.ErrGlobalLimit
			}
			out.HasMore = true
			break
		}
		v.Neighbor = raw
		out.Items = append(out.Items, v)
		out.NextAfterID = v.ID
	}
	return out, rows.Err()
}
