package storage

import (
	"context"
	"encoding/json"
	"errors"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assethardware"
	cc "github.com/InfraPulse2026/InfraPulse-Backend/internal/collectorconfig"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"sync"
	"testing"
	"time"
)

func configStorageFixture(t *testing.T) (*Store, context.Context, access.Principal) {
	t.Helper()
	s := integrationStore(t)
	s.ConfigureCollectorConfig(true)
	_, admin := adminFixture(t, s)
	ctx := access.WithPrincipal(t.Context(), admin)
	e := registration("collector-config", time.Now())
	h := e.GetHello()
	h.ApplicationAck = true
	h.ManagedConfigV1 = true
	h.RemoteManagementEnabled = true
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	return s, ctx, admin
}
func configInput(rev int64) cc.Input {
	return cc.Input{ExpectedRevision: rev, Confirmed: true, Reason: "verified managed target", Site: "demo-site", Targets: json.RawMessage(`[{"target_id":"sip-managed","protocol":"sip","endpoint":"192.0.2.10","enabled":true,"polling":{"fast_interval":"3s"}}]`)}
}
func configStatusEnvelope(rev cc.Revision, name pb.ConfigStatus, message string) *pb.AgentEnvelope {
	r := &pb.ConfigApplyStatus{CollectorId: rev.CollectorID, Revision: uint64(rev.Revision), ConfigurationDigest: rev.Digest, Status: name}
	if name == pb.ConfigStatus_CONFIG_STATUS_APPLIED {
		r.AppliedRevision = r.Revision
		r.AppliedDigest = r.ConfigurationDigest
		r.AppliedAt = timestamppb.Now()
	}
	return &pb.AgentEnvelope{AgentId: rev.CollectorID, SessionId: "session-1", MessageId: message, Sequence: 20, SentAt: timestamppb.Now(), Payload: &pb.AgentEnvelope_ConfigStatus{ConfigStatus: r}}
}
func TestCollectorConfigAtomicRevisionStatusReplayAndReconnect(t *testing.T) {
	s, ctx, _ := configStorageFixture(t)
	in := configInput(0)
	rev, replay, err := s.PutCollectorConfig(ctx, "collector-config", in, "config-1")
	if err != nil || replay || rev.Revision != 1 {
		t.Fatal("create", rev, replay, err)
	}
	prior, replay, err := s.PutCollectorConfig(ctx, "collector-config", in, "config-1")
	if err != nil || !replay || prior.Revision != rev.Revision {
		t.Fatal("idempotency", err)
	}
	in.Reason = "different contents"
	if _, _, err = s.PutCollectorConfig(ctx, "collector-config", in, "config-1"); !errors.Is(err, cc.ErrConflict) {
		t.Fatal("key conflict", err)
	}
	in = configInput(0)
	if _, _, err = s.PutCollectorConfig(ctx, "collector-config", in, "config-old"); !errors.Is(err, cc.ErrConflict) {
		t.Fatal("revision conflict", err)
	}
	// A plausible but unsolicited apply result must not claim runtime success.
	e := configStatusEnvelope(rev, pb.ConfigStatus_CONFIG_STATUS_APPLIED, "unsolicited")
	if err = s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	state, err := s.CollectorConfig(ctx, rev.CollectorID)
	if err != nil || state.InSync || state.AppliedRevision != 0 {
		t.Fatal("unsolicited applied", state, err)
	}
	ds, err := s.PendingCollectorConfigs(ctx, 16)
	if err != nil || len(ds) != 1 {
		t.Fatal("queue", ds, err)
	}
	ready, err := s.PrepareCollectorConfig(ctx, ds[0])
	if err != nil || !ready {
		t.Fatal("prepare", err)
	}
	if ready, err = s.PrepareCollectorConfig(ctx, ds[0]); err != nil || ready {
		t.Fatal("duplicate worker claim", err)
	}
	applied := configStatusEnvelope(rev, pb.ConfigStatus_CONFIG_STATUS_APPLIED, "applied")
	if err = s.Ingest(ctx, applied); err != nil {
		t.Fatal(err)
	}
	if err = s.Ingest(ctx, applied); err != nil {
		t.Fatal("replay", err)
	}
	if err = s.Ingest(ctx, configStatusEnvelope(rev, pb.ConfigStatus_CONFIG_STATUS_RECEIVED, "late-received")); err != nil {
		t.Fatal(err)
	}
	heartbeat := proto.Clone(applied).(*pb.AgentEnvelope)
	heartbeat.MessageId = "applied-live-heartbeat"
	heartbeat.Payload = &pb.AgentEnvelope_Heartbeat{Heartbeat: &pb.Heartbeat{Timestamp: timestamppb.Now(), ManagedConfig: applied.GetConfigStatus()}}
	if err = s.Ingest(ctx, heartbeat); err != nil {
		t.Fatal(err)
	}
	state, err = s.CollectorConfig(ctx, rev.CollectorID)
	if err != nil || !state.InSync || state.Status != "applied" {
		t.Fatal("terminal regression", state, err)
	}
	ds, err = s.PendingCollectorConfigs(ctx, 16)
	if err != nil || len(ds) != 0 {
		t.Fatal("terminal retried", err)
	}
	if err = s.Disconnect(ctx, rev.CollectorID, "session-1"); err != nil {
		t.Fatal(err)
	}
	state, _ = s.CollectorConfig(ctx, rev.CollectorID)
	if state.InSync {
		t.Fatal("disconnected in sync")
	}
	hello := registration(rev.CollectorID, time.Now())
	hello.SessionId = "session-2"
	hello.MessageId = "hello-2"
	hello.GetHello().ApplicationAck = true
	hello.GetHello().ManagedConfigV1 = true
	hello.GetHello().RemoteManagementEnabled = true
	hello.GetHello().AppliedConfig = &pb.AppliedConfigState{Revision: uint64(rev.Revision), ConfigurationDigest: rev.Digest, Status: pb.ConfigStatus_CONFIG_STATUS_APPLIED, AppliedAt: applied.GetConfigStatus().AppliedAt, TargetCount: 1}
	if err = s.Ingest(ctx, hello); err != nil {
		t.Fatal(err)
	}
	state, err = s.CollectorConfig(ctx, rev.CollectorID)
	if err != nil || !state.InSync || state.VerifiedSessionID != "session-2" {
		t.Fatal("LKG reconnect reconciliation", state, err)
	}
	in = configInput(1)
	next, _, err := s.PutCollectorConfig(ctx, rev.CollectorID, in, "config-2")
	if err != nil || next.Revision != 2 {
		t.Fatal(err)
	}
	state, _ = s.CollectorConfig(ctx, rev.CollectorID)
	if state.InSync || state.AppliedRevision != 1 {
		t.Fatal("desired was treated as applied", state)
	}
	events, err := s.CollectorConfigEvents(ctx, rev.CollectorID, 50, 0)
	if err != nil || len(events) != 6 {
		t.Fatal("durable events/replay", len(events), err)
	}
	history, err := s.CollectorConfigHistory(ctx, rev.CollectorID, 50, 0)
	if err != nil || len(history) != 2 {
		t.Fatal("history", err)
	}
}
func TestCollectorConfigAdminAuditRollbackConcurrencyAndClosedSecrets(t *testing.T) {
	s, ctx, admin := configStorageFixture(t)
	if _, _, err := s.PutCollectorConfig(t.Context(), "collector-config", configInput(0), "no-auth"); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("anonymous", err)
	}
	bad := configInput(0)
	bad.Targets = json.RawMessage(`[{"target_id":"sip","protocol":"sip","endpoint":"192.0.2.1","password":"secret"}]`)
	if _, _, err := s.PutCollectorConfig(ctx, "collector-config", bad, "secret"); !errors.Is(err, cc.ErrInvalid) {
		t.Fatal("secret accepted", err)
	}
	bad = configInput(0)
	bad.Targets = json.RawMessage(`[]`)
	if _, _, err := s.PutCollectorConfig(ctx, "collector-config", bad, "empty"); !errors.Is(err, cc.ErrInvalid) {
		t.Fatal("accidental empty", err)
	}
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_config_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='collector.config.update' THEN RAISE EXCEPTION 'fixture'; END IF;RETURN NEW;END $$;CREATE TRIGGER reject_config_audit BEFORE INSERT ON access_audit FOR EACH ROW EXECUTE FUNCTION reject_config_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutCollectorConfig(ctx, "collector-config", configInput(0), "audit-fail"); err == nil {
		t.Fatal("audit not atomic")
	}
	state, _ := s.CollectorConfig(ctx, "collector-config")
	if state.Desired != nil {
		t.Fatal("failed audit persisted config")
	}
	if _, err := s.pool.Exec(ctx, `DROP TRIGGER reject_config_audit ON access_audit;DROP FUNCTION reject_config_audit()`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, key := range []string{"race-a", "race-b"} {
		wg.Go(func() { _, _, err := s.PutCollectorConfig(ctx, "collector-config", configInput(0), key); errs <- err })
	}
	wg.Wait()
	close(errs)
	successes, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			successes++
		} else if errors.Is(err, cc.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal("lost update", successes, conflicts)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE access_users SET role='viewer' WHERE id=$1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutCollectorConfig(ctx, "collector-config", configInput(1), "demoted"); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("cached admin bypass", err)
	}
}
func TestCollectorConfigHighestRejectedRevisionAndDisabledBackend(t *testing.T) {
	s, ctx, _ := configStorageFixture(t)
	rev, _, err := s.PutCollectorConfig(ctx, "collector-config", configInput(0), "first")
	if err != nil {
		t.Fatal(err)
	}
	e := configStatusEnvelope(rev, pb.ConfigStatus_CONFIG_STATUS_REJECTED, "unknown-high")
	e.GetConfigStatus().Revision = 100
	if err = s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	next, _, err := s.PutCollectorConfig(ctx, "collector-config", configInput(1), "after-high")
	if err != nil || next.Revision != 101 {
		t.Fatal("rejected revision reuse", next.Revision, err)
	}
	huge := proto.Clone(e).(*pb.AgentEnvelope)
	huge.MessageId = "too-high"
	huge.GetConfigStatus().Revision = ^uint64(0)
	if err = s.Ingest(ctx, huge); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.PutCollectorConfig(ctx, "collector-config", configInput(101), "overflow"); !errors.Is(err, cc.ErrConflict) {
		t.Fatal("overflow", err)
	}
	s.ConfigureCollectorConfig(false)
	if _, _, err = s.PutCollectorConfig(ctx, "collector-config", configInput(101), "disabled"); !errors.Is(err, cc.ErrDisabled) {
		t.Fatal(err)
	}
	pending, err := s.PendingCollectorConfigs(ctx, 16)
	if err != nil || len(pending) != 0 {
		t.Fatal("disabled dispatch", err)
	}
}

