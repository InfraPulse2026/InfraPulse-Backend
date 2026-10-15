package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func commandStorageInventory(at time.Time) *pb.AgentEnvelope {
	return &pb.AgentEnvelope{AgentId: "agent-1", SessionId: "session-1", MessageId: "command-inventory", Sequence: 2,
		SentAt: timestamppb.New(at), Payload: &pb.AgentEnvelope_Inventory{Inventory: &pb.InventoryUpdate{
			ObservedAt: timestamppb.New(at), Devices: []*pb.Device{{Id: "shared-router", Name: "Remote fixture router", Source: "remote", Driver: "routeros-rest", Online: true,
				LastSeen: timestamppb.New(at), ManagementAddress: "https://192.0.2.1", Capabilities: []*pb.Capability{
					{Name: "interfaces.enable", Supported: true}, {Name: "interfaces.disable", Supported: true},
				}}},
		}}}
}

func commandStorageTelemetry(at time.Time) *pb.AgentEnvelope {
	e := sampleEnvelope(at)
	e.MessageId = "command-telemetry"
	e.GetTelemetry().Samples[0].Source = "routeros-rest"
	e.GetTelemetry().Samples[0].Interfaces = []*pb.Interface{{Id: "*2", Name: "ether2", Mac: "02:00:00:00:00:02", AdminUp: true, Metadata: &pb.InterfaceMetadata{AdminStatus: true}}}
	return e
}

func commandStorageFixture(t *testing.T) (*Store, context.Context, access.Principal, time.Time) {
	t.Helper()
	s := integrationStore(t)
	_, admin := adminFixture(t, s)
	s.ConfigureCommands(commands.Policy{Enabled: true, AllowDisable: true, ProtectedInterfaces: []string{"ether1"}})
	at := time.Now().UTC().Truncate(time.Microsecond)
	hello := registration("agent-1", at)
	hello.GetHello().Device.Id = "local:agent-1"
	for _, e := range []*pb.AgentEnvelope{hello, commandStorageInventory(at), commandStorageTelemetry(at)} {
		if err := s.Ingest(t.Context(), e); err != nil {
			t.Fatalf("fixture ingest %s: %v", e.MessageId, err)
		}
	}
	ctx := access.WithPrincipal(t.Context(), admin)
	return s, ctx, admin, at
}

func commandStorageRequest() commands.Request {
	expected := true
	return commands.Request{AgentID: "agent-1", DeviceID: "shared-router", InterfaceID: "ether2", Operation: "disable", ExpectedAdminUp: &expected, TimeoutSeconds: 30, Confirmed: true}
}

func createStorageCommand(t *testing.T, s *Store, ctx context.Context, key string) commands.Record {
	t.Helper()
	c, replay, err := s.CreateCommand(ctx, commandStorageRequest(), key)
	if err != nil || replay {
		t.Fatalf("create command: %+v replay=%t err=%v", c, replay, err)
	}
	return c
}

func assertStorageCommandState(t *testing.T, s *Store, id, state string) commands.Record {
	t.Helper()
	c, err := s.Command(t.Context(), id)
	if err != nil || c.State != state {
		t.Fatalf("command %s state: %+v err=%v; want %s", id, c, err, state)
	}
	return c
}

func commandStorageResult(id, messageID string, at time.Time) *pb.AgentEnvelope {
	observed := false
	return &pb.AgentEnvelope{AgentId: "agent-1", SessionId: "session-1", MessageId: messageID, Sequence: 4, SentAt: timestamppb.New(at),
		Payload: &pb.AgentEnvelope_CommandResult{CommandResult: &pb.CommandResult{CommandId: id, Status: pb.CommandStatus_COMMAND_STATUS_SUCCESS,
			StartedAt: timestamppb.New(at), FinishedAt: timestamppb.New(at), Changed: true, SnapshotId: "agent-owned-snapshot",
			Verification: &pb.InterfaceVerification{InterfaceId: "*2", Name: "ether2", ExpectedAdminUp: false, ObservedAdminUp: &observed, ObservedAt: timestamppb.New(at), Verified: true, Changed: true}}}}
}

