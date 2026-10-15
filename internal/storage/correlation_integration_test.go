package storage

import (
	"encoding/json"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/analysisai"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/correlation"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/externalmonitor"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/reports"
	"testing"
	"time"
)

func TestCorrelationQueueGroupingRecoveryAndReplay(t *testing.T) {
	s := integrationStore(t)
	_, p := adminFixture(t, s)
	ctx := access.WithPrincipal(t.Context(), p)
	now := time.Now().UTC().Add(-time.Second)
	hello := registration("agent-1", now)
	hello.GetHello().Device.Source = "linux"
	if err := s.Ingest(ctx, hello); err != nil {
		t.Fatal(err)
	}
	emit := func(source, rule string, recover bool) {
		t.Helper()
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		a := health.Assessment{RuleID: rule, RuleVersion: "1", Status: "warning", Severity: "warning", ObservedAt: &now, Evidence: []health.Point{}, Policy: health.Defaults()}
		if recover {
			a.Status = "ok"
			a.RecoveryEligible = true
		}
		raw, _ := json.Marshal(a)
		if err = applyIncident(ctx, tx, "agent-1", "shared-router", source, "", a, raw); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	emit("snmp", "interface.errors", false)
	emit("linux", "cpu.sustained", false)
	if n, err := s.ProcessCorrelation(ctx); err != nil || n != 2 {
		t.Fatalf("projection %d: %v", n, err)
	}
	cases, err := s.CorrelatedIncidents(ctx, correlation.Filter{Limit: 100})
	if err != nil || len(cases) != 1 {
		t.Fatalf("cases=%+v err=%v", cases, err)
	}
	id := cases[0].ID
	card, err := s.CorrelatedIncident(ctx, id)
	if err != nil || len(card.Signals) != 2 || len(card.Evidence) == 0 {
		t.Fatalf("card=%+v err=%v", card, err)
	}
	initialTimeline := len(card.Timeline)
	if _, err = s.RebuildCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	card, err = s.CorrelatedIncident(ctx, id)
	if err != nil || len(card.Timeline) != initialTimeline {
		t.Fatalf("replay changes timeline: %v %d", err, len(card.Timeline))
	}
	now = now.Add(time.Second)
	emit("snmp", "interface.errors", true)
	if _, err = s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	card, err = s.CorrelatedIncident(ctx, id)
	if err != nil || card.Incident.State != "open" || card.Incident.ActiveSignalCount != 1 {
		t.Fatalf("one recovery closed all: %+v %v", card.Incident, err)
	}
	emit("linux", "cpu.sustained", true)
	if _, err = s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	card, err = s.CorrelatedIncident(ctx, id)
	if err != nil || card.Incident.State != "resolved" || card.Incident.ActiveSignalCount != 0 {
		t.Fatalf("all recovery: %+v %v", card.Incident, err)
	}
}

func TestCorrelationServiceRevisionAndIdempotency(t *testing.T) {
	s, ctx, _ := healthFixture(t)
	var assetID string
	if err := s.pool.QueryRow(ctx, `SELECT asset_id FROM asset_source_bindings WHERE legacy_agent_id='agent-1' AND legacy_device_id='shared-router' LIMIT 1`).Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	in := correlation.ServiceInput{Name: "Branch voice", Kind: "voice", Enabled: true, Assets: []correlation.AssetRole{{AssetID: assetID, Role: "provider"}}, Confirmed: true, Reason: "reviewed voice dependency"}
	first, replay, err := s.SaveCorrelationService(ctx, "", in, "service-create")
	if err != nil || replay {
		t.Fatalf("create %v", err)
	}
	again, replay, err := s.SaveCorrelationService(ctx, "", in, "service-create")
	if err != nil || !replay || first.ID != again.ID {
		t.Fatalf("replay %v", err)
	}
	in.ExpectedRevision = first.Revision
	in.Enabled = false
	next, _, err := s.SaveCorrelationService(ctx, first.ID, in, "service-update")
	if err != nil || next.Revision != 2 {
		t.Fatalf("update %v", err)
	}
	if _, _, err = s.SaveCorrelationService(ctx, first.ID, in, "stale-update"); err != correlation.ErrConflict {
		t.Fatalf("stale revision %v", err)
	}
}

func TestCorrelationFailureKeepsDurablePending(t *testing.T) {
	s, ctx, now := healthFixture(t)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a := health.Assessment{RuleID: "cpu.sustained", Status: "warning", Severity: "warning", ObservedAt: &now, Evidence: []health.Point{}, Policy: health.Defaults()}
	raw, _ := json.Marshal(a)
	if err = applyIncident(ctx, tx, "agent-1", "shared-router", "simulated", "session-1", a, raw); err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `ALTER TABLE normalized_signals ADD CONSTRAINT fixture_rejection CHECK(category<>'performance')`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProcessCorrelation(ctx); err == nil {
		t.Fatal("projection failure ignored")
	}
	var pending, signals int
	if err = s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM correlation_pending),(SELECT count(*) FROM normalized_signals)`).Scan(&pending, &signals); err != nil || pending != 1 || signals != 0 {
		t.Fatalf("lost or partial projection %d %d %v", pending, signals, err)
	}
	if _, err = s.pool.Exec(ctx, `ALTER TABLE normalized_signals DROP CONSTRAINT fixture_rejection`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ProcessCorrelation(ctx); err != nil || n != 1 {
		t.Fatalf("retry %d %v", n, err)
	}
}

// A deterministic fixture, not acceptance of a live Zabbix server or PBX.
func TestCorrelationZabbixSNMPAndSIPAcrossReviewedService(t *testing.T) {
	s, ctx, items, bindings, voip := assetVoIPFixture(t)
	if _, _, err := s.SaveAssetVoIP(ctx, items[0].ID, "", voip, "correlation-voip"); err != nil {
		t.Fatal(err)
	}
	source, err := s.SaveDataSource(ctx, "", datasources.Input{Type: "ZABBIX", Name: "Correlation Zabbix fixture", Endpoint: "https://fixture.invalid", Enabled: true, SyncIntervalSeconds: 3, Configuration: datasources.Settings{Zabbix: &datasources.ZabbixSettings{HostIDs: []string{"101"}, DeviceType: "switch", Monitoring: &datasources.ZabbixMonitoring{Enabled: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Second)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	d := autoNetworkDevice("101")
	d.Source = "zabbix"
	if err = s.projectExternalAssets(ctx, tx, source.ID, "fixture", []assets.Observation{{ExternalType: "host", ExternalID: "101", ObservedAt: now, Provenance: "external", Device: d}}); err != nil {
		t.Fatal(err)
	}
	var externalAsset string
	if err = tx.QueryRow(ctx, `SELECT asset_id FROM asset_source_bindings WHERE source_instance_id=$1 AND external_id='101'`, source.ID).Scan(&externalAsset); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	service := correlation.ServiceInput{Name: "Verified voice dependency", Kind: "voice", Enabled: true, Confirmed: true, Reason: "reviewed lab dependencies", Assets: []correlation.AssetRole{{AssetID: externalAsset, Role: "dependency"}, {AssetID: items[0].ID, Role: "provider"}, {AssetID: items[1].ID, Role: "dependency"}}}
	if _, _, err = s.SaveCorrelationService(ctx, "", service, "correlation-service"); err != nil {
		t.Fatal(err)
	}
	tx, err = s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	signal := externalmonitor.Signal{EventID: "11", TriggerID: "700", HostIDs: []string{"101"}, Name: "Interface down", Severity: 4, ObservedAt: now, State: "active", MappingStatus: "host_filter"}
	if err = s.projectExternalMonitoring(ctx, tx, source, &externalmonitor.Batch{Signals: []externalmonitor.Signal{signal}}); err != nil {
		t.Fatal(err)
	}
	a := health.Assessment{RuleID: "interface.errors", RuleVersion: "1", Status: "warning", Severity: "warning", ObservedAt: &now, Evidence: []health.Point{}, Policy: health.Defaults()}
	raw, _ := json.Marshal(a)
	if err = applyIncident(ctx, tx, *bindings[1].LegacyAgentID, *bindings[1].LegacyDeviceID, "snmp", "", a, raw); err != nil {
		t.Fatal(err)
	}
	a.RuleID = "sip.availability"
	a.Status = "critical"
	a.Severity = "critical"
	raw, _ = json.Marshal(a)
	if err = applyResourceIncident(ctx, tx, voip.AgentID, "", voip.Source, "", "sip", voip.TargetID, health.SIPContext(voip.Address, voip.Transport), a, raw); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ProcessCorrelation(ctx); err != nil || n != 3 {
		t.Fatalf("project %d %v", n, err)
	}
	list, err := s.CorrelatedIncidents(ctx, correlation.Filter{Limit: 100})
	if err != nil || len(list) != 1 || list[0].SourceCount != 3 || list[0].SignalCount != 3 {
		t.Fatalf("multi-source grouping %+v %v", list, err)
	}
	card, err := s.CorrelatedIncident(ctx, list[0].ID)
	if err != nil || len(card.Evidence) == 0 {
		t.Fatal("evidence missing", err)
	}
	// B16 backend-only chain: three source projections -> one case -> reviewed
	// AI job -> validated controlled result -> immutable unified report.
	// This does not claim live Zabbix, SNMP hardware, PBX or LLM acceptance.
	cfg := analysisai.Config{Enabled: true, Model: "fixture-model", APIKey: "not-real", Timeout: time.Second}
	preview, err := s.CreateCorrelatedPreview(ctx, card.Incident.ID, cfg)
	if err != nil || len(preview.Payload.Facts) != 3 {
		t.Fatal("multi-source chain preview", preview, err)
	}
	body := analysisai.Create{PreviewID: preview.ID, RequestHash: preview.Hash}
	if _, _, err = s.CreateCorrelatedJob(ctx, card.Incident.ID, body, "b16-no-consent", cfg); err == nil {
		t.Fatal("chain bypassed AI consent")
	}
	body.Confirmed = true
	job, _, err := s.CreateCorrelatedJob(ctx, card.Incident.ID, body, "b16-reviewed", cfg)
	if err != nil {
		t.Fatal(err)
	}
	claimed, frozen, err := s.ClaimJob(ctx, cfg)
	if err != nil || claimed.ID != job.ID || frozen.Hash != preview.Hash {
		t.Fatal("chain claim", claimed, err)
	}
	result := &analysisai.Result{Summary: "Controlled backend fixture; not a live model.", Observations: []analysisai.Statement{{Text: "Three supplied signals share a reviewed service.", Refs: []string{frozen.Payload.Facts[0].Ref, frozen.Payload.Facts[1].Ref, frozen.Payload.Facts[2].Ref}}}, Hypotheses: []analysisai.Hypothesis{}, Recommendations: []analysisai.Recommendation{}, Limitations: []string{"No physical cause or live model is verified."}}
	if err = s.FinishJob(ctx, job.ID, result, "", false); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Job(ctx, job.ID)
	if err != nil || saved.State != "succeeded" || saved.Result == nil {
		t.Fatal("chain analysis result", saved, err)
	}
	filter := reports.V2Filter{From: now.Add(-time.Hour), To: time.Now().UTC(), Scope: "all"}
	snapshot, replay, err := s.CreateReportV2Snapshot(ctx, filter, "b16-chain-report")
	if err != nil || replay || snapshot.Report == nil || snapshot.Report.Stats.Cases != 1 || snapshot.Report.Stats.Signals != 3 {
		t.Fatal("chain unified report", snapshot, err)
	}
	again, replay, err := s.CreateReportV2Snapshot(ctx, filter, "b16-chain-report")
	if err != nil || !replay || again.ID != snapshot.ID {
		t.Fatal("chain duplicate report", again, err)
	}
}