func TestCollectorConfigAppliedIntervalsOwnSourceAndPreserveManualOverride(t *testing.T) {
	s, ctx, _ := configStorageFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	in := configInput(0)
	in.Targets = json.RawMessage(`[{"target_id":"bmc","protocol":"redfish","endpoint":"https://192.0.2.10","credential_ref":"read","enabled":true,"polling":{"medium_interval":"86400s"}}]`)
	rev, _, err := s.PutCollectorConfig(ctx, "collector-config", in, "day-period")
	if err != nil {
		t.Fatal(err)
	}
	d := &pb.Device{Id: "hardware-managed", Name: "managed hardware", Source: "redfish", Observation: assetHardwareMeta(rev.CollectorID, "remote:bmc", "/Systems/1", now), Hardware: &pb.HardwareInventory{ResourceType: "System", PowerState: "On"}}
	if err = s.Ingest(ctx, assetInventory(rev.CollectorID, "session-1", "managed-inventory", now, d)); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-10 * time.Minute)
	if err = s.Ingest(ctx, assetHardwareTelemetry(rev.CollectorID, d.Id, "old-real-metric", "remote:bmc", old, 42)); err != nil {
		t.Fatal(err)
	}
	ds, err := s.PendingCollectorConfigs(ctx, 16)
	if err != nil || len(ds) != 1 {
		t.Fatal(err)
	}
	if _, err = s.PrepareCollectorConfig(ctx, ds[0]); err != nil {
		t.Fatal(err)
	}
	reported := configStatusEnvelope(rev, pb.ConfigStatus_CONFIG_STATUS_APPLIED, "reported-only")
	if err = s.Ingest(ctx, reported); err != nil {
		t.Fatal(err)
	}
	card, err := s.Device(ctx, rev.CollectorID, d.Id)
	if err != nil || card.Monitoring.Origin != "backend_default" {
		t.Fatal("report-only changed expectations", card.Monitoring, err)
	}
	heartbeat := proto.Clone(reported).(*pb.AgentEnvelope)
	heartbeat.MessageId = "runtime-proof"
	heartbeat.Payload = &pb.AgentEnvelope_Heartbeat{Heartbeat: &pb.Heartbeat{Timestamp: timestamppb.Now(), ManagedConfig: reported.GetConfigStatus()}}
	if err = s.Ingest(ctx, heartbeat); err != nil {
		t.Fatal(err)
	}
	card, err = s.Device(ctx, rev.CollectorID, d.Id)
	if err != nil || card.Monitoring.Origin != "collector_applied" || card.Monitoring.TelemetryIntervalSeconds != 86400 {
		t.Fatal("applied hardware cadence", card.Monitoring, err)
	}
	id := hardwareAssetID(t, s, rev.CollectorID, d.Id)
	page, err := s.AssetHardware(ctx, id, assethardware.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range page.Items {
		if item.Kind == "metric" {
			found = true
			if item.Freshness != "fresh" || item.FreshnessMaxAgeSeconds != 259200 {
				t.Fatal("long polling produced false stale", item)
			}
		}
	}
	if !found {
		t.Fatal("hardware metric missing")
	}
	explicit, err := s.AssetHardware(ctx, id, assethardware.Filter{MaxAgeSeconds: 300})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range explicit.Items {
		if item.Kind == "metric" && item.Freshness != "stale" {
			t.Fatal("explicit freshness filter ignored", item)
		}
	}
	override := monitoring.Policy{TelemetryIntervalSeconds: 60, InventoryIntervalSeconds: 600, TopologyIntervalSeconds: 600}
	if err = s.UpdateMonitoring(ctx, rev.CollectorID, d.Id, override); err != nil {
		t.Fatal(err)
	}
	heartbeat.MessageId = "runtime-proof-repeat"
	if err = s.Ingest(ctx, heartbeat); err != nil {
		t.Fatal(err)
	}
	card, err = s.Device(ctx, rev.CollectorID, d.Id)
	if err != nil || card.Monitoring.Origin != "device_override" || card.Monitoring.TelemetryIntervalSeconds != 60 {
		t.Fatal("manual policy overwritten", card.Monitoring, err)
	}
	// A similarly named local target stays under local/backend expectations.
	local := proto.Clone(d).(*pb.Device)
	local.Id = "hardware-local"
	local.Observation.Source.TargetId = "bmc"
	if err = s.Ingest(ctx, assetInventory(rev.CollectorID, "session-1", "local-inventory", now, local)); err != nil {
		t.Fatal(err)
	}
	card, err = s.Device(ctx, rev.CollectorID, local.Id)
	if err != nil || card.Monitoring.Origin != "backend_default" {
		t.Fatal("local target matched managed raw ID", err)
	}
}

