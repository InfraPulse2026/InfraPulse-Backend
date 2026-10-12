package storage

import (
	"errors"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assethardware"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func assetHardwareMeta(agent, target, resource string, at time.Time) *pb.ObservationMetadata {
	return &pb.ObservationMetadata{Source: &pb.SourceRef{Type: "redfish", CollectorId: agent, TargetId: target, Protocol: "redfish", InstanceId: "driver-1", Provenance: "real", ResourceId: resource}, Time: &pb.ObservationTime{ObservedAt: timestamppb.New(at), CollectedAt: timestamppb.New(at), ClockQuality: "unknown"}}
}
func assetHardwareTelemetry(agent, id, message, target string, at time.Time, value float64) *pb.AgentEnvelope {
	return &pb.AgentEnvelope{AgentId: agent, SessionId: "session-1", MessageId: message, Sequence: 10, SentAt: timestamppb.New(at), Payload: &pb.AgentEnvelope_Telemetry{Telemetry: &pb.TelemetryBatch{Samples: []*pb.TelemetrySample{{DeviceId: id, Source: "redfish", ObservedAt: timestamppb.New(at), CollectedAt: timestamppb.New(at), Observation: assetHardwareMeta(agent, target, "/Systems/1", at), HardwareHealth: []*pb.ComponentHealth{{Id: "fan-1", ResourceId: "/Fans/1", Name: "fan", Status: &pb.HardwareStatus{Health: "ok"}}}, Metrics: []*pb.Metric{{Name: "hardware.fan.rpm", ResourceId: "/Fans/1", ComponentId: "fan-1", Unit: "RPM", Value: value}}}}}}}
}
func hardwareAssetID(t *testing.T, s *Store, agent, id string) string {
	t.Helper()
	var asset string
	if err := s.pool.QueryRow(t.Context(), `SELECT asset_id FROM asset_source_bindings WHERE legacy_agent_id=$1 AND external_id=$2 AND scope=''`, agent, id).Scan(&asset); err != nil {
		t.Fatal(err)
	}
	return asset
}

