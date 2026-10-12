// Package commands defines the bounded, typed interface commands accepted by
// the controller. It never interprets a shell command or executes an action.
package commands

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Request includes the expected initial interface state. That precondition is
// checked by the backend against qualified telemetry; the current Agent wire contract has
// no expected-state field, so it is not sent as an invented protobuf field.
type Request struct {
	AgentID         string `json:"agent_id"`
	DeviceID        string `json:"device_id"`
	InterfaceID     string `json:"interface_id"`
	Operation       string `json:"operation"`
	ExpectedAdminUp *bool  `json:"expected_admin_up"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
	Confirmed       bool   `json:"confirmed"`
}

func ValidateRequest(r Request) error {
	for _, field := range []struct {
		name, value string
		max         int
	}{{"agent_id", r.AgentID, 256}, {"device_id", r.DeviceID, 128}, {"interface_id", r.InterfaceID, 128}} {
		if err := identifier(field.value, field.max); err != nil {
			return fmt.Errorf("invalid %s: %w", field.name, err)
		}
	}
	if r.Operation != "enable" && r.Operation != "disable" {
		return errors.New("operation must be enable or disable")
	}
	if r.ExpectedAdminUp == nil {
		return errors.New("expected_admin_up must be provided as true or false")
	}
	if r.TimeoutSeconds < 1 || r.TimeoutSeconds > 60 {
		return errors.New("timeout_seconds must be an integer between 1 and 60")
	}
	if !r.Confirmed {
		return errors.New("confirmed must be true")
	}
	return nil
}

// ValidateKey accepts a header-safe ASCII key without modifying its identity.
func ValidateKey(key string) error {
	if len(key) < 1 || len(key) > 128 {
		return errors.New("Idempotency-Key must contain 1 to 128 printable ASCII bytes")
	}
	for _, b := range []byte(key) {
		if b < 32 || b > 126 {
			return errors.New("Idempotency-Key must contain 1 to 128 printable ASCII bytes")
		}
	}
	return nil
}

func identifier(value string, limit int) error {
	if strings.TrimSpace(value) == "" || len(value) > limit || !utf8.ValidString(value) {
		return errors.New("identifier is missing, invalid UTF-8, or too long")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return errors.New("identifier contains control characters")
		}
	}
	return nil
}

// Build is called after validation and uses the server's command ID and clock.
func Build(id, key string, r Request, created time.Time) *pb.Command {
	operation := pb.InterfaceOperation_INTERFACE_OPERATION_UNSPECIFIED
	switch r.Operation {
	case "enable":
		operation = pb.InterfaceOperation_INTERFACE_OPERATION_ENABLE
	case "disable":
		operation = pb.InterfaceOperation_INTERFACE_OPERATION_DISABLE
	}
	return &pb.Command{
		Id: id, TargetDeviceId: r.DeviceID,
		RequestedAt: timestamppb.New(created.UTC()),
		Timeout:     durationpb.New(time.Duration(r.TimeoutSeconds) * time.Second),
		Initiator:   pb.CommandInitiator_COMMAND_INITIATOR_HUMAN, IdempotencyKey: key,
		Action: &pb.Command_Interface{Interface: &pb.InterfaceAction{InterfaceId: r.InterfaceID, Operation: operation}},
	}
}

// FingerprintRequest uses deterministic struct JSON, including the false/null
// distinction in expected_admin_up, without agent/session delivery metadata.
func FingerprintRequest(r Request) []byte {
	encoded, _ := json.Marshal(r) // All Request fields have infallible JSON encodings.
	hash := sha256.Sum256(encoded)
	return hash[:]
}

type Record struct {
	ID                  string     `json:"id"`
	AgentID             string     `json:"agent_id"`
	DeviceID            string     `json:"device_id"`
	InterfaceID         string     `json:"interface_id"`
	Operation           string     `json:"operation"`
	ExpectedAdminUp     bool       `json:"expected_admin_up"`
	TimeoutSeconds      int        `json:"timeout_seconds"`
	IdempotencyKey      string     `json:"idempotency_key"`
	ActorID             string     `json:"actor_id"`
	State               string     `json:"state"`
	Reason              string     `json:"reason"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	DeadlineAt          time.Time  `json:"deadline_at"`
	SessionID           string     `json:"session_id"`
	DispatchedAt        *time.Time `json:"dispatched_at"`
	Result              *Result    `json:"result"`
	RollbackOfCommandID *string    `json:"rollback_of_command_id"`
	RollbackSnapshotID  *string    `json:"rollback_snapshot_id"`
}

