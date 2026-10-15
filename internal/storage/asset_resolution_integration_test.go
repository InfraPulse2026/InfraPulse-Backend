package storage

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"google.golang.org/protobuf/proto"
)

func autoNetworkDevice(id string) *pb.Device {
	return &pb.Device{Id: id, Name: "Eltex switch", Vendor: "Eltex", Model: "MES2324B", SerialNumber: "ELTEX-SERIAL-1", Type: pb.DeviceType_DEVICE_TYPE_SWITCH, Source: "snmp", IdentityEvidence: []*pb.IdentityEvidence{{Type: "serial", Value: "ELTEX-SERIAL-1", Confidence: "high", Source: &pb.SourceRef{Type: "snmp", Provenance: "real"}}}}
}

func TestAssetAutoResolutionTwoCollectorsAndExternalSource(t *testing.T) {
	s := integrationStore(t)
	s.ConfigureAssetResolution(true, 24*time.Hour)
	_, admin := adminFixture(t, s)
	ctx := access.WithPrincipal(t.Context(), admin)
	now := time.Now()
	for _, agent := range []string{"collector-a", "collector-b"} {
		if err := s.Ingest(ctx, registration(agent, now)); err != nil {
			t.Fatal(err)
		}
		if err := s.Ingest(ctx, assetInventory(agent, "session-1", "switch", now, autoNetworkDevice(agent+"-switch"))); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.Assets(ctx, "network_device", "real", 50, 0)
	if err != nil || len(items) != 1 || items[0].SourceCount != 2 {
		t.Fatal("collectors not resolved", items, err)
	}
	registry, err := datasources.NewRegistry(datasources.Operation{Kind: "ZABBIX", Name: "sync", ReadOnly: true, Run: func(_ context.Context, _ datasources.Request) (datasources.Result, error) {
		return datasources.Result{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureDataSourceOperations(registry)
	source, err := s.SaveDataSource(ctx, "", datasources.Input{Type: "ZABBIX", Name: "external fixture", Endpoint: "https://fixture.invalid", Enabled: true, SyncIntervalSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.QueueDataSourceJob(ctx, source.ID, "sync", "external-observation"); err != nil {
		t.Fatal(err)
	}
	request := claimSourceJob(t, s, ctx)
	d := autoNetworkDevice("host-42")
	d.Source = "external"
	d.IdentityEvidence[0].Source = &pb.SourceRef{Type: "zabbix", Provenance: "external"}
	result := datasources.Result{Cursor: json.RawMessage(`{"host_id":"42"}`), Observations: []assets.Observation{{ExternalType: "host", ExternalID: d.Id, ObservedAt: now, Provenance: "external", Device: d}}}
	if err = s.FinishDataSourceJob(ctx, *request, result, nil); err != nil {
		t.Fatal(err)
	}
	items, err = s.Assets(ctx, "network_device", "real", 50, 0)
	if err != nil || len(items) != 1 || items[0].SourceCount != 3 {
		t.Fatal("three sources not one asset", items, err)
	}
	bindings, err := s.AssetBindings(ctx, items[0].ID, true, 50, 0)
	if err != nil || len(bindings) != 3 {
		t.Fatal(err)
	}
	external := false
	for _, b := range bindings {
		if b.SourceType == "ZABBIX" {
			external = true
			if b.LegacyAgentID != nil || b.ExternalID != "host-42" {
				t.Fatal("external observation invented Agent", b)
			}
		}
	}
	if !external {
		t.Fatal("external binding missing")
	}
	events, err := s.AssetResolutionEvents(ctx, items[0].ID, 50, 0)
	if err != nil || len(events) != 2 || events[0].FactID == 0 || len(events[0].TargetFactIDs) == 0 {
		t.Fatal("resolution evidence missing", events, err)
	}
	if _, err = s.Device(ctx, "collector-a", "collector-a-switch"); err != nil {
		t.Fatal("legacy API broken", err)
	}
	move := bindings[0]
	for _, b := range bindings {
		if b.LegacyAgentID != nil && *b.LegacyAgentID == "collector-b" {
			move = b
		}
	}
	decision, _, err := s.SplitAsset(ctx, assets.SplitInput{AssetID: items[0].ID, Revision: items[0].Revision, BindingIDs: []string{move.ID}, CanonicalBindingID: move.ID, Confirmed: true, Reason: "separate for verification"}, "split-automatic")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Ingest(ctx, assetInventory("collector-b", "session-1", "after-split", now.Add(time.Second), autoNetworkDevice("collector-b-switch"))); err != nil {
		t.Fatal(err)
	}
	newAsset, err := s.Asset(ctx, decision.TargetID)
	if err != nil || newAsset.SourceCount != 1 {
		t.Fatal("automatic resolver undid split", newAsset, err)
	}
	if err = s.Ingest(ctx, registration("collector-c", now)); err != nil {
		t.Fatal(err)
	}
	if err = s.Ingest(ctx, assetInventory("collector-c", "session-1", "ambiguous", now, autoNetworkDevice("collector-c-switch"))); err != nil {
		t.Fatal(err)
	}
	items, err = s.Assets(ctx, "network_device", "real", 50, 0)
	if err != nil || len(items) != 3 {
		t.Fatal("ambiguous match guessed a target", items, err)
	}
}

func TestExternalAssetProjectionAndCursorCommitTogether(t *testing.T) {
	s, ctx, source := sourceQueueFixture(t)
	if _, _, err := s.QueueDataSourceJob(ctx, source.ID, "sync", "atomic-observation"); err != nil {
		t.Fatal(err)
	}
	request := claimSourceJob(t, s, ctx)
	d := autoNetworkDevice("external-1")
	result := datasources.Result{Cursor: json.RawMessage(`{"position":1}`), Observations: []assets.Observation{{ExternalType: "host", ExternalID: d.Id, ObservedAt: time.Now(), Provenance: "external", Device: d}}}
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_external_fact() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture failure'; END $$; CREATE TRIGGER reject_external_fact BEFORE INSERT ON asset_fact_versions FOR EACH ROW EXECUTE FUNCTION reject_external_fact()`); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDataSourceJob(ctx, *request, result, nil); err == nil {
		t.Fatal("projection failure acknowledged sync")
	}
	var cursor string
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT sync_cursor::text FROM data_sources WHERE id=$1`, source.ID).Scan(&cursor); err != nil || cursor != "{}" {
		t.Fatal("cursor advanced", cursor, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM asset_source_bindings WHERE source_instance_id=$1`, source.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial binding committed", count, err)
	}
	j, err := s.DataSourceJob(ctx, source.ID, request.Job.ID)
	if err != nil || j.State != "running" {
		t.Fatal("job terminal before projection commit", j, err)
	}
	if _, err = s.pool.Exec(ctx, `DROP TRIGGER reject_external_fact ON asset_fact_versions`); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishDataSourceJob(ctx, *request, result, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishDataSourceJob(ctx, *request, result, nil); err != datasources.ErrLease {
		t.Fatal("duplicate job republished data", err)
	}
	var actors int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM agents`).Scan(&actors); err != nil || actors != 0 {
		t.Fatal("external source created Agent", actors, err)
	}
}

func TestAssetBackfillPreservesSessionsAndOriginalScope(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now()
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	next := proto.Clone(registration("agent-1", now.Add(time.Second))).(*pb.AgentEnvelope)
	next.SessionId = "session-2"
	next.MessageId = "hello-session-2"
	next.GetHello().SessionId = "session-2"
	if err := s.Ingest(ctx, next); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM asset_resolution_events; UPDATE assets SET canonical_binding_id=NULL,merged_into_id=NULL; UPDATE asset_source_bindings SET latest_fact_id=NULL; DELETE FROM asset_identity_evidence; DELETE FROM asset_fact_versions; DELETE FROM asset_source_bindings; DELETE FROM assets; DELETE FROM asset_projection_marks`); err != nil {
		t.Fatal(err)
	}
	count, err := s.PendingAssetBackfill(ctx)
	if err != nil || count != 2 {
		t.Fatal("preview", count, err)
	}
	out, err := s.BackfillAssets(ctx, 50)
	if err != nil || out.Processed != 2 || out.Failed != 0 || out.Remaining != 0 {
		t.Fatal("backfill", out, err)
	}
	items, err := s.Assets(ctx, "", "simulated", 50, 0)
	if err != nil || len(items) != 2 || items[0].Scope == items[1].Scope {
		t.Fatal("archived sessions conflated", items, err)
	}
	var connected bool
	var session string
	if err = s.pool.QueryRow(ctx, `SELECT connected,session_id FROM agents WHERE agent_id='agent-1'`).Scan(&connected, &session); err != nil || !connected || session != "session-2" {
		t.Fatal("backfill reset active Agent", connected, session, err)
	}
	again, err := s.BackfillAssets(ctx, 50)
	if err != nil || again.Scanned != 0 {
		t.Fatal("backfill repeated archived projections", again, err)
	}
}

func TestAssetResolverConflictAndStaleEvidenceStaySeparate(t *testing.T) {
	s := integrationStore(t)
	s.ConfigureAssetResolution(true, time.Minute)
	ctx := t.Context()
	now := time.Now()
	for _, agent := range []string{"old", "fresh"} {
		if err := s.Ingest(ctx, registration(agent, now)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Ingest(ctx, assetInventory("old", "session-1", "old-switch", now.Add(-2*time.Minute), autoNetworkDevice("switch-old"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, assetInventory("fresh", "session-1", "fresh-switch", now, autoNetworkDevice("switch-fresh"))); err != nil {
		t.Fatal(err)
	}
	items, err := s.Assets(ctx, "network_device", "real", 50, 0)
	if err != nil || len(items) != 2 {
		t.Fatal("stale evidence automatically merged", items, err)
	}
	if err = s.Ingest(ctx, assetInventory("old", "session-1", "now-fresh", now, autoNetworkDevice("switch-old"))); err != nil {
		t.Fatal(err)
	}
	items, err = s.Assets(ctx, "network_device", "real", 50, 0)
	if err != nil || len(items) != 1 {
		t.Fatal("fresh corroboration did not resolve", items, err)
	}
	if err = s.Ingest(ctx, registration("contradictory", now)); err != nil {
		t.Fatal(err)
	}
	bad := autoNetworkDevice("wrong-switch")
	bad.Vendor = "different manufacturer"
	if err = s.Ingest(ctx, assetInventory("contradictory", "session-1", "conflict", now, bad)); err != nil {
		t.Fatal(err)
	}
	items, err = s.Assets(ctx, "network_device", "real", 50, 0)
	if err != nil || len(items) != 2 {
		t.Fatal("manufacturer contradiction merged", items, err)
	}
}

func TestExternalInvalidOrCancelledResultCannotPublishAssets(t *testing.T) {
	s, ctx, source := sourceQueueFixture(t)
	if _, _, err := s.QueueDataSourceJob(ctx, source.ID, "sync", "invalid-result"); err != nil {
		t.Fatal(err)
	}
	r := claimSourceJob(t, s, ctx)
	d := autoNetworkDevice("external")
	d.Components = []*pb.InventoryComponent{nil}
	result := datasources.Result{Cursor: json.RawMessage(`{"position":9}`), Observations: []assets.Observation{{ExternalType: "host", ExternalID: d.Id, ObservedAt: time.Now(), Provenance: "external", Device: d}}}
	if err := s.FinishDataSourceJob(ctx, *r, result, nil); err != nil {
		t.Fatal(err)
	}
	j, err := s.DataSourceJob(ctx, source.ID, r.Job.ID)
	if err != nil || j.State != "failed" || j.Code != "invalid_response" {
		t.Fatal("invalid adapter input accepted", j, err)
	}
	if _, _, err = s.QueueDataSourceJob(ctx, source.ID, "sync", "obsolete-result"); err != nil {
		t.Fatal(err)
	}
	r = claimSourceJob(t, s, ctx)
	if _, err = s.SaveDataSource(ctx, source.ID, datasources.Input{Type: source.Type, Name: "new settings", Endpoint: source.Endpoint, Enabled: true, SyncIntervalSeconds: 30, Revision: source.Revision}); err != nil {
		t.Fatal(err)
	}
	d = autoNetworkDevice("external")
	result.Observations[0].Device = d
	if err = s.FinishDataSourceJob(ctx, *r, result, nil); err != nil {
		t.Fatal(err)
	}
	j, err = s.DataSourceJob(ctx, source.ID, r.Job.ID)
	if err != nil || j.State != "cancelled" {
		t.Fatal(j, err)
	}
	items, err := s.Assets(ctx, "network_device", "real", 50, 0)
	if err != nil || len(items) != 0 {
		t.Fatal("invalid/obsolete result published inventory", items, err)
	}
	var cursor string
	if err = s.pool.QueryRow(ctx, `SELECT sync_cursor::text FROM data_sources WHERE id=$1`, source.ID).Scan(&cursor); err != nil || cursor != "{}" {
		t.Fatal("invalid/obsolete result advanced cursor", cursor, err)
	}
}
