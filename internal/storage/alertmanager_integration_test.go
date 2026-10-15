package storage

import (
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/correlation"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
	"testing"
	"time"
)

func TestAlertmanagerScopedRecoveryRotationReplayAndAtomicity(t *testing.T) {
	s := integrationStore(t)
	_, principal := adminFixture(t, s)
	ctx := access.WithPrincipal(t.Context(), principal)
	cfg, _ := (prommodel.Settings{ScrapePools: []string{"node"}, AlertsEnabled: true}).Normalize()
	source, err := s.SaveDataSource(ctx, "", datasources.Input{Type: "PROMETHEUS", Name: "AM fixture", Endpoint: "https://fixture.invalid", Enabled: true, SyncIntervalSeconds: 3, Configuration: datasources.Settings{Prometheus: &cfg}})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Minute)
	identity := map[string]string{"job": "node", "instance": "host:9100"}
	target := prommodel.Target{ID: prommodel.TargetID("node", identity), ScrapePool: "node", Identity: identity, Labels: identity, Health: "up", LastScrape: &at, SourceRevision: source.Revision}
	labels := []map[string]string{{"job": "node", "instance": "host:9100", "alertname": "CPU high"}, {"job": "node", "instance": "host:9100", "alertname": "RAM high"}}
	batch := &prommodel.Batch{Targets: []prommodel.Target{target}}
	for _, l := range labels {
		batch.Alerts = append(batch.Alerts, prommodel.Alert{Fingerprint: prommodel.Hash("prom_alert_", []any{target.ID, l, at.Format(time.RFC3339Nano)}), LabelFingerprint: prommodel.Hash("prom_labels_", l), TargetID: target.ID, Name: l["alertname"], Severity: "critical", ActiveAt: at})
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	d := autoNetworkDevice(target.ID)
	d.Source = "prometheus"
	if err = s.projectExternalAssets(ctx, tx, source.ID, "fixture", []assets.Observation{{ExternalType: "host", ExternalID: target.ID, Provenance: "external", ObservedAt: at, Device: d}}); err != nil {
		t.Fatal(err)
	}
	if err = s.projectPrometheus(ctx, tx, source, batch); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	receiver, err := s.SaveAlertmanagerReceiver(ctx, source.ID, prommodel.ReceiverInput{Enabled: true, TTLSeconds: 3600, Confirmed: true, Reason: "reviewed source"})
	if err != nil || receiver.Token == "" {
		t.Fatal(receiver, err)
	}
	if err = s.CheckAlertmanagerToken(ctx, source.ID, "bad"); err != prommodel.ErrWebhookAuth {
		t.Fatal("bad token", err)
	}
	public, err := s.AlertmanagerReceiver(ctx, source.ID)
	if err != nil || public.Token != "" {
		t.Fatal("token exposed on GET", err)
	}
	if _, err = s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	cases, err := s.CorrelatedIncidents(ctx, correlation.Filter{Limit: 100})
	if err != nil || len(cases) != 1 || cases[0].ActiveSignalCount != 2 {
		t.Fatal("initial grouping", cases, err)
	}
	payload := func(l map[string]string) prommodel.Webhook {
		return prommodel.Webhook{Version: "4", Status: "resolved", Alerts: []prommodel.WebhookAlert{{Status: "resolved", Labels: l, StartsAt: at.Add(10 * time.Second), EndsAt: time.Now().UTC().Add(-time.Second)}}}
	}
	w := payload(labels[0])
	mixed := w
	mixed.Status = "firing"
	mixed.Alerts = append(append([]prommodel.WebhookAlert{}, w.Alerts...), prommodel.WebhookAlert{Status: "firing", Labels: map[string]string{"alertname": "still active"}})
	result, err := s.ReceiveAlertmanager(ctx, source.ID, receiver.Token, mixed)
	if err != nil || result.Resolved != 1 {
		t.Fatal("different ActiveAt/FiredAt failed", result, err)
	}
	result, err = s.ReceiveAlertmanager(ctx, source.ID, receiver.Token, w)
	if err != nil || result.Resolved != 0 || result.Ignored != 1 {
		t.Fatal("replay", result, err)
	}
	if _, err = s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	card, err := s.CorrelatedIncident(ctx, cases[0].ID)
	if err != nil || card.Incident.State != "open" || card.Incident.ActiveSignalCount != 1 {
		t.Fatal("partial recovery closed all", card.Incident, err)
	}
	rotated, err := s.SaveAlertmanagerReceiver(ctx, source.ID, prommodel.ReceiverInput{Enabled: true, TTLSeconds: 3600, ExpectedRevision: 1, Confirmed: true, Reason: "rotate integration token"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CheckAlertmanagerToken(ctx, source.ID, receiver.Token); err != prommodel.ErrWebhookAuth {
		t.Fatal("old token accepted", err)
	}
	if _, err = s.pool.Exec(ctx, `ALTER TABLE alertmanager_receipts ADD CONSTRAINT reject_receipt_fixture CHECK(event_id<0) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReceiveAlertmanager(ctx, source.ID, rotated.Token, payload(labels[1])); err == nil {
		t.Fatal("SQL failure ignored")
	}
	incidents, err := s.Incidents(ctx, health.Filter{Source: "prometheus", State: "open"})
	if err != nil || len(incidents) != 1 {
		t.Fatal("partial recovery committed", incidents, err)
	}
	if _, err = s.pool.Exec(ctx, `ALTER TABLE alertmanager_receipts DROP CONSTRAINT reject_receipt_fixture`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReceiveAlertmanager(ctx, source.ID, rotated.Token, payload(labels[1])); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	card, err = s.CorrelatedIncident(ctx, cases[0].ID)
	if err != nil || card.Incident.State != "resolved" {
		t.Fatal("full recovery", card.Incident, err)
	}
	if _, err = s.ReceiveAlertmanager(ctx, source.ID, rotated.Token, payload(map[string]string{"alertname": "unknown"})); err != prommodel.ErrWebhookConflict {
		t.Fatal("unknown episode guessed", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE data_sources SET revision=revision+1 WHERE id=$1`, source.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckAlertmanagerToken(ctx, source.ID, rotated.Token); err != prommodel.ErrWebhookAuth {
		t.Fatal("source fence ignored", err)
	}
}
