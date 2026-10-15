package topology

import (
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"testing"
	"time"
)

func globalFixture(now time.Time) ([]GlobalBinding, []GlobalObservation) {
	bindings := []GlobalBinding{}
	for _, source := range []string{"one", "two"} {
		for _, id := range []string{"a", "b"} {
			mac := "02:00:00:00:00:01"
			if id == "b" {
				mac = "02:00:00:00:00:02"
			}
			bindings = append(bindings, GlobalBinding{Asset: assets.Asset{ID: id, Kind: "network_device", CanonicalName: id}, BindingID: source + id, SourceID: source, DeviceID: id, Device: &pb.Device{Id: id, Hostname: id, ChassisId: mac}})
		}
	}
	makeObs := func(source, from, to, local, remote string) GlobalObservation {
		last := byte(1)
		if to == "b" {
			last = 2
		}
		n := &pb.Neighbor{LocalDeviceId: from, LocalPortSubtype: 5, LocalPortId: []byte(local), RemotePortSubtype: 5, RemotePortId: []byte(remote), RemoteChassisSubtype: 4, RemoteChassisId: []byte{2, 0, 0, 0, 0, last}, Protocol: pb.DiscoveryProtocol_DISCOVERY_PROTOCOL_LLDP, Confidence: "high"}
		return GlobalObservation{SourceID: source, Observation: Observation{ID: source + from, AgentID: source, DeviceID: from, Protocol: n.Protocol.String(), ObservedAt: now, ExpiresAt: now.Add(time.Minute), Neighbor: n}}
	}
	return bindings, []GlobalObservation{makeObs("one", "a", "b", "eth1", "eth2"), makeObs("two", "a", "b", "eth1", "eth2"), makeObs("two", "b", "a", "eth2", "eth1")}
}
func TestGlobalTopologyDeduplicatesAssetsAndReciprocalEvidence(t *testing.T) {
	now := time.Now()
	b, o := globalFixture(now)
	g := BuildGlobal(now, b, o, GlobalFilter{Limit: 500}, false, false)
	if len(g.Nodes) != 2 || len(g.Edges) != 1 || len(g.Edges[0].Evidence) != 3 || g.Edges[0].Confirmation != "bidirectional" {
		t.Fatal("global folding", g)
	}
	o[2].ExpiresAt = now.Add(-time.Second)
	g = BuildGlobal(now, b, o, GlobalFilter{Limit: 500, IncludeExpired: true}, false, false)
	if len(g.Edges) != 1 || g.Edges[0].Confirmation != "one_sided" || g.Edges[0].Status != "active" {
		t.Fatal("expired reverse confirmed live link", g)
	}
	o[2].ExpiresAt = now.Add(time.Minute)
	o[2].Neighbor = proto.Clone(o[2].Neighbor).(*pb.Neighbor)
	o[2].Neighbor.LocalPortId = []byte("renamed-port")
	g = BuildGlobal(now, b, o, GlobalFilter{Limit: 500}, false, false)
	if len(g.Edges) != 2 {
		t.Fatal("port identity change guessed away", g)
	}
}
func TestGlobalTopologyAmbiguitySimulationAndTruncation(t *testing.T) {
	now := time.Now()
	b, o := globalFixture(now)
	duplicate := b[1]
	duplicate.Asset.ID = "another-physical-device"
	duplicate.BindingID = "duplicate"
	b = append(b, duplicate)
	g := BuildGlobal(now, b, o[:1], GlobalFilter{Limit: 500}, false, false)
	if g.Edges[0].Evidence[0].Resolution.Status != "ambiguous" || g.Edges[0].Evidence[0].To == "b" {
		t.Fatal("MAC ambiguity hidden", g)
	}
	b, _ = globalFixture(now)
	o[0].Scope = "simulated-session"
	g = BuildGlobal(now, b, o[:1], GlobalFilter{Limit: 500}, false, false)
	if g.Edges[0].Evidence[0].Resolution.Status != "unresolved" || g.Edges[0].Evidence[0].From == "a" {
		t.Fatal("simulation crossed real namespace", g)
	}
	o[0].Scope = ""
	g = BuildGlobal(now, b, o[:1], GlobalFilter{Limit: 500}, true, false)
	if !g.Truncated || g.Edges[0].Evidence[0].Resolution.Method != "inventory_truncated" {
		t.Fatal("truncated inventory appeared unique", g)
	}
	o[0].Neighbor = proto.Clone(o[0].Neighbor).(*pb.Neighbor)
	o[0].Neighbor.RemoteChassisId = nil
	o[0].Neighbor.RemoteSystemName = "b"
	g = BuildGlobal(now, b, o[:1], GlobalFilter{Limit: 500}, false, false)
	if g.Edges[0].Evidence[0].Resolution.Status != "tentative" || g.Edges[0].Evidence[0].To == "b" {
		t.Fatal("hostname became physical identity", g)
	}
}
func TestGlobalManagementRelationsRespectBMCTargetAndStaySeparate(t *testing.T) {
	ref := func(target, resource string) *pb.ObservationMetadata {
		return &pb.ObservationMetadata{Source: &pb.SourceRef{Type: "redfish", TargetId: target, ResourceId: resource}}
	}
	bindings := []GlobalBinding{{Asset: assets.Asset{ID: "bmc", Kind: "bmc"}, BindingID: "bmcb", SourceID: "collector", Device: &pb.Device{Observation: ref("bmc-one", "/redfish/v1/Managers/1"), Hardware: &pb.HardwareInventory{Relations: []*pb.ResourceRelationship{{Type: "BMC_MANAGES_SYSTEM", TargetResourceId: "/redfish/v1/Systems/1"}}}}}, {Asset: assets.Asset{ID: "correct-system", Kind: "system"}, BindingID: "sys1", SourceID: "collector", Device: &pb.Device{Observation: ref("bmc-one", "/redfish/v1/Systems/1"), Hardware: &pb.HardwareInventory{Relations: []*pb.ResourceRelationship{{Type: "MANAGED_BY_BMC", TargetResourceId: "/redfish/v1/Managers/1"}}}}}, {Asset: assets.Asset{ID: "other-system", Kind: "system"}, BindingID: "sys2", SourceID: "collector", Device: &pb.Device{Observation: ref("bmc-two", "/redfish/v1/Systems/1")}}}
	g := BuildGlobal(time.Now(), bindings, nil, GlobalFilter{Limit: 500}, false, false)
	if len(g.Edges) != 0 || len(g.ManagementRelations) != 1 || g.ManagementRelations[0].From != "bmc" || g.ManagementRelations[0].To != "correct-system" || len(g.ManagementRelations[0].Evidence) != 2 {
		t.Fatal("BMC became cable or crossed target", g)
	}
}
func TestTopologyBusinessFingerprintIgnoresClocksButKeepsPortAndConfidence(t *testing.T) {
	n := &pb.Neighbor{RemotePortId: []byte("eth1"), ObservedAt: timestamppb.Now(), ExpiresAt: timestamppb.Now()}
	copy := proto.Clone(n).(*pb.Neighbor)
	copy.ObservedAt = timestamppb.New(time.Now().Add(time.Second))
	if BusinessFingerprint(n) != BusinessFingerprint(copy) {
		t.Fatal("clock creates topology change")
	}
	copy.RemotePortId = []byte("eth2")
	if BusinessFingerprint(n) == BusinessFingerprint(copy) {
		t.Fatal("port change lost")
	}
}
