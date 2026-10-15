package storage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assetvoip"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/locations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func voipEnvelope(agent, session, message string, at time.Time) *pb.AgentEnvelope {
	e := sipEnvelope(message, at)
	e.AgentId, e.SessionId = agent, session
	h := e.GetTelephony().Targets[0]
	h.State, h.ErrorCategory = "up", ""
	code := uint32(200)
	h.ResponseCode = &code
	return e
}

func assetVoIPFixture(t *testing.T) (*Store, context.Context, []assets.Asset, []assets.Binding, assetvoip.Input) {
	t.Helper()
	s, ctx, items, bindings, _ := assetDecisionFixture(t)
	e := voipEnvelope(*bindings[0].LegacyAgentID, "session-1", "voip-first", time.Now().UTC())
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	h := e.GetTelephony().Targets[0]
	in := assetvoip.Input{BindingID: bindings[0].ID, AgentID: e.AgentId, TargetID: h.TargetId, Address: h.Address, Transport: h.Transport, Source: h.Source, ServiceKind: "pbx", Name: "Main PBX", ExpectedAssetRevision: items[0].Revision, Enabled: true, Confirmed: true, Reason: "checked serial and equipment label"}
	return s, ctx, items, bindings, in
}

func TestAssetVoIPReviewedIdentityPlacementAndNullZero(t *testing.T) {
	s, ctx, items, _, in := assetVoIPFixture(t)
	_, rack := physicalFixture(t, s, ctx)
	if _, _, err := s.PlaceAssets(ctx, locations.BatchInput{Placements: []locations.PlacementInput{placeRequest(items[0].ID, rack.ID, 1, 2, 0)}, Reason: "PBX installed"}, "voip-place"); err != nil {
		t.Fatal(err)
	}
	association, replay, err := s.SaveAssetVoIP(ctx, items[0].ID, "", in, "map-pbx")
	if err != nil || replay {
		t.Fatal("review mapping", association, err)
	}
	old, replay, err := s.SaveAssetVoIP(ctx, items[0].ID, "", in, "map-pbx")
	if err != nil || !replay || old.ID != association.ID {
		t.Fatal("idempotent mapping", old, err)
	}
	changed := in
	changed.Name = "different request"
	if _, _, err = s.SaveAssetVoIP(ctx, items[0].ID, "", changed, "map-pbx"); !errors.Is(err, assetvoip.ErrConflict) {
		t.Fatal("different idempotent input accepted", err)
	}
	card, err := s.AssetVoIP(ctx, items[0].ID, 50, 0)
	if err != nil || len(card.Items) != 1 || card.TopologyNodeID != items[0].ID || card.Placement.Mode != "rack" || len(card.Placement.Path) != 4 || card.Placement.Path[0].Kind != "site" {
		t.Fatal("physical context", card, err)
	}
	service := card.Items[0]
	if service.MappingStatus != "active" || service.Health.Status != "up" || service.Latest == nil || service.Latest.LatencyMS == nil || *service.Latest.LatencyMS != 0 {
		t.Fatal("measured zero or health lost", service)
	}
	failed := voipEnvelope(in.AgentID, "session-1", "voip-down", time.Now().Add(time.Second))
	h := failed.GetTelephony().Targets[0]
	h.Reachable, h.State, h.ErrorCategory = false, "down", "timeout"
	h.ResponseCode, h.LatencyMs = nil, nil
	if err = s.Ingest(ctx, failed); err != nil {
		t.Fatal(err)
	}
	card, err = s.AssetVoIP(ctx, items[0].ID, 50, 0)
	if err != nil || card.Items[0].Health.Status != "down" || card.Items[0].Latest.LatencyMS != nil || card.Items[0].Latest.ResponseCode != nil {
		t.Fatal("missing latency became zero", card, err)
	}
	history, err := s.AssetVoIPHistory(ctx, items[0].ID, association.ID, assetvoip.HistoryFilter{Limit: 1})
	if err != nil || len(history.Items) != 1 || history.Items[0].MessageID != "voip-down" || history.NextBeforeID == nil {
		t.Fatal("history cursor", history, err)
	}
	next, err := s.AssetVoIPHistory(ctx, items[0].ID, association.ID, assetvoip.HistoryFilter{Limit: 1, BeforeID: *history.NextBeforeID})
	if err != nil || len(next.Items) != 1 || next.Items[0].MessageID != "voip-first" || next.Items[0].LatencyMS == nil || *next.Items[0].LatencyMS != 0 {
		t.Fatal("second history page", next, err)
	}
	if _, err = s.AssetVoIPHistory(ctx, items[1].ID, association.ID, assetvoip.HistoryFilter{Limit: 10}); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign Asset history exposed", err)
	}
	var events, audit int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM asset_voip_events`).Scan(&events); err != nil || events != 1 {
		t.Fatal("replay duplicated change event", events, err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM access_audit WHERE action='asset_voip_saved'`).Scan(&audit); err != nil || audit != 1 {
		t.Fatal("audit not atomic", audit, err)
	}
}

