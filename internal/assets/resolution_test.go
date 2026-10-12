package assets

import (
	"testing"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
)

func TestAssetComparisonConflictsOverrideStrongMatches(t *testing.T) {
	a := Signature{Kind: "system", Vendor: "eltex", UUIDs: []string{"uuid-a"}, StrongUUIDs: []string{"uuid-a"}, Serials: []string{"serial-a"}}
	b := a
	b.Serials = []string{"serial-b"}
	c := Compare(a, b)
	if c.StrongMatch || len(c.Conflicts) == 0 || len(c.Matches) == 0 {
		t.Fatal("UUID match hid serial contradiction", c)
	}
	b = a
	b.Kind = "bmc"
	if Compare(a, b).StrongMatch {
		t.Fatal("BMC and system matched")
	}
	if Compare(Signature{Kind: "host"}, Signature{Kind: "host"}).StrongMatch {
		t.Fatal("weak name/IP-free facts matched automatically")
	}
	if _, usable := NormalizeIdentity("serial", "[REDACTED]"); usable {
		t.Fatal("redacted serial usable")
	}
}
func TestAssetComparisonRejectsComponentOrDifferentResourceEvidence(t *testing.T) {
	d := &pb.Device{Hardware: &pb.HardwareInventory{ResourceType: "ComputerSystem"}, IdentityEvidence: []*pb.IdentityEvidence{{Type: "serial", Value: "DRIVE-1", Confidence: "high", SubjectId: "drive"}, {Type: "serial", Value: "CHASSIS-1", Confidence: "high", Source: &pb.SourceRef{ResourceId: "/redfish/v1/Chassis/1"}}}}
	if s := RootSignature(d); len(s.StrongSerials) != 0 {
		t.Fatal("component/resource evidence promoted", s)
	}
}
