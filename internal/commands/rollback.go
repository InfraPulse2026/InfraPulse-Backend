package commands

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"time"
)

var ErrSnapshot = errors.New("snapshot is unavailable or incompatible")

func ValidateSnapshotID(id string) error { return identifier(id, 128) }

type RollbackRequest struct {
	ExpectedAdminUp *bool `json:"expected_admin_up"`
	TimeoutSeconds  int   `json:"timeout_seconds"`
	Confirmed       bool  `json:"confirmed"`
}

func ValidateRollback(r RollbackRequest) error {
	if r.ExpectedAdminUp == nil || !r.Confirmed || r.TimeoutSeconds < 1 || r.TimeoutSeconds > 60 {
		return errors.New("rollback requires confirmed=true, initial expected_admin_up and timeout_seconds 1..60")
	}
	return nil
}
func RollbackFingerprint(original string, r RollbackRequest) []byte {
	encoded, _ := json.Marshal(struct {
		Kind     string
		Original string
		Request  RollbackRequest
	}{"rollback", original, r})
	hash := sha256.Sum256(encoded)
	return hash[:]
}
func BuildRollback(id, device, snapshot string, r RollbackRequest, at time.Time) *pb.Command {
	return &pb.Command{Id: id, TargetDeviceId: device, RequestedAt: timestamppb.New(at.UTC()), Timeout: durationpb.New(time.Duration(r.TimeoutSeconds) * time.Second), Initiator: pb.CommandInitiator_COMMAND_INITIATOR_HUMAN, IdempotencyKey: id, Action: &pb.Command_Rollback{Rollback: &pb.RollbackAction{SnapshotId: snapshot}}}
}

type Snapshot struct {
	AgentID                string     `json:"agent_id"`
	SnapshotID             string     `json:"snapshot_id"`
	OriginCommandID        string     `json:"origin_command_id"`
	DeviceID               string     `json:"device_id"`
	InterfaceID            string     `json:"interface_id"`
	Status                 string     `json:"status"`
	Reason                 string     `json:"reason"`
	InitialObservedAdminUp bool       `json:"initial_observed_admin_up"`
	AfterAdminUp           *bool      `json:"after_admin_up"`
	PreviousStateBasis     string     `json:"previous_state_basis"`
	RecordedAt             time.Time  `json:"recorded_at"`
	ExpiresAt              *time.Time `json:"expires_at"`
	ExpiryBasis            string     `json:"expiry_basis"`
	RollbackCommandID      *string    `json:"rollback_command_id"`
}