func TestAssetVoIPEndpointChangeRequiresReviewAndPreservesLegacy(t *testing.T) {
	s, ctx, items, bindings, in := assetVoIPFixture(t)
	association, _, err := s.SaveAssetVoIP(ctx, items[0].ID, "", in, "map-pbx")
	if err != nil {
		t.Fatal(err)
	}
	changed := voipEnvelope(in.AgentID, "session-1", "endpoint-change", time.Now().Add(time.Second))
	changed.GetTelephony().Targets[0].Address = "different.example:5060"
	changed.GetTelephony().Targets[0].Transport = "udp"
	if err = s.Ingest(ctx, changed); err != nil {
		t.Fatal(err)
	}
	card, err := s.AssetVoIP(ctx, items[0].ID, 50, 0)
	if err != nil || card.Items[0].MappingStatus != "review_required" || card.Items[0].Latest != nil || card.Items[0].Health.Status != "stale" || card.Items[0].ObservedEndpoint.Address != "different.example:5060" {
		t.Fatal("unreviewed endpoint attributed to hardware", card, err)
	}
	legacy, err := s.TelephonyTarget(ctx, in.AgentID, in.TargetID)
	if err != nil || legacy.Address != "different.example:5060" || legacy.Health.Status != "up" {
		t.Fatal("association changed legacy SIP view", legacy, err)
	}
	history, err := s.AssetVoIPHistory(ctx, items[0].ID, association.ID, assetvoip.HistoryFilter{Limit: 10})
	if err != nil || len(history.Items) != 1 || history.Items[0].Address != in.Address {
		t.Fatal("history included changed endpoint", history, err)
	}
	foreign := in
	foreign.BindingID = bindings[1].ID
	if _, _, err = s.SaveAssetVoIP(ctx, items[0].ID, "", foreign, "foreign-binding"); !errors.Is(err, assetvoip.ErrConflict) {
		t.Fatal("foreign source binding accepted", err)
	}
	in.ExpectedRevision, in.Enabled = association.Revision, false
	disabled, _, err := s.SaveAssetVoIP(ctx, items[0].ID, association.ID, in, "disable-pbx")
	if err != nil || disabled.Enabled {
		t.Fatal("changed endpoint cannot be disabled", disabled, err)
	}
	in.ExpectedRevision, in.Enabled = disabled.Revision, true
	in.Address, in.Transport = changed.GetTelephony().Targets[0].Address, changed.GetTelephony().Targets[0].Transport
	updated, _, err := s.SaveAssetVoIP(ctx, items[0].ID, association.ID, in, "review-new-endpoint")
	if err != nil || updated.Revision != 3 {
		t.Fatal("review new endpoint", updated, err)
	}
	card, err = s.AssetVoIP(ctx, items[0].ID, 50, 0)
	if err != nil || card.Items[0].MappingStatus != "active" || card.Items[0].Latest.MessageID != "endpoint-change" {
		t.Fatal("reviewed endpoint did not resume", card, err)
	}
	changes, err := s.AssetVoIPEvents(ctx, items[0].ID, association.ID, 50, 0)
	if err != nil || len(changes) != 3 || changes[0].Before.Address == changes[0].After.Address || changes[2].Before != nil {
		t.Fatal("review events lost endpoint history", changes, err)
	}
	in.ExpectedRevision = 1
	if _, _, err = s.SaveAssetVoIP(ctx, items[0].ID, association.ID, in, "stale-update"); !errors.Is(err, assetvoip.ErrConflict) {
		t.Fatal("stale association revision accepted", err)
	}
}

