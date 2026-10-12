package commands

import (
	"bytes"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"testing"
	"time"
)

func TestRollbackTypedWireAndIdentity(t *testing.T) {
	no := false
	r := RollbackRequest{ExpectedAdminUp: &no, TimeoutSeconds: 30, Confirmed: true}
	if err := ValidateRollback(r); err != nil {
		t.Fatal(err)
	}
	c := BuildRollback("cmd", "device", "agent-snapshot", r, time.Now())
	if c.GetRollback() == nil || c.GetInterface() != nil || c.GetRollback().SnapshotId != "agent-snapshot" || c.Initiator != pb.CommandInitiator_COMMAND_INITIATOR_HUMAN {
		t.Fatal("rollback is not exact typed action")
	}
	if bytes.Equal(RollbackFingerprint("origin1", r), RollbackFingerprint("origin2", r)) {
		t.Fatal("rollback origin lost from idempotency")
	}
	r.Confirmed = false
	if ValidateRollback(r) == nil {
		t.Fatal("unconfirmed rollback accepted")
	}
	if ValidateSnapshotID("\ninvalid") == nil {
		t.Fatal("unsafe snapshot ID accepted")
	}
}
