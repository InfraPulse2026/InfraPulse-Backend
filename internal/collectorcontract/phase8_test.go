package collectorcontract

import (
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
)

func TestPhase8ExtendedFactsAndConfigIdentity(t *testing.T) {
	e := fixture()
	d := e.GetInventory().Devices[0]
	d.ExtendedIdentityEvidence = []*pb.IdentityEvidence{{Type: "ip", Value: "192.0.2.1", Confidence: "low", SubjectId: "eth0", SubjectKind: "network_interface", Normalization: "canonical", Origin: "redfish.IPv4Addresses.Address", Time: d.Observation.Time, Source: d.Observation.Source}, {Type: "serial", Value: "[rejected identifier]", Confidence: "unresolved", SubjectKind: "computer_system", Normalization: "rejected", Source: d.Observation.Source}}
	if err := Validate(e); err != nil {
		t.Fatal("Phase8 facts rejected", err)
	}
	wrong := proto.Clone(e).(*pb.AgentEnvelope)
	wrong.GetInventory().Devices[0].ExtendedIdentityEvidence[0].SubjectKind = "global_asset"
	if Validate(wrong) == nil {
		t.Fatal("global identity accepted")
	}
	wrong = proto.Clone(e).(*pb.AgentEnvelope)
	wrong.GetInventory().Devices[0].ExtendedIdentityEvidence[0].Normalization = "unique"
	if Validate(wrong) == nil {
		t.Fatal("unknown normalization accepted")
	}
	status := &pb.AgentEnvelope{AgentId: "collector-1", Payload: &pb.AgentEnvelope_ConfigStatus{ConfigStatus: &pb.ConfigApplyStatus{CollectorId: "collector-1", Revision: 1, ConfigurationDigest: strings.Repeat("a", 64), Status: pb.ConfigStatus_CONFIG_STATUS_REJECTED, ErrorCode: "backend_not_negotiated"}}}
	if err := Validate(status); err != nil {
		t.Fatal(err)
	}
	status.GetConfigStatus().CollectorId = "another"
	if Validate(status) == nil {
		t.Fatal("config status spoof accepted")
	}
}
