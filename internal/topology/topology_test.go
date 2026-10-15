package topology

import (
	"encoding/base64"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func fixture(now time.Time) *pb.TopologyUpdate {
	return &pb.TopologyUpdate{DeviceId: "router", Source: "snmp.lldp", ObservedAt: timestamppb.New(now), TtlSeconds: 60,
		Completeness: pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_COMPLETE,
		Observations: []*pb.Neighbor{{LocalDeviceId: "router", LocalPortId: []byte("ether1"), RemoteChassisSubtype: 4, RemoteChassisId: []byte{0, 1, 2, 3, 4, 5}, RemotePortId: []byte{0xff, 0x00},
			RemoteSystemName: "SWITCH.", Protocol: pb.DiscoveryProtocol_DISCOVERY_PROTOCOL_LLDP}}}
}

func TestObservationIdentityTTLAndInputImmutability(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	update := fixture(now)
	original := proto.Clone(update)
	items, err := Prepare("agent", update, now.Add(20*time.Second))
	if err != nil || len(items) != 1 || !items[0].ExpiresAt.Equal(now.Add(time.Minute)) || items[0].Source != "snmp.lldp" {
		t.Fatalf("bad prepared observation: %+v %v", items, err)
	}
	if !proto.Equal(original, update) {
		t.Fatal("preparation mutated the archived business payload")
	}
	update.Observations[0].RemoteSystemName = "renamed"
	renamed, _ := Prepare("agent", update, now)
	if renamed[0].ID != items[0].ID {
		t.Fatal("hostname change duplicated a chassis-identified edge")
	}
	update.Observations[0].LocalPortId = []byte("ether2")
	otherPort, _ := Prepare("agent", update, now)
	if otherPort[0].ID == items[0].ID {
		t.Fatal("parallel ports merged")
	}
	update.Completeness = pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_FAILED
	failed, err := Prepare("agent", update, now)
	if err != nil || len(failed) != 0 {
		t.Fatal("failed collection supplied link evidence")
	}
}

func TestInvalidTopologyCannotEnterProjection(t *testing.T) {
	now := time.Now().UTC()
	for name, modify := range map[string]func(*pb.TopologyUpdate){
		"missing device":       func(t *pb.TopologyUpdate) { t.DeviceId = "" },
		"foreign local device": func(t *pb.TopologyUpdate) { t.Observations[0].LocalDeviceId = "another" },
		"invalid time":         func(t *pb.TopologyUpdate) { t.ObservedAt = &timestamppb.Timestamp{Nanos: 1_000_000_000} },
		"negative TTL":         func(t *pb.TopologyUpdate) { t.Observations[0].ExpiresAt = timestamppb.New(now.Add(-time.Second)) },
		"unbounded TTL":        func(t *pb.TopologyUpdate) { t.TtlSeconds = 86401 },
		"unbounded expiry":     func(t *pb.TopologyUpdate) { t.Observations[0].ExpiresAt = timestamppb.New(now.Add(25 * time.Hour)) },
		"nil observation":      func(t *pb.TopologyUpdate) { t.Observations[0] = nil },
	} {
		t.Run(name, func(t *testing.T) {
			update := fixture(now)
			modify(update)
			if Validate(update) == nil {
				t.Fatal("invalid topology accepted")
			}
		})
	}
}

func TestResolutionPreservesAmbiguityAndAgentScope(t *testing.T) {
	now := time.Now().UTC()
	base := []Device{{AgentID: "agent", ID: "router", Name: "Router"}, {AgentID: "agent", ID: "switch", Hostname: "switch", ChassisID: "00:01:02:03:04:05"}}
	for _, tc := range []struct {
		name           string
		devices        []Device
		subtype        uint32
		chassis        []byte
		status, method string
	}{
		{"strong chassis", base, 4, []byte{0, 1, 2, 3, 4, 5}, "resolved", "chassis_mac"},
		{"tentative name", base, 0, []byte{0, 1, 2, 3, 4, 5}, "resolved", "unique_hostname"},
		{"hostname conflict", base, 4, []byte{6, 7, 8, 9, 10, 11}, "conflicting", "hostname_conflicts_with_chassis"},
		{"duplicate chassis", append(append([]Device{}, base...), Device{AgentID: "agent", ID: "duplicate", ChassisID: "00:01:02:03:04:05"}), 4, []byte{0, 1, 2, 3, 4, 5}, "ambiguous", "chassis_mac"},
		{"duplicate hostname", append(append([]Device{}, base...), Device{AgentID: "agent", ID: "duplicate", Hostname: "SWITCH"}), 0, nil, "ambiguous", "unique_hostname"},
		{"different collector", []Device{{AgentID: "other", ID: "switch", Hostname: "switch", ChassisID: "00:01:02:03:04:05"}}, 4, []byte{0, 1, 2, 3, 4, 5}, "unresolved", "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := fixture(now)
			u.Observations[0].RemoteChassisSubtype = tc.subtype
			u.Observations[0].RemoteChassisId = tc.chassis
			observations, _ := Prepare("agent", u, now)
			g := Build(now, tc.devices, observations, nil, "router", false, false)
			if len(g.Edges) != 1 || g.Edges[0].Resolution.Status != tc.status || g.Edges[0].Resolution.Method != tc.method {
				t.Fatalf("wrong resolution: %+v", g)
			}
			if g.Edges[0].RemotePort.RawID != base64.StdEncoding.EncodeToString([]byte{0xff, 0}) {
				t.Fatal("opaque port bytes were converted into text")
			}
		})
	}
}

func TestExpiryClockSkewAndIncompleteInventory(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	items, _ := Prepare("agent", fixture(now), now.Add(3*time.Minute))
	devices := []Device{{AgentID: "agent", ID: "switch", Hostname: "switch", ChassisID: "00:01:02:03:04:05"}}
	expired := Build(now.Add(time.Minute), devices, items, nil, "router", false, false)
	if len(expired.Edges) != 0 {
		t.Fatal("boundary-expired edge remains active")
	}
	diagnostic := Build(now.Add(time.Minute), devices, items, nil, "router", true, false)
	if len(diagnostic.Edges) != 1 || diagnostic.Edges[0].Status != "expired" {
		t.Fatal("expired evidence lost")
	}
	future := Build(now.Add(-31*time.Second), devices, items, nil, "router", false, false)
	if len(future.Edges) != 0 {
		t.Fatal("future evidence displayed as active")
	}
	incomplete := Build(now, devices, items, nil, "router", false, true)
	if incomplete.Edges[0].Resolution.Method != "inventory_truncated" {
		t.Fatal("partial inventory caused a false unique identity")
	}
}
