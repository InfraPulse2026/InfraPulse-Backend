package storage

import (
	"context"
	"errors"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"testing"
	"time"
)

func rollbackFixture(t *testing.T) (*Store, context.Context, commands.Record, time.Time) {
	t.Helper()
	s, ctx, _, at := commandStorageFixture(t)
	s.ConfigureCommands(commands.Policy{Enabled: true, AllowDisable: true, AllowRollback: true, ProtectedInterfaces: []string{"ether1"}})
	inv := commandStorageInventory(at)
	inv.MessageId = "rollback-capability"
	inv.GetInventory().Devices[0].Capabilities = append(inv.GetInventory().Devices[0].Capabilities, &pb.Capability{Name: "resource.rollback", Supported: true})
	if err := s.Ingest(ctx, inv); err != nil {
		t.Fatal(err)
	}
	original := createStorageCommand(t, s, ctx, "initial-disable")
	if _, wire, err := s.ClaimCommand(ctx, original.ID); err != nil || wire == nil {
		t.Fatal(err)
	}
	if err := s.FinishCommandSend(ctx, original.ID, true, nil); err != nil {
		t.Fatal(err)
	}
	result := commandStorageResult(original.ID, "snapshot-original-result", at)
	result.GetCommandResult().SnapshotId = "snapshot-original"
	if err := s.Ingest(ctx, result); err != nil {
		t.Fatal(err)
	}
	tel := commandStorageTelemetry(at.Add(time.Millisecond))
	tel.MessageId = "after-disable-telemetry"
	tel.GetTelemetry().Samples[0].Interfaces[0].AdminUp = false
	if err := s.Ingest(ctx, tel); err != nil {
		t.Fatal(err)
	}
	original = assertStorageCommandState(t, s, original.ID, "success")
	return s, ctx, original, at
}
func rollbackRequest() commands.RollbackRequest {
	no := false
	return commands.RollbackRequest{ExpectedAdminUp: &no, TimeoutSeconds: 30, Confirmed: true}
}
func rollbackSuccess(id, message string, at time.Time) *pb.AgentEnvelope {
	e := commandStorageResult(id, message, at)
	yes := true
	e.GetCommandResult().SnapshotId = "snapshot-from-rollback"
	e.GetCommandResult().Verification.ExpectedAdminUp = true
	e.GetCommandResult().Verification.ObservedAdminUp = &yes
	return e
}
func TestRollbackPostgresTypedOwnershipAndConsumption(t *testing.T) {
	s, ctx, parent, at := rollbackFixture(t)
	snapshot, err := s.CommandSnapshot(ctx, parent.ID)
	if err != nil || snapshot.Status != "candidate" || snapshot.ExpiresAt != nil || snapshot.ExpiryBasis != "not_transmitted_by_agent" || snapshot.PreviousStateBasis == "agent_snapshot" {
		t.Fatalf("invented snapshot metadata %+v %v", snapshot, err)
	}
	c, replay, err := s.CreateRollback(ctx, parent.ID, rollbackRequest(), "undo-once")
	if err != nil || replay || c.Operation != "rollback" || c.RollbackOfCommandID == nil || *c.RollbackOfCommandID != parent.ID {
		t.Fatalf("rollback creation %+v %v", c, err)
	}
	_, wire, err := s.ClaimCommand(ctx, c.ID)
	if err != nil || wire == nil || wire.GetRollback() == nil || wire.GetInterface() != nil || wire.GetRollback().SnapshotId != "snapshot-original" || wire.TargetDeviceId != parent.DeviceID {
		t.Fatal("rollback replaced by ordinary reverse interface command")
	}
	if err = s.Ingest(ctx, rollbackSuccess(c.ID, "rollback-success", at.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishCommandSend(ctx, c.ID, true, nil); err != nil {
		t.Fatal(err)
	}
	assertStorageCommandState(t, s, c.ID, "success")
	snapshot, err = s.CommandSnapshot(ctx, parent.ID)
	if err != nil || snapshot.Status != "consumed" {
		t.Fatal("original snapshot not consumed")
	}
	next, err := s.CommandSnapshot(ctx, c.ID)
	if err != nil || next.Status != "candidate" || next.AfterAdminUp == nil || !*next.AfterAdminUp {
		t.Fatal("Agent's new rollback snapshot missing")
	}
	same, replay, err := s.CreateRollback(ctx, parent.ID, rollbackRequest(), "undo-once")
	if err != nil || !replay || same.ID != c.ID {
		t.Fatal("idempotent rollback repeated execution")
	}
	if _, _, err = s.CreateRollback(ctx, parent.ID, rollbackRequest(), "undo-twice"); !errors.Is(err, commands.ErrSnapshot) {
		t.Fatal("consumed snapshot reused")
	}
}
func TestRollbackPostgresPreflightDriftFlagsAndSnapshotClaim(t *testing.T) {
	for _, mode := range []string{"changed-state", "rollback-disabled", "missing-capability", "claim-changed-snapshot"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx, parent, at := rollbackFixture(t)
			switch mode {
			case "changed-state":
				tel := commandStorageTelemetry(at.Add(2 * time.Millisecond))
				tel.MessageId = "drift"
				if err := s.Ingest(ctx, tel); err != nil {
					t.Fatal(err)
				}
			case "rollback-disabled":
				p := s.CommandPolicy()
				p.AllowRollback = false
				s.ConfigureCommands(p)
			case "missing-capability":
				inv := commandStorageInventory(at.Add(time.Millisecond))
				inv.MessageId = "lost-capability"
				if err := s.Ingest(ctx, inv); err != nil {
					t.Fatal(err)
				}
			}
			c, _, err := s.CreateRollback(ctx, parent.ID, rollbackRequest(), "undo")
			if mode != "claim-changed-snapshot" {
				if err == nil {
					t.Fatal("unsafe rollback admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.pool.Exec(ctx, `UPDATE command_snapshots SET status='superseded' WHERE origin_command_id=$1`, parent.ID); err != nil {
				t.Fatal(err)
			}
			record, wire, err := s.ClaimCommand(ctx, c.ID)
			if err != nil || wire != nil || record.State != "rejected" {
				t.Fatal("claim ignored snapshot change")
			}
		})
	}
}
func TestRollbackPostgresUnknownOutcomeAndUnsentRelease(t *testing.T) {
	for _, attempted := range []bool{false, true} {
		t.Run(map[bool]string{false: "never-sent", true: "maybe-sent"}[attempted], func(t *testing.T) {
			s, ctx, parent, _ := rollbackFixture(t)
			c, _, err := s.CreateRollback(ctx, parent.ID, rollbackRequest(), "undo")
			if err != nil {
				t.Fatal(err)
			}
			if _, wire, err := s.ClaimCommand(ctx, c.ID); err != nil || wire == nil {
				t.Fatal(err)
			}
			if err = s.FinishCommandSend(ctx, c.ID, attempted, errors.New("fixture transport failure")); err != nil {
				t.Fatal(err)
			}
			snapshot, err := s.CommandSnapshot(ctx, parent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if attempted {
				if snapshot.Status != "uncertain" {
					t.Fatal("possible rollback consumption was forgotten")
				}
				if _, _, err = s.CreateRollback(ctx, parent.ID, rollbackRequest(), "retry"); err == nil {
					t.Fatal("uncertain rollback automatically repeated")
				}
			} else {
				if snapshot.Status != "candidate" {
					t.Fatal("never-sent reservation permanently consumed")
				}
				if _, _, err = s.CreateRollback(ctx, parent.ID, rollbackRequest(), "new-explicit-intent"); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestRollbackPostgresSnapshotProjectionIsAtomic(t *testing.T) {
	s, ctx, parent, at := rollbackFixture(t)
	c, _, err := s.CreateRollback(ctx, parent.ID, rollbackRequest(), "undo")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ClaimCommand(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `ALTER TABLE command_snapshots ADD CONSTRAINT reject_fixture_consumed CHECK(status <> 'consumed')`); err != nil {
		t.Fatal(err)
	}
	e := rollbackSuccess(c.ID, "rollback-failed-db", at.Add(time.Second))
	if err = s.Ingest(ctx, e); err == nil {
		t.Fatal("snapshot failure acknowledged")
	}
	assertStorageCommandState(t, s, c.ID, "dispatching")
	var messages int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM agent_messages WHERE message_id='rollback-failed-db'`).Scan(&messages); err != nil || messages != 0 {
		t.Fatal("partial rollback result committed")
	}
	snapshot, err := s.CommandSnapshot(ctx, parent.ID)
	if err != nil || snapshot.Status != "reserved" {
		t.Fatal("snapshot transition survived rollback")
	}
}
func TestRollbackPostgresNewMutationSupersedesOldReference(t *testing.T) {
	s, ctx, parent, at := rollbackFixture(t)
	no := false
	req := commandStorageRequest()
	req.Operation = "enable"
	req.ExpectedAdminUp = &no
	c, _, err := s.CreateCommand(ctx, req, "new-mutation")
	if err != nil {
		t.Fatal(err)
	}
	if _, wire, err := s.ClaimCommand(ctx, c.ID); err != nil || wire == nil {
		t.Fatal(err)
	}
	if err = s.Ingest(ctx, rollbackSuccess(c.ID, "newer-snapshot", at.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	old, err := s.CommandSnapshot(ctx, parent.ID)
	if err != nil || old.Status != "superseded" {
		t.Fatal("ABA restored an old rollback candidate")
	}
	if _, _, err = s.CreateRollback(ctx, parent.ID, rollbackRequest(), "undo-obsolete"); !errors.Is(err, commands.ErrSnapshot) {
		t.Fatal("superseded snapshot admitted")
	}
}
func TestRollbackPostgresAgentRejectionPreserved(t *testing.T) {
	s, ctx, parent, at := rollbackFixture(t)
	c, _, err := s.CreateRollback(ctx, parent.ID, rollbackRequest(), "undo")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ClaimCommand(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	e := commandStorageResult(c.ID, "agent-expired", at)
	e.GetCommandResult().Status = pb.CommandStatus_COMMAND_STATUS_REJECTED
	e.GetCommandResult().ErrorCode = "snapshot_expired"
	e.GetCommandResult().SnapshotId = ""
	e.GetCommandResult().Verification = nil
	if err = s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	result := assertStorageCommandState(t, s, c.ID, "rejected")
	if result.Result.ErrorCode != "snapshot_expired" {
		t.Fatal("Agent rejection replaced by assumed TTL")
	}
	snapshot, err := s.CommandSnapshot(ctx, parent.ID)
	if err != nil || snapshot.Status != "unavailable" || snapshot.ExpiresAt != nil {
		t.Fatal("invented expiry instead of Agent evidence")
	}
}

func TestRollbackPostgresMigrationPreservesStage6Data(t *testing.T) {
	s, ctx, parent, _ := rollbackFixture(t)
	// Reproduce the published stage6 shape in this test's expendable schema.
	_, err := s.pool.Exec(ctx, `DROP TABLE command_snapshots;
 ALTER TABLE controller_commands DROP COLUMN rollback_of_command_id,DROP COLUMN rollback_snapshot_id;
 ALTER TABLE controller_commands DROP CONSTRAINT controller_commands_operation_check;
 ALTER TABLE controller_commands ADD CONSTRAINT controller_commands_operation_check CHECK(operation IN ('enable','disable'));
 DELETE FROM schema_migrations WHERE version=10;`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	command, err := s.Command(ctx, parent.ID)
	if err != nil || command.State != "success" || command.Result.SnapshotID != "snapshot-original" {
		t.Fatal("existing command was lost")
	}
	snapshot, err := s.CommandSnapshot(ctx, parent.ID)
	if err != nil || snapshot.Status != "candidate" || snapshot.ExpiresAt != nil {
		t.Fatal("old observed reference not backfilled safely")
	}
	var users int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM access_users`).Scan(&users); err != nil || users != 1 {
		t.Fatal("migration changed user accounts")
	}
}
