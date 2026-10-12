package storage

import (
	"strings"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func assetInventory(agent, session, message string, now time.Time, device *pb.Device) *pb.AgentEnvelope {
	return &pb.AgentEnvelope{AgentId: agent, SessionId: session, MessageId: message, Sequence: 2, SentAt: timestamppb.New(now), Payload: &pb.AgentEnvelope_Inventory{Inventory: &pb.InventoryUpdate{ObservedAt: timestamppb.New(now), Devices: []*pb.Device{device}}}}
}

func TestAssetRegistrySourceIsolationHistoryReplayAndSubjects(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, agent := range []string{"collector-a", "collector-b"} {
		if err := s.Ingest(ctx, registration(agent, now)); err != nil {
			t.Fatal(err)
		}
	}
	device := &pb.Device{Id: "system-1", Name: "same-name", Hostname: "same-host", ManagementAddress: "10.0.0.1", Source: "redfish", Vendor: "Eltex", SerialNumber: "SERIAL-1", Hardware: &pb.HardwareInventory{ResourceType: "system"}, Components: []*pb.InventoryComponent{{Id: "drive", SerialNumber: "DRIVE-1"}}, IdentityEvidence: []*pb.IdentityEvidence{{Type: "serial", Value: "SERIAL-1", Confidence: "high"}, {Type: "serial", Value: "DRIVE-1", Confidence: "high", SubjectId: "drive"}}}
	for _, identity := range device.IdentityEvidence {
		identity.Source = &pb.SourceRef{Type: "redfish", Provenance: "real"}
	}
	for _, agent := range []string{"collector-a", "collector-b"} {
		if err := s.Ingest(ctx, assetInventory(agent, "session-1", "inventory-1", now, device)); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.Assets(ctx, "system", "real", 50, 0)
	if err != nil || len(items) != 2 {
		t.Fatal("cross-source/name/IP merge", len(items), err)
	}
	a := items[0]
	bindings, err := s.AssetBindings(ctx, a.ID, true, 50, 0)
	if err != nil || len(bindings) != 1 || bindings[0].SourceInstanceID == "" || bindings[0].ExternalID != "system-1" {
		t.Fatal("source provenance lost", bindings, err)
	}
	owner := *bindings[0].LegacyAgentID
	replay := assetInventory(owner, "session-1", "inventory-1", now, device)
	replay.Sequence = 80
	if err = s.Ingest(ctx, replay); err != nil {
		t.Fatal(err)
	}
	clock := proto.Clone(device).(*pb.Device)
	clock.LastSeen = timestamppb.New(now.Add(time.Second))
	clock.UptimeSeconds = 3
	if err = s.Ingest(ctx, assetInventory(owner, "session-1", "inventory-same", now.Add(time.Second), clock)); err != nil {
		t.Fatal(err)
	}
	facts, err := s.AssetFacts(ctx, a.ID, 50, 0)
	if err != nil || len(facts) != 1 {
		t.Fatal("same facts duplicated on replay/clocks", len(facts), err)
	}
	change := proto.Clone(device).(*pb.Device)
	change.SerialNumber = "SERIAL-2"
	change.IdentityEvidence[0].Value = "SERIAL-2"
	if err = s.Ingest(ctx, assetInventory(owner, "session-1", "inventory-change", now.Add(2*time.Second), change)); err != nil {
		t.Fatal(err)
	}
	if err = s.Ingest(ctx, assetInventory(owner, "session-1", "inventory-return", now.Add(3*time.Second), device)); err != nil {
		t.Fatal(err)
	}
	facts, err = s.AssetFacts(ctx, a.ID, 50, 0)
	if err != nil || len(facts) != 3 || !facts[0].Current || facts[1].Current {
		t.Fatal("A-B-A chronology lost", facts, err)
	}
	identities, err := s.AssetIdentities(ctx, a.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	old, newSerial := false, false
	for _, identity := range identities {
		if identity.Value == "DRIVE-1" && identity.SubjectID == "" {
			t.Fatal("component serial became system identity")
		}
		if identity.Value == "SERIAL-2" {
			old = !identity.Current
		}
		if identity.Value == "SERIAL-1" && identity.Current {
			newSerial = true
		}
	}
	if !old || !newSerial {
		t.Fatal("identity change history lost")
	}
	if err = s.Ingest(ctx, assetInventory(owner, "session-1", "inventory-old", now.Add(-time.Minute), change)); err != nil {
		t.Fatal(err)
	}
	a, err = s.Asset(ctx, a.ID)
	if err != nil || a.Serial != "SERIAL-1" {
		t.Fatal("late inventory overwrote current facts", a, err)
	}
	legacy, err := s.Device(ctx, owner, "system-1")
	if err != nil || !strings.Contains(string(legacy.Inventory), "SERIAL-1") {
		t.Fatal("legacy device API broken", err)
	}
}

func TestAssetRegistrySimulationScopeAndAtomicRollback(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now()
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	before, err := s.Assets(ctx, "", "all", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `CREATE FUNCTION reject_asset_fact() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture asset projection failure'; END $$; CREATE TRIGGER reject_asset_fact BEFORE INSERT ON asset_fact_versions FOR EACH ROW EXECUTE FUNCTION reject_asset_fact()`); err != nil {
		t.Fatal(err)
	}
	failed := assetInventory("agent-1", "session-1", "must-rollback", now, &pb.Device{Id: "atomic-new", Source: "local"})
	if err = s.Ingest(ctx, failed); err == nil {
		t.Fatal("projection failure acknowledged")
	}
	var inbox, legacy bool
	if err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_messages WHERE message_id='must-rollback'),EXISTS(SELECT 1 FROM devices WHERE device_id='atomic-new')`).Scan(&inbox, &legacy); err != nil || inbox || legacy {
		t.Fatal("partial ingest committed", inbox, legacy, err)
	}
	after, err := s.Assets(ctx, "", "all", 100, 0)
	if err != nil || len(after) != len(before) {
		t.Fatal("partial asset committed", err)
	}
	if _, err = s.pool.Exec(ctx, `DROP TRIGGER reject_asset_fact ON asset_fact_versions`); err != nil {
		t.Fatal(err)
	}
	if err = s.Ingest(ctx, failed); err != nil {
		t.Fatal("retry after projection recovery", err)
	}
	hello := registration("agent-1", now.Add(time.Second))
	hello.SessionId = "session-2"
	hello.MessageId = "new-session"
	hello.GetHello().SessionId = "session-2"
	if err = s.Ingest(ctx, hello); err != nil {
		t.Fatal(err)
	}
	simulated, err := s.Assets(ctx, "", "simulated", 100, 0)
	if err != nil || len(simulated) != 2 || simulated[0].Scope == simulated[1].Scope {
		t.Fatal("simulator sessions merged", simulated, err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("repeat migration", err)
	}
	again, err := s.Assets(ctx, "", "simulated", 100, 0)
	if err != nil || len(again) != 2 {
		t.Fatal("migration lost assets", again, err)
	}
}

func TestAssetRegistryUpgradePreservesLegacyAndCollectorOwnership(t *testing.T) {
	s := integrationStore(t)
	_, admin := adminFixture(t, s)
	ctx := access.WithPrincipal(t.Context(), admin)
	now := time.Now()
	source, err := s.SaveDataSource(ctx, "", datasources.Input{Type: "COLLECTOR", Name: "custom collector", ConnectionRef: "agent-1", Enabled: false, SyncIntervalSeconds: 86400})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the populated B2.3 schema inside this disposable schema only.
	if _, err = s.pool.Exec(ctx, `DROP TABLE configuration_drift_results,configuration_baseline_state,configuration_baseline_versions; ALTER TABLE automation_snapshots DROP CONSTRAINT automation_snapshot_asset_identity; ALTER TABLE automation_jobs DROP CONSTRAINT automation_job_snapshot; DROP TABLE automation_snapshots,automation_events,automation_jobs,automation_workers,automation_targets; DROP TABLE collector_config_events,collector_config_deliveries,collector_config_revisions,collector_config_state; ALTER TABLE monitoring_policies DROP COLUMN origin,DROP COLUMN managed_revision; ALTER TABLE telephony_policies DROP COLUMN origin,DROP COLUMN managed_revision; DROP TABLE asset_hardware_latest,asset_hardware_samples,asset_voip_events,asset_voip_operations,asset_voip_associations; DROP FUNCTION validate_asset_voip_association(),asset_voip_scope(jsonb,text,text,text); DROP INDEX log_records_asset_scope_cursor_idx; DROP TABLE physical_location_events,placement_events,rack_unit_occupancy,asset_placements,location_operations,physical_locations; DROP FUNCTION validate_rack_unit(),validate_asset_placement(),validate_physical_location(); DROP TABLE asset_projection_marks,asset_resolution_events,asset_separations,asset_decisions; ALTER TABLE assets DROP CONSTRAINT asset_canonical_binding; ALTER TABLE asset_source_bindings DROP CONSTRAINT asset_latest_fact; DROP TABLE asset_identity_evidence,asset_fact_versions,asset_source_bindings,assets; ALTER TABLE data_source_audit ALTER COLUMN actor_id SET NOT NULL; DELETE FROM schema_migrations WHERE version IN (20,21,22,23,26,28,29,30,31,32,33)`); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("upgrade 020", err)
	}
	if _, err = s.Device(ctx, "agent-1", "shared-router"); err != nil {
		t.Fatal("upgrade lost legacy card", err)
	}
	before, err := s.Assets(ctx, "", "all", 100, 0)
	if err != nil || len(before) != 0 {
		t.Fatal("migration silently invented facts", err)
	}
	hello := registration("agent-1", now.Add(time.Second))
	hello.MessageId = "post-upgrade-hello"
	if err = s.Ingest(ctx, hello); err != nil {
		t.Fatal(err)
	}
	after, err := s.Assets(ctx, "", "all", 100, 0)
	if err != nil || len(after) != 1 {
		t.Fatal("new observation not projected", after, err)
	}
	bindings, err := s.AssetBindings(ctx, after[0].ID, false, 50, 0)
	if err != nil || bindings[0].SourceInstanceID != source.ID {
		t.Fatal("manual source instance replaced", err)
	}
	source, err = s.DataSource(ctx, source.ID)
	if err != nil || source.Name != "custom collector" || source.Enabled || source.SyncIntervalSeconds != 86400 {
		t.Fatal("Collector data overwrote admin settings", source, err)
	}
	if _, err = s.SaveDataSource(ctx, source.ID, datasources.Input{Type: "COLLECTOR", Name: source.Name, ConnectionRef: "another-agent", Enabled: false, SyncIntervalSeconds: 86400, Revision: source.Revision}); err != datasources.ErrInvalid {
		t.Fatal("Collector ownership reassigned", err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
}