func TestCollectorConfigRetainedHeartbeatCannotProveNewProcess(t *testing.T) {
	s, ctx, _ := configStorageFixture(t)
	rev, _, err := s.PutCollectorConfig(ctx, "collector-config", configInput(0), "retained-proof")
	if err != nil {
		t.Fatal(err)
	}
	ds, _ := s.PendingCollectorConfigs(ctx, 16)
	if _, err = s.PrepareCollectorConfig(ctx, ds[0]); err != nil {
		t.Fatal(err)
	}
	result := configStatusEnvelope(rev, pb.ConfigStatus_CONFIG_STATUS_APPLIED, "previous-process-status")
	if err = s.Ingest(ctx, result); err != nil {
		t.Fatal(err)
	}
	previous := timestamppb.New(time.Now().Add(-time.Hour))
	hello := registration(rev.CollectorID, time.Now())
	hello.SessionId = "new-process-session"
	hello.MessageId = "new-process-hello"
	hello.GetHello().ApplicationAck = true
	hello.GetHello().ManagedConfigV1 = true
	hello.GetHello().RemoteManagementEnabled = true
	if err = s.Ingest(ctx, hello); err != nil {
		t.Fatal(err)
	}
	ds, _ = s.PendingCollectorConfigs(ctx, 16)
	if len(ds) != 1 {
		t.Fatal("new session not reconciled")
	}
	if _, err = s.PrepareCollectorConfig(ctx, ds[0]); err != nil {
		t.Fatal(err)
	}
	replay := proto.Clone(result).(*pb.AgentEnvelope)
	replay.SessionId = hello.SessionId
	replay.MessageId = "retained-old-heartbeat"
	replay.Payload = &pb.AgentEnvelope_Heartbeat{Heartbeat: &pb.Heartbeat{Timestamp: previous, ManagedConfig: result.GetConfigStatus()}}
	if err = s.Ingest(ctx, replay); err != nil {
		t.Fatal(err)
	}
	state, err := s.CollectorConfig(ctx, rev.CollectorID)
	if err != nil || state.InSync || state.AppliedRevision != 0 {
		t.Fatal("retained heartbeat promoted old runtime", state, err)
	}
	current := proto.Clone(replay).(*pb.AgentEnvelope)
	current.MessageId = "live-process-heartbeat"
	current.GetHeartbeat().Timestamp = timestamppb.Now()
	if err = s.Ingest(ctx, current); err != nil {
		t.Fatal(err)
	}
	state, err = s.CollectorConfig(ctx, rev.CollectorID)
	if err != nil || !state.InSync {
		t.Fatal("live proof rejected", state, err)
	}
	events, err := s.CollectorConfigEvents(ctx, rev.CollectorID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.SessionID == hello.SessionId && event.Status == "applied" && !event.Correlated {
			found = true
		}
	}
	if !found {
		t.Fatal("retained uncorrelated status not archived")
	}
	current.MessageId = "live-process-heartbeat-repeat"
	if err = s.Ingest(ctx, current); err != nil {
		t.Fatal(err)
	}
	repeated, err := s.CollectorConfigEvents(ctx, rev.CollectorID, 50, 0)
	if err != nil || len(repeated) != len(events) {
		t.Fatal("repeated heartbeat inflated events", err)
	}
}
