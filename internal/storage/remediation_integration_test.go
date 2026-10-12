package storage

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	au "github.com/InfraPulse2026/InfraPulse-Backend/internal/automation"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	cd "github.com/InfraPulse2026/InfraPulse-Backend/internal/configdrift"
	rm "github.com/InfraPulse2026/InfraPulse-Backend/internal/remediation"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Deliberately test-only: the team's migration number is not reserved. Normal
// Store.Migrate continues to apply the existing numbered migrations through 033.
//
//go:embed pending_migrations/NEEDS_NUMBER_COORDINATION_b11_2.sql
var pendingRemediationSchema string

type remediationFixtureData struct {
	s        *Store
	ctx      context.Context
	admin    access.Principal
	target   au.Target
	policy   rm.Policy
	baseline cd.Baseline
	drift    cd.Record
}

func remediationFixture(t *testing.T) remediationFixtureData {
	t.Helper()
	s, ctx, admin, target := automationFixture(t)
	// Apply the pending upgrade over a populated B11.1 database, not just an
	// empty fake schema. Existing jobs/snapshots/drift history must survive.
	expected := createDriftSnapshot(t, s, ctx, target, driftApproved, "b112-expected")
	baseline := approveDriftBaseline(t, s, ctx, target, expected)
	createDriftSnapshot(t, s, ctx, target, strings.Replace(driftApproved, "vlan 20", "vlan 30", 1), "b112-actual")
	state, err := s.ConfigurationDrift(ctx, target.AssetID)
	if err != nil || state.Status != "drift" || state.Latest == nil {
		t.Fatal("B11.1 drift fixture", state, err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	d := &pb.Device{Id: "switch_b112", Name: "SW-ACCESS-02", Vendor: "Cisco", Os: "ios", Model: "fixture-C9300", ManagementAddress: target.Host, Source: "snmp", Type: pb.DeviceType_DEVICE_TYPE_SWITCH, Online: true}
	hello := registration("collector_b112", now)
	hello.GetHello().Device = d
	if err = s.Ingest(ctx, hello); err != nil {
		t.Fatal("source fixture", err)
	}
	if err = s.Ingest(ctx, assetInventory("collector_b112", "session-1", "inventory_b112", now, d)); err != nil {
		t.Fatal("inventory fixture", err)
	}
	port := &pb.Interface{Id: "12", Name: "GigabitEthernet0/12", Alias: "lab-access", Type: proto.Uint32(6), AdminUp: true}
	sample := &pb.TelemetrySample{DeviceId: d.Id, ObservedAt: timestamppb.New(now), CollectedAt: timestamppb.New(now), Source: "snmp", Interfaces: []*pb.Interface{port}}
	envelope := &pb.AgentEnvelope{AgentId: "collector_b112", SessionId: "session-1", MessageId: "telemetry_b112", Sequence: 3, SentAt: timestamppb.New(now), Payload: &pb.AgentEnvelope_Telemetry{Telemetry: &pb.TelemetryBatch{Samples: []*pb.TelemetrySample{sample}}}}
	if err = s.Ingest(ctx, envelope); err != nil {
		t.Fatal("port fixture", err)
	}
	var binding, originalAsset string
	if err = s.pool.QueryRow(ctx, `SELECT id,asset_id FROM asset_source_bindings WHERE legacy_agent_id='collector_b112' AND legacy_device_id='switch_b112' AND scope=''`).Scan(&binding, &originalAsset); err != nil {
		t.Fatal("canonical source fixture", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE assets SET canonical_binding_id=NULL WHERE id=$1`, originalAsset); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE asset_source_bindings SET asset_id=$1 WHERE id=$2`, target.AssetID, binding); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE assets SET canonical_binding_id=$2,vendor='Cisco',canonical_name='SW-ACCESS-02' WHERE id=$1`, target.AssetID, binding); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, pendingRemediationSchema); err != nil {
		t.Fatal("pending B11.2 migration over B11.1 histories", err)
	}
	s.ConfigureCommands(commands.Defaults())
	policy := rm.Policy{Enabled: true, Version: "lab_b112_v1", AllowedUsers: []string{admin.User.ID}, Rules: []rm.Rule{{AssetID: target.AssetID, BindingID: binding, InterfaceID: port.Id, Interface: port.Name, Platform: rm.Platform, Role: "access", AllowedVLANs: []int{20, 30}, ManagementVLANs: []int{1}}}}
	if err = s.ConfigureRemediation(ctx, policy); err != nil {
		t.Fatal("reviewed local policy", err)
	}
	if err = s.AutomationWorker(ctx, au.WorkerStatus{ID: "worker_one", Available: true, Templates: []string{target.TemplateID, au.RestoreTemplateID}, Versions: map[string]string{"restore_template_sha256": au.RestoreTemplateDigest()}}); err != nil {
		t.Fatal(err)
	}
	return remediationFixtureData{s, ctx, admin, target, policy, baseline, *state.Latest}
}

func (f remediationFixtureData) proposal(t *testing.T, key string) rm.Proposal {
	t.Helper()
	p, replay, err := f.s.CreateRemediation(f.ctx, f.target.AssetID, rm.CreateInput{DriftID: f.drift.ID, Reason: "restore reviewed lab access VLAN"}, key)
	if err != nil || replay || p.State != "proposed" || p.Digest != rm.Fingerprint(p.Action) {
		t.Fatal("concrete immutable proposal", p, replay, err)
	}
	return p
}

func approveRemediationInput(p rm.Proposal) rm.ApproveInput {
	return rm.ApproveInput{Confirmed: true, Digest: p.Digest, Reason: "human approves exact VLAN action"}
}

func (f remediationFixtureData) claimed(t *testing.T, p rm.Proposal) au.Job {
	t.Helper()
	approved, replay, err := f.s.ApproveRemediation(f.ctx, p.ID, approveRemediationInput(p))
	if err != nil || replay || approved.JobID == "" || approved.State != "queued" {
		t.Fatal("explicit approval", approved, replay, err)
	}
	j, err := f.s.ClaimAutomation(f.ctx, "worker_one")
	if err != nil || j.ID != approved.JobID || j.ProposalID != p.ID || j.ActionDigest != p.Digest || j.Restore == nil || j.Restore.TargetVLAN != 20 || j.Restore.Interface != "GigabitEthernet0/12" {
		t.Fatal("persisted job/action hydration", j, err)
	}
	return j
}

// This is a controlled Executor fixture, not a real Linux Ansible/device test.
// It records every reviewed template, exact bound target and typed parameter.
type remediationExecutorFixture struct {
	configuration string
	reads         int
	writes        int
	calls         []au.Job
	unchanged     bool
	failRead      int
	writeError    string
	writeHook     func()
}

func (e *remediationExecutorFixture) Probe(context.Context) au.WorkerStatus {
	return au.WorkerStatus{Available: true}
}
func (e *remediationExecutorFixture) Execute(_ context.Context, j au.Job) au.Result {
	e.calls = append(e.calls, j)
	if j.Restore == nil {
		e.reads++
		if e.reads == e.failRead {
			return au.Result{ErrorCode: "runner_failed"}
		}
		return au.Result{Configuration: e.configuration, Facts: json.RawMessage(`{}`)}
	}
	e.writes++
	if e.writeHook != nil {
		e.writeHook()
	}
	if !e.unchanged && e.writeError != "precondition_failed" {
		e.configuration = strings.Replace(e.configuration, "vlan 30", "vlan 20", 1)
	}
	return au.Result{ErrorCode: e.writeError, Facts: json.RawMessage(`{}`)}
}

func remediationActual() string { return strings.Replace(driftApproved, "vlan 20", "vlan 30", 1) }

func remediationRole(t *testing.T, f remediationFixtureData, name, role string) (context.Context, access.Principal) {
	t.Helper()
	hash, err := access.HashPassword(fixturePassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.AccessCreateUser(f.ctx, f.admin, name, role, hash, false, access.Meta{}); err != nil {
		t.Fatal(err)
	}
	service := access.New(f.s, access.Config{Mode: "required", SessionTTL: time.Hour})
	p, err := service.Login(f.ctx, name, fixturePassword, access.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	return access.WithPrincipal(f.ctx, p), p
}

func TestRemediationPersistentFixtureE2EAndIdempotency(t *testing.T) {
	f := remediationFixture(t)
	viewerCtx, _ := remediationRole(t, f, "b112_viewer", "viewer")
	operatorCtx, operator := remediationRole(t, f, "b112_operator", "operator")
	f.policy.AllowedUsers = append(f.policy.AllowedUsers, operator.User.ID)
	if err := f.s.ConfigureRemediation(f.ctx, f.policy); err != nil {
		t.Fatal(err)
	}
	proposalAt := time.Now()
	p := f.proposal(t, "e2e-proposal")
	proposalLatency := time.Since(proposalAt)
	if _, _, err := f.s.ApproveRemediation(viewerCtx, p.ID, approveRemediationInput(p)); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("viewer approval allowed", err)
	}
	preview, err := f.s.Remediation(operatorCtx, p.ID)
	if err != nil || preview.Action.InterfaceID != "12" || preview.Action.CurrentVLAN != 30 || preview.Action.TargetVLAN != 20 {
		t.Fatal("exact preview", preview, err)
	}
	approvalAt := time.Now()
	approved, replay, err := f.s.ApproveRemediation(operatorCtx, p.ID, approveRemediationInput(p))
	approvalLatency := time.Since(approvalAt)
	if err != nil || replay || approved.State != "queued" || approved.ApprovedDigest != p.Digest || approved.ApprovedBy != operator.User.ID {
		t.Fatal(approved, replay, err)
	}
	claimAt := time.Now()
	j, err := f.s.ClaimAutomation(f.ctx, "worker_one")
	claimLatency := time.Since(claimAt)
	if err != nil || j.Restore == nil || j.ProposalID != p.ID {
		t.Fatal(j, err)
	}
	executor := &remediationExecutorFixture{configuration: remediationActual()}
	verifyAt := time.Now()
	state, result := f.s.ExecuteRemediation(f.ctx, executor, j)
	workflowLatency := time.Since(verifyAt)
	if state != "succeeded" || executor.reads != 2 || executor.writes != 1 {
		t.Fatal("gather/write/gather verification", state, executor.reads, executor.writes, result)
	}
	for _, call := range executor.calls {
		if call.AssetID != f.target.AssetID || call.Target.Host != f.target.Host || call.Target.TemplateID != f.target.TemplateID {
			t.Fatal("wrong Asset/inventory binding", call)
		}
		if (call.Restore == nil && call.TemplateID != f.target.TemplateID) || (call.Restore != nil && call.TemplateID != au.RestoreTemplateID) {
			t.Fatal("read/write job template confused", call.TemplateID)
		}
		if call.Restore != nil && (call.Restore.TemplateID != au.RestoreTemplateID || call.Restore.TemplateDigest != au.RestoreTemplateDigest() || call.Restore.CurrentVLAN != 30 || call.Restore.TargetVLAN != 20) {
			t.Fatal("wrong exact action", call.Restore)
		}
	}
	finishAt := time.Now()
	if err = f.s.FinishAutomation(f.ctx, j, state, result); err != nil {
		t.Fatal(err)
	}
	t.Logf("PostgreSQL + fake Executor latency: proposal=%s approval=%s claim=%s workflow=%s evidence+audit=%s", proposalLatency, approvalLatency, claimLatency, workflowLatency, time.Since(finishAt))
	final, err := f.s.Remediation(operatorCtx, p.ID)
	if err != nil || final.State != "succeeded" || final.BeforeSnapshotID == "" || final.AfterSnapshotID == "" || final.BeforeSnapshotID == final.AfterSnapshotID || final.Verification == nil || final.Verification.Status != "compliant" {
		t.Fatal("persistent verification", final, err)
	}
	drift, err := f.s.ConfigurationDrift(f.ctx, f.target.AssetID)
	if err != nil || drift.Status != "compliant" || drift.Latest.ActualSnapshotID != final.AfterSnapshotID {
		t.Fatal("B11.1 projection after verification", drift, err)
	}
	history, err := f.s.ConfigurationDriftHistory(f.ctx, f.target.AssetID, 50, 0)
	if err != nil || len(history) != 3 {
		t.Fatal("historical drift retained", len(history), err)
	}
	repeated, replay, err := f.s.ApproveRemediation(operatorCtx, p.ID, approveRemediationInput(p))
	if err != nil || !replay || repeated.JobID != approved.JobID {
		t.Fatal("one-time approval replay", repeated, replay, err)
	}
	if another, err := f.s.ClaimAutomation(f.ctx, "worker_one"); err != nil || another.ID != "" {
		t.Fatal("duplicate execution queued", another, err)
	}
	if err = f.s.FinishAutomation(f.ctx, j, state, result); !errors.Is(err, au.ErrConflict) {
		t.Fatal("duplicate callback changed evidence", err)
	}
	audit, err := f.s.RemediationHistory(operatorCtx, p.ID, 200, 0)
	if err != nil || len(audit) < 7 {
		t.Fatal("approval/action/result audit", len(audit), err)
	}
	seenAudit := map[string]bool{}
	for _, event := range audit {
		seenAudit[event.Action] = true
	}
	for _, action := range []string{"remediation.proposal.created", "remediation.approved", "remediation.queued", "remediation.job.started", "remediation.preflight", "remediation.verification.started", "remediation.verification.finished"} {
		if !seenAudit[action] {
			t.Fatal("required audit phase missing", action)
		}
	}
	raw, _ := json.Marshal(audit)
	if strings.Contains(string(raw), "description Voice") || strings.Contains(string(raw), "switchport") || strings.Contains(string(raw), "credential") {
		t.Fatal("raw config or credentials leaked in history")
	}
	var phaseCount int
	if err = f.s.pool.QueryRow(f.ctx, `SELECT count(*) FROM automation_snapshots WHERE job_id=$1`, j.ID).Scan(&phaseCount); err != nil || phaseCount != 2 {
		t.Fatal("fresh before/after snapshots", phaseCount, err)
	}
}

func TestRemediationPreflightAndExecutionFailuresNeverMisreportSuccess(t *testing.T) {
	for _, scenario := range []string{"changed_actual", "trunk", "lag", "unknown_mode", "already_compliant", "preflight_offline", "guard_reject", "exit_zero_unchanged", "verification_offline", "write_timeout", "write_authentication", "write_failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := remediationFixture(t)
			j := f.claimed(t, f.proposal(t, "failure-proposal"))
			e := &remediationExecutorFixture{configuration: remediationActual()}
			wanted, writes := "stale", 0
			switch scenario {
			case "changed_actual":
				e.configuration = strings.Replace(e.configuration, "vlan 30", "vlan 40", 1)
			case "trunk":
				e.configuration = strings.Replace(e.configuration, "mode access", "mode trunk", 1)
			case "lag":
				e.configuration = strings.Replace(e.configuration, " no shutdown", " channel-group 1 mode active\n no shutdown", 1)
			case "unknown_mode":
				e.configuration = strings.Replace(e.configuration, " switchport mode access\n", "", 1)
			case "already_compliant":
				e.configuration, wanted = driftApproved, "no_action"
			case "preflight_offline":
				e.failRead, wanted = 1, "failed"
			case "guard_reject":
				e.writeError, writes = "precondition_failed", 1 // Executor dispatch; fixture guard performs zero mutation.
			case "exit_zero_unchanged":
				e.unchanged, wanted, writes = true, "verification_failed", 1
			case "verification_offline":
				e.failRead, wanted, writes = 2, "unknown", 1
			case "write_timeout":
				e.writeError, wanted, writes = "timed_out", "unknown", 1
			case "write_authentication":
				e.writeError, wanted, writes = "authentication_failed", "unknown", 1
			case "write_failure":
				e.writeError, wanted, writes = "runner_failed", "unknown", 1
			}
			state, result := f.s.ExecuteRemediation(f.ctx, e, j)
			if state != wanted || e.writes != writes {
				t.Fatal("unsafe state/write count", state, wanted, e.writes, writes, result)
			}
			if err := f.s.FinishAutomation(f.ctx, j, state, result); err != nil {
				t.Fatal(err)
			}
			job, err := f.s.AutomationJob(f.ctx, j.ID)
			if err != nil || job.State != wanted {
				t.Fatal("persistent terminal state", job, err)
			}
			if another, err := f.s.ClaimAutomation(f.ctx, "worker_one"); err != nil || another.ID != "" {
				t.Fatal("blind retry after failure", another, err)
			}
		})
	}
}

func TestRemediationConcurrentApprovalsAndConflictingJobs(t *testing.T) {
	f := remediationFixture(t)
	p := f.proposal(t, "concurrent-proposal")
	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := []string{}
	created, replayed := 0, 0
	for range 2 {
		wg.Go(func() {
			v, replay, err := f.s.ApproveRemediation(f.ctx, p.ID, approveRemediationInput(p))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Error(err)
				return
			}
			ids = append(ids, v.JobID)
			if replay {
				replayed++
			} else {
				created++
			}
		})
	}
	wg.Wait()
	if len(ids) != 2 || ids[0] != ids[1] || created != 1 || replayed != 1 {
		t.Fatal("approval race created multiple jobs", ids, created, replayed)
	}
	other := f.proposal(t, "conflicting-proposal")
	if _, _, err := f.s.ApproveRemediation(f.ctx, other.ID, approveRemediationInput(other)); !errors.Is(err, rm.ErrConflict) {
		t.Fatal("conflicting same-Asset job accepted", err)
	}
	var count int
	if err := f.s.pool.QueryRow(f.ctx, `SELECT count(*) FROM automation_jobs WHERE state='queued'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func mutateRemediationPort(t *testing.T, f remediationFixtureData, mutate func(*pb.Interface)) {
	t.Helper()
	mutateRemediationTelemetry(t, f, func(sample *pb.TelemetrySample) { mutate(sample.Interfaces[0]) })
}

func mutateRemediationTelemetry(t *testing.T, f remediationFixtureData, mutate func(*pb.TelemetrySample)) {
	t.Helper()
	var id int64
	var raw []byte
	if err := f.s.pool.QueryRow(f.ctx, `SELECT id,payload FROM telemetry WHERE agent_id='collector_b112' AND device_id='switch_b112' ORDER BY id DESC LIMIT 1`).Scan(&id, &raw); err != nil {
		t.Fatal(err)
	}
	sample := &pb.TelemetrySample{}
	if err := protojson.Unmarshal(raw, sample); err != nil {
		t.Fatal(err)
	}
	mutate(sample)
	raw, err := protojson.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.pool.Exec(f.ctx, `UPDATE telemetry SET payload=$2 WHERE id=$1`, id, raw); err != nil {
		t.Fatal(err)
	}
}

func TestRemediationTopologyCanonicalPortResolutionFailsClosed(t *testing.T) {
	for _, scenario := range []string{"resolved_id", "resolved_name", "resolved_alias", "resolved_mac", "raw_ifname", "explicit_ifindex", "unresolved_lldp_local_number", "index_without_metadata", "other_known_port", "other_known_ifindex"} {
		t.Run(scenario, func(t *testing.T) {
			f := remediationFixture(t)
			mutateRemediationTelemetry(t, f, func(sample *pb.TelemetrySample) {
				sample.Interfaces[0].Index = 120 // deliberately different from ID/local LLDP port number12.
				sample.Interfaces[0].Metadata = &pb.InterfaceMetadata{Index: true}
				sample.Interfaces[0].Mac = "02:00:00:00:00:12"
				if scenario == "index_without_metadata" {
					sample.Interfaces[0].Metadata = nil
				}
				if strings.HasPrefix(scenario, "other_known") {
					sample.Interfaces = append(sample.Interfaces, &pb.Interface{Id: "13", Name: "GigabitEthernet0/13", Index: 130, Metadata: &pb.InterfaceMetadata{Index: true}, Type: proto.Uint32(6)})
				}
			})
			neighbor := &pb.Neighbor{LocalDeviceId: "switch_b112", LocalPortNumber: 12, RemoteChassisSubtype: 4, RemoteChassisId: []byte{2, 0, 0, 0, 0, 99}, RemotePortSubtype: 5, RemotePortId: []byte("GigabitEthernet0/1"), Protocol: pb.DiscoveryProtocol_DISCOVERY_PROTOCOL_LLDP, Source: "snmp.lldp", Confidence: "high"}
			switch scenario {
			case "resolved_id":
				neighbor.LocalInterfaceId = "12"
			case "resolved_name":
				neighbor.LocalInterfaceName = "GigabitEthernet0/12"
			case "resolved_alias":
				neighbor.LocalInterfaceAlias = "lab-access"
			case "resolved_mac":
				neighbor.LocalInterfaceMac = "02:00:00:00:00:12"
			case "raw_ifname":
				neighbor.LocalPortSubtype, neighbor.LocalPortId = 5, []byte("GigabitEthernet0/12")
			case "explicit_ifindex", "index_without_metadata":
				neighbor.LocalInterfaceIndex = proto.Uint32(120)
			case "unresolved_lldp_local_number":
				neighbor.LocalPortSubtype, neighbor.LocalPortId = 7, []byte("12")
			case "other_known_port":
				neighbor.LocalInterfaceId = "13"
			case "other_known_ifindex":
				neighbor.LocalInterfaceIndex = proto.Uint32(130)
			}
			now := time.Now().UTC()
			envelope := &pb.AgentEnvelope{AgentId: "collector_b112", SessionId: "session-1", MessageId: "topology_b112", Sequence: 4, SentAt: timestamppb.New(now), Payload: &pb.AgentEnvelope_Topology{Topology: &pb.TopologyUpdate{DeviceId: "switch_b112", Source: "snmp.lldp", ObservedAt: timestamppb.New(now), TtlSeconds: 900, Completeness: pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_COMPLETE, Observations: []*pb.Neighbor{neighbor}}}}
			if err := f.s.Ingest(f.ctx, envelope); err != nil {
				t.Fatal(err)
			}
			p, _, err := f.s.CreateRemediation(f.ctx, f.target.AssetID, rm.CreateInput{DriftID: f.drift.ID, Reason: "verify canonical topology port security gate"}, "topology-proposal")
			if strings.HasPrefix(scenario, "other_known") {
				if err != nil || p.Action.InterfaceID != "12" {
					t.Fatal("unrelated resolved neighbor vetoed access port", p, err)
				}
			} else if !errors.Is(err, rm.ErrPolicy) {
				t.Fatal("observed/unresolved uplink did not fail closed", scenario, err)
			}
			var active int
			if err := f.s.pool.QueryRow(f.ctx, `SELECT count(*) FROM automation_jobs WHERE state IN ('queued','running','verifying')`).Scan(&active); err != nil || active != 0 {
				t.Fatal("unapproved/denied topology action launched execution", active, err)
			}
		})
	}
}

func TestRemediationCancellationAndVerificationShareConsistentLockOrder(t *testing.T) {
	f := remediationFixture(t)
	p := f.proposal(t, "cancel-verification-race")
	j := f.claimed(t, p)
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; _, err := f.s.CancelAutomation(ctx, j.ID); results <- err }()
	go func() { <-start; results <- f.s.beginRemediationVerification(ctx, j) }()
	close(start)
	for range 2 {
		select {
		case err := <-results:
			if err != nil && !errors.Is(err, rm.ErrConflict) {
				t.Fatal("cancel/verification race deadlocked or failed", err)
			}
		case <-ctx.Done():
			t.Fatal("job/proposal row lock inversion blocked cancellation/verification", ctx.Err())
		}
	}
	if err := f.s.FinishAutomation(f.ctx, j, "unknown", au.Result{ErrorCode: "execution_uncertain"}); err != nil {
		t.Fatal(err)
	}
	stored, err := f.s.Remediation(f.ctx, p.ID)
	if err != nil || stored.State != "unknown" {
		t.Fatal("concurrent cancellation/verification misreported outcome", stored, err)
	}
}

func TestRemediationRegistryPolicyAndProtectedResourceRejections(t *testing.T) {
	for _, scenario := range []string{"protected_interface_id", "protected_interface_alias", "protected_device", "management_address", "unknown_interface_type", "changed_interface_id", "unsupported_vendor", "unsupported_nxos", "unknown_platform", "missing_binding", "stale_inventory", "stale_telemetry", "forbidden_vlan", "protected_rule"} {
		t.Run(scenario, func(t *testing.T) {
			f := remediationFixture(t)
			switch scenario {
			case "protected_interface_id":
				f.s.ConfigureCommands(commands.Policy{ProtectedInterfaces: []string{"12"}})
			case "protected_interface_alias":
				f.s.ConfigureCommands(commands.Policy{ProtectedInterfaces: []string{"lab-access"}})
			case "protected_device":
				f.s.ConfigureCommands(commands.Policy{ProtectedDevices: []string{"switch_b112"}})
			case "management_address":
				mutateRemediationPort(t, f, func(p *pb.Interface) { p.Addresses = []string{f.target.Host + "/24"} })
			case "unknown_interface_type":
				mutateRemediationPort(t, f, func(p *pb.Interface) { p.Type = nil })
			case "changed_interface_id":
				mutateRemediationPort(t, f, func(p *pb.Interface) { p.Id = "99" })
			case "unsupported_vendor":
				_, err := f.s.pool.Exec(f.ctx, `UPDATE assets SET vendor='UnsupportedVendor' WHERE id=$1`, f.target.AssetID)
				if err != nil {
					t.Fatal(err)
				}
			case "unsupported_nxos", "unknown_platform":
				platform := "nxos"
				if scenario == "unknown_platform" {
					platform = ""
				}
				if _, err := f.s.pool.Exec(f.ctx, `UPDATE asset_source_bindings SET inventory=jsonb_set(inventory,'{os}',to_jsonb($2::text)) WHERE asset_id=$1`, f.target.AssetID, platform); err != nil {
					t.Fatal(err)
				}
			case "missing_binding":
				_, err := f.s.pool.Exec(f.ctx, `UPDATE assets SET canonical_binding_id=NULL WHERE id=$1`, f.target.AssetID)
				if err != nil {
					t.Fatal(err)
				}
			case "stale_inventory":
				_, err := f.s.pool.Exec(f.ctx, `UPDATE asset_source_bindings SET inventory_at=now()-interval '1 hour' WHERE asset_id=$1`, f.target.AssetID)
				if err != nil {
					t.Fatal(err)
				}
			case "stale_telemetry":
				_, err := f.s.pool.Exec(f.ctx, `UPDATE telemetry SET observed_at=now()-interval '1 hour' WHERE agent_id='collector_b112'`)
				if err != nil {
					t.Fatal(err)
				}
			case "forbidden_vlan":
				f.policy.Rules[0].AllowedVLANs = []int{30}
				if err := f.s.ConfigureRemediation(f.ctx, f.policy); err != nil {
					t.Fatal(err)
				}
			case "protected_rule":
				f.policy.Rules[0].Protected = true
				if err := f.s.ConfigureRemediation(f.ctx, f.policy); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := f.s.CreateRemediation(f.ctx, f.target.AssetID, rm.CreateInput{DriftID: f.drift.ID, Reason: "must deny unsafe resource"}, "denied-proposal"); err == nil {
				t.Fatal("unsafe proposal accepted")
			}
			var active int
			if err := f.s.pool.QueryRow(f.ctx, `SELECT count(*) FROM automation_jobs WHERE state IN ('queued','running','verifying')`).Scan(&active); err != nil || active != 0 {
				t.Fatal("denied action queued execution", active, err)
			}
		})
	}
}

func TestRemediationQueuedApprovalIsRevalidatedAfterRestart(t *testing.T) {
	for _, scenario := range []string{"unchanged_restart", "policy_changed", "expected_revision", "binding_changed", "protected_port", "expired_queue", "revoked_session", "demoted_approver", "retargeted_host"} {
		t.Run(scenario, func(t *testing.T) {
			f := remediationFixture(t)
			p := f.proposal(t, "queued-proposal")
			approved, _, err := f.s.ApproveRemediation(f.ctx, p.ID, approveRemediationInput(p))
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "policy_changed":
				f.policy.Version = "lab_b112_v2"
				if err = f.s.ConfigureRemediation(f.ctx, f.policy); err != nil {
					t.Fatal(err)
				}
			case "expected_revision":
				_, _, err = f.s.PutConfigurationBaseline(f.ctx, f.target.AssetID, cd.BaselineInput{ExpectedRevision: f.baseline.Revision, AssetRevision: 1, SnapshotID: f.baseline.SnapshotID, Confirmed: true, Reason: "new separately reviewed expected revision"}, "baseline-changed")
				if err != nil {
					t.Fatal(err)
				}
			case "binding_changed":
				mutateRemediationPort(t, f, func(p *pb.Interface) { p.Alias = "different-role" })
			case "protected_port":
				f.s.ConfigureCommands(commands.Policy{ProtectedInterfaces: []string{"12"}})
			case "expired_queue":
				_, err = f.s.pool.Exec(f.ctx, `UPDATE automation_jobs SET created_at=now()-interval '2 minutes' WHERE id=$1`, approved.JobID)
				if err != nil {
					t.Fatal(err)
				}
			case "revoked_session":
				if _, err = f.s.pool.Exec(f.ctx, `DELETE FROM access_sessions WHERE token_hash=$1`, f.admin.SessionHash); err != nil {
					t.Fatal(err)
				}
			case "demoted_approver":
				if _, err = f.s.pool.Exec(f.ctx, `UPDATE access_users SET role='viewer' WHERE id=$1`, f.admin.User.ID); err != nil {
					t.Fatal(err)
				}
			case "retargeted_host":
				input := f.target.TargetInput
				input.ExpectedRevision = f.target.Revision
				input.Host = "192.0.2.21"
				if _, err = f.s.PutAutomationTarget(f.ctx, f.target.AssetID, input); err != nil {
					t.Fatal(err)
				}
			}
			// A new Store models another process consuming the same persistent
			// queue; no RAM mutex or old process ownership is needed.
			restarted := &Store{pool: f.s.pool, automationEnabled: true, automationVault: f.s.automationVault, remediationReady: true, remediationPolicy: f.s.remediationPolicy, commandPolicy: f.s.commandPolicy}
			j, err := restarted.ClaimAutomation(f.ctx, "worker_one")
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "unchanged_restart" {
				if j.ID != approved.JobID || j.Restore == nil {
					t.Fatal("queued job lost after restart", j)
				}
				return
			}
			if j.ID != "" {
				t.Fatal("stale queued approval dispatched", j)
			}
			stored, err := readRemediation(f.s.pool.QueryRow(f.ctx, `SELECT `+remediationColumns+` FROM remediation_proposals WHERE id=$1`, p.ID))
			if err != nil || stored.State != "stale" {
				t.Fatal("stale approval status", stored, err)
			}
		})
	}
}

func TestRemediationApprovalImmutabilityRevocationAndWorkerPin(t *testing.T) {
	f := remediationFixture(t)
	p := f.proposal(t, "immutable-proposal")
	for _, query := range []string{
		`UPDATE remediation_proposals SET action=jsonb_set(action,'{target_vlan}','40') WHERE id=$1`,
		`UPDATE remediation_proposals SET digest=repeat('0',64) WHERE id=$1`,
		`UPDATE remediation_proposals SET expires_at=expires_at+interval '1 minute' WHERE id=$1`,
	} {
		if _, err := f.s.pool.Exec(f.ctx, query, p.ID); err == nil {
			t.Fatal("database permits immutable action mutation")
		}
	}
	changed := approveRemediationInput(p)
	changed.Digest = strings.Repeat("0", 64)
	if _, _, err := f.s.ApproveRemediation(f.ctx, p.ID, changed); !errors.Is(err, rm.ErrConflict) {
		t.Fatal("different action digest approved", err)
	}
	operatorCtx, _ := remediationRole(t, f, "no_capability", "operator")
	if _, _, err := f.s.ApproveRemediation(operatorCtx, p.ID, approveRemediationInput(p)); !errors.Is(err, rm.ErrPolicy) {
		t.Fatal("operator role without capability approved", err)
	}
	if err := f.s.AutomationWorker(f.ctx, au.WorkerStatus{ID: "worker_one", Available: true, Templates: []string{f.target.TemplateID}, Versions: map[string]string{"restore_template_sha256": strings.Repeat("0", 64)}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.ApproveRemediation(f.ctx, p.ID, approveRemediationInput(p)); !errors.Is(err, rm.ErrUnavailable) {
		t.Fatal("unreviewed worker artifact accepted", err)
	}
	if err := f.s.AutomationWorker(f.ctx, au.WorkerStatus{ID: "worker_one", Available: true, Templates: []string{f.target.TemplateID, au.RestoreTemplateID}, Versions: map[string]string{"restore_template_sha256": au.RestoreTemplateDigest()}}); err != nil {
		t.Fatal(err)
	}
	approved, _, err := f.s.ApproveRemediation(f.ctx, p.ID, approveRemediationInput(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.pool.Exec(f.ctx, `UPDATE remediation_proposals SET approved_at=approved_at+interval '1 second' WHERE id=$1`, p.ID); err == nil {
		t.Fatal("approval can be changed after consume")
	}
	if _, err = f.s.pool.Exec(f.ctx, `UPDATE remediation_proposals SET state='proposed' WHERE id=$1`, p.ID); err == nil {
		t.Fatal("queued approval can be reopened")
	}
	var count int
	if err = f.s.pool.QueryRow(f.ctx, `SELECT count(*) FROM automation_jobs WHERE id=$1`, approved.JobID).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestRemediationOrphanUnknownLateResultAndNoBlindRetry(t *testing.T) {
	f := remediationFixture(t)
	p := f.proposal(t, "orphan-proposal")
	j := f.claimed(t, p)
	if _, err := f.s.pool.Exec(f.ctx, `UPDATE automation_jobs SET deadline=now()-interval '1 minute' WHERE id=$1`, j.ID); err != nil {
		t.Fatal(err)
	}
	if next, err := f.s.ClaimAutomation(f.ctx, "worker_one"); err != nil || next.ID != "" {
		t.Fatal("orphan was replayed", next, err)
	}
	stored, err := f.s.Remediation(f.ctx, p.ID)
	if err != nil || stored.State != "unknown" || stored.ErrorCode != "worker_lost" {
		t.Fatal("orphan state", stored, err)
	}
	if err = f.s.FinishAutomation(f.ctx, j, "succeeded", au.Result{Configuration: driftApproved, Facts: json.RawMessage(`{}`)}); !errors.Is(err, au.ErrConflict) {
		t.Fatal("lost/late worker callback overwrote unknown", err)
	}
	other := f.proposal(t, "new-attempt-blocked")
	if _, _, err = f.s.ApproveRemediation(f.ctx, other.ID, approveRemediationInput(other)); !errors.Is(err, rm.ErrConflict) {
		t.Fatal("unknown execution permitted blind retry", err)
	}
}

func expireRemediationFixture(t *testing.T, f remediationFixtureData, id string) {
	t.Helper()
	// No wall-clock wait or production backdoor. Only this isolated test schema
	// simulates a proposal created ten minutes ago while preserving TTL ordering.
	if _, err := f.s.pool.Exec(f.ctx, `ALTER TABLE remediation_proposals DISABLE TRIGGER remediation_immutable`); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.pool.Exec(f.ctx, `UPDATE remediation_proposals SET created_at=now()-interval '10 minutes',expires_at=now()-interval '5 minutes' WHERE id=$1`, id)
	_, enabledErr := f.s.pool.Exec(f.ctx, `ALTER TABLE remediation_proposals ENABLE TRIGGER remediation_immutable`)
	if err != nil || enabledErr != nil {
		t.Fatal(err, enabledErr)
	}
}

func TestRemediationExpiredApprovalAndNullableApprovalConstraints(t *testing.T) {
	f := remediationFixture(t)
	p := f.proposal(t, "expire-proposal")
	var oldReadJob string
	if err := f.s.pool.QueryRow(f.ctx, `SELECT job_id FROM automation_snapshots WHERE id=$1`, p.Action.ActualSnapshotID).Scan(&oldReadJob); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(f.ctx, `UPDATE remediation_proposals SET approved_by=$2,approved_at=now(),approved_digest=NULL,job_id=$3 WHERE id=$1`, p.ID, f.admin.User.ID, oldReadJob); err == nil {
		t.Fatal("NULL digest bypasses approval tuple CHECK")
	}
	expireRemediationFixture(t, f, p.ID)
	if _, _, err := f.s.ApproveRemediation(f.ctx, p.ID, approveRemediationInput(p)); !errors.Is(err, rm.ErrExpired) {
		t.Fatal("expired proposal approved", err)
	}
	stored, err := f.s.Remediation(f.ctx, p.ID)
	if err != nil || stored.State != "expired" || stored.JobID != "" {
		t.Fatal("expired proposal queued work", stored, err)
	}
}

func TestRemediationManualQueuedCancellationAndRejection(t *testing.T) {
	f := remediationFixture(t)
	p := f.proposal(t, "reject-proposal")
	rejected, err := f.s.RejectRemediation(f.ctx, p.ID, rm.RejectInput{Reason: "human rejects exact action"})
	if err != nil || rejected.State != "rejected" {
		t.Fatal(rejected, err)
	}
	if _, _, err = f.s.ApproveRemediation(f.ctx, p.ID, approveRemediationInput(p)); !errors.Is(err, rm.ErrConflict) {
		t.Fatal("rejected proposal reopened", err)
	}
	p = f.proposal(t, "cancel-proposal")
	approved, _, err := f.s.ApproveRemediation(f.ctx, p.ID, approveRemediationInput(p))
	if err != nil {
		t.Fatal(err)
	}
	if cancelled, err := f.s.CancelAutomation(f.ctx, approved.JobID); err != nil || cancelled.State != "cancelled" {
		t.Fatal(cancelled, err)
	}
	stored, err := f.s.Remediation(f.ctx, p.ID)
	if err != nil || stored.State != "cancelled" {
		t.Fatal("queued proposal state not cancelled", stored, err)
	}
	if j, err := f.s.ClaimAutomation(f.ctx, "worker_one"); err != nil || j.ID != "" {
		t.Fatal("rejected/cancelled job reached worker", j, err)
	}
}

func TestRemediationChangedStateAfterClaimCannotDispatchChangedAction(t *testing.T) {
	for _, scenario := range []string{"expected_revision", "policy_version", "binding_alias", "protected_port", "tampered_typed_vlan", "tampered_target_host"} {
		t.Run(scenario, func(t *testing.T) {
			f := remediationFixture(t)
			p := f.proposal(t, "after-claim-proposal")
			j := f.claimed(t, p)
			switch scenario {
			case "expected_revision":
				_, _, err := f.s.PutConfigurationBaseline(f.ctx, f.target.AssetID, cd.BaselineInput{ExpectedRevision: f.baseline.Revision, AssetRevision: 1, SnapshotID: f.baseline.SnapshotID, Confirmed: true, Reason: "expected revision changed after claim"}, "after-claim-baseline")
				if err != nil {
					t.Fatal(err)
				}
			case "policy_version":
				f.policy.Version = "after_claim_v2"
				if err := f.s.ConfigureRemediation(f.ctx, f.policy); err != nil {
					t.Fatal(err)
				}
			case "binding_alias":
				mutateRemediationPort(t, f, func(p *pb.Interface) { p.Alias = "different-port-role" })
			case "protected_port":
				f.s.ConfigureCommands(commands.Policy{ProtectedInterfaces: []string{"12"}})
			case "tampered_typed_vlan":
				j.Restore.TargetVLAN = 40
			case "tampered_target_host":
				j.Target.Host = "192.0.2.99"
			}
			e := &remediationExecutorFixture{configuration: remediationActual()}
			state, _ := f.s.ExecuteRemediation(f.ctx, e, j)
			if scenario == "tampered_typed_vlan" {
				if state != "succeeded" || e.writes != 1 {
					t.Fatal("persisted action reconstruction failed", state, e.writes)
				}
				for _, call := range e.calls {
					if call.Restore != nil && call.Restore.TargetVLAN != p.Action.TargetVLAN {
						t.Fatal("mutated VLAN reached executor", call.Restore)
					}
				}
				return
			}
			if state != "stale" || e.writes != 0 {
				t.Fatal("changed immutable/security state dispatched write", scenario, state, e.writes)
			}
		})
	}
}

func TestRemediationUncertaintyDuringWritePreservesEvidenceAndNeverSucceeds(t *testing.T) {
	for _, scenario := range []string{"cancelled", "deadline", "policy_changed", "expected_changed"} {
		t.Run(scenario, func(t *testing.T) {
			f := remediationFixture(t)
			p := f.proposal(t, "during-write-proposal")
			j := f.claimed(t, p)
			e := &remediationExecutorFixture{configuration: remediationActual()}
			e.writeHook = func() {
				switch scenario {
				case "cancelled":
					if _, err := f.s.CancelAutomation(f.ctx, j.ID); err != nil {
						t.Fatal(err)
					}
				case "deadline":
					if _, err := f.s.pool.Exec(f.ctx, `UPDATE automation_jobs SET deadline=now()-interval '1 second' WHERE id=$1`, j.ID); err != nil {
						t.Fatal(err)
					}
				case "policy_changed":
					f.policy.Version = "during_write_v2"
					if err := f.s.ConfigureRemediation(f.ctx, f.policy); err != nil {
						t.Fatal(err)
					}
				case "expected_changed":
					_, _, err := f.s.PutConfigurationBaseline(f.ctx, f.target.AssetID, cd.BaselineInput{ExpectedRevision: f.baseline.Revision, AssetRevision: 1, SnapshotID: f.baseline.SnapshotID, Confirmed: true, Reason: "expected revision changed during write"}, "during-write-baseline")
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			state, result := f.s.ExecuteRemediation(f.ctx, e, j)
			if err := f.s.FinishAutomation(f.ctx, j, state, result); err != nil {
				t.Fatal(err)
			}
			stored, err := f.s.Remediation(f.ctx, p.ID)
			if err != nil || stored.State != "unknown" || stored.BeforeSnapshotID == "" || e.writes != 1 {
				t.Fatal("uncertain write reported success", stored, e.writes, err)
			}
		})
	}
}

func TestRemediationUnknownRequiresReadOnlyReconciliationAndFreshApproval(t *testing.T) {
	f := remediationFixture(t)
	p := f.proposal(t, "reconcile-original")
	j := f.claimed(t, p)
	if _, err := f.s.pool.Exec(f.ctx, `UPDATE automation_jobs SET deadline=now()-interval '1 minute' WHERE id=$1`, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ClaimAutomation(f.ctx, "worker_one"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.ApproveRemediation(f.ctx, p.ID, approveRemediationInput(p)); err != nil {
		t.Fatal("original one-time replay should return existing unknown job", err)
	}
	// A separately requested existing B10 read creates safe evidence.
	// Reconciliation has no executor and cannot mutate the device.
	readback := createDriftSnapshot(t, f.s, f.ctx, f.target, remediationActual(), "reconciliation-read-only")
	input := rm.ReconcileInput{Confirmed: true, SnapshotID: readback.SnapshotID, Reason: "human reviews fresh safe read after unknown execution"}
	viewerCtx, _ := remediationRole(t, f, "reconcile_viewer", "viewer")
	if _, err := f.s.ReconcileRemediation(viewerCtx, p.ID, input); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("viewer reconciled unknown", err)
	}
	operatorCtx, _ := remediationRole(t, f, "reconcile_no_cap", "operator")
	if _, err := f.s.ReconcileRemediation(operatorCtx, p.ID, input); !errors.Is(err, rm.ErrPolicy) {
		t.Fatal("operator without capability reconciled unknown", err)
	}
	if _, err := f.s.ReconcileRemediation(f.ctx, p.ID, rm.ReconcileInput{Confirmed: true, SnapshotID: p.Action.ActualSnapshotID, Reason: "old read cannot reconcile execution"}); !errors.Is(err, rm.ErrConflict) {
		t.Fatal("old approval evidence accepted as readback", err)
	}
	reconciled, err := f.s.ReconcileRemediation(f.ctx, p.ID, input)
	if err != nil || reconciled.State != "reconciled" || reconciled.Verification == nil || reconciled.Verification.Status != "drift" {
		t.Fatal("safe read-only reconciliation", reconciled, err)
	}
	oldJob, err := f.s.AutomationJob(f.ctx, j.ID)
	if err != nil || oldJob.State != "unknown" {
		t.Fatal("reconciliation falsely marked original write succeeded", oldJob, err)
	}
	if repeated, err := f.s.ReconcileRemediation(f.ctx, p.ID, input); err != nil || repeated.State != "reconciled" {
		t.Fatal("reconcile replay", repeated, err)
	}
	latest, err := f.s.ConfigurationDrift(f.ctx, f.target.AssetID)
	if err != nil || latest.Latest == nil {
		t.Fatal(latest, err)
	}
	f.drift = *latest.Latest
	newProposal := f.proposal(t, "reconcile-new-proposal")
	if err = f.s.AutomationWorker(f.ctx, au.WorkerStatus{ID: "worker_one", Available: true, Templates: []string{f.target.TemplateID, au.RestoreTemplateID}, Versions: map[string]string{"restore_template_sha256": au.RestoreTemplateDigest()}}); err != nil {
		t.Fatal(err)
	}
	approved, replay, err := f.s.ApproveRemediation(f.ctx, newProposal.ID, approveRemediationInput(newProposal))
	if err != nil || replay || approved.JobID == j.ID {
		t.Fatal("new action after reconciliation needs distinct approval/job", approved, replay, err)
	}
}

func TestRemediationOldReadOnlyWorkerCannotClaimApprovedMutation(t *testing.T) {
	f := remediationFixture(t)
	p := f.proposal(t, "mixed-worker-proposal")
	approved, _, err := f.s.ApproveRemediation(f.ctx, p.ID, approveRemediationInput(p))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.AutomationWorker(f.ctx, au.WorkerStatus{ID: "old_b10_worker", Available: true, Templates: []string{f.target.TemplateID}, Versions: map[string]string{"ansible-core": "old-read-only-fixture"}}); err != nil {
		t.Fatal(err)
	}
	if claimed, err := f.s.ClaimAutomation(f.ctx, "old_b10_worker"); err != nil || claimed.ID != "" {
		t.Fatal("old worker can claim mutation and falsely gather-success", claimed, err)
	}
	job, err := f.s.AutomationJob(f.ctx, approved.JobID)
	if err != nil || job.TemplateID != au.RestoreTemplateID || job.State != "queued" {
		t.Fatal("write queue identity is not isolated", job, err)
	}
	newClaim, err := f.s.ClaimAutomation(f.ctx, "worker_one")
	if err != nil || newClaim.ID != approved.JobID || newClaim.Restore == nil || newClaim.Target.TemplateID != f.target.TemplateID {
		t.Fatal("pinned upgraded worker cannot claim mutation", newClaim, err)
	}
}

func TestRemediationQueueExpirySweepsEvenWhenRunnerUnavailable(t *testing.T) {
	f := remediationFixture(t)
	p := f.proposal(t, "offline-queue-proposal")
	approved, _, err := f.s.ApproveRemediation(f.ctx, p.ID, approveRemediationInput(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.pool.Exec(f.ctx, `UPDATE automation_jobs SET created_at=now()-interval '2 minutes' WHERE id=$1`, approved.JobID); err != nil {
		t.Fatal(err)
	}
	if err = f.s.AutomationWorker(f.ctx, au.WorkerStatus{ID: "worker_one", Available: false, ErrorCode: "runner_unavailable"}); err != nil {
		t.Fatal(err)
	}
	stored, err := f.s.Remediation(f.ctx, p.ID)
	if err != nil || stored.State != "stale" {
		t.Fatal("offline Runner left expired work pending", stored, err)
	}
	if claimed, err := f.s.ClaimAutomation(f.ctx, "worker_one"); err != nil || claimed.ID != "" {
		t.Fatal("expired/offline job dispatched", claimed, err)
	}
}