func TestCommandsPostgresCreateReplayBusyAndCanonicalWire(t *testing.T) {
	s, ctx, admin, _ := commandStorageFixture(t)
	r := commandStorageRequest()
	c := createStorageCommand(t, s, ctx, "browser-request-1")
	if c.State != "queued" || c.InterfaceID != "*2" || c.ActorID != admin.User.ID || c.SessionID != "session-1" || !c.ExpectedAdminUp {
		t.Fatalf("unresolved target or missing creator provenance: %+v", c)
	}
	replayed, replay, err := s.CreateCommand(ctx, r, "browser-request-1")
	if err != nil || !replay || replayed.ID != c.ID || !replayed.CreatedAt.Equal(c.CreatedAt) {
		t.Fatalf("same request was not idempotent: %+v replay=%t err=%v", replayed, replay, err)
	}
	r.TimeoutSeconds++
	if _, _, err = s.CreateCommand(ctx, r, "browser-request-1"); !errors.Is(err, commands.ErrConflict) {
		t.Fatalf("changed request reused client key: %v", err)
	}
	if _, _, err = s.CreateCommand(ctx, commandStorageRequest(), "other-request"); !errors.Is(err, commands.ErrBusy) {
		t.Fatalf("second command admitted to a busy Agent: %v", err)
	}
	claimed, wire, err := s.ClaimCommand(ctx, c.ID)
	if err != nil || claimed.State != "dispatching" || wire == nil || wire.Id != c.ID || wire.TargetDeviceId != "shared-router" || wire.GetInterface().InterfaceId != "*2" ||
		wire.GetInterface().Operation != pb.InterfaceOperation_INTERFACE_OPERATION_DISABLE || wire.Initiator != pb.CommandInitiator_COMMAND_INITIATOR_HUMAN || wire.IdempotencyKey != c.ID || wire.Timeout.AsDuration() != 30*time.Second || !wire.RequestedAt.AsTime().Equal(c.CreatedAt) {
		t.Fatalf("claim produced incompatible wire command: record=%+v wire=%+v err=%v", claimed, wire, err)
	}
	if _, again, err := s.ClaimCommand(ctx, c.ID); err != nil || again != nil {
		t.Fatalf("claimed command automatically redispatched: %+v err=%v", again, err)
	}
	events, err := s.CommandEvents(ctx, c.ID)
	if err != nil || len(events) != 2 || events[0].State != "queued" || events[1].State != "dispatching" {
		t.Fatalf("state transitions missing or duplicated: %+v err=%v", events, err)
	}
}

