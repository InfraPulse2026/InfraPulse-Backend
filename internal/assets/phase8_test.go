package assets

import (
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"testing"
	"time"
)

func TestPhase8ScopedDiagnosticsAndIdentityClocks(t *testing.T) {
	now := time.Now()
	source := &pb.SourceRef{Type: "snmp", ResourceId: "/switch/1", Provenance: "real"}
	d := &pb.Device{Id: "switch-1", Type: pb.DeviceType_DEVICE_TYPE_SWITCH, IdentityEvidence: []*pb.IdentityEvidence{{Type: "serial", Value: "ROOT-1", Confidence: "high", Source: source, SubjectKind: "network_device", SubjectId: "switch-1", Normalization: "canonical", Time: &pb.ObservationTime{ObservedAt: timestamppb.New(now)}}}, ExtendedIdentityEvidence: []*pb.IdentityEvidence{
		{Type: "serial", Value: "DIAGNOSTIC-1", Confidence: "high", Source: source, SubjectKind: "network_device", Normalization: "invalid"},
		{Type: "serial", Value: "INTERFACE-1", Confidence: "high", Source: source, SubjectKind: "network_interface"},
		{Type: "serial", Value: "COMPONENT-1", Confidence: "high", Source: source, SubjectKind: "component", SubjectId: "component-1"},
	}}
	// Duplicate diagnostic must not change another fact or panic.
	d.ExtendedIdentityEvidence = append(d.ExtendedIdentityEvidence, proto.Clone(d.ExtendedIdentityEvidence[0]).(*pb.IdentityEvidence))
	root := false
	for _, i := range Identities(d) {
		switch i.Value {
		case "ROOT-1":
			root = i.Usable && i.SubjectID == ""
		case "DIAGNOSTIC-1", "INTERFACE-1":
			if i.Usable {
				t.Fatal("diagnostic or foreign owner promoted", i)
			}
		case "COMPONENT-1":
			if i.SubjectID == "" {
				t.Fatal("component promoted")
			}
		}
	}
	if !root {
		t.Fatal("explicit owner identity not projected")
	}
	before, _ := Fingerprint(d)
	changed := proto.Clone(d).(*pb.Device)
	changed.IdentityEvidence[0].Time.ObservedAt = timestamppb.New(now.Add(time.Second))
	after, _ := Fingerprint(changed)
	if before != after || d.IdentityEvidence[0].Time.ObservedAt.AsTime().Equal(changed.IdentityEvidence[0].Time.ObservedAt.AsTime()) {
		t.Fatal("identity clocks grew history or original mutated")
	}
}
