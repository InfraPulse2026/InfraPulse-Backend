package storage

import (
	"encoding/json"
	"errors"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"strings"
	"testing"
	"time"
)

func TestTopologyIncidentPeerChangeReviewReplayAndPartialIsolation(t *testing.T) {
	s, ctx, _, now := physicalTopologyFixture(t)
	emit := func(message, remote string, seconds int, partial bool) {
		t.Helper()
		e := globalEnvelope("collector-one", message, "switch-a", "switch-b", "eth1", remote, now.Add(time.Duration(seconds)*time.Second))
		if partial {
			e.GetTopology().Completeness = pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_PARTIAL
		}
		if err := s.Ingest(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	process := func() {
		t.Helper()
		if _, err := s.ProcessCorrelation(ctx); err != nil {
			t.Fatal(err)
		}
	}
	list := func() []health.Incident {
		t.Helper()
		v, err := s.Incidents(ctx, health.Filter{ResourceKind: "topology", State: "open"})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	emit("first", "eth2", 0, false)
	process()
	if len(list()) != 0 {
		t.Fatal("initial discovery alerted")
	}
	emit("refresh", "eth2", 1, false)
	process()
	if len(list()) != 0 {
		t.Fatal("refresh alerted")
	}
	emit("replacement", "eth3", 2, false)
	process()
	v := list()
	if len(v) != 1 || v[0].Signal.Topology.EventCount != 1 || v[0].RuleID != "topology.peer_changed" {
		t.Fatal(v)
	}
	first := v[0]
	// A nearby CPU symptom remains active after review of a topology event.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	at := now.Add(2 * time.Second)
	a := health.Assessment{RuleID: "cpu.sustained", RuleVersion: "1", Status: "critical", Severity: "critical", ObservedAt: &at, Evidence: []health.Point{}}
	encoded, _ := json.Marshal(a)
	if err = applyIncident(ctx, tx, "collector-one", "switch-a", "snmp", "", a, encoded); err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	process()
	emit("replacement", "eth3", 2, false)
	process()
	if list()[0].Signal.Topology.EventCount != 1 {
		t.Fatal("replay duplicated event")
	}
	emit("partial", "eth4", 3, true)
	process()
	if list()[0].Signal.Topology.EventCount != 1 {
		t.Fatal("partial alerted")
	}
	emit("complete-same", "eth3", 4, false)
	process()
	if list()[0].Signal.Topology.EventCount != 1 {
		t.Fatal("partial changed baseline")
	}
	emit("replacement-again", "eth4", 5, false)
	process()
	if list()[0].Signal.Topology.EventCount != 2 {
		t.Fatal("second replacement missing")
	}
	emit("pending", "eth5", 6, false)
	review := health.LogReview{Confirmed: true, ExpectedEventCount: 2, Reason: "checked reported topology change"}
	if _, err := s.ReviewTopologyIncident(ctx, first.ID, review); !errors.Is(err, health.ErrConflict) {
		t.Fatal("pending change closed unseen", err)
	}
	process()
	review.ExpectedEventCount = 3
	closed, err := s.ReviewTopologyIncident(ctx, first.ID, review)
	if err != nil || closed.State != "resolved" || closed.Signal.RecoveryEligible {
		t.Fatal(closed, err)
	}
	if _, err = s.ReviewTopologyIncident(ctx, first.ID, review); err != nil {
		t.Fatal("review retry", err)
	}
	process()
	components, err := s.Incidents(ctx, health.Filter{RuleID: "cpu.sustained", State: "open"})
	if err != nil || len(components) != 1 {
		t.Fatal("topology review closed component problem", components, err)
	}
	proof, err := s.TopologyIncidentRecords(ctx, first.ID, 100, 0)
	if err != nil || len(proof) != 3 || proof[0].BeforeSnapshotID == proof[0].AfterSnapshotID {
		t.Fatal(proof, err)
	}
	var category string
	var assets int
	if err = s.pool.QueryRow(ctx, `SELECT category,(SELECT count(*) FROM normalized_signal_assets a WHERE a.signal_id=n.id) FROM normalized_signals n WHERE incident_id=$1`, first.ID).Scan(&category, &assets); err != nil || category != "topology" || assets != 1 {
		t.Fatal(category, assets, err)
	}
	emit("new-occurrence", "eth6", 7, false)
	process()
	v = list()
	if len(v) != 1 || v[0].Occurrence != 2 || v[0].Signal.Topology.EventCount != 1 {
		t.Fatal(v)
	}
	e := globalEnvelope("collector-one", "empty-full", "switch-a", "switch-b", "eth1", "eth6", now.Add(8*time.Second))
	e.GetTopology().Observations = nil
	if err = s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	process()
	emit("rediscovery", "eth7", 9, false)
	process()
	if list()[0].Signal.Topology.EventCount != 1 {
		t.Fatal("absence/rediscovery treated as replacement")
	}
	stale := []health.Incident{v[0]}
	if err = s.topologyIncidentFreshness(ctx, s.pool, stale, now.Add(2*time.Minute)); err != nil || stale[0].State != "open" || stale[0].EvidenceStatus == "fresh" {
		t.Fatal("expiry recovered event", stale, err)
	}
	raw, _ := json.Marshal(first)
	if strings.Contains(string(raw), "neighbor") && strings.Contains(string(raw), "remote_chassis_id") {
		t.Fatal("raw Neighbor copied to incident", string(raw))
	}
	for _, step := range []struct {
		message, port string
		seconds       int
	}{{"other-first", "eth2", 0}, {"other-changed", "eth3", 1}} {
		other := globalEnvelope("collector-two", step.message, "switch-a", "switch-b", "eth1", step.port, now.Add(time.Duration(step.seconds)*time.Second))
		if err = s.Ingest(ctx, other); err != nil {
			t.Fatal(err)
		}
		process()
	}
	foreign, err := s.Incidents(ctx, health.Filter{ResourceKind: "topology", AgentID: "collector-two"})
	if err != nil || len(foreign) != 1 || foreign[0].ResourceID == v[0].ResourceID {
		t.Fatal("Collector scopes collapsed", foreign, err)
	}
	var audits int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM access_audit WHERE action='topology_event_reviewed'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("review retry duplicated audit", audits, err)
	}
	actor, _ := access.Current(ctx)
	if _, err = s.pool.Exec(ctx, `UPDATE access_users SET role='viewer' WHERE id=$1`, actor.User.ID); err != nil {
		t.Fatal(err)
	}
	review.ExpectedEventCount = 1
	if _, err = s.ReviewTopologyIncident(ctx, v[0].ID, review); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("revoked role accepted", err)
	}
}

func TestTopologyIncidentWorkerRollbackPreservesQueueAndSnapshots(t *testing.T) {
	s, ctx, _, now := physicalTopologyFixture(t)
	first := globalEnvelope("collector-one", "first", "switch-a", "switch-b", "eth1", "eth2", now)
	if err := s.Ingest(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessTopologySignals(ctx); err != nil {
		t.Fatal(err)
	}
	changed := globalEnvelope("collector-one", "changed", "switch-a", "switch-b", "eth1", "eth3", now.Add(time.Second))
	if err := s.Ingest(ctx, changed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE topology_incident_records ADD CONSTRAINT reject_peer_change CHECK(before_snapshot_id<0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessTopologySignals(ctx); err == nil {
		t.Fatal("expected SQL failure")
	}
	var pending, incidents, snapshots int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM topology_signal_pending),(SELECT count(*) FROM incidents WHERE resource_kind='topology'),(SELECT count(*) FROM topology_snapshots)`).Scan(&pending, &incidents, &snapshots); err != nil || pending != 1 || incidents != 0 || snapshots != 2 {
		t.Fatal(pending, incidents, snapshots, err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE topology_incident_records DROP CONSTRAINT reject_peer_change`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
}