func TestCommandsPostgresPreflightRejectsUnsafeOrStaleEvidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*pb.Device, *pb.TelemetrySample, *commands.Request, *commands.Policy)
		want  error
	}{
		{"stale-device-last-seen", func(d *pb.Device, _ *pb.TelemetrySample, _ *commands.Request, _ *commands.Policy) {
			d.LastSeen = timestamppb.New(time.Now().Add(-time.Hour))
		}, commands.ErrPrecondition},
		{"offline-device", func(d *pb.Device, _ *pb.TelemetrySample, _ *commands.Request, _ *commands.Policy) { d.Online = false }, commands.ErrPrecondition},
		{"unsupported-capability", func(d *pb.Device, _ *pb.TelemetrySample, _ *commands.Request, _ *commands.Policy) {
			d.Capabilities[1].Supported = false
		}, commands.ErrPrecondition},
		{"missing-admin-metadata", func(_ *pb.Device, sample *pb.TelemetrySample, _ *commands.Request, _ *commands.Policy) {
			sample.Interfaces[0].Metadata.AdminStatus = false
		}, commands.ErrPrecondition},
		{"missing-admin-metadata-message", func(_ *pb.Device, sample *pb.TelemetrySample, _ *commands.Request, _ *commands.Policy) {
			sample.Interfaces[0].Metadata = nil
		}, commands.ErrPrecondition},
		{"expected-state-mismatch", func(_ *pb.Device, _ *pb.TelemetrySample, r *commands.Request, _ *commands.Policy) {
			value := false
			r.ExpectedAdminUp = &value
		}, commands.ErrPrecondition},
		{"ambiguous-interface-name", func(_ *pb.Device, sample *pb.TelemetrySample, _ *commands.Request, _ *commands.Policy) {
			other := proto.Clone(sample.Interfaces[0]).(*pb.Interface)
			other.Id = "*3"
			sample.Interfaces = append(sample.Interfaces, other)
		}, commands.ErrPrecondition},
		{"protected-canonical-id", func(_ *pb.Device, _ *pb.TelemetrySample, _ *commands.Request, p *commands.Policy) {
			p.ProtectedInterfaces = []string{"*2"}
		}, commands.ErrProtected},
		{"protected-case-insensitive-mac", func(_ *pb.Device, sample *pb.TelemetrySample, _ *commands.Request, p *commands.Policy) {
			sample.Interfaces[0].Mac = "02:AB:CD:00:00:02"
			p.ProtectedInterfaces = []string{"02:ab:cd:00:00:02"}
		}, commands.ErrProtected},
		{"management-address", func(_ *pb.Device, sample *pb.TelemetrySample, _ *commands.Request, _ *commands.Policy) {
			sample.Interfaces[0].Addresses = []string{"192.0.2.1/24"}
		}, commands.ErrProtected},
		{"dispatch-disabled", func(_ *pb.Device, _ *pb.TelemetrySample, _ *commands.Request, p *commands.Policy) { p.Enabled = false }, commands.ErrDisabled},
		{"disable-disabled", func(_ *pb.Device, _ *pb.TelemetrySample, _ *commands.Request, p *commands.Policy) {
			p.AllowDisable = false
		}, commands.ErrDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ctx, _, at := commandStorageFixture(t)
			inv, telemetry := commandStorageInventory(at.Add(time.Millisecond)), commandStorageTelemetry(at.Add(time.Millisecond))
			inv.MessageId, telemetry.MessageId = "changed-inventory", "changed-telemetry"
			r, policy := commandStorageRequest(), s.CommandPolicy()
			tc.alter(inv.GetInventory().Devices[0], telemetry.GetTelemetry().Samples[0], &r, &policy)
			s.ConfigureCommands(policy)
			for _, e := range []*pb.AgentEnvelope{inv, telemetry} {
				if err := s.Ingest(ctx, e); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := s.CreateCommand(ctx, r, "unsafe-request"); !errors.Is(err, tc.want) {
				t.Fatalf("unsafe evidence admitted: err=%v; want %v", err, tc.want)
			}
			var count int
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM controller_commands`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("failed precondition persisted a command: %d err=%v", count, err)
			}
		})
	}
}

func TestCommandsPostgresClaimRechecksRevocationEvidenceAndSession(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *Store, context.Context, access.Principal)
	}{
		{"revoked-session", func(t *testing.T, s *Store, ctx context.Context, p access.Principal) {
			if err := s.AccessLogout(ctx, p, false, access.Meta{}); err != nil {
				t.Fatal(err)
			}
		}},
		{"changed-admin-state", func(t *testing.T, s *Store, ctx context.Context, _ access.Principal) {
			e := commandStorageTelemetry(time.Now().UTC())
			e.MessageId = "changed-before-claim"
			e.GetTelemetry().Samples[0].Interfaces[0].AdminUp = false
			if err := s.Ingest(ctx, e); err != nil {
				t.Fatal(err)
			}
		}},
		{"reconnected-session", func(t *testing.T, s *Store, ctx context.Context, _ access.Principal) {
			if _, err := s.pool.Exec(ctx, `UPDATE agents SET session_id='new-session' WHERE agent_id='agent-1'`); err != nil {
				t.Fatal(err)
			}
		}},
		{"stale-telemetry", func(t *testing.T, s *Store, ctx context.Context, _ access.Principal) {
			if _, err := s.pool.Exec(ctx, `UPDATE telemetry SET observed_at=now()-interval '1 minute'`); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ctx, p, _ := commandStorageFixture(t)
			c := createStorageCommand(t, s, ctx, "claim-request")
			tc.change(t, s, ctx, p)
			got, wire, err := s.ClaimCommand(ctx, c.ID)
			if err != nil || wire != nil || got.State != "rejected" {
				t.Fatalf("unsafe pending command dispatched: %+v wire=%+v err=%v", got, wire, err)
			}
			persisted := assertStorageCommandState(t, s, c.ID, "rejected")
			if persisted.DispatchedAt != nil {
				t.Fatal("rejection acquired an execution timestamp")
			}
		})
	}
}

func TestCommandsPostgresFastResultSendCASReplayAndTerminalIsolation(t *testing.T) {
	s, ctx, _, at := commandStorageFixture(t)
	c := createStorageCommand(t, s, ctx, "fast-result")
	if _, wire, err := s.ClaimCommand(ctx, c.ID); err != nil || wire == nil {
		t.Fatalf("claim: %v", err)
	}
	e := commandStorageResult(c.ID, "fast-agent-result", at)
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishCommandSend(ctx, c.ID, true, nil); err != nil {
		t.Fatal(err)
	}
	got := assertStorageCommandState(t, s, c.ID, "success")
	if got.Result == nil || got.Result.SnapshotID != "agent-owned-snapshot" || got.Result.Verification == nil || got.Result.Verification.ObservedAdminUp == nil || *got.Result.Verification.ObservedAdminUp || got.Result.Uncertain {
		t.Fatalf("verified false/snapshot presence lost: %+v", got.Result)
	}
	replay := proto.Clone(e).(*pb.AgentEnvelope)
	replay.Sequence++
	replay.SentAt = timestamppb.New(at.Add(time.Second))
	if err := s.Ingest(ctx, replay); err != nil {
		t.Fatalf("same message replay rejected: %v", err)
	}
	duplicate := proto.Clone(e).(*pb.AgentEnvelope)
	duplicate.MessageId = "same-business-new-envelope"
	if err := s.Ingest(ctx, duplicate); err != nil {
		t.Fatal(err)
	}
	conflict := proto.Clone(e).(*pb.AgentEnvelope)
	conflict.MessageId = "conflicting-terminal-result"
	conflict.GetCommandResult().Status = pb.CommandStatus_COMMAND_STATUS_FAILED
	if err := s.Ingest(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, registration("agent-2", at)); err != nil {
		t.Fatal(err)
	}
	wrong := proto.Clone(conflict).(*pb.AgentEnvelope)
	wrong.AgentId, wrong.MessageId = "agent-2", "wrong-agent-result"
	if err := s.Ingest(ctx, wrong); err != nil {
		t.Fatal(err)
	}
	orphan := commandStorageResult("unknown-controller-command", "orphan-agent-result", at)
	if err := s.Ingest(ctx, orphan); err != nil {
		t.Fatalf("orphan result stopped telemetry transport: %v", err)
	}
	assertStorageCommandState(t, s, c.ID, "success")
	for _, tc := range []struct{ agent, message, disposition string }{
		{"agent-1", duplicate.MessageId, "duplicate_result"}, {"agent-1", conflict.MessageId, "conflicting_terminal_ignored"}, {"agent-2", wrong.MessageId, "agent_mismatch"}, {"agent-1", orphan.MessageId, "unmatched_command"},
	} {
		var disposition string
		if err := s.pool.QueryRow(ctx, `SELECT disposition FROM command_results WHERE agent_id=$1 AND message_id=$2`, tc.agent, tc.message).Scan(&disposition); err != nil || disposition != tc.disposition {
			t.Fatalf("result provenance %s: %q err=%v", tc.message, disposition, err)
		}
	}
	events, err := s.CommandEvents(ctx, c.ID)
	if err != nil || len(events) != 3 || events[2].State != "success" {
		t.Fatalf("replay/CAS/conflict changed event history: %+v err=%v", events, err)
	}
}

func TestCommandsPostgresResultFailureRollsBackInboxProjectionAndTransition(t *testing.T) {
	s, ctx, _, at := commandStorageFixture(t)
	c := createStorageCommand(t, s, ctx, "atomic-result")
	if _, wire, err := s.ClaimCommand(ctx, c.ID); err != nil || wire == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.FinishCommandSend(ctx, c.ID, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE command_events ADD CONSTRAINT reject_fixture_success CHECK(state <> 'success')`); err != nil {
		t.Fatal(err)
	}
	e := commandStorageResult(c.ID, "must-rollback-result", at)
	if err := s.Ingest(ctx, e); err == nil {
		t.Fatal("failed state event allowed a partial result commit")
	}
	assertStorageCommandState(t, s, c.ID, "sent")
	var inbox, projections, successEvents int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_messages WHERE message_id=$1),(SELECT count(*) FROM command_results WHERE message_id=$1),(SELECT count(*) FROM command_events WHERE state='success')`, e.MessageId).Scan(&inbox, &projections, &successEvents); err != nil || inbox != 0 || projections != 0 || successEvents != 0 {
		t.Fatalf("partial command commit: inbox=%d projection=%d events=%d err=%v", inbox, projections, successEvents, err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE command_events DROP CONSTRAINT reject_fixture_success`); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatalf("rollback poisoned result retry: %v", err)
	}
	assertStorageCommandState(t, s, c.ID, "success")
}

