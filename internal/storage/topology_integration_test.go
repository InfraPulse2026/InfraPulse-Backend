package storage

import (
	"fmt"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestTopologyBoundedBatchAndGraphPagination(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	e := topologyEnvelope("large-topology", now)
	sample := e.GetTopology().Observations[0]
	e.GetTopology().Observations = nil
	for i := 0; i < 300; i++ {
		n := proto.Clone(sample).(*pb.Neighbor)
		n.LocalInterfaceId = fmt.Sprintf("ether-%03d", i)
		e.GetTopology().Observations = append(e.GetTopology().Observations, n)
	}
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	page, err := s.Topology(ctx, "agent-1", "shared-router", false, 256, 0)
	if err != nil || len(page.Edges) != 256 || !page.Truncated {
		t.Fatalf("first graph page: %d %v %v", len(page.Edges), page.Truncated, err)
	}
	last, err := s.Topology(ctx, "agent-1", "shared-router", false, 256, 256)
	if err != nil || len(last.Edges) != 44 {
		t.Fatalf("second graph page: %d %v", len(last.Edges), err)
	}
	all, err := s.Topology(ctx, "agent-1", "shared-router", false, 500, 0)
	if err != nil || len(all.Edges) != 300 || all.Truncated {
		t.Fatalf("batched observations lost: %d %v %v", len(all.Edges), all.Truncated, err)
	}
}

func topologyEnvelope(id string, now time.Time) *pb.AgentEnvelope {
	return &pb.AgentEnvelope{AgentId: "agent-1", SessionId: "session-1", MessageId: id, Sequence: 3, SentAt: timestamppb.New(now),
		Payload: &pb.AgentEnvelope_Topology{Topology: &pb.TopologyUpdate{DeviceId: "shared-router", Source: "simulated", ObservedAt: timestamppb.New(now), CollectedAt: timestamppb.New(now),
			Completeness: pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_COMPLETE, TtlSeconds: 900, ExpiryAssumption: "collector assumption",
			Observations: []*pb.Neighbor{{LocalDeviceId: "shared-router", LocalInterfaceId: "ether1", RemoteSystemName: "switch", RemoteChassisSubtype: 4, RemoteChassisId: []byte{0, 1, 2, 3, 4, 5},
				RemotePortId: []byte("ether2"), Protocol: pb.DiscoveryProtocol_DISCOVERY_PROTOCOL_LLDP, Source: "simulated", ObservedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(now.Add(15 * time.Minute))}}}}}
}

func TestTopologyPostgresReplayPartialFailureAndExpiry(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	// PostgreSQL timestamptz stores microseconds; raw protobuf stays lossless.
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, &pb.AgentEnvelope{AgentId: "agent-1", SessionId: "session-1", MessageId: "switch-inventory", Sequence: 2, SentAt: timestamppb.New(now),
		Payload: &pb.AgentEnvelope_Inventory{Inventory: &pb.InventoryUpdate{ObservedAt: timestamppb.New(now), Devices: []*pb.Device{{Id: "switch-1", Hostname: "switch", ChassisId: "00:01:02:03:04:05", Source: "simulated"}}}}}); err != nil {
		t.Fatal(err)
	}
	original := topologyEnvelope("topology-1", now)
	if err := s.Ingest(ctx, original); err != nil {
		t.Fatal(err)
	}
	g, err := s.Topology(ctx, "agent-1", "shared-router", false, 500, 0)
	if err != nil || len(g.Edges) != 1 || g.Edges[0].Resolution.Method != "chassis_mac" || len(g.Nodes) != 2 {
		t.Fatalf("graph projection failed: %+v %v", g, err)
	}
	replay := proto.Clone(original).(*pb.AgentEnvelope)
	replay.Sequence = 10
	replay.SentAt = timestamppb.New(now.Add(time.Minute))
	if err = s.Ingest(ctx, replay); err != nil {
		t.Fatal(err)
	}
	var snapshots, observations int
	if err = s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM topology_snapshots),(SELECT count(*) FROM topology_observations)`).Scan(&snapshots, &observations); err != nil || snapshots != 1 || observations != 1 {
		t.Fatalf("replay duplicated graph: %d %d %v", snapshots, observations, err)
	}
	for i, state := range []pb.SnapshotCompleteness{pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_PARTIAL, pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_FAILED, pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_COMPLETE} {
		update := topologyEnvelope("empty-"+state.String(), now.Add(time.Duration(i+1)*time.Second))
		update.GetTopology().Completeness = state
		update.GetTopology().Observations = nil
		if err = s.Ingest(ctx, update); err != nil {
			t.Fatal(err)
		}
		g, err = s.Topology(ctx, "agent-1", "shared-router", false, 500, 0)
		if err != nil || len(g.Edges) != 1 || g.Collections[0].Completeness != state.String() {
			t.Fatalf("empty poll removed a live link: %+v %v", g, err)
		}
	}
	old := topologyEnvelope("older", now.Add(-time.Hour))
	if err = s.Ingest(ctx, old); err != nil {
		t.Fatal(err)
	}
	g, err = s.Topology(ctx, "agent-1", "shared-router", false, 500, 0)
	if err != nil || len(g.Edges) != 1 || !g.Edges[0].ObservedAt.Equal(now) {
		t.Fatalf("older poll regressed graph: %+v %v", g, err)
	}
	expired := topologyEnvelope("expired-evidence", now.Add(-time.Hour))
	expired.GetTopology().Observations[0].LocalInterfaceId = "ether3"
	if err = s.Ingest(ctx, expired); err != nil {
		t.Fatal(err)
	}
	g, err = s.Topology(ctx, "agent-1", "shared-router", false, 500, 0)
	if err != nil || len(g.Edges) != 1 {
		t.Fatalf("late receipt revived expired evidence: %+v %v", g, err)
	}
	g, err = s.Topology(ctx, "agent-1", "shared-router", true, 500, 0)
	if err != nil || len(g.Edges) != 2 {
		t.Fatalf("expired evidence missing from diagnostics: %+v %v", g, err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	g, err = s.Topology(ctx, "agent-1", "shared-router", false, 500, 0)
	if err != nil || len(g.Edges) != 1 {
		t.Fatalf("migration lost graph: %+v %v", g, err)
	}
}

func TestTopologyProjectionAtomicityAndEarlyInventory(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	bad := topologyEnvelope("bad", now)
	bad.GetTopology().Observations = append(bad.GetTopology().Observations, &pb.Neighbor{LocalDeviceId: "different-device"})
	if err := s.Ingest(ctx, bad); err == nil {
		t.Fatal("invalid topology committed")
	}
	var inbox, snapshots int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_messages WHERE message_id='bad'),(SELECT count(*) FROM topology_snapshots)`).Scan(&inbox, &snapshots); err != nil || inbox != 0 || snapshots != 0 {
		t.Fatalf("partial topology committed: %d %d %v", inbox, snapshots, err)
	}
	early := topologyEnvelope("early", now)
	early.GetTopology().DeviceId = "before-inventory"
	early.GetTopology().Observations[0].LocalDeviceId = "before-inventory"
	if err := s.Ingest(ctx, early); err != nil {
		t.Fatal(err)
	}
	d, err := s.Device(ctx, "agent-1", "before-inventory")
	if err != nil || d.Online || d.Name != "before-inventory" {
		t.Fatalf("topology invented device health: %+v %v", d, err)
	}
	earlyGraph, err := s.Topology(ctx, "agent-1", "before-inventory", false, 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range earlyGraph.Nodes {
		if node.DeviceID == "before-inventory" && node.ReportedOnline != nil {
			t.Fatal("placeholder invented a reported offline state")
		}
	}
	if err = s.Ingest(ctx, registration("agent-2", now)); err != nil {
		t.Fatal(err)
	}
	g, err := s.Topology(ctx, "agent-2", "shared-router", false, 500, 0)
	if err != nil || len(g.Edges) != 0 {
		t.Fatalf("another Agent sees foreign edges: %+v %v", g, err)
	}
}

