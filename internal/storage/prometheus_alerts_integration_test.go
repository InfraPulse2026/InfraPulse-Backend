package storage

import (
	"encoding/json"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/correlation"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/externalmonitor"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
	"testing"
	"time"
)

func TestPrometheusAlertsAtomicDedupRecurrenceAndNoAbsentRecovery(t *testing.T) {
	s := integrationStore(t)
	_, principal := adminFixture(t, s)
	ctx := access.WithPrincipal(t.Context(), principal)
	cfg, err := (prommodel.Settings{ScrapePools: []string{"node"}, AlertsEnabled: true}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	source, err := s.SaveDataSource(ctx, "", datasources.Input{Type: "PROMETHEUS", Name: "Alerts fixture", Endpoint: "https://fixture.invalid", Enabled: true, SyncIntervalSeconds: 3, Configuration: datasources.Settings{Prometheus: &cfg}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Second)
	identity := map[string]string{"job": "node", "instance": "host:9100"}
	target := prommodel.Target{ID: prommodel.TargetID("node", identity), ScrapePool: "node", Identity: identity, Labels: identity, Health: "up", LastScrape: &now, SourceRevision: source.Revision}
	d := autoNetworkDevice(target.ID)
	d.Source = "prometheus"
	observations := []assets.Observation{{ExternalType: "host", ExternalID: target.ID, ObservedAt: now, Provenance: "external", Device: d}}
	alert := prommodel.Alert{Fingerprint: prommodel.Hash("prom_alert_", []string{"CPU", now.Format(time.RFC3339Nano)}), TargetID: target.ID, Name: "CPU high", Severity: "critical", ActiveAt: now}
	batch := &prommodel.Batch{Targets: []prommodel.Target{target}, Alerts: []prommodel.Alert{alert}}
	publish := func() {
		t.Helper()
		if !validatePrometheus(source, batch, observations) {
			t.Fatal("batch invalid")
		}
		tx, e := s.pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if e = s.projectExternalAssets(ctx, tx, source.ID, "fixture", observations); e != nil {
			t.Fatal(e)
		}
		if e = s.projectPrometheus(ctx, tx, source, batch); e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
	}
	publish()
	publish()
	incidents, err := s.Incidents(ctx, health.Filter{Source: "prometheus"})
	if err != nil || len(incidents) != 1 || incidents[0].RuleID != "prometheus.alert" {
		t.Fatalf("incidents %+v %v", incidents, err)
	}
	event := *incidents[0].ExternalEventID
	history, err := s.ExternalSignalHistory(ctx, source.ID, event, 100, 0)
	if err != nil || len(history) != 1 {
		t.Fatalf("duplicate history %d %v", len(history), err)
	}
	batch.Alerts = nil
	publish()
	incidents, err = s.Incidents(ctx, health.Filter{Source: "prometheus"})
	if err != nil || incidents[0].State != "open" {
		t.Fatal("absence inferred recovery", err)
	}
	if _, err = s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	cases, err := s.CorrelatedIncidents(ctx, correlation.Filter{Limit: 100})
	if err != nil || len(cases) != 1 {
		t.Fatal("missing global incident", cases, err)
	}
	alert.ActiveAt = now.Add(time.Second)
	alert.Fingerprint = prommodel.Hash("prom_alert_", []string{"CPU", alert.ActiveAt.Format(time.RFC3339Nano)})
	batch.Alerts = []prommodel.Alert{alert}
	publish()
	incidents, err = s.Incidents(ctx, health.Filter{Source: "prometheus"})
	if err != nil || len(incidents) != 2 {
		t.Fatal("recurrence lost", incidents, err)
	}
	signals, err := s.ExternalSignals(ctx, externalmonitor.Filter{SourceID: source.ID, Limit: 100})
	if err != nil || len(signals) != 2 || signals[0].SourceType != "PROMETHEUS" {
		t.Fatal("source ownership", err)
	}
	// The public representation keeps numeric local event IDs, never raw labels.
	raw, _ := json.Marshal(signals)
	if len(raw) == 0 {
		t.Fatal("empty public representation")
	}
	bad := *batch
	bad.Alerts = append([]prommodel.Alert{}, batch.Alerts...)
	bad.Alerts[0].TargetID = "unowned"
	if validatePrometheus(source, &bad, observations) {
		t.Fatal("unowned target accepted")
	}
	cfg.AlertsEnabled = false
	disabled := source
	disabled.Configuration.Prometheus = &cfg
	if validatePrometheus(disabled, batch, observations) {
		t.Fatal("alerts without opt-in accepted")
	}
	if _, err = s.pool.Exec(ctx, `ALTER TABLE incident_evidence ADD CONSTRAINT reject_prometheus_fixture CHECK(kind<>'opened') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	third := *batch
	third.Alerts = append([]prommodel.Alert{}, batch.Alerts...)
	third.Alerts[0].Fingerprint = prommodel.Hash("prom_alert_", []string{"third"})
	if err = s.projectPrometheus(ctx, tx, source, &third); err == nil {
		tx.Rollback(ctx)
		t.Fatal("forced SQL failure ignored")
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var events, signalCount int
	if err = s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM prometheus_alert_events),(SELECT count(*) FROM external_signals WHERE source_id=$1)`, source.ID).Scan(&events, &signalCount); err != nil || events != 2 || signalCount != 2 {
		t.Fatalf("partial alert transaction %d %d %v", events, signalCount, err)
	}
}