func TestCommandsPostgresUnverifiedSuccessNeverClaimsSuccess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*pb.CommandResult)
	}{
		{"missing-verification", func(r *pb.CommandResult) { r.Verification = nil }},
		{"missing-observed-false", func(r *pb.CommandResult) { r.Verification.ObservedAdminUp = nil }},
		{"missing-observation-time", func(r *pb.CommandResult) { r.Verification.ObservedAt = nil }},
		{"missing-execution-start-time", func(r *pb.CommandResult) { r.StartedAt = nil }},
		{"missing-execution-finish-time", func(r *pb.CommandResult) { r.FinishedAt = nil }},
		{"execution-times-reversed", func(r *pb.CommandResult) { r.FinishedAt = timestamppb.New(r.StartedAt.AsTime().Add(-time.Second)) }},
		{"unverified", func(r *pb.CommandResult) { r.Verification.Verified = false }},
		{"wrong-resource", func(r *pb.CommandResult) { r.Verification.InterfaceId = "*1" }},
		{"wrong-observed-state", func(r *pb.CommandResult) { value := true; r.Verification.ObservedAdminUp = &value }},
		{"explicit-uncertainty", func(r *pb.CommandResult) { r.Uncertain = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ctx, _, at := commandStorageFixture(t)
			c := createStorageCommand(t, s, ctx, "unverified-result")
			if _, wire, err := s.ClaimCommand(ctx, c.ID); err != nil || wire == nil {
				t.Fatalf("claim: %v", err)
			}
			e := commandStorageResult(c.ID, "unverified-agent-result", at)
			tc.change(e.GetCommandResult())
			if err := s.Ingest(ctx, e); err != nil {
				t.Fatal(err)
			}
			got := assertStorageCommandState(t, s, c.ID, "uncertain")
			if got.Result == nil || !got.Result.Uncertain {
				t.Fatalf("public result hides uncertainty: %+v", got.Result)
			}
			if _, _, err := s.CreateCommand(ctx, commandStorageRequest(), "blocked-by-uncertainty"); !errors.Is(err, commands.ErrBusy) {
				t.Fatalf("unresolved mutation did not block Agent: %v", err)
			}
		})
	}
}

