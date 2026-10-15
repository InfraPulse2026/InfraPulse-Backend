package storage

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assethardware"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/topology"
	"github.com/jackc/pgx/v5"
)

func hardwareAssetSelection(ctx context.Context, tx pgx.Tx, id string, related bool) ([]string, map[string][]string, bool, error) {
	a, err := readAsset(tx.QueryRow(ctx, `SELECT `+assetColumns+` FROM assets a WHERE id=$1`, id))
	if err != nil {
		return nil, nil, false, err
	}
	if a.LifecycleStatus != "observed" {
		return nil, nil, false, assets.ErrConflict
	}
	ids := []string{id}
	relationIDs := map[string][]string{}
	if !related {
		return ids, relationIDs, false, nil
	}
	bindings, truncated, err := globalTopologyBindings(ctx, tx, "all")
	if err != nil {
		return nil, nil, false, err
	}
	// B7 resolves exact resource relationships in the Collector+target+protocol+
	// instance+simulation namespace. A truncated inventory cannot prove uniqueness.
	if truncated {
		return ids, relationIDs, true, nil
	}
	g := topology.BuildGlobal(time.Now().UTC(), bindings, nil, topology.GlobalFilter{AssetID: id, Scope: "all", Limit: 1}, false, false)
	for _, r := range g.ManagementRelations {
		if r.Status != "resolved" {
			continue
		}
		other := ""
		if r.To == id {
			other = r.From
		} else if r.From == id {
			other = r.To
		}
		if other != "" && other != id {
			relationIDs[other] = append(relationIDs[other], r.ID)
		}
	}
	for other := range relationIDs {
		ids = append(ids, other)
		sort.Strings(relationIDs[other])
	}
	sort.Strings(ids)
	return ids, relationIDs, false, nil
}

const hardwareReadColumns = `h.id,h.observation_key,h.message_id,h.binding_id,h.source_instance_id,h.kind,h.observed_at,h.received_at,h.payload,b.asset_id,b.scope,s.name,s.enabled,COALESCE(a.connected AND a.last_seen>now()-($6::double precision*interval '1 second') AND (b.scope='' OR b.scope=a.session_id),false),CASE WHEN b.scope='' AND p.origin='collector_applied' THEN 3*CASE WHEN h.kind IN ('inventory','component') THEN p.inventory_interval_seconds ELSE p.telemetry_interval_seconds END ELSE 0 END`

func readHardwareRows(rows pgx.Rows, requested string, related map[string][]string, now time.Time, maxAge, limit int, explicitMaxAge bool) ([]assethardware.Item, bool, error) {
	defer rows.Close()
	items := []assethardware.Item{}
	more := false
	total := 0
	for rows.Next() {
		var item assethardware.Item
		var raw []byte
		var kind string
		var observed time.Time
		var derivedMaxAge int
		if err := rows.Scan(&item.ID, &item.ObservationKey, &item.MessageID, &item.BindingID, &item.SourceID, &kind, &observed, &item.ReceivedAt, &raw, &item.AssetID, &item.Scope, &item.SourceName, &item.SourceEnabled, &item.AgentConnected, &derivedMaxAge); err != nil {
			return nil, false, err
		}
		if len(items) >= limit {
			more = true
			break
		}
		total += len(raw)
		if total > 8<<20 {
			if len(items) == 0 {
				return nil, false, assethardware.ErrLimit
			}
			more = true
			break
		}
		if err := json.Unmarshal(raw, &item.Observation); err != nil {
			return nil, false, err
		}
		item.Kind = kind
		item.ObservedAt = observed
		item.Related = item.AssetID != requested
		item.RelationIDs = append([]string{}, related[item.AssetID]...)
		effectiveMaxAge := maxAge
		if !explicitMaxAge && derivedMaxAge > effectiveMaxAge {
			effectiveMaxAge = derivedMaxAge
		}
		item.FreshnessMaxAgeSeconds = effectiveMaxAge
		item.Freshness = assethardware.Freshness(item.Observation, item.ReceivedAt, now, effectiveMaxAge, item.SourceEnabled, item.AgentConnected)
		items = append(items, item)
	}
	return items, more, rows.Err()
}