func TestTopologyDatabaseFailureRollsBackInboxAndAllLinks(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	_, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_test_topology() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN IF NEW.payload->>'local_interface_id'='reject-me' THEN RAISE EXCEPTION 'injected topology persistence failure'; END IF; RETURN NEW; END $$;
	CREATE TRIGGER reject_test_topology BEFORE INSERT ON topology_observations FOR EACH ROW EXECUTE FUNCTION reject_test_topology()`)
	if err != nil {
		t.Fatal(err)
	}
	e := topologyEnvelope("database-failure", now)
	n := proto.Clone(e.GetTopology().Observations[0]).(*pb.Neighbor)
	n.LocalInterfaceId = "reject-me"
	e.GetTopology().Observations = append(e.GetTopology().Observations, n)
	if err = s.Ingest(ctx, e); err == nil {
		t.Fatal("injected database failure was acknowledged")
	}
	var inbox, snapshots, links int
	err = s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_messages WHERE message_id='database-failure'),
	(SELECT count(*) FROM topology_snapshots),(SELECT count(*) FROM topology_observations)`).Scan(&inbox, &snapshots, &links)
	if err != nil || inbox != 0 || snapshots != 0 || links != 0 {
		t.Fatalf("partial batch committed: %d/%d/%d %v", inbox, snapshots, links, err)
	}
}

func TestFutureTopologyCannotPoisonCurrentGraphAndRecovers(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, topologyEnvelope("valid", now)); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, topologyEnvelope("future-same-edge", now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	g, err := s.Topology(ctx, "agent-1", "shared-router", false, 500, 0)
	if err != nil || len(g.Edges) != 1 || !g.Edges[0].ObservedAt.Equal(now) || g.Collections[0].ClockSkew {
		t.Fatalf("future poll poisoned valid evidence: %+v %v", g, err)
	}
	future := topologyEnvelope("future-new-edge", now.Add(time.Hour))
	future.GetTopology().Observations[0].LocalInterfaceId = "future-port"
	if err = s.Ingest(ctx, future); err != nil {
		t.Fatal(err)
	}
	g, err = s.Topology(ctx, "agent-1", "shared-router", true, 500, 0)
	if err != nil || len(g.Edges) != 2 {
		t.Fatalf("future diagnostic lost: %+v %v", g, err)
	}
	skew := 0
	for _, edge := range g.Edges {
		if edge.Status == "clock_skew" {
			skew++
		}
	}
	if skew != 1 {
		t.Fatal("future diagnostic marked active")
	}
	recovered := topologyEnvelope("clock-corrected", now)
	recovered.GetTopology().Observations[0].LocalInterfaceId = "future-port"
	if err = s.Ingest(ctx, recovered); err != nil {
		t.Fatal(err)
	}
	g, err = s.Topology(ctx, "agent-1", "shared-router", false, 500, 0)
	if err != nil || len(g.Edges) != 2 {
		t.Fatalf("corrected clock could not replace future evidence: %+v %v", g, err)
	}
}
