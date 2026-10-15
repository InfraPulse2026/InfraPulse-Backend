package storage

import (
	"context"
	"encoding/json"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/externalmonitor"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
)

func externalStaleAfter(cfg datasources.Settings, interval, active int) time.Duration {
	pages := 1
	if cfg.Zabbix != nil {
		size := min(max(cfg.Zabbix.PageSize, 1), 5)
		pages = max(1, (len(cfg.Zabbix.HostIDs)+size-1)/size)
	}
	if cfg.Prometheus != nil {
		pages = max(pages, (cfg.Prometheus.MaxTargets+max(cfg.Prometheus.PageSize, 1)-1)/max(cfg.Prometheus.PageSize, 1))
	}
	pages = max(pages, (active+99)/100)
	return time.Duration(3*pages*interval+30) * time.Second
}
func externalFreshness(enabled bool, status string, current, observed int64, at, now time.Time, age time.Duration) string {
	if !enabled || status == "error" {
		return "source_unavailable"
	}
	if current != observed {
		return "configuration_changed"
	}
	if now.Sub(at) > age {
		return "stale"
	}
	return "fresh"
}
func (s *Store) externalIncidentFreshness(ctx context.Context, q freshnessQueryer, items []health.Incident, now time.Time) error {
	ids := []string{}
	for _, v := range items {
		if v.ResourceKind == "external" && v.State != "resolved" && v.SourceInstanceID != nil {
			ids = append(ids, *v.SourceInstanceID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `SELECT d.id,d.enabled,d.connection_status,d.revision,d.sync_interval_seconds,d.configuration,(SELECT count(*) FROM external_signals s WHERE s.source_id=d.id AND s.state='active') FROM data_sources d WHERE d.id=ANY($1::text[])`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	type sourceState struct {
		enabled  bool
		status   string
		revision int64
		age      time.Duration
	}
	states := map[string]sourceState{}
	for rows.Next() {
		var id string
		var st sourceState
		var interval, active int
		var raw []byte
		var cfg datasources.Settings
		if err = rows.Scan(&id, &st.enabled, &st.status, &st.revision, &interval, &raw, &active); err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &cfg); err != nil {
			return err
		}
		st.age = externalStaleAfter(cfg, interval, active)
		states[id] = st
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for i := range items {
		v := &items[i]
		if v.ResourceKind != "external" || v.State == "resolved" || v.SourceInstanceID == nil {
			continue
		}
		st, ok := states[*v.SourceInstanceID]
		if !ok {
			v.EvidenceStatus = "source_unavailable"
			continue
		}
		// Revision belongs to the normalized signal, not the Collector namespace.
		v.EvidenceStatus = externalFreshness(st.enabled, st.status, st.revision, v.Signal.SourceRevision, v.LastObservedAt, now, st.age)
	}
	return nil
}

func (s *Store) externalFilter(ctx context.Context, f externalmonitor.Filter) error {
	if f.Limit < 1 || f.Limit > 200 || f.Offset < 0 || f.Offset > 100000 {
		return externalmonitor.ErrInvalid
	}
	if f.AssetID != "" {
		if _, err := s.Asset(ctx, f.AssetID); err != nil {
			return err
		}
	}
	if f.SourceID != "" {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM data_sources WHERE id=$1)`, f.SourceID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
	}
	if f.ItemID != "" && !externalmonitor.ValidID(f.ItemID) && !prommodel.MetricIDValid(f.ItemID) {
		return externalmonitor.ErrInvalid
	}
	return nil
}
func (s *Store) ExternalMetrics(ctx context.Context, f externalmonitor.Filter, history bool) ([]externalmonitor.MetricRecord, error) {
	if err := s.externalFilter(ctx, f); err != nil {
		return nil, err
	}
	query := `SELECT source_id,source_type,source_revision,asset_id,payload,received_at,last_seen,enabled,status,revision FROM (
 SELECT d.id AS source_id,d.type AS source_type,m.source_revision,COALESCE(b.asset_id,'') AS asset_id,v.payload,v.received_at,m.last_seen,d.enabled,d.connection_status AS status,d.revision,m.item_id FROM external_metrics m JOIN external_metric_samples v ON v.id=m.latest_sample_id JOIN data_sources d ON d.id=m.source_id LEFT JOIN asset_source_bindings b ON b.source_instance_id=m.source_id AND b.external_type='host' AND b.external_id=m.host_id AND b.scope='' WHERE ($1='' OR d.id=$1) AND ($2='' OR b.asset_id=$2) AND ($3='' OR m.item_id=$3)
 UNION ALL
 SELECT d.id,d.type,m.source_revision,COALESCE(b.asset_id,''),m.payload,m.received_at,m.last_seen,d.enabled,d.connection_status,d.revision,m.item_id FROM prometheus_measurements m JOIN data_sources d ON d.id=m.source_id LEFT JOIN asset_source_bindings b ON b.source_instance_id=m.source_id AND b.external_type='host' AND b.external_id=m.host_id AND b.scope='' WHERE ($1='' OR d.id=$1) AND ($2='' OR b.asset_id=$2) AND ($3='' OR m.item_id=$3)
 ) snapshots ORDER BY source_id,item_id LIMIT $4 OFFSET $5`
	if history {
		if f.SourceID != "" {
			source, err := s.DataSource(ctx, f.SourceID)
			if err != nil {
				return nil, err
			}
			if source.Type == "PROMETHEUS" {
				return nil, prommodel.ErrHistory
			}
		}
		query = `SELECT d.id,d.type,v.source_revision,COALESCE(b.asset_id,''),v.payload,v.received_at,v.received_at,d.enabled,d.connection_status,d.revision FROM external_metric_samples v JOIN data_sources d ON d.id=v.source_id LEFT JOIN asset_source_bindings b ON b.source_instance_id=v.source_id AND b.external_type='host' AND b.external_id=v.host_id AND b.scope='' WHERE ($1='' OR d.id=$1) AND ($2='' OR b.asset_id=$2) AND ($3='' OR v.item_id=$3) ORDER BY v.id DESC LIMIT $4 OFFSET $5`
	}
	rows, err := s.pool.Query(ctx, query, f.SourceID, f.AssetID, f.ItemID, f.Limit, f.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []externalmonitor.MetricRecord{}
	for rows.Next() {
		var v externalmonitor.MetricRecord
		var raw []byte
		var enabled bool
		var status string
		var revision int64
		if err = rows.Scan(&v.SourceInstanceID, &v.SourceType, &v.SourceRevision, &v.AssetID, &raw, &v.ReceivedAt, &v.LastSeen, &enabled, &status, &revision); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &v.Measurement); err != nil {
			return nil, err
		}
		if !history {
			if !enabled || status == "error" {
				v.Quality = "source_unavailable"
			} else if revision != v.SourceRevision {
				v.Quality = "configuration_changed"
			} else if v.Quality == "fresh" && (v.ObservedAt == nil || time.Since(*v.ObservedAt) > time.Duration(v.MaxAgeSeconds)*time.Second || v.SourceType == "PROMETHEUS" && (v.ScrapedAt == nil || time.Since(*v.ScrapedAt) > time.Duration(v.MaxAgeSeconds)*time.Second)) {
				v.Quality = "stale"
			}
		}
		items = append(items, v)
	}
	return items, rows.Err()
}
func (s *Store) ExternalSignals(ctx context.Context, f externalmonitor.Filter) ([]externalmonitor.SignalRecord, error) {
	if f.State != "" && f.State != "active" && f.State != "resolved" {
		return nil, externalmonitor.ErrInvalid
	}
	if err := s.externalFilter(ctx, f); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT s.source_id,d.type,s.source_revision,s.payload,COALESCE(s.incident_id,''),s.first_seen,s.last_seen,d.enabled,d.connection_status,d.revision,d.sync_interval_seconds,d.configuration,ARRAY(SELECT DISTINCT b.asset_id FROM external_signal_hosts h JOIN asset_source_bindings b ON b.source_instance_id=h.source_id AND b.external_type='host' AND b.external_id=h.host_id AND b.scope='' WHERE h.source_id=s.source_id AND h.event_id=s.event_id ORDER BY b.asset_id),(SELECT count(*) FROM external_signals c WHERE c.source_id=s.source_id AND c.state='active') FROM external_signals s JOIN data_sources d ON d.id=s.source_id WHERE ($1='' OR s.source_id=$1) AND ($2='' OR s.state=$2) AND ($3='' OR EXISTS(SELECT 1 FROM external_signal_hosts h JOIN asset_source_bindings b ON b.source_instance_id=h.source_id AND b.external_type='host' AND b.external_id=h.host_id AND b.scope='' WHERE h.source_id=s.source_id AND h.event_id=s.event_id AND b.asset_id=$3)) ORDER BY s.first_seen DESC,s.source_id,s.event_id::numeric DESC LIMIT $4 OFFSET $5`, f.SourceID, f.State, f.AssetID, f.Limit, f.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []externalmonitor.SignalRecord{}
	for rows.Next() {
		var v externalmonitor.SignalRecord
		var raw, config []byte
		var enabled bool
		var status string
		var revision int64
		var interval, active int
		if err = rows.Scan(&v.SourceInstanceID, &v.SourceType, &v.SourceRevision, &raw, &v.IncidentID, &v.FirstSeen, &v.LastSeen, &enabled, &status, &revision, &interval, &config, &v.AssetIDs, &active); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &v.Signal); err != nil {
			return nil, err
		}
		var cfg datasources.Settings
		if err = json.Unmarshal(config, &cfg); err != nil {
			return nil, err
		}
		v.Freshness = externalFreshness(enabled, status, revision, v.SourceRevision, v.LastSeen, time.Now(), externalStaleAfter(cfg, interval, active))
		if v.AssetIDs == nil {
			v.AssetIDs = []string{}
		}
		if v.State == "resolved" {
			v.Freshness = "historical"
		}
		items = append(items, v)
	}
	return items, rows.Err()
}
func (s *Store) ExternalSignalHistory(ctx context.Context, sourceID, eventID string, limit, offset int) ([]externalmonitor.SignalVersion, error) {
	if !externalmonitor.ValidID(eventID) || limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, externalmonitor.ErrInvalid
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM external_signals WHERE source_id=$1 AND event_id=$2)`, sourceID, eventID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT id,source_revision,payload,received_at FROM external_signal_versions WHERE source_id=$1 AND event_id=$2 ORDER BY id DESC LIMIT $3 OFFSET $4`, sourceID, eventID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []externalmonitor.SignalVersion{}
	for rows.Next() {
		var v externalmonitor.SignalVersion
		var raw []byte
		if err = rows.Scan(&v.ID, &v.SourceRevision, &raw, &v.ReceivedAt); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &v.Signal); err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}
