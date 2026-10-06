package proto_test

import (
	_ "embed"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	infrapulsev1 "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Frozen descriptor of the Backend P2 contract before the additive P3 sync.
// It protects every existing field number, wire type, presence and enum value.
//
//go:embed testdata/p2-descriptor.pb
var p2Descriptor []byte

func TestP3PreservesP2WireContract(t *testing.T) {
	var baseline descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(p2Descriptor, &baseline); err != nil {
		t.Fatal(err)
	}
	if len(baseline.File) != 1 {
		t.Fatalf("expected one baseline file, got %d", len(baseline.File))
	}
	old := baseline.File[0]
	current := protodesc.ToFileDescriptorProto(infrapulsev1.File_infrapulse_v1_agent_proto)
	if old.GetPackage() != current.GetPackage() || old.GetSyntax() != current.GetSyntax() {
		t.Fatal("protobuf package or syntax changed")
	}
	for _, oldService := range old.Service {
		var matched *descriptorpb.ServiceDescriptorProto
		for _, service := range current.Service {
			if service.GetName() == oldService.GetName() {
				matched = service
				break
			}
		}
		if !proto.Equal(oldService, matched) {
			t.Errorf("existing service contract changed: %s", oldService.GetName())
		}
	}
	compareEnums(t, old.EnumType, current.EnumType, old.GetPackage())
	compareMessages(t, old.MessageType, current.MessageType, old.GetPackage())
}

func compareMessages(t *testing.T, old, current []*descriptorpb.DescriptorProto, prefix string) {
	t.Helper()
	currentByName := make(map[string]*descriptorpb.DescriptorProto, len(current))
	for _, message := range current {
		currentByName[message.GetName()] = message
	}
	for _, before := range old {
		name := prefix + "." + before.GetName()
		after := currentByName[before.GetName()]
		if after == nil {
			t.Errorf("removed message %s", name)
			continue
		}
		fields := make(map[string]*descriptorpb.FieldDescriptorProto, len(after.Field))
		for _, field := range after.Field {
			fields[field.GetName()] = field
		}
		for _, oldField := range before.Field {
			newField := fields[oldField.GetName()]
			if newField == nil {
				t.Errorf("removed field %s.%s", name, oldField.GetName())
				continue
			}
			if oldField.GetNumber() != newField.GetNumber() ||
				oldField.GetType() != newField.GetType() ||
				oldField.GetTypeName() != newField.GetTypeName() ||
				oldField.GetLabel() != newField.GetLabel() ||
				oldField.GetProto3Optional() != newField.GetProto3Optional() ||
				oneofName(before, oldField) != oneofName(after, newField) {
				t.Errorf("wire contract changed for %s.%s: before=%v after=%v", name, oldField.GetName(), oldField, newField)
			}
		}
		compareEnums(t, before.EnumType, after.EnumType, name)
		compareMessages(t, before.NestedType, after.NestedType, name)
	}
}

func oneofName(message *descriptorpb.DescriptorProto, field *descriptorpb.FieldDescriptorProto) string {
	if field.OneofIndex == nil {
		return ""
	}
	return message.OneofDecl[field.GetOneofIndex()].GetName()
}

func compareEnums(t *testing.T, old, current []*descriptorpb.EnumDescriptorProto, prefix string) {
	t.Helper()
	currentByName := make(map[string]*descriptorpb.EnumDescriptorProto, len(current))
	for _, enum := range current {
		currentByName[enum.GetName()] = enum
	}
	for _, before := range old {
		after := currentByName[before.GetName()]
		if after == nil {
			t.Errorf("removed enum %s.%s", prefix, before.GetName())
			continue
		}
		values := make(map[string]int32, len(after.Value))
		for _, value := range after.Value {
			values[value.GetName()] = value.GetNumber()
		}
		for _, value := range before.Value {
			if number, exists := values[value.GetName()]; !exists || number != value.GetNumber() {
				t.Errorf("enum value changed: %s.%s.%s", prefix, before.GetName(), value.GetName())
			}
		}
	}
}

func TestCanonicalAgentProto(t *testing.T) {
	agentRoot := os.Getenv("AGENT_ROOT")
	if agentRoot == "" {
		agentRoot = filepath.Join("..", "..", "..", "InfraPulseAgent")
	}
	canonicalPath := filepath.Join(agentRoot, "api", "proto", "infrapulse", "v1", "agent.proto")
	canonical, err := os.ReadFile(canonicalPath)
	if os.IsNotExist(err) {
		t.Skip("independent Agent checkout absent; scripts/generate-proto --check requires it")
	}
	if err != nil {
		t.Fatal(err)
	}
	backend, err := os.ReadFile(filepath.Join("infrapulse", "v1", "agent.proto"))
	if err != nil {
		t.Fatal(err)
	}
	goPackage := regexp.MustCompile(`(?m)^option go_package = "[^"]+";\n`)
	normalize := func(contents []byte) string {
		return goPackage.ReplaceAllString(strings.ReplaceAll(string(contents), "\r\n", "\n"), "")
	}
	if normalize(canonical) != normalize(backend) {
		t.Fatal("Backend proto differs from canonical Agent proto beyond go_package")
	}
}

func TestP3GeneratedFields(t *testing.T) {
	file := infrapulsev1.File_infrapulse_v1_agent_proto
	const backendGoPackage = "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1;infrapulsev1"
	if got := file.Options().(*descriptorpb.FileOptions).GetGoPackage(); got != backendGoPackage {
		t.Errorf("generated go_package=%q, want %q", got, backendGoPackage)
	}
	for message, fields := range map[string]map[string]protoreflect.FieldNumber{
		"ControllerEnvelope": {"message_ack": 3},
		"MessageACK":         {"message_id": 1},
		"AgentHello":         {"application_ack": 8},
		"HelloAck":           {"application_ack": 3},
		"QueueState":         {"mode": 6, "storage_bytes": 7, "oldest_pending": 8, "unacked": 9, "pending_results": 10, "storage_error": 11},
		"Heartbeat":          {"certificate": 7},
		"CertificateStatus":  {"expires_at": 1, "remaining_seconds": 2, "enrollment_state": 3, "renewal_state": 4, "last_successful_renewal": 5, "last_error": 6},
		"Command":            {"rollback": 12},
		"RollbackAction":     {"snapshot_id": 1},
		"CommandResult":      {"snapshot_id": 10},
	} {
		descriptor := file.Messages().ByName(protoreflect.Name(message))
		if descriptor == nil {
			t.Errorf("missing P3 message %s", message)
			continue
		}
		for field, number := range fields {
			actual := descriptor.Fields().ByName(protoreflect.Name(field))
			if actual == nil || actual.Number() != number {
				t.Errorf("missing or renumbered P3 field %s.%s: want %d", message, field, number)
			}
		}
	}
}

func TestP3FieldsSurviveWireDecodeAndJSONArchive(t *testing.T) {
	now := timestamppb.New(time.Unix(1800000000, 123000000).UTC())
	messages := []proto.Message{
		&infrapulsev1.AgentEnvelope{Payload: &infrapulsev1.AgentEnvelope_Heartbeat{Heartbeat: &infrapulsev1.Heartbeat{
			Queue:       &infrapulsev1.QueueState{Mode: "durable", StorageBytes: 8192, OldestPending: now, Unacked: 2, PendingResults: 1, StorageError: "test-error"},
			Certificate: &infrapulsev1.CertificateStatus{ExpiresAt: now, RemainingSeconds: 3600, EnrollmentState: "enrolled", RenewalState: "idle", LastSuccessfulRenewal: now, LastError: "test-renewal"},
		}}},
		&infrapulsev1.AgentEnvelope{Payload: &infrapulsev1.AgentEnvelope_CommandResult{CommandResult: &infrapulsev1.CommandResult{CommandId: "command", SnapshotId: "snapshot-p3"}}},
		&infrapulsev1.ControllerEnvelope{Payload: &infrapulsev1.ControllerEnvelope_Command{Command: &infrapulsev1.Command{Action: &infrapulsev1.Command_Rollback{Rollback: &infrapulsev1.RollbackAction{SnapshotId: "snapshot-p3"}}}}},
		&infrapulsev1.ControllerEnvelope{Payload: &infrapulsev1.ControllerEnvelope_MessageAck{MessageAck: &infrapulsev1.MessageACK{MessageId: "message-p3"}}},
		&infrapulsev1.AgentHello{ApplicationAck: true},
		&infrapulsev1.HelloAck{Accepted: true, ProtocolVersion: 1, ApplicationAck: true},
	}
	for _, original := range messages {
		t.Run(string(original.ProtoReflect().Descriptor().FullName()), func(t *testing.T) {
			wire, err := proto.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			decoded := original.ProtoReflect().Type().New().Interface()
			if err := proto.Unmarshal(wire, decoded); err != nil {
				t.Fatal(err)
			}
			archive, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(decoded)
			if err != nil {
				t.Fatal(err)
			}
			recovered := original.ProtoReflect().Type().New().Interface()
			if err := protojson.Unmarshal(archive, recovered); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(original, recovered) {
				t.Fatalf("P3 data lost in wire/archive roundtrip: %s", archive)
			}
		})
	}
}