type Result struct {
	MessageID    string        `json:"message_id"`
	Status       string        `json:"status"`
	StartedAt    *time.Time    `json:"started_at"`
	FinishedAt   *time.Time    `json:"finished_at"`
	Changed      bool          `json:"changed"`
	Uncertain    bool          `json:"uncertain"`
	ErrorCode    string        `json:"error_code"`
	ErrorMessage string        `json:"error_message"`
	SnapshotID   string        `json:"snapshot_id"`
	Verification *Verification `json:"verification"`
}

type Verification struct {
	InterfaceID     string     `json:"interface_id"`
	Name            string     `json:"name"`
	ExpectedAdminUp bool       `json:"expected_admin_up"`
	ObservedAdminUp *bool      `json:"observed_admin_up"`
	ObservedAt      *time.Time `json:"observed_at"`
	Verified        bool       `json:"verified"`
	Changed         bool       `json:"changed"`
	Uncertain       bool       `json:"uncertain"`
	Evidence        string     `json:"evidence"`
}

// ResultState preserves Agent statuses, except that explicit uncertainty always
// wins. A timeout can be definitive when the Agent never attempted mutation.
func ResultState(r *pb.CommandResult) string {
	if r == nil {
		return "unspecified"
	}
	if r.Uncertain || r.Verification.GetUncertain() {
		return "uncertain"
	}
	return statusName(r.Status)
}

func statusName(status pb.CommandStatus) string {
	switch status {
	case pb.CommandStatus_COMMAND_STATUS_ACCEPTED:
		return "accepted"
	case pb.CommandStatus_COMMAND_STATUS_RUNNING:
		return "running"
	case pb.CommandStatus_COMMAND_STATUS_SUCCESS:
		return "success"
	case pb.CommandStatus_COMMAND_STATUS_FAILED:
		return "failed"
	case pb.CommandStatus_COMMAND_STATUS_REJECTED:
		return "rejected"
	case pb.CommandStatus_COMMAND_STATUS_TIMEOUT:
		return "timeout"
	case pb.CommandStatus_COMMAND_STATUS_UNSUPPORTED:
		return "unsupported"
	default:
		return "unspecified"
	}
}

func timestamp(t *timestamppb.Timestamp) *time.Time {
	if t == nil || t.CheckValid() != nil {
		return nil
	}
	value := t.AsTime().UTC()
	return &value
}

// ResultFromEnvelope copies a validated payload so callers can retain it without
// sharing optional-value pointers with the incoming protobuf.
func ResultFromEnvelope(e *pb.AgentEnvelope) *Result {
	if e == nil || e.GetCommandResult() == nil {
		return nil
	}
	r := e.GetCommandResult()
	result := &Result{
		MessageID: e.MessageId, Status: statusName(r.Status),
		StartedAt: timestamp(r.StartedAt), FinishedAt: timestamp(r.FinishedAt),
		Changed: r.Changed, Uncertain: r.Uncertain || r.Verification.GetUncertain(),
		ErrorCode: r.ErrorCode, ErrorMessage: r.ErrorMessage, SnapshotID: r.SnapshotId,
	}
	if v := r.Verification; v != nil {
		result.Verification = &Verification{
			InterfaceID: v.InterfaceId, Name: v.Name, ExpectedAdminUp: v.ExpectedAdminUp,
			ObservedAt: timestamp(v.ObservedAt), Verified: v.Verified,
			Changed: v.Changed, Uncertain: v.Uncertain, Evidence: v.Evidence,
		}
		if v.ObservedAdminUp != nil {
			observed := *v.ObservedAdminUp
			result.Verification.ObservedAdminUp = &observed
		}
	}
	return result
}

// ValidateResult accepts unspecified/unknown enum values for archive compatibility;
// storage decides whether a payload belongs to a dispatched command/session.
func ValidateResult(r *pb.CommandResult) error {
	if r == nil {
		return errors.New("missing command result")
	}
	if err := identifier(r.CommandId, 128); err != nil {
		return fmt.Errorf("invalid command_id: %w", err)
	}
	for _, t := range []*timestamppb.Timestamp{r.StartedAt, r.FinishedAt, r.Verification.GetObservedAt()} {
		if t != nil && t.CheckValid() != nil {
			return errors.New("invalid command result timestamp")
		}
	}
	for _, field := range []struct {
		value string
		max   int
	}{{r.ErrorCode, 128}, {r.ErrorMessage, 4096}, {r.SnapshotId, 256}} {
		if len(field.value) > field.max || !utf8.ValidString(field.value) {
			return errors.New("command result field is too long or invalid UTF-8")
		}
	}
	if v := r.Verification; v != nil {
		for _, field := range []struct {
			value string
			max   int
		}{{v.InterfaceId, 128}, {v.Name, 128}, {v.Evidence, 4096}} {
			if len(field.value) > field.max || !utf8.ValidString(field.value) {
				return errors.New("command verification field is too long or invalid UTF-8")
			}
		}
	}
	return nil
}
