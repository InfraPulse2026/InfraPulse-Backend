package storage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/analysisai"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/externalmonitor"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/reports"
)

func TestExternalMonitoringAtomicLifecycleFreshnessAndAssetMoves(t *testing.T) {
	s, ctx, _ := sourceQueueFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	in := datasources.Input{Type: "ZABBIX", Name: "Monitoring fixture", Endpoint: "https://fixture.invalid", Enabled: true, SyncIntervalSeconds: 3, Configuration: datasources.Settings{Zabbix: &datasources.ZabbixSettings{HostIDs: []string{"101"}, DeviceType: "switch", Monitoring: &datasources.ZabbixMonitoring{Enabled: true, ItemMappings: []externalmonitor.Mapping{{HostID: "101", ItemID: "901", Metric: "cpu.utilization", SourceUnit: "%", Unit: "percent", Multiplier: "1", MaxAgeSeconds: 300}}}}}}
	source, err := s.SaveDataSource(ctx, "", in)
	if err != nil {
		t.Fatal(err)
	}
	value := "82.125"
	observed := now.Add(-time.Minute)
	metric := externalmonitor.Measurement{HostID: "101", ItemID: "901", Name: "CPU", Metric: "cpu.utilization", SourceUnit: "%", Unit: "percent", Multiplier: "1", MaxAgeSeconds: 300, Value: &value, RawValue: value, ObservedAt: &observed, Quality: "fresh"}
	signal := externalmonitor.Signal{EventID: "11", TriggerID: "700", HostIDs: []string{"101"}, Name: "CPU high", Severity: 4, ObservedAt: now.Add(-time.Hour), State: "active", MappingStatus: "host_filter"}
	d := autoNetworkDevice("101")
	d.Source = "zabbix"
	result := datasources.Result{Cursor: json.RawMessage(`{"host_offset":0,"reconcile_after":"0"}`), Observations: []assets.Observation{{ExternalType: "host", ExternalID: "101", ObservedAt: now, Provenance: "external", Device: d}}, Monitoring: &externalmonitor.Batch{Measurements: []externalmonitor.Measurement{metric}, Signals: []externalmonitor.Signal{signal}}}
	run := func(key string, r datasources.Result, runErr error) {
		t.Helper()
		if _, _, err := s.QueueDataSourceJob(ctx, source.ID, "sync", key); err != nil {
			t.Fatal(err)
		}
		request := claimSourceJob(t, s, ctx)
		if err = s.FinishDataSourceJob(ctx, *request, r, runErr); err != nil {
			t.Fatal(err)
		}
	}
	run("first", result, nil)
	items, err := s.ExternalSignals(ctx, externalmonitor.Filter{SourceID: source.ID, Limit: 50})
	if err != nil || len(items) != 1 || len(items[0].AssetIDs) != 1 || items[0].Freshness != "fresh" {
		t.Fatal("signals/asset/freshness", items, err)
	}
	assetID := items[0].AssetIDs[0]
	incidentID := items[0].IncidentID
	incident, err := s.Incident(ctx, incidentID)
	if err != nil || incident.AgentID != "" || incident.ResourceKind != "external" || incident.SourceInstanceID == nil || *incident.SourceInstanceID != source.ID || incident.Signal.External == nil || incident.EvidenceStatus != "fresh" {
		t.Fatal("external incident scope", incident, err)
	}
	// B13 intentionally supports allowlisted external evidence. Preview remains
	// local and must exclude names, addresses and actual source/incident IDs.
	preview, err := s.CreatePreview(ctx, incidentID, analysisai.Config{})
	if err != nil || preview.Payload.Version != analysisai.PromptVersion || len(preview.Payload.Facts) != 1 || preview.Payload.Facts[0].Rule != "zabbix.problem" {
		t.Fatal("external B13 preview", preview, err)
	}
	export, err := json.Marshal(preview.Payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{incidentID, source.ID, source.Name, "fixture.invalid", signal.Name} {
		if strings.Contains(string(export), private) {
			t.Fatal("external private context exported", private)
		}
	}
	run("repeat", result, nil)
	history, err := s.ExternalSignalHistory(ctx, source.ID, "11", 50, 0)
	if err != nil || len(history) != 1 {
		t.Fatal("repeated signal grew history", len(history), err)
	}
	metrics, err := s.ExternalMetrics(ctx, externalmonitor.Filter{SourceID: source.ID, Limit: 50}, true)
	if err != nil || len(metrics) != 1 || metrics[0].Value == nil || *metrics[0].Value != value {
		t.Fatal("metric dedup/precision", metrics, err)
	}
	ack, err := s.AcknowledgeIncident(ctx, incidentID)
	if err != nil || ack.State != "acknowledged" {
		t.Fatal("common acknowledge", ack, err)
	}
	run("empty-is-not-recovery", datasources.Result{Monitoring: &externalmonitor.Batch{}}, nil)
	incident, err = s.Incident(ctx, incidentID)
	if err != nil || incident.State != "acknowledged" {
		t.Fatal("absence inferred recovery", incident, err)
	}
	run("api-failed", datasources.Result{}, datasources.Failure{Code: "api_rejected"})
	incident, err = s.Incident(ctx, incidentID)
	if err != nil || incident.State != "acknowledged" || incident.EvidenceStatus != "source_unavailable" {
		t.Fatal("failed poll erased or resolved state", incident, err)
	}
	if err = s.Ingest(ctx, registration("collector", now)); err != nil {
		t.Fatal(err)
	}
	local := autoNetworkDevice("local-switch")
	if err = s.Ingest(ctx, assetInventory("collector", "session-1", "local-inventory", now, local)); err != nil {
		t.Fatal(err)
	}
	assetItems, err := s.Assets(ctx, "network_device", "real", 50, 0)
	if err != nil || len(assetItems) != 2 {
		t.Fatal(err)
	}
	var localAsset assets.Asset
	for _, a := range assetItems {
		if a.ID != assetID {
			localAsset = a
		}
	}
	externalAsset, _ := s.Asset(ctx, assetID)
	bindings, _ := s.AssetBindings(ctx, localAsset.ID, true, 50, 0)
	decision, _, err := s.MergeAssets(ctx, assets.MergeInput{TargetID: localAsset.ID, TargetRevision: localAsset.Revision, SourceID: assetID, SourceRevision: externalAsset.Revision, CanonicalBindingID: bindings[0].ID, Confirmed: true, Reason: "verified same switch"}, "external-link-merge")
	if err != nil {
		t.Fatal(err)
	}
	items, err = s.ExternalSignals(ctx, externalmonitor.Filter{AssetID: decision.TargetID, Limit: 50})
	if err != nil || len(items) != 1 || items[0].AssetIDs[0] != decision.TargetID {
		t.Fatal("signal link froze old Asset", items, err)
	}
	metrics, err = s.ExternalMetrics(ctx, externalmonitor.Filter{AssetID: decision.TargetID, Limit: 50}, false)
	if err != nil || len(metrics) != 1 || metrics[0].AssetID != decision.TargetID {
		t.Fatal("metric link froze old Asset", metrics, err)
	}
	// Recovery + cursor + normalized data + common incident are one transaction.
	closed := now
	signal.State = "resolved"
	signal.RecoveryEventID = "22"
	signal.RecoveredAt = &closed
	result.Monitoring.Signals = []externalmonitor.Signal{signal}
	result.Cursor = json.RawMessage(`{"host_offset":0,"after":"22"}`)
	if _, err = s.pool.Exec(ctx, `CREATE FUNCTION reject_external_evidence() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture rejection'; END $$; CREATE TRIGGER reject_external_evidence BEFORE INSERT ON incident_evidence FOR EACH ROW EXECUTE FUNCTION reject_external_evidence()`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.QueueDataSourceJob(ctx, source.ID, "sync", "atomic-recovery"); err != nil {
		t.Fatal(err)
	}
	request := claimSourceJob(t, s, ctx)
	if err = s.FinishDataSourceJob(ctx, *request, result, nil); err == nil {
		t.Fatal("SQL failure acknowledged partial recovery")
	}
	var state, cursor string
	if err = s.pool.QueryRow(ctx, `SELECT state FROM external_signals WHERE source_id=$1 AND event_id='11'`, source.ID).Scan(&state); err != nil || state != "active" {
		t.Fatal("partial signal committed", state, err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT sync_cursor::text FROM data_sources WHERE id=$1`, source.ID).Scan(&cursor); err != nil || cursor == `{"after": "22", "host_offset": 0}` {
		t.Fatal("partial cursor committed", cursor, err)
	}
	if _, err = s.pool.Exec(ctx, `DROP TRIGGER reject_external_evidence ON incident_evidence`); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishDataSourceJob(ctx, *request, result, nil); err != nil {
		t.Fatal(err)
	}
	incident, err = s.Incident(ctx, incidentID)
	if err != nil || incident.State != "resolved" || incident.EvidenceStatus != "historical" {
		t.Fatal("explicit recovery missing", incident, err)
	}
	if _, err = s.AcknowledgeIncident(ctx, incidentID); err != health.ErrConflict {
		t.Fatal("resolved incident acknowledged", err)
	}
	history, err = s.ExternalSignalHistory(ctx, source.ID, "11", 50, 0)
	if err != nil || len(history) != 2 {
		t.Fatal("recovery version", len(history), err)
	}
	result.Monitoring.Signals[0].State = "active"
	result.Monitoring.Signals[0].RecoveredAt = nil
	result.Monitoring.Signals[0].RecoveryEventID = ""
	run("stale-active-response", result, nil)
	incident, _ = s.Incident(ctx, incidentID)
	if incident.State != "resolved" {
		t.Fatal("reopened recovered event")
	}
	report, err := s.ReportSummary(ctx, reports.Filter{From: now.Add(-2 * time.Hour), To: time.Now(), ResourceKind: "external", TopLimit: 10})
	if err != nil || len(report.TopResources) == 0 {
		t.Fatal("external report broke nullable Agent", err)
	}
}

func TestExternalMonitoringInstancesAndRevisionFence(t *testing.T) {
	s, ctx, _ := sourceQueueFixture(t)
	now := time.Now().UTC()
	input := datasources.Input{Type: "ZABBIX", Name: "A", Endpoint: "https://fixture.invalid", Enabled: true, SyncIntervalSeconds: 3, Configuration: datasources.Settings{Zabbix: &datasources.ZabbixSettings{HostIDs: []string{"101"}, Monitoring: &datasources.ZabbixMonitoring{Enabled: true}}}}
	a, err := s.SaveDataSource(ctx, "", input)
	if err != nil {
		t.Fatal(err)
	}
	input.Name = "B"
	b, err := s.SaveDataSource(ctx, "", input)
	if err != nil {
		t.Fatal(err)
	}
	batch := datasources.Result{Cursor: json.RawMessage(`{"after":"11"}`), Monitoring: &externalmonitor.Batch{Signals: []externalmonitor.Signal{{EventID: "11", TriggerID: "700", HostIDs: []string{"101"}, Name: "same event ID different server", Severity: 3, ObservedAt: now, State: "active", MappingStatus: "host_filter"}}}}
	for _, source := range []datasources.Source{a, b} {
		if _, _, err = s.QueueDataSourceJob(ctx, source.ID, "sync", "initial-"+source.ID); err != nil {
			t.Fatal(err)
		}
		r := claimSourceJob(t, s, ctx)
		if err = s.FinishDataSourceJob(ctx, *r, batch, nil); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.ExternalSignals(ctx, externalmonitor.Filter{Limit: 50})
	if err != nil || len(items) != 2 || items[0].IncidentID == items[1].IncidentID || items[0].SourceInstanceID == items[1].SourceInstanceID {
		t.Fatal("instances collided", items, err)
	}
	if _, _, err = s.QueueDataSourceJob(ctx, a.ID, "sync", "obsolete-monitoring"); err != nil {
		t.Fatal(err)
	}
	request := claimSourceJob(t, s, ctx)
	input.Name = "A updated"
	input.Revision = a.Revision
	if _, err = s.SaveDataSource(ctx, a.ID, input); err != nil {
		t.Fatal(err)
	}
	closed := now
	batch.Monitoring.Signals[0].State = "resolved"
	batch.Monitoring.Signals[0].RecoveryEventID = "22"
	batch.Monitoring.Signals[0].RecoveredAt = &closed
	if err = s.FinishDataSourceJob(ctx, *request, batch, nil); err != nil {
		t.Fatal(err)
	}
	job, err := s.DataSourceJob(ctx, a.ID, request.Job.ID)
	if err != nil || job.State != "cancelled" {
		t.Fatal("revision did not fence publish", job, err)
	}
	items, err = s.ExternalSignals(ctx, externalmonitor.Filter{SourceID: a.ID, Limit: 50})
	if err != nil || len(items) != 1 || items[0].State != "active" || items[0].Freshness != "configuration_changed" {
		t.Fatal("stale recovery published", items, err)
	}
	if _, err = s.ExternalSignalHistory(ctx, "different-source", "11", 50, 0); err != ErrNotFound {
		t.Fatal("history crossed source boundary", err)
	}
}

func TestExternalMonitoringCannotOverrideStoredPolicy(t *testing.T) {
	s, ctx, source := sourceQueueFixture(t)
	if _, _, err := s.QueueDataSourceJob(ctx, source.ID, "sync", "policy-cannot-be-invented"); err != nil {
		t.Fatal(err)
	}
	r := claimSourceJob(t, s, ctx)
	// An adapter/request copy cannot turn on monitoring absent from the DB.
	r.Source.Configuration.Zabbix = &datasources.ZabbixSettings{HostIDs: []string{"101"}, Monitoring: &datasources.ZabbixMonitoring{Enabled: true}}
	result := datasources.Result{Cursor: json.RawMessage(`{"after":"11"}`), Monitoring: &externalmonitor.Batch{Signals: []externalmonitor.Signal{{EventID: "11", TriggerID: "700", HostIDs: []string{"101"}, Name: "must not publish", Severity: 4, ObservedAt: time.Now(), State: "active", MappingStatus: "host_filter"}}}}
	if err := s.FinishDataSourceJob(ctx, *r, result, nil); err != nil {
		t.Fatal(err)
	}
	job, err := s.DataSourceJob(ctx, source.ID, r.Job.ID)
	if err != nil || job.State != "failed" || job.Code != "invalid_response" {
		t.Fatal("adapter overrode persisted policy", job, err)
	}
	items, err := s.ExternalSignals(ctx, externalmonitor.Filter{SourceID: source.ID, Limit: 50})
	if err != nil || len(items) != 0 {
		t.Fatal("unapproved signals stored", items, err)
	}
}
