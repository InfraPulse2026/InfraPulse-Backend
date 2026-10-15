package storage

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/externalmonitor"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prometheus"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
	"github.com/jackc/pgx/v5"
)

func (s *Store) ConfigurePrometheus(connector *prometheus.Connector) { s.prometheus = connector }

var prometheusAlertFingerprint = regexp.MustCompile(`^prom_alert_[a-f0-9]{64}$`)

func validatePrometheus(source datasources.Source, b *prommodel.Batch, observations []assets.Observation) bool {
	if b == nil {
		return true
	}
	if source.Type != "PROMETHEUS" || source.Configuration.Prometheus == nil {
		return false
	}
	cfg, err := source.Configuration.Prometheus.Normalize()
	if err != nil || len(b.Targets) > 5 || len(b.Measurements) > 80 || len(b.Alerts) > 100 || len(b.Targets) != len(observations) {
		return false
	}
	targets := map[string]prommodel.Target{}
	for i, t := range b.Targets {
		if t.ID != prommodel.TargetID(t.ScrapePool, t.Identity) || !cfg.Selected(t.ScrapePool, t.Labels) || t.SourceRevision != source.Revision || len(t.Identity) != len(cfg.IdentityLabels) || len(t.Labels) > 32 {
			return false
		}
		for _, key := range cfg.IdentityLabels {
			if t.Identity[key] == "" || t.Identity[key] != t.Labels[key] || !prommodel.Text(t.Identity[key], 512) {
				return false
			}
		}
		if observations[i].ExternalType != "host" || observations[i].ExternalID != t.ID || observations[i].Provenance != "external" {
			return false
		}
		if _, ok := targets[t.ID]; ok {
			return false
		}
		targets[t.ID] = t
	}
	mappings := map[string]prommodel.Mapping{}
	for _, m := range cfg.Mappings {
		mappings[m.ID] = m
	}
	seenAlerts := map[string]bool{}
	for _, a := range b.Alerts {
		if !cfg.AlertsEnabled || !prometheusAlertFingerprint.MatchString(a.Fingerprint) || seenAlerts[a.Fingerprint] || a.Name == "" || !prommodel.Text(a.Name, 256) || a.Severity != "warning" && a.Severity != "critical" || a.ActiveAt.Year() < 1970 || a.ActiveAt.Year() > 9999 || a.ActiveAt.After(time.Now().Add(30*time.Second)) {
			return false
		}
		if _, ok := targets[a.TargetID]; !ok {
			return false
		}
		seenAlerts[a.Fingerprint] = true
	}
	seen := map[string]bool{}
	for _, v := range b.Measurements {
		m, ok := mappings[v.MappingID]
		t, hasTarget := targets[v.HostID]
		if !ok || !hasTarget || seen[v.ItemID] || v.ItemID != prommodel.MetricID(t.ID, m.ID) || v.Metric != m.Metric || v.Unit != m.Unit || v.Multiplier != m.Multiplier || v.MaxAgeSeconds != m.MaxAgeSeconds || v.TimeSemantics != "promql_evaluation" {
			return false
		}
		seen[v.ItemID] = true
		switch v.Quality {
		case "fresh", "stale", "no_data", "clock_skew", "source_unavailable", "unsupported", "ambiguous", "non_finite", "invalid_value", "partial":
		default:
			return false
		}
		if v.Value != nil {
			if _, ok := externalmonitor.Number(*v.Value); !ok || v.ObservedAt == nil {
				return false
			}
		}
		if v.Quality == "fresh" && (v.Value == nil || v.ScrapedAt == nil) {
			return false
		}
		if v.ObservedAt != nil && (v.ObservedAt.Year() < 1970 || v.ObservedAt.Year() > 9999 || v.ObservedAt.After(time.Now().Add(30*time.Second))) {
			return false
		}
	}
	raw, err := json.Marshal(b)
	return err == nil && len(raw) <= 4<<20
}
func (s *Store) projectPrometheus(ctx context.Context, tx pgx.Tx, source datasources.Source, b *prommodel.Batch) error {
	if b == nil {
		return nil
	}
	for _, v := range b.Measurements {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO prometheus_measurements(source_id,item_id,host_id,source_revision,payload) VALUES($1,$2,$3,$4,$5) ON CONFLICT(source_id,item_id) DO UPDATE SET host_id=EXCLUDED.host_id,source_revision=EXCLUDED.source_revision,payload=EXCLUDED.payload,received_at=CASE WHEN prometheus_measurements.payload=EXCLUDED.payload THEN prometheus_measurements.received_at ELSE now() END,last_seen=now()`, source.ID, v.ItemID, v.HostID, source.Revision, raw); err != nil {
			return err
		}
	}
	alerts := []externalmonitor.Signal{}
	for _, a := range b.Alerts {
		var eventID int64
		if err := tx.QueryRow(ctx, `INSERT INTO prometheus_alert_events(source_id,fingerprint,active_at,label_fingerprint,target_id,source_revision) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(source_id,fingerprint) DO UPDATE SET label_fingerprint=EXCLUDED.label_fingerprint,target_id=EXCLUDED.target_id,source_revision=EXCLUDED.source_revision RETURNING id`, source.ID, a.Fingerprint, a.ActiveAt, a.LabelFingerprint, a.TargetID, source.Revision).Scan(&eventID); err != nil {
			return err
		}
		id := strconv.FormatInt(eventID, 10)
		severity := 2
		if a.Severity == "critical" {
			severity = 4
		}
		alerts = append(alerts, externalmonitor.Signal{EventID: id, TriggerID: id, HostIDs: []string{a.TargetID}, Name: a.Name, Severity: severity, ObservedAt: a.ActiveAt, State: "active", MappingStatus: "selected_prometheus_target"})
	}
	return s.projectExternalMonitoring(ctx, tx, source, &externalmonitor.Batch{Signals: alerts})
}
func (s *Store) PrometheusQuery(ctx context.Context, q prommodel.Query) (prommodel.QueryResult, error) {
	var empty prommodel.QueryResult
	if err := q.Validate(time.Now()); err != nil {
		return empty, err
	}
	if s.prometheus == nil {
		return empty, datasources.ErrUnsupported
	}
	// Endpoint, settings, binding and secret must belong to ONE DB snapshot.
	// Otherwise a concurrent endpoint/token edit could mix their revisions.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(context.Background())
	source, err := readSource(tx.QueryRow(ctx, `SELECT `+sourceColumns+` FROM data_sources WHERE id=$1`, q.SourceID))
	if errors.Is(err, datasources.ErrNotFound) {
		return empty, ErrNotFound
	}
	if err != nil {
		return empty, err
	}
	if source.Type != "PROMETHEUS" || source.Configuration.Prometheus == nil {
		return empty, datasources.ErrUnsupported
	}
	if !source.Enabled {
		return empty, datasources.ErrDisabled
	}
	var raw json.RawMessage
	err = tx.QueryRow(ctx, `SELECT provider_state FROM asset_source_bindings WHERE asset_id=$1 AND source_instance_id=$2 AND external_type='host' AND external_id=$3 AND scope=''`, q.AssetID, q.SourceID, q.TargetID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, ErrNotFound
	}
	if err != nil {
		return empty, err
	}
	var target prommodel.Target
	if json.Unmarshal(raw, &target) != nil {
		return empty, prommodel.ErrInvalid
	}
	if target.SourceRevision != source.Revision {
		return empty, datasources.ErrConflict
	}
	var encrypted []byte
	if err = tx.QueryRow(ctx, `SELECT credential_ciphertext FROM data_sources WHERE id=$1`, source.ID).Scan(&encrypted); err != nil {
		return empty, err
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, err
	}
	var credential []byte
	if encrypted != nil {
		credential, err = s.sourceVault.Open(source.ID, encrypted)
		if err != nil {
			return empty, datasources.ErrKeyUnavailable
		}
		defer clear(credential)
	}
	call, cancel := context.WithTimeout(ctx, time.Duration(source.Configuration.TimeoutSeconds)*time.Second)
	defer cancel()
	result, err := s.prometheus.Query(call, source, credential, target, q)
	if err != nil {
		return empty, err
	}
	var current bool
	if err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM data_sources d JOIN asset_source_bindings b ON b.source_instance_id=d.id WHERE d.id=$1 AND d.enabled AND d.revision=$2 AND b.asset_id=$3 AND b.external_type='host' AND b.external_id=$4 AND b.scope='')`, source.ID, source.Revision, q.AssetID, q.TargetID).Scan(&current); err != nil {
		return empty, err
	}
	if !current {
		return empty, datasources.ErrConflict
	}
	return result, nil
}
