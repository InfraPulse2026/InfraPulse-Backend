package assets

import (
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestAssetKindsAndIdentitySubjects(t *testing.T) {
	for _, test := range []struct{ resource, kind string }{{"system", "system"}, {"ComputerSystem", "system"}, {"computer_system", "system"}, {"manager", "bmc"}, {"chassis", "chassis"}, {"drive", "component"}} {
		if Kind(&pb.Device{Hardware: &pb.HardwareInventory{ResourceType: test.resource}}) != test.kind {
			t.Fatal(test)
		}
	}
	d := &pb.Device{SerialNumber: "ROOT-1", Components: []*pb.InventoryComponent{{Id: "drive-1", SerialNumber: "DRIVE-1"}, {SerialNumber: "UNSCOPED-COMPONENT"}}, IdentityEvidence: []*pb.IdentityEvidence{{Type: "serial", Value: "DRIVE-1", Confidence: "high", SubjectId: "drive-1"}}}
	for _, i := range Identities(d) {
		if i.Value == "DRIVE-1" && i.SubjectID == "" {
			t.Fatal("component identity promoted to system")
		}
		if i.Value == "UNSCOPED-COMPONENT" {
			t.Fatal("unscoped component promoted")
		}
		if i.Value == "ROOT-1" && i.Confidence == "high" {
			t.Fatal("legacy confidence invented")
		}
	}
	for _, value := range []string{"00000000-0000-0000-0000-000000000000", "ffffffff-ffff-ffff-ffff-ffffffffffff", "not-a-uuid"} {
		if _, usable := NormalizeIdentity("hardware_uuid", value); usable {
			t.Fatal("placeholder identity usable", value)
		}
	}
	if normalized, usable := NormalizeIdentity("hardware_uuid", "ABCDEF12-1234-1234-1234-123456789ABC"); !usable || normalized != "abcdef12-1234-1234-1234-123456789abc" {
		t.Fatal("UUID normalization")
	}
}

func TestAssetFingerprintIgnoresClocksPreservesBusinessFacts(t *testing.T) {
	now := time.Now()
	d := &pb.Device{Id: "system", SerialNumber: "SERIAL-1", LastSeen: timestamppb.New(now), UptimeSeconds: 12, Observation: &pb.ObservationMetadata{Time: &pb.ObservationTime{ObservedAt: timestamppb.New(now)}}}
	a, err := Fingerprint(d)
	if err != nil {
		t.Fatal(err)
	}
	changed := proto.Clone(d).(*pb.Device)
	changed.LastSeen = timestamppb.New(now.Add(time.Second))
	changed.UptimeSeconds = 13
	changed.Observation.Time.CollectedAt = timestamppb.New(now)
	for range 10 {
		b, err := Fingerprint(changed)
		if err != nil || a != b {
			t.Fatal("clocks or serialization create fact changes", err)
		}
	}
	changed.SerialNumber = "SERIAL-2"
	b, _ := Fingerprint(changed)
	if a == b {
		t.Fatal("identity change not recorded")
	}
	if d.Observation.Time == nil || d.UptimeSeconds != 12 {
		t.Fatal("Agent observation mutated during fingerprinting")
	}
}