func TestCommandsPostgresFutureClockCannotReplaceQualifiedInterfaceEvidence(t *testing.T) {
	for _, onlyFuture := range []bool{false, true} {
		name := "future-after-valid"
		if onlyFuture {
			name = "future-only-then-valid"
		}
		t.Run(name, func(t *testing.T) {
			s, ctx, _, at := commandStorageFixture(t)
			if onlyFuture {
				// This schema belongs exclusively to this test. Remove the normal
				// sample to prove a skewed sample alone never authorizes mutation.
				if _, err := s.pool.Exec(ctx, `DELETE FROM telemetry WHERE agent_id='agent-1' AND device_id='shared-router'`); err != nil {
					t.Fatal(err)
				}
			}
			future := commandStorageTelemetry(at.Add(2 * time.Hour))
			future.MessageId = "future-clock-command-sample"
			future.GetTelemetry().Samples[0].Interfaces[0].AdminUp = false
			if err := s.Ingest(ctx, future); err != nil {
				t.Fatal(err)
			}
			if onlyFuture {
				if _, _, err := s.CreateCommand(ctx, commandStorageRequest(), "future-only-request"); !errors.Is(err, commands.ErrPrecondition) {
					t.Fatalf("future-only telemetry authorized mutation: %v", err)
				}
			}
			valid := commandStorageTelemetry(time.Now().UTC())
			valid.MessageId = "qualified-clock-command-sample"
			if err := s.Ingest(ctx, valid); err != nil {
				t.Fatal(err)
			}
			c := createStorageCommand(t, s, ctx, "qualified-clock-request")
			if _, wire, err := s.ClaimCommand(ctx, c.ID); err != nil || wire == nil || wire.GetInterface().InterfaceId != "*2" {
				t.Fatalf("future sample blocked later qualified evidence: wire=%+v err=%v", wire, err)
			}
		})
	}
}

