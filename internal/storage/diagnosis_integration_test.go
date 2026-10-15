package storage

import (
	"encoding/json"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestIncidentContextFreshResolvedTopologyAndExpiry(t *testing.T) {
	s, ctx, now := healthFixture(t)
	inv := &pb.AgentEnvelope{AgentId: "agent-1", SessionId: "session-1", MessageId: "context-inventory", Sequence: 2, SentAt: timestamppb.Now(), Payload: &pb.AgentEnvelope_Inventory{Inventory: &pb.InventoryUpdate{ObservedAt: timestamppb.New(now), Devices: []*pb.Device{{Id: "shared-router", Source: "lldp", ChassisId: "00:10:20:30:40:50"}, {Id: "switch-1", Source: "lldp", ChassisId: "00:01:02:03:04:05"}}}}}
	if err := s.Ingest(ctx, inv); err != nil {
		t.Fatal(err)
	}
	topo := topologyEnvelope("context-topology", now)
	topo.GetTopology().Source = "lldp"
	topo.GetTopology().Observations[0].Source = "lldp"
	if err := s.Ingest(ctx, topo); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	at := now
	a := health.Assessment{RuleID: "interface.oper_down", Status: "critical", Severity: "critical", ObservedAt: &at, Evidence: []health.Point{}, Policy: health.Defaults()}
	raw, _ := json.Marshal(a)
	if err = applyResourceIncident(ctx, tx, "agent-1", "shared-router", "linux", "", "interface", "ether1", "", a, raw); err != nil {
		t.Fatal(err)
	}
	a.RuleID = "cpu.sustained"
	raw, _ = json.Marshal(a)
	if err = applyIncident(ctx, tx, "agent-1", "switch-1", "linux", "", a, raw); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	incidents, _ := s.Incidents(ctx, health.Filter{RuleID: "interface.oper_down"})
	if len(incidents) != 1 {
		t.Fatal("seed missing")
	}
	id := incidents[0].ID
	result, err := s.IncidentContext(ctx, id, 300, 20)
	if err != nil || len(result.Related) != 1 || result.Related[0].Relation != "port_and_neighbor_symptom_candidate" || result.Related[0].CausalClaim {
		t.Fatalf("context=%+v err=%v", result, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE topology_observations SET observed_at=$1::timestamptz-interval '1 minute',expires_at=$1`, now.Add(-time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	result, err = s.IncidentContext(ctx, id, 300, 20)
	if err != nil || len(result.Related) != 1 || len(result.Related[0].Links) != 0 || result.Related[0].Strength != "weak" {
		t.Fatal("expired topology still used")
	}
	if _, err = s.pool.Exec(ctx, `UPDATE incidents SET state='resolved',resolved_at=now() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	result, err = s.IncidentContext(ctx, id, 300, 20)
	if err != nil || result.TopologyConsidered {
		t.Fatal("resolved incident uses live topology as historical evidence")
	}
}
