package storage

import (
	"context"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/topology"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"testing"
	"time"
)

func physicalTopologyFixture(t *testing.T) (*Store, context.Context, map[string]string, time.Time) {
	t.Helper()
	s := integrationStore(t)
	_, admin := adminFixture(t, s)
	ctx := access.WithPrincipal(t.Context(), admin)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, agent := range []string{"collector-one", "collector-two"} {
		if err := s.Ingest(ctx, registration(agent, now)); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"switch-a", "switch-b"} {
			mac := "02:00:00:00:00:01"
			if id == "switch-b" {
				mac = "02:00:00:00:00:02"
			}
			d := &pb.Device{Id: id, Name: id, Hostname: id, Vendor: "Eltex", Model: "MES", SerialNumber: "SERIAL-" + id, ChassisId: mac, Source: "snmp", Type: pb.DeviceType_DEVICE_TYPE_SWITCH}
			if err := s.Ingest(ctx, assetInventory(agent, "session-1", id, now, d)); err != nil {
				t.Fatal(err)
			}
		}
	}
	ids := map[string]string{}
	for _, device := range []string{"switch-a", "switch-b"} {
		items, err := s.Assets(ctx, "network_device", "real", 50, 0)
		if err != nil {
			t.Fatal(err)
		}
		selected := []assets.Asset{}
		bindingIDs := []string{}
		for _, a := range items {
			if a.CanonicalName == device {
				b, err := s.AssetBindings(ctx, a.ID, false, 50, 0)
				if err != nil {
					t.Fatal(err)
				}
				selected = append(selected, a)
				bindingIDs = append(bindingIDs, b[0].ID)
			}
		}
		if len(selected) != 2 {
			t.Fatal("source fixture", selected)
		}
		in := assets.MergeInput{TargetID: selected[0].ID, SourceID: selected[1].ID, TargetRevision: selected[0].Revision, SourceRevision: selected[1].Revision, CanonicalBindingID: bindingIDs[0], Confirmed: true, Reason: "verified same physical serial"}
		if _, _, err = s.MergeAssets(ctx, in, "merge-"+device); err != nil {
			t.Fatal(err)
		}
		ids[device] = selected[0].ID
	}
	return s, ctx, ids, now
}
func globalEnvelope(agent, message, from, to, local, remote string, now time.Time) *pb.AgentEnvelope {
	mac := []byte{2, 0, 0, 0, 0, 1}
	if to == "switch-b" {
		mac[5] = 2
	}
	return &pb.AgentEnvelope{AgentId: agent, SessionId: "session-1", MessageId: message, Sequence: 3, SentAt: timestamppb.New(now), Payload: &pb.AgentEnvelope_Topology{Topology: &pb.TopologyUpdate{DeviceId: from, Source: "snmp.lldp", ObservedAt: timestamppb.New(now), TtlSeconds: 60, Completeness: pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_COMPLETE, Observations: []*pb.Neighbor{{LocalDeviceId: from, LocalPortSubtype: 5, LocalPortId: []byte(local), RemotePortSubtype: 5, RemotePortId: []byte(remote), RemoteChassisSubtype: 4, RemoteChassisId: mac, Protocol: pb.DiscoveryProtocol_DISCOVERY_PROTOCOL_LLDP, Source: "snmp.lldp", Confidence: "high"}}}}}
}
func TestGlobalTopologyAcrossCollectorsHistoryExpiryAndReplay(t *testing.T) {
	s, ctx, ids, now := physicalTopologyFixture(t)
	a := globalEnvelope("collector-one", "one-a", "switch-a", "switch-b", "eth1", "eth2", now)
	b := globalEnvelope("collector-two", "two-a", "switch-a", "switch-b", "eth1", "eth2", now)
	reverse := globalEnvelope("collector-two", "two-b", "switch-b", "switch-a", "eth2", "eth1", now)
	for _, e := range []*pb.AgentEnvelope{a, b, reverse} {
		if err := s.Ingest(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	f := topology.GlobalFilter{Scope: "real", Limit: 500}
	g, err := s.GlobalTopology(ctx, f)
	if err != nil || len(g.Nodes) != 2 || len(g.Edges) != 1 || g.Edges[0].Confirmation != "bidirectional" || len(g.Edges[0].Evidence) != 3 {
		t.Fatal("global network duplicated", g, err)
	}
	replay := proto.Clone(a).(*pb.AgentEnvelope)
	replay.Sequence = 99
	if err = s.Ingest(ctx, replay); err != nil {
		t.Fatal(err)
	}
	refresh := proto.Clone(a).(*pb.AgentEnvelope)
	refresh.MessageId = "one-refresh"
	refresh.SentAt = timestamppb.New(now.Add(time.Second))
	refresh.GetTopology().ObservedAt = refresh.SentAt
	if err = s.Ingest(ctx, refresh); err != nil {
		t.Fatal(err)
	}
	history, err := s.GlobalTopologyChanges(ctx, "", "all", "", 0, 50)
	if err != nil || len(history.Items) != 3 {
		t.Fatal("clock/replay duplicated business history", history, err)
	}
	page, err := s.GlobalTopologyChanges(ctx, "", "all", "", 0, 2)
	if err != nil || !page.HasMore || len(page.Items) != 2 {
		t.Fatal("history pagination", page, err)
	}
	next, err := s.GlobalTopologyChanges(ctx, "", "all", "", page.NextAfterID, 2)
	if err != nil || len(next.Items) != 1 || next.HasMore {
		t.Fatal("history continuation", next, err)
	}
	f.AssetID = ids["switch-a"]
	neighbors, err := s.GlobalTopology(ctx, f)
	if err != nil || len(neighbors.Edges) != 1 {
		t.Fatal("asset neighbors", neighbors, err)
	}
	if err = s.ExpireGlobalTopology(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = s.ExpireGlobalTopology(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	history, err = s.GlobalTopologyChanges(ctx, "", "all", "", 0, 50)
	if err != nil || len(history.Items) != 6 {
		t.Fatal("expiry history duplicated", history, err)
	}
	revived := globalEnvelope("collector-one", "revived", "switch-a", "switch-b", "eth1", "eth2", time.Now().UTC().Add(2*time.Second))
	if err = s.Ingest(ctx, revived); err != nil {
		t.Fatal(err)
	}
	history, err = s.GlobalTopologyChanges(ctx, "", "all", "", 0, 50)
	if err != nil || history.Items[len(history.Items)-1].Kind != "revived" {
		t.Fatal("revival not recorded", history, err)
	}
	changed := globalEnvelope("collector-one", "port-changed", "switch-a", "switch-b", "eth-new", "eth2", time.Now().UTC().Add(3*time.Second))
	if err = s.Ingest(ctx, changed); err != nil {
		t.Fatal(err)
	}
	f.AssetID = ""
	g, err = s.GlobalTopology(ctx, f)
	if err != nil || len(g.Edges) != 2 {
		t.Fatal("new port replaced unrelated observation", g, err)
	}
	old := globalEnvelope("collector-one", "old", "switch-a", "switch-b", "eth1", "eth2", now.Add(-time.Hour))
	if err = s.Ingest(ctx, old); err != nil {
		t.Fatal(err)
	}
	history, err = s.GlobalTopologyChanges(ctx, "", "all", "", 0, 50)
	if err != nil || len(history.Items) != 8 {
		t.Fatal("late input rewrote history", history, err)
	}
	future := globalEnvelope("collector-one", "bad-future", "switch-a", "switch-b", "eth1", "eth2", now.Add(time.Hour))
	if err = s.Ingest(ctx, future); err != nil {
		t.Fatal(err)
	}
	history, err = s.GlobalTopologyChanges(ctx, "", "all", "", 0, 50)
	if err != nil || len(history.Items) != 8 {
		t.Fatal("future poll replaced accepted evidence", history, err)
	}
	change := proto.Clone(revived).(*pb.AgentEnvelope)
	change.MessageId = "confidence-change"
	change.GetTopology().ObservedAt = timestamppb.New(now.Add(4 * time.Second))
	change.SentAt = change.GetTopology().ObservedAt
	change.GetTopology().Observations[0].Confidence = "medium"
	if err = s.Ingest(ctx, change); err != nil {
		t.Fatal(err)
	}
	history, err = s.GlobalTopologyChanges(ctx, "", "all", "", 0, 50)
	if err != nil || len(history.Items) != 9 || history.Items[8].Kind != "changed" {
		t.Fatal("business change lost", history, err)
	}
}
func TestGlobalTopologyUpgradeAtomicFailureAndSimulation(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_global_topology() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture projection failure'; END $$; CREATE TRIGGER reject_global_topology BEFORE INSERT ON global_topology_versions FOR EACH ROW EXECUTE FUNCTION reject_global_topology()`); err != nil {
		t.Fatal(err)
	}
	e := topologyEnvelope("atomic-global", now)
	if err := s.Ingest(ctx, e); err == nil {
		t.Fatal("global projection failure acknowledged")
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM agent_messages WHERE message_id='atomic-global'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial inbox committed", count, err)
	}
	if _, err := s.pool.Exec(ctx, `DROP TRIGGER reject_global_topology ON global_topology_versions; DROP FUNCTION reject_global_topology(); DROP TABLE global_topology_events,global_topology_latest,global_topology_versions; DELETE FROM schema_migrations WHERE version=27`); err != nil {
		t.Fatal(err)
	}
	// Seed a populated B6 topology without invoking the new projection.
	raw, err := wireJSON(e)
	if err != nil {
		t.Fatal(err)
	}
	neighbor, err := wireJSON(e.GetTopology().Observations[0])
	if err != nil {
		t.Fatal(err)
	}
	observations, err := topology.Prepare(e.AgentId, e.GetTopology(), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO agent_messages(agent_id,message_id,session_id,sequence,sent_at,kind,payload) VALUES($1,$2,$3,3,$4,'topology',$5)`, e.AgentId, e.MessageId, e.SessionId, now, raw); err != nil {
		t.Fatal(err)
	}
	o := observations[0]
	if _, err = s.pool.Exec(ctx, `INSERT INTO topology_observations(agent_id,edge_id,device_id,message_id,source,protocol,observed_at,expires_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, o.AgentID, o.ID, o.DeviceID, e.MessageId, o.Source, o.Protocol, o.ObservedAt, o.ExpiresAt, neighbor); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("B6 upgrade", err)
	}
	history, err := s.GlobalTopologyChanges(ctx, "", "simulated", "", 0, 50)
	if err != nil || len(history.Items) != 1 || history.Items[0].Kind != "imported" || history.Items[0].Scope != "session-1" {
		t.Fatal("upgrade lost source namespace", history, err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	g, err := s.GlobalTopology(ctx, topology.GlobalFilter{Scope: "real", Limit: 500})
	if err != nil || len(g.Edges) != 0 {
		t.Fatal("simulation became real network", g, err)
	}
	g, err = s.GlobalTopology(ctx, topology.GlobalFilter{Scope: "simulated", Limit: 500})
	if err != nil || len(g.Edges) != 1 {
		t.Fatal("simulated graph lost", g, err)
	}
}

func TestGlobalTopologyRevivalRecordsMissedExpiryAtomically(t *testing.T) {
	s, ctx, _, now := physicalTopologyFixture(t)
	e := globalEnvelope("collector-one", "before-expiry", "switch-a", "switch-b", "eth1", "eth2", now)
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE global_topology_latest SET expires_at=now()-interval '1 second',expiry_logged=false`); err != nil {
		t.Fatal(err)
	}
	refresh := proto.Clone(e).(*pb.AgentEnvelope)
	refresh.MessageId = "after-expiry"
	refresh.GetTopology().ObservedAt = timestamppb.New(now.Add(time.Second))
	refresh.SentAt = refresh.GetTopology().ObservedAt
	if err := s.Ingest(ctx, refresh); err != nil {
		t.Fatal(err)
	}
	history, err := s.GlobalTopologyChanges(ctx, "", "all", "", 0, 50)
	if err != nil || len(history.Items) != 3 {
		t.Fatal("missed expiry lost", history, err)
	}
	kinds := map[string]int{}
	for _, v := range history.Items {
		kinds[v.Kind]++
	}
	if kinds["expired"] != 1 || kinds["revived"] != 1 {
		t.Fatal("expiry/revival transition", kinds)
	}
	if history.Items[1].Kind != "expired" || history.Items[2].Kind != "revived" {
		t.Fatal("revival committed before its missed expiry", history)
	}
	if err = s.ExpireGlobalTopology(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	history, err = s.GlobalTopologyChanges(ctx, "", "all", "", 0, 50)
	if err != nil || len(history.Items) != 3 {
		t.Fatal("worker duplicated catchup expiry", history, err)
	}
}
