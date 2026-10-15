package commands

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func boolPointer(value bool) *bool { return &value }

func validRequest() Request {
	return Request{AgentID: "agent-1", DeviceID: "device-1", InterfaceID: "interface-1", Operation: "disable", ExpectedAdminUp: boolPointer(true), TimeoutSeconds: 10, Confirmed: true}
}

func TestValidateRequestRequiresBoundedConfirmedTypedAction(t *testing.T) {
	if err := ValidateRequest(validRequest()); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*Request){
		"missing agent":       func(r *Request) { r.AgentID = "" },
		"blank device":        func(r *Request) { r.DeviceID = "  " },
		"missing interface":   func(r *Request) { r.InterfaceID = "" },
		"agent too long":      func(r *Request) { r.AgentID = strings.Repeat("a", 257) },
		"device too long":     func(r *Request) { r.DeviceID = strings.Repeat("d", 129) },
		"interface too long":  func(r *Request) { r.InterfaceID = strings.Repeat("i", 129) },
		"ASCII control":       func(r *Request) { r.DeviceID = "device\n1" },
		"unicode control":     func(r *Request) { r.InterfaceID = "interface\u0085x" },
		"invalid UTF8":        func(r *Request) { r.AgentID = string([]byte{0xff}) },
		"no expected state":   func(r *Request) { r.ExpectedAdminUp = nil },
		"no timeout":          func(r *Request) { r.TimeoutSeconds = 0 },
		"negative timeout":    func(r *Request) { r.TimeoutSeconds = -1 },
		"unbounded timeout":   func(r *Request) { r.TimeoutSeconds = 61 },
		"unconfirmed":         func(r *Request) { r.Confirmed = false },
		"shell command":       func(r *Request) { r.Operation = "ip link set eth0 down" },
		"unsupported restart": func(r *Request) { r.Operation = "restart" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			r := validRequest()
			mutate(&r)
			if ValidateRequest(r) == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
	r := validRequest()
	r.AgentID, r.DeviceID, r.InterfaceID = strings.Repeat("a", 256), strings.Repeat("d", 128), strings.Repeat("i", 128)
	r.ExpectedAdminUp = boolPointer(false)
	r.Operation = "enable"
	for _, timeout := range []int{1, 60} {
		r.TimeoutSeconds = timeout
		if err := ValidateRequest(r); err != nil {
			t.Fatalf("valid boundary request rejected: %v", err)
		}
	}
}

func TestValidateKeyPrintableASCIIOnly(t *testing.T) {
	for _, key := range []string{"retry-1", "key:~! 1", strings.Repeat("x", 128)} {
		if err := ValidateKey(key); err != nil {
			t.Errorf("valid key %q: %v", key, err)
		}
	}
	for _, key := range []string{"", strings.Repeat("x", 129), "key\nheader", "key\rheader", "key\x00", "key\x7f", "ключ", "\t"} {
		if ValidateKey(key) == nil {
			t.Errorf("invalid key %q accepted", key)
		}
	}
}

func TestBuildMatchesUnchangedAgentWireContract(t *testing.T) {
	created := time.Date(2026, 10, 8, 12, 30, 1, 123, time.FixedZone("client", 10800))
	r := validRequest()
	c := Build("server-command-id", "retry-1", r, created)
	if c.Id != "server-command-id" || c.TargetDeviceId != r.DeviceID || c.IdempotencyKey != "retry-1" || c.Initiator != pb.CommandInitiator_COMMAND_INITIATOR_HUMAN {
		t.Fatalf("incorrect server command identity: %v", c)
	}
	if c.GetInterface() == nil || c.GetService() != nil || c.GetRollback() != nil || c.GetInterface().InterfaceId != r.InterfaceID || c.GetInterface().Operation != pb.InterfaceOperation_INTERFACE_OPERATION_DISABLE {
		t.Fatalf("incorrect typed action: %v", c)
	}
	if c.Timeout.AsDuration() != 10*time.Second || !c.RequestedAt.AsTime().Equal(created) {
		t.Fatalf("incorrect timeout/server timestamp: %v", c)
	}
	encoded, err := proto.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	decoded := &pb.Command{}
	if err := proto.Unmarshal(encoded, decoded); err != nil || !proto.Equal(c, decoded) {
		t.Fatalf("wire round trip failed: %v", err)
	}
	jsonValue, err := protojson.Marshal(c)
	if err != nil || bytes.Contains(jsonValue, []byte("expected_admin_up")) || bytes.Contains(jsonValue, []byte("expectedAdminUp")) {
		t.Fatalf("invented wire precondition field: %s, %v", jsonValue, err)
	}
	r.Operation = "enable"
	if Build("id", "key", r, created).GetInterface().Operation != pb.InterfaceOperation_INTERFACE_OPERATION_ENABLE {
		t.Fatal("enable not mapped")
	}
	r.Operation = "invalid"
	if Build("id", "key", r, created).GetInterface().Operation != pb.InterfaceOperation_INTERFACE_OPERATION_UNSPECIFIED {
		t.Fatal("an invalid action must not silently enable an interface")
	}
}

func TestFingerprintIncludesBusinessPreconditionAndTimeout(t *testing.T) {
	r := validRequest()
	base := FingerprintRequest(r)
	if len(base) != 32 || !bytes.Equal(base, FingerprintRequest(r)) {
		t.Fatal("fingerprint is not deterministic SHA256")
	}
	changes := []func(*Request){
		func(r *Request) { r.ExpectedAdminUp = boolPointer(false) },
		func(r *Request) { r.ExpectedAdminUp = nil },
		func(r *Request) { r.TimeoutSeconds++ },
		func(r *Request) { r.AgentID += "2" },
		func(r *Request) { r.DeviceID += "2" },
		func(r *Request) { r.InterfaceID += "2" },
		func(r *Request) { r.Operation = "enable" },
		func(r *Request) { r.Confirmed = false },
	}
	for _, mutate := range changes {
		other := validRequest()
		mutate(&other)
		if bytes.Equal(base, FingerprintRequest(other)) {
			t.Fatal("changed request has the same fingerprint")
		}
	}
}

func TestResultStateUncertaintyWinsWithoutInventingTimeoutUncertainty(t *testing.T) {
	statuses := []string{"unspecified", "accepted", "running", "success", "failed", "rejected", "timeout", "unsupported"}
	for i, want := range statuses {
		r := &pb.CommandResult{Status: pb.CommandStatus(i)}
		if got := ResultState(r); got != want {
			t.Errorf("status %d: got %s want %s", i, got, want)
		}
		r.Uncertain = true
		if got := ResultState(r); got != "uncertain" {
			t.Errorf("explicit uncertain status %d: %s", i, got)
		}
		r.Uncertain = false
		r.Verification = &pb.InterfaceVerification{Uncertain: true}
		if ResultState(r) != "uncertain" {
			t.Fatal("verification uncertainty lost")
		}
	}
	if ResultState(nil) != "unspecified" || ResultState(&pb.CommandResult{Status: 999}) != "unspecified" {
		t.Fatal("nil or unknown enum must not imply success")
	}
}

func TestResultJSONPreservesNullFalseZeroAndCopiesOptionalValues(t *testing.T) {
	r := &pb.CommandResult{CommandId: "command-1", Status: pb.CommandStatus_COMMAND_STATUS_FAILED, SnapshotId: "snapshot-1", Verification: &pb.InterfaceVerification{InterfaceId: "interface-1", ObservedAdminUp: boolPointer(false), ObservedAt: &timestamppb.Timestamp{}}}
	e := &pb.AgentEnvelope{MessageId: "message-1", Payload: &pb.AgentEnvelope_CommandResult{CommandResult: r}}
	result := ResultFromEnvelope(e)
	if result == nil || result.MessageID != "message-1" || result.Status != "failed" || result.SnapshotID != "snapshot-1" {
		t.Fatalf("result conversion failed: %#v", result)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"observed_admin_up":false`, `"changed":false`, `"started_at":null`, `"finished_at":null`, `"observed_at":"1970-01-01T00:00:00Z"`} {
		if !bytes.Contains(data, []byte(fragment)) {
			t.Errorf("missing JSON fragment %s in %s", fragment, data)
		}
	}
	*r.Verification.ObservedAdminUp = true
	if *result.Verification.ObservedAdminUp {
		t.Fatal("DTO shares protobuf optional bool pointer")
	}
	r.Verification.ObservedAdminUp, r.Verification.ObservedAt = nil, nil
	data, _ = json.Marshal(ResultFromEnvelope(e))
	if !bytes.Contains(data, []byte(`"observed_admin_up":null`)) || !bytes.Contains(data, []byte(`"observed_at":null`)) {
		t.Fatalf("unknown observation was converted to false/epoch: %s", data)
	}
	if ResultFromEnvelope(nil) != nil || ResultFromEnvelope(&pb.AgentEnvelope{}) != nil {
		t.Fatal("no payload produced a result")
	}
	r.Verification.Uncertain = true
	if !ResultFromEnvelope(e).Uncertain {
		t.Fatal("verification uncertainty not exposed")
	}
}

func TestValidateResultKeepsLegacyUnspecifiedAndRejectsMalformedPayload(t *testing.T) {
	for _, result := range []*pb.CommandResult{{CommandId: "command-1"}, {CommandId: "command-1", Status: 999}, {CommandId: "command-1", StartedAt: &timestamppb.Timestamp{}}} {
		if err := ValidateResult(result); err != nil {
			t.Errorf("valid/legacy result rejected: %v", err)
		}
	}
	invalid := []*pb.CommandResult{
		nil,
		{},
		{CommandId: "command\n1"},
		{CommandId: strings.Repeat("x", 129)},
		{CommandId: "command-1", StartedAt: &timestamppb.Timestamp{Nanos: -1}},
		{CommandId: "command-1", FinishedAt: &timestamppb.Timestamp{Seconds: 253402300800}},
		{CommandId: "command-1", Verification: &pb.InterfaceVerification{ObservedAt: &timestamppb.Timestamp{Nanos: 1000000000}}},
		{CommandId: "command-1", ErrorCode: strings.Repeat("e", 129)},
		{CommandId: "command-1", ErrorMessage: strings.Repeat("e", 4097)},
		{CommandId: "command-1", SnapshotId: strings.Repeat("s", 257)},
		{CommandId: "command-1", Verification: &pb.InterfaceVerification{InterfaceId: strings.Repeat("i", 129)}},
		{CommandId: "command-1", Verification: &pb.InterfaceVerification{Evidence: strings.Repeat("e", 4097)}},
	}
	for _, result := range invalid {
		if ValidateResult(result) == nil {
			t.Errorf("invalid result accepted: %v", result)
		}
	}
}