func TestAssetHardwareZeroOffIsolationReplayChronologyAndOriginGuard(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, agent := range []string{"hardware-a", "hardware-b"} {
		if err := s.Ingest(ctx, registration(agent, now)); err != nil {
			t.Fatal(err)
		}
		zero := uint64(0)
		d := &pb.Device{Id: "system", Source: "redfish", Name: "System", Observation: assetHardwareMeta(agent, "target", "/Systems/1", now), Hardware: &pb.HardwareInventory{ResourceType: "System", PowerState: "Off", CapacityBytes: &zero, Status: &pb.HardwareStatus{Health: "ok"}}, Components: []*pb.InventoryComponent{{Id: "fan-1", Hardware: &pb.HardwareInventory{ResourceType: "Fan"}}}}
		if err := s.Ingest(ctx, assetInventory(agent, "session-1", "inventory", now, d)); err != nil {
			t.Fatal(err)
		}
		if err := s.Ingest(ctx, assetHardwareTelemetry(agent, "system", "metric-zero", "target", now, 0)); err != nil {
			t.Fatal(err)
		}
	}
	asset := hardwareAssetID(t, s, "hardware-a", "system")
	page, err := s.AssetHardware(ctx, asset, assethardware.Filter{})
	if err != nil || len(page.Items) != 4 {
		t.Fatal("current projection", page, err)
	}
	var metricKey string
	for _, item := range page.Items {
		if item.DeviceID != "system" || item.Related {
			t.Fatal("foreign asset included", item)
		}
		if item.Kind == "metric" {
			if item.Value == nil || *item.Value != 0 || item.Freshness != "fresh" {
				t.Fatal("zero lost", item)
			}
			metricKey = item.ObservationKey
		}
		if item.Kind == "inventory" && (item.PowerState != "Off" || item.Health != "ok") {
			t.Fatal("Off became failed", string(item.Payload))
		}
	}
	if metricKey == "" {
		t.Fatal("metric absent")
	}
	for _, e := range []*pb.AgentEnvelope{assetHardwareTelemetry("hardware-a", "system", "metric-zero", "target", now, 0), assetHardwareTelemetry("hardware-a", "system", "metric-old", "target", now.Add(-time.Minute), 55), assetHardwareTelemetry("hardware-a", "system", "metric-future", "target", now.Add(2*time.Minute), 99), assetHardwareTelemetry("hardware-a", "system", "metric-wrong-target", "other-target", now.Add(time.Second), 88)} {
		if err = s.Ingest(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	page, err = s.AssetHardware(ctx, asset, assethardware.Filter{Kind: "metric"})
	if err != nil || len(page.Items) != 1 || *page.Items[0].Value != 0 {
		t.Fatal("late/skew/foreign target corrupted current", page, err)
	}
	history, err := s.AssetHardwareHistory(ctx, asset, assethardware.HistoryFilter{ObservationKey: metricKey})
	if err != nil || len(history.Items) != 3 {
		t.Fatal("replay duplicated or foreign target attributed", history, err)
	}
	if history.Items[0].Value == nil || *history.Items[0].Value != 0 || history.Items[1].Value == nil || *history.Items[1].Value != 55 || history.Items[2].Freshness != "clock_skew" {
		t.Fatal("history clocks/order", history)
	}
	first, err := s.AssetHardwareHistory(ctx, asset, assethardware.HistoryFilter{ObservationKey: metricKey, Limit: 1})
	if err != nil || !first.HasMore || len(first.Items) != 1 {
		t.Fatal("cursor bounds", first, err)
	}
	second, err := s.AssetHardwareHistory(ctx, asset, assethardware.HistoryFilter{ObservationKey: metricKey, AfterID: first.NextAfterID, Limit: 2})
	if err != nil || second.HasMore || len(second.Items) != 2 {
		t.Fatal("cursor skipped data", second, err)
	}
	other := hardwareAssetID(t, s, "hardware-b", "system")
	otherPage, err := s.AssetHardware(ctx, other, assethardware.Filter{Kind: "metric"})
	if err != nil || len(otherPage.Items) != 1 || otherPage.Items[0].SourceID == page.Items[0].SourceID {
		t.Fatal("source instances collapsed", otherPage, err)
	}
	if _, err = s.AssetHardware(ctx, "missing", assethardware.Filter{}); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing asset wrong error", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE agents SET last_seen=now()-interval '2 days' WHERE agent_id='hardware-a'`); err != nil {
		t.Fatal(err)
	}
	page, err = s.AssetHardware(ctx, asset, assethardware.Filter{Kind: "metric"})
	if err != nil || page.Items[0].Freshness != "source_unavailable" || page.Items[0].AgentConnected {
		t.Fatal("stale connection flag reported as live", page, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE agents SET connected=false WHERE agent_id='hardware-a'`); err != nil {
		t.Fatal(err)
	}
	page, err = s.AssetHardware(ctx, asset, assethardware.Filter{Kind: "metric"})
	if err != nil || page.Items[0].Freshness != "source_unavailable" {
		t.Fatal("disconnected reading fresh", page, err)
	}
}

func TestAssetHardwareVerifiedScopedManagementRelation(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now()
	agent := "hardware-relations"
	if err := s.Ingest(ctx, registration(agent, now)); err != nil {
		t.Fatal(err)
	}
	system := &pb.Device{Id: "system", Name: "System", Source: "redfish", Observation: assetHardwareMeta(agent, "target", "/Systems/1", now), Hardware: &pb.HardwareInventory{ResourceType: "System", PowerState: "On"}}
	bmc := &pb.Device{Id: "manager", Name: "BMC", Source: "redfish", Observation: assetHardwareMeta(agent, "target", "/Managers/1", now), Hardware: &pb.HardwareInventory{ResourceType: "Manager", Relations: []*pb.ResourceRelationship{{Type: "BMC_MANAGES_SYSTEM", TargetResourceId: "/Systems/1"}}}}
	foreign := proto.Clone(system).(*pb.Device)
	foreign.Id = "foreign-system"
	foreign.Observation.Source.TargetId = "another-target"
	e := assetInventory(agent, "session-1", "related-inventory", now, system)
	e.GetInventory().Devices = []*pb.Device{system, bmc, foreign}
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, assetHardwareTelemetry(agent, "manager", "manager-metrics", "target", now, 1200)); err != nil {
		t.Fatal(err)
	}
	id := hardwareAssetID(t, s, agent, "system")
	own, err := s.AssetHardware(ctx, id, assethardware.Filter{})
	if err != nil || len(own.Items) != 1 {
		t.Fatal("own view", own, err)
	}
	page, err := s.AssetHardware(ctx, id, assethardware.Filter{IncludeRelated: true})
	if err != nil || len(page.Items) != 4 || page.RelationsTruncated {
		t.Fatal("related BMC data absent", page, err)
	}
	related := 0
	for _, item := range page.Items {
		if item.Related {
			related++
			if len(item.RelationIDs) != 1 || item.AssetID == id {
				t.Fatal("related data forged ownership", item)
			}
		}
	}
	if related != 3 {
		t.Fatal("related count", related)
	}
	foreignID := hardwareAssetID(t, s, agent, "foreign-system")
	page, err = s.AssetHardware(ctx, foreignID, assethardware.Filter{IncludeRelated: true})
	if err != nil || len(page.Items) != 1 || page.Items[0].Related {
		t.Fatal("resource URI crossed target namespace", page, err)
	}
}

func TestAssetHardwareMergeSplitFollowsSourceBindingsAndSeedIsIdempotent(t *testing.T) {
	s, ctx, items, bindings, _ := assetDecisionFixture(t)
	before, err := s.AssetHardware(ctx, items[0].ID, assethardware.Filter{})
	if err != nil || len(before.Items) != 1 {
		t.Fatal("fixture hardware", before, err)
	}
	decision, _, err := s.MergeAssets(ctx, assetMergeInput(items, bindings), "hardware-merge")
	if err != nil {
		t.Fatal(err)
	}
	merged, err := s.AssetHardware(ctx, decision.TargetID, assethardware.Filter{})
	if err != nil || len(merged.Items) != 2 {
		t.Fatal("merged data ownership", merged, err)
	}
	a, err := s.Asset(ctx, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	split, _, err := s.SplitAsset(ctx, assets.SplitInput{AssetID: a.ID, Revision: a.Revision, BindingIDs: []string{bindings[1].ID}, CanonicalBindingID: bindings[1].ID, Confirmed: true, Reason: "separate hardware observations"}, "hardware-split")
	if err != nil {
		t.Fatal(err)
	}
	separated, err := s.AssetHardware(ctx, split.TargetID, assethardware.Filter{})
	if err != nil || len(separated.Items) != 1 || separated.Items[0].BindingID != bindings[1].ID {
		t.Fatal("split history disappeared", separated, err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM asset_hardware_samples`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = seedAssetHardware(ctx, tx); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var after int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM asset_hardware_samples`).Scan(&after); err != nil || after != count {
		t.Fatal("seed duplicated accepted messages", count, after, err)
	}
}

func TestAssetHardwareProjectionFailureRollsBackInboxAndAsset(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now()
	agent := "hardware-atomic"
	if err := s.Ingest(ctx, registration(agent, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_hardware_sample() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture hardware failure'; END $$; CREATE TRIGGER reject_hardware_sample BEFORE INSERT ON asset_hardware_samples FOR EACH ROW EXECUTE FUNCTION reject_hardware_sample()`); err != nil {
		t.Fatal(err)
	}
	d := &pb.Device{Id: "atomic-hardware", Source: "redfish", Hardware: &pb.HardwareInventory{ResourceType: "System"}}
	if err := s.Ingest(ctx, assetInventory(agent, "session-1", "atomic-inventory", now, d)); err == nil {
		t.Fatal("failed durable projection acknowledged")
	}
	var inbox, asset, legacy bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_messages WHERE agent_id=$1 AND message_id='atomic-inventory'),EXISTS(SELECT 1 FROM asset_source_bindings WHERE legacy_agent_id=$1 AND external_id='atomic-hardware'),EXISTS(SELECT 1 FROM devices WHERE agent_id=$1 AND device_id='atomic-hardware')`, agent).Scan(&inbox, &asset, &legacy); err != nil || inbox || asset || legacy {
		t.Fatal("partial packet committed", inbox, asset, legacy, err)
	}
}

func TestAssetHardwareUpgradeSeedsCurrentOnlyAndKeepsOriginalReceiveTime(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	agent := "hardware-seed"
	if err := s.Ingest(ctx, registration(agent, now)); err != nil {
		t.Fatal(err)
	}
	d := &pb.Device{Id: "system", Source: "redfish", Observation: assetHardwareMeta(agent, "target", "/Systems/1", now), Hardware: &pb.HardwareInventory{ResourceType: "System", PowerState: "On"}}
	if err := s.Ingest(ctx, assetInventory(agent, "session-1", "inventory", now, d)); err != nil {
		t.Fatal(err)
	}
	for i, message := range []string{"history-one", "history-two", "latest"} {
		if err := s.Ingest(ctx, assetHardwareTelemetry(agent, "system", message, "target", now.Add(time.Duration(i)*time.Second), float64(i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Ingest(ctx, assetHardwareTelemetry(agent, "system", "future-skew", "target", now.Add(time.Hour), 999)); err != nil {
		t.Fatal(err)
	}
	var received time.Time
	if err := s.pool.QueryRow(ctx, `SELECT received_at FROM agent_messages WHERE agent_id=$1 AND message_id='latest'`, agent).Scan(&received); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM asset_hardware_latest;DELETE FROM asset_hardware_samples`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = seedAssetHardware(ctx, tx); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	id := hardwareAssetID(t, s, agent, "system")
	history, err := s.AssetHardwareHistory(ctx, id, assethardware.HistoryFilter{})
	if err != nil || len(history.Items) != 3 {
		t.Fatal("upgrade copied entire history or missed current", history, err)
	}
	page, err := s.AssetHardware(ctx, id, assethardware.Filter{Kind: "metric"})
	if err != nil || len(page.Items) != 1 || *page.Items[0].Value != 2 || !page.Items[0].ReceivedAt.Equal(received) {
		t.Fatal("seed fabricated current time or admitted skew", page, received, err)
	}
}