func (s *Store) AssetHardware(ctx context.Context, id string, f assethardware.Filter) (assethardware.Page, error) {
	explicitMaxAge := f.MaxAgeSeconds != 0
	f, err := assethardware.Defaults(f)
	out := assethardware.Page{AssetID: id, GeneratedAt: time.Now().UTC(), Items: []assethardware.Item{}, Limit: f.Limit, Offset: f.Offset, MaxAgeSeconds: f.MaxAgeSeconds}
	if err != nil {
		return out, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	ids, relations, truncated, err := hardwareAssetSelection(ctx, tx, id, f.IncludeRelated)
	if err != nil {
		return out, err
	}
	out.RelationsTruncated = truncated
	rows, err := tx.Query(ctx, `SELECT `+hardwareReadColumns+` FROM asset_hardware_latest l JOIN asset_hardware_samples h ON h.id=l.sample_id JOIN asset_source_bindings b ON b.id=h.binding_id JOIN data_sources s ON s.id=h.source_instance_id LEFT JOIN agents a ON a.agent_id=b.legacy_agent_id LEFT JOIN monitoring_policies p ON p.agent_id=b.legacy_agent_id AND p.device_id=b.external_id WHERE b.asset_id=ANY($1::text[]) AND ($2='' OR h.source_instance_id=$2) AND ($3='' OR h.kind=$3) ORDER BY b.asset_id,h.source_instance_id,h.kind,h.observation_key LIMIT $4 OFFSET $5`, ids, f.SourceID, f.Kind, f.Limit+1, f.Offset, s.timeout.Seconds())
	if err != nil {
		return out, err
	}
	out.Items, out.HasMore, err = readHardwareRows(rows, id, relations, out.GeneratedAt, f.MaxAgeSeconds, f.Limit, explicitMaxAge)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func (s *Store) AssetHardwareHistory(ctx context.Context, id string, f assethardware.HistoryFilter) (assethardware.HistoryPage, error) {
	explicitMaxAge := f.MaxAgeSeconds != 0
	f, err := assethardware.HistoryDefaults(f)
	out := assethardware.HistoryPage{AssetID: id, GeneratedAt: time.Now().UTC(), Items: []assethardware.Item{}, Limit: f.Limit, NextAfterID: f.AfterID, MaxAgeSeconds: f.MaxAgeSeconds}
	if err != nil {
		return out, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	ids, relations, truncated, err := hardwareAssetSelection(ctx, tx, id, f.IncludeRelated)
	if err != nil {
		return out, err
	}
	out.RelationsTruncated = truncated
	rows, err := tx.Query(ctx, `SELECT `+hardwareReadColumns+` FROM asset_hardware_samples h JOIN asset_source_bindings b ON b.id=h.binding_id JOIN data_sources s ON s.id=h.source_instance_id LEFT JOIN agents a ON a.agent_id=b.legacy_agent_id LEFT JOIN monitoring_policies p ON p.agent_id=b.legacy_agent_id AND p.device_id=b.external_id WHERE b.asset_id=ANY($1::text[]) AND ($2='' OR h.source_instance_id=$2) AND ($3='' OR h.observation_key=$3) AND h.id>$4 ORDER BY h.id LIMIT $5`, ids, f.SourceID, f.ObservationKey, f.AfterID, f.Limit+1, s.timeout.Seconds())
	if err != nil {
		return out, err
	}
	out.Items, out.HasMore, err = readHardwareRows(rows, id, relations, out.GeneratedAt, f.MaxAgeSeconds, f.Limit, explicitMaxAge)
	if err != nil {
		return out, err
	}
	if len(out.Items) > 0 {
		out.NextAfterID = out.Items[len(out.Items)-1].ID
	}
	return out, tx.Commit(ctx)
}