func TestAssetVoIPMappingFollowsMergeAndSplitBinding(t *testing.T) {
	s, ctx, items, bindings, in := assetVoIPFixture(t)
	association, _, err := s.SaveAssetVoIP(ctx, items[0].ID, "", in, "map-pbx")
	if err != nil {
		t.Fatal(err)
	}
	merge := assets.MergeInput{TargetID: items[1].ID, TargetRevision: items[1].Revision, SourceID: items[0].ID, SourceRevision: items[0].Revision, CanonicalBindingID: bindings[1].ID, Confirmed: true, Reason: "checked physical serial"}
	if _, _, err = s.MergeAssets(ctx, merge, "merge-with-voip"); err != nil {
		t.Fatal(err)
	}
	card, err := s.AssetVoIP(ctx, items[1].ID, 50, 0)
	if err != nil || len(card.Items) != 1 || card.Items[0].AssetID != items[1].ID || card.Items[0].ID != association.ID {
		t.Fatal("merge lost reviewed service", card, err)
	}
	if _, err = s.AssetVoIPHistory(ctx, items[0].ID, association.ID, assetvoip.HistoryFilter{Limit: 10}); !errors.Is(err, ErrNotFound) {
		t.Fatal("merged alias kept service attribution", err)
	}
	a, err := s.Asset(ctx, items[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	split, _, err := s.SplitAsset(ctx, assets.SplitInput{AssetID: a.ID, Revision: a.Revision, BindingIDs: []string{bindings[0].ID}, CanonicalBindingID: bindings[0].ID, Confirmed: true, Reason: "split after hardware review"}, "split-with-voip")
	if err != nil {
		t.Fatal(err)
	}
	card, err = s.AssetVoIP(ctx, split.TargetID, 50, 0)
	if err != nil || len(card.Items) != 1 || card.Items[0].AssetID != split.TargetID || card.Items[0].Health.Status != "up" {
		t.Fatal("split service followed name instead of owned binding", card, err)
	}
	old, err := s.AssetVoIP(ctx, items[1].ID, 50, 0)
	if err != nil || len(old.Items) != 0 {
		t.Fatal("old Asset retained moved binding service", old, err)
	}
	changes, err := s.AssetVoIPEvents(ctx, split.TargetID, association.ID, 50, 0)
	if err != nil || len(changes) != 1 || changes[0].After.AssetID != items[0].ID {
		t.Fatal("immutable event falsely rewritten on merge/split", changes, err)
	}
}

func TestAssetVoIPTypedSimulationAndRealScopeDoNotMix(t *testing.T) {
	s, ctx, items, bindings, realInput := assetVoIPFixture(t)
	realMap, _, err := s.SaveAssetVoIP(ctx, items[0].ID, "", realInput, "real-pbx")
	if err != nil {
		t.Fatal(err)
	}
	simAssets, err := s.Assets(ctx, "", "simulated", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	var simAsset assets.Asset
	var simBinding assets.Binding
	for _, a := range simAssets {
		bs, e := s.AssetBindings(ctx, a.ID, false, 10, 0)
		if e != nil {
			t.Fatal(e)
		}
		if *bs[0].LegacyAgentID == realInput.AgentID {
			simAsset, simBinding = a, bs[0]
		}
	}
	if simAsset.ID == "" {
		t.Fatal("simulated source binding missing")
	}
	sim := voipEnvelope(realInput.AgentID, "session-1", "typed-simulated-sip", time.Now().Add(time.Second))
	h := sim.GetTelephony().Targets[0]
	h.Observation = &pb.ObservationMetadata{Source: &pb.SourceRef{Type: "sip", CollectorId: realInput.AgentID, TargetId: h.TargetId, Provenance: "simulated"}, Time: &pb.ObservationTime{ObservedAt: h.ObservedAt, CollectedAt: h.ObservedAt, ClockQuality: "unknown"}}
	h.Address = "simulated.example:5060"
	h.LatencyMs = nil
	if err = s.Ingest(ctx, sim); err != nil {
		t.Fatal("typed simulation ingest", err)
	}
	simInput := realInput
	simInput.BindingID, simInput.Scope = simBinding.ID, "session-1"
	simInput.Address, simInput.ExpectedAssetRevision = h.Address, simAsset.Revision
	simMap, _, err := s.SaveAssetVoIP(ctx, simAsset.ID, "", simInput, "simulated-pbx")
	if err != nil {
		t.Fatal("typed scoped association", err)
	}
	card, err := s.AssetVoIP(ctx, items[0].ID, 50, 0)
	if err != nil || card.Items[0].MappingStatus != "active" || card.Items[0].Latest.MessageID != "voip-first" || card.Items[0].Address == h.Address {
		t.Fatal("simulated observation overwrote real Asset health", card, err)
	}
	simCard, err := s.AssetVoIP(ctx, simAsset.ID, 50, 0)
	if err != nil || simCard.Items[0].Latest == nil || simCard.Items[0].Latest.MessageID != sim.MessageId || simCard.Items[0].Scope != "session-1" {
		t.Fatal("simulation attribution", simCard, err)
	}
	for _, mapping := range []assetvoip.Association{realMap, simMap} {
		history, e := s.AssetVoIPHistory(ctx, mapping.AssetID, mapping.ID, assetvoip.HistoryFilter{Limit: 20})
		if e != nil || len(history.Items) != 1 {
			t.Fatal("scoped history mixed observations", history, e)
		}
	}
	foreign := simInput
	foreign.BindingID, foreign.ExpectedAssetRevision = bindings[0].ID, items[0].Revision
	if _, _, err = s.SaveAssetVoIP(ctx, items[0].ID, "", foreign, "simulation-to-real"); !errors.Is(err, assetvoip.ErrConflict) {
		t.Fatal("simulation association linked real binding", err)
	}
	// Batch-level typed provenance is authoritative too, even when target's
	// source string and own metadata appear real.
	batch := proto.Clone(sim).(*pb.AgentEnvelope)
	batch.MessageId = "batch-simulated-sip"
	batch.GetTelephony().Targets[0].Observation.Source.Provenance = "real"
	batch.GetTelephony().Targets[0].ObservedAt = timestamppb.New(time.Now().Add(2 * time.Second))
	batch.GetTelephony().Observation = proto.Clone(h.Observation).(*pb.ObservationMetadata)
	batch.GetTelephony().Observation.Source.Provenance = "simulated"
	if err = s.Ingest(ctx, batch); err != nil {
		t.Fatal(err)
	}
	card, err = s.AssetVoIP(ctx, items[0].ID, 50, 0)
	if err != nil || card.Items[0].Latest.MessageID != "voip-first" {
		t.Fatal("batch provenance simulation leaked", card, err)
	}
	// A new Collector session must not turn old simulation into current health.
	reconnect := registration(realInput.AgentID, time.Now().Add(3*time.Second))
	reconnect.SessionId, reconnect.MessageId = "session-2", "reconnect-voip"
	reconnect.GetHello().SessionId = "session-2"
	if err = s.Ingest(ctx, reconnect); err != nil {
		t.Fatal(err)
	}
	simCard, err = s.AssetVoIP(ctx, simAsset.ID, 50, 0)
	if err != nil || simCard.Items[0].MappingStatus != "session_expired" || simCard.Items[0].Latest != nil || simCard.Items[0].Health.Status != "stale" {
		t.Fatal("old simulation remained current", simCard, err)
	}
}

func TestAssetVoIPAtomicAuditLiveAdminAndConcurrentRevision(t *testing.T) {
	s, ctx, items, _, in := assetVoIPFixture(t)
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_voip_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='asset_voip_saved' THEN RAISE EXCEPTION 'fixture audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_voip_audit BEFORE INSERT ON access_audit FOR EACH ROW EXECUTE FUNCTION reject_voip_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SaveAssetVoIP(ctx, items[0].ID, "", in, "failed-map"); err == nil {
		t.Fatal("audit failure committed association")
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM asset_voip_associations`).Scan(&count); err != nil || count != 0 {
		t.Fatal("mapping survived rollback", count, err)
	}
	if _, err := s.pool.Exec(ctx, `DROP TRIGGER reject_voip_audit ON access_audit`); err != nil {
		t.Fatal(err)
	}
	mapping, _, err := s.SaveAssetVoIP(ctx, items[0].ID, "", in, "map-pbx")
	if err != nil {
		t.Fatal(err)
	}
	in.ExpectedRevision = mapping.Revision
	in.Enabled = false
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, e := s.SaveAssetVoIP(ctx, items[0].ID, mapping.ID, in, fmt.Sprintf("concurrent-disable-%d", i))
			errs <- e
		}(i)
	}
	wg.Wait()
	close(errs)
	var success, conflict int
	for e := range errs {
		if e == nil {
			success++
		} else if errors.Is(e, assetvoip.ErrConflict) {
			conflict++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("concurrent stale edits both succeeded", success, conflict)
	}
	actor, _ := access.Current(ctx)
	if err = s.AccessLogout(ctx, actor, false, access.Meta{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.SaveAssetVoIP(ctx, items[0].ID, "", in, "after-logout"); !errors.Is(err, assetvoip.ErrInvalid) {
		// Create input with nonzero association revision is malformed before
		// auth. Use a valid update below to test live session revocation.
		t.Fatal("invalid create unexpectedly accepted", err)
	}
	in.ExpectedRevision = 2
	if _, _, err = s.SaveAssetVoIP(ctx, items[0].ID, mapping.ID, in, "after-logout-update"); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("revoked admin mapping succeeded", err)
	}
}

func TestAssetVoIPExplicitCrossSourceInventoryAssociation(t *testing.T) {
	s, ctx, _, bindings, in := assetVoIPFixture(t)
	noop := func(context.Context, datasources.Request) (datasources.Result, error) {
		return datasources.Result{}, nil
	}
	registry, err := datasources.NewRegistry(datasources.Operation{Kind: "ZABBIX", Name: "sync", ReadOnly: true, Run: noop})
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureDataSourceOperations(registry)
	source, err := s.SaveDataSource(ctx, "", datasources.Input{Type: "ZABBIX", Name: "PBX inventory", Endpoint: "https://fixture.invalid", Enabled: true, SyncIntervalSeconds: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.QueueDataSourceJob(ctx, source.ID, "sync", "publish-pbx-inventory"); err != nil {
		t.Fatal(err)
	}
	request := claimSourceJob(t, s, ctx)
	now := time.Now()
	d := &pb.Device{Id: "zabbix-pbx-42", Type: pb.DeviceType_DEVICE_TYPE_PBX, Name: "PBX known only to Zabbix", Source: "zabbix"}
	if err = s.FinishDataSourceJob(ctx, *request, datasources.Result{Observations: []assets.Observation{{ExternalType: "host", ExternalID: d.Id, ObservedAt: now, Provenance: "external", Device: d}}}, nil); err != nil {
		t.Fatal(err)
	}
	items, err := s.Assets(ctx, "host", "real", 20, 0)
	if err != nil || len(items) != 1 {
		t.Fatal("external PBX inventory", items, err)
	}
	bs, err := s.AssetBindings(ctx, items[0].ID, false, 10, 0)
	if err != nil || len(bs) != 1 || bs[0].LegacyAgentID != nil {
		t.Fatal("external inventory invented Collector device", bs, err)
	}
	in.BindingID, in.ExpectedAssetRevision = bs[0].ID, items[0].Revision
	association, _, err := s.SaveAssetVoIP(ctx, items[0].ID, "", in, "explicit-cross-source-pbx")
	if err != nil || association.InventorySourceInstanceID != source.ID || association.SourceInstanceID != bindings[0].SourceInstanceID || association.SourceInstanceID == source.ID {
		t.Fatal("probing Collector and inventory source conflated", association, err)
	}
	card, err := s.AssetVoIP(ctx, items[0].ID, 50, 0)
	if err != nil || len(card.Items) != 1 || card.Items[0].MappingStatus != "active" || card.Items[0].Latest.AgentID != in.AgentID || card.Items[0].Health.Status != "up" {
		t.Fatal("explicit cross-source service unavailable", card, err)
	}
	var deviceCount int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM devices WHERE device_id=$1`, d.Id).Scan(&deviceCount); err != nil || deviceCount != 0 {
		t.Fatal("association manufactured legacy physical device", deviceCount, err)
	}
}