func TestCommandsPostgresSendDeadlineAndRestartNeverRetryMutations(t *testing.T) {
	for _, phase := range []string{"send-uncertain", "not-attempted", "deadline-queued", "deadline-sent", "restart-queued", "restart-dispatching", "restart-sent"} {
		t.Run(phase, func(t *testing.T) {
			s, ctx, _, at := commandStorageFixture(t)
			c := createStorageCommand(t, s, ctx, "lifecycle-request")
			if phase != "deadline-queued" && phase != "restart-queued" {
				if _, wire, err := s.ClaimCommand(ctx, c.ID); err != nil || wire == nil {
					t.Fatalf("claim: %v", err)
				}
			}
			want := "uncertain"
			switch phase {
			case "send-uncertain":
				if err := s.FinishCommandSend(ctx, c.ID, true, errors.New("fixture transport interrupted")); err != nil {
					t.Fatal(err)
				}
			case "not-attempted":
				want = "rejected"
				if err := s.FinishCommandSend(ctx, c.ID, false, errors.New("fixture session missing")); err != nil {
					t.Fatal(err)
				}
			case "deadline-queued", "deadline-sent":
				if phase == "deadline-sent" {
					if err := s.FinishCommandSend(ctx, c.ID, true, nil); err != nil {
						t.Fatal(err)
					}
				} else {
					want = "expired"
				}
				if _, err := s.pool.Exec(ctx, `UPDATE controller_commands SET deadline_at=now()-interval '10 seconds' WHERE id=$1`, c.ID); err != nil {
					t.Fatal(err)
				}
				if err := s.ExpireCommands(ctx); err != nil {
					t.Fatal(err)
				}
			case "restart-queued", "restart-dispatching", "restart-sent":
				if phase == "restart-sent" {
					if err := s.FinishCommandSend(ctx, c.ID, true, nil); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "restart-queued" {
					want = "rejected"
				}
				if err := s.RecoverCommands(ctx); err != nil {
					t.Fatal(err)
				}
			}
			assertStorageCommandState(t, s, c.ID, want)
			ids, err := s.QueuedCommands(ctx)
			if err != nil || len(ids) != 0 {
				t.Fatalf("automatic retry after ambiguous delivery: %+v err=%v", ids, err)
			}
			if _, wire, err := s.ClaimCommand(ctx, c.ID); err != nil || wire != nil {
				t.Fatalf("terminal/uncertain claim replayed wire: %+v err=%v", wire, err)
			}
			if want == "uncertain" {
				if _, _, err := s.CreateCommand(ctx, commandStorageRequest(), "while-uncertain"); !errors.Is(err, commands.ErrBusy) {
					t.Fatalf("uncertainty admitted another mutation: %v", err)
				}
				if err := s.Ingest(ctx, commandStorageResult(c.ID, "late-definitive-result", at)); err != nil {
					t.Fatal(err)
				}
				assertStorageCommandState(t, s, c.ID, "success")
				createStorageCommand(t, s, ctx, "after-definitive-result")
			}
		})
	}
}

func TestCommandsPostgresCreationAuditFailureIsAtomic(t *testing.T) {
	s, ctx, admin, _ := commandStorageFixture(t)
	if _, err := s.pool.Exec(ctx, `ALTER TABLE access_audit ADD CONSTRAINT reject_fixture_command_audit CHECK(action <> 'command_created')`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateCommand(ctx, commandStorageRequest(), "audit-must-commit"); err == nil {
		t.Fatal("unaudited command committed")
	}
	var commandsCount, eventsCount int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM controller_commands),(SELECT count(*) FROM command_events)`).Scan(&commandsCount, &eventsCount); err != nil || commandsCount != 0 || eventsCount != 0 {
		t.Fatalf("failed creation leaked command/events: %d/%d err=%v", commandsCount, eventsCount, err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE access_audit DROP CONSTRAINT reject_fixture_command_audit`); err != nil {
		t.Fatal(err)
	}
	if err := s.AccessLogout(ctx, admin, false, access.Meta{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateCommand(ctx, commandStorageRequest(), "revoked-session"); !errors.Is(err, access.ErrForbidden) {
		t.Fatalf("revoked actor created mutation: %v", err)
	}
}
