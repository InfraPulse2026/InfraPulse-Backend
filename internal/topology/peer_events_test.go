package topology

import (
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"testing"
	"time"
)

func TestPeerSnapshotIdentityCompletenessAmbiguityAndTime(t *testing.T) {
	now := time.Now().UTC()
	base := &pb.TopologyUpdate{DeviceId: "switch", Source: "snmp.lldp", ObservedAt: timestamppb.New(now), TtlSeconds: 60, Completeness: pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_COMPLETE, Observations: []*pb.Neighbor{{LocalPortSubtype: 5, LocalPortId: []byte("eth1"), RemoteChassisSubtype: 4, RemoteChassisId: []byte{2, 0, 0, 0, 0, 1}, RemotePortSubtype: 5, RemotePortId: []byte("eth2"), Protocol: pb.DiscoveryProtocol_DISCOVERY_PROTOCOL_LLDP, Confidence: "high"}}}
	initial, ok := PeerSnapshot(base, now)
	if !ok || len(initial) != 1 {
		t.Fatal(initial, ok)
	}
	metadata := proto.Clone(base).(*pb.TopologyUpdate)
	metadata.Observations[0].RemoteSystemName = "new display label"
	other, ok := PeerSnapshot(metadata, now)
	if !ok {
		t.Fatal("metadata rejected")
	}
	for k, v := range initial {
		if v.PeerHash != other[k].PeerHash {
			t.Fatal("name interpreted as peer replacement")
		}
	}
	retargeted := proto.Clone(base).(*pb.TopologyUpdate)
	retargeted.Observation = &pb.ObservationMetadata{Source: &pb.SourceRef{Type: "snmp", TargetId: "different-target"}}
	retargetedPeers, ok := PeerSnapshot(retargeted, now)
	if !ok {
		t.Fatal("new context rejected")
	}
	for k := range initial {
		if _, same := retargetedPeers[k]; same {
			t.Fatal("retargeting interpreted as same port context")
		}
	}
	for _, scenario := range []string{"partial", "failed", "confidence", "duplicate", "old", "future", "clock", "mndp"} {
		t.Run(scenario, func(t *testing.T) {
			v := proto.Clone(base).(*pb.TopologyUpdate)
			switch scenario {
			case "partial":
				v.Completeness = pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_PARTIAL
			case "failed":
				v.Completeness = pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_FAILED
			case "confidence":
				v.Observations[0].Confidence = "medium"
			case "duplicate":
				v.Observations = append(v.Observations, proto.Clone(v.Observations[0]).(*pb.Neighbor))
			case "old":
				v.ObservedAt = timestamppb.New(now.Add(-6 * time.Minute))
			case "future":
				v.ObservedAt = timestamppb.New(now.Add(time.Minute))
			case "clock":
				v.Observation = &pb.ObservationMetadata{Time: &pb.ObservationTime{ClockQuality: "unsynced"}}
			case "mndp":
				v.Observations[0].Protocol = pb.DiscoveryProtocol_DISCOVERY_PROTOCOL_MNDP
			}
			if _, ok := PeerSnapshot(v, now); ok {
				t.Fatal("unsafe incident baseline accepted")
			}
		})
	}
	if PeerEventKey("b", "source", "", "port") == PeerEventKey("b", "source", "session", "port") || PeerEventKey("b", "source", "", "port") == PeerEventKey("other", "source", "", "port") {
		t.Fatal("binding/simulation keys mixed")
	}
}
