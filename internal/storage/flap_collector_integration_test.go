package storage

import (
	"fmt"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
)

func TestFlapPersistsChangesAndRecoversBeyond120Samples(t *testing.T) {
	s, ctx, now := healthFixture(t)
	start := now.Add(-315 * time.Second)
	for i := 0; i < 5; i++ {
		at := start.Add(time.Duration(i) * 3 * time.Second)
		s.healthClock = func() time.Time { return at }
		if err := s.Ingest(ctx, resourceEnvelope(fmt.Sprint("flap", i), at, resourcePort(i%2 == 0))); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.Incidents(ctx, health.Filter{RuleID: "interface.flap"})
	if err != nil || len(items) != 1 || items[0].Severity != "warning" {
		t.Fatalf("flap incident: %+v %v", items, err)
	}
	id := items[0].ID
	// Faster sampling rolls the telemetry window away; persistent change/baseline
	// history still permits quiet recovery after 300 seconds.
	for i := 1; i <= 303; i++ {
		at := start.Add(12 * time.Second).Add(time.Duration(i) * time.Second)
		s.healthClock = func() time.Time { return at }
		if err := s.Ingest(ctx, resourceEnvelope(fmt.Sprint("quiet", i), at, resourcePort(true))); err != nil {
			t.Fatal(err)
		}
	}
	incident, err := s.Incident(ctx, id)
	if err != nil || incident.State != "resolved" {
		t.Fatal("quiet recovery lost after telemetry window rollover")
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM interface_state_changes`).Scan(&count); err != nil || count != 4 {
		t.Fatal("repeated up generated state transitions")
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	incident, err = s.Incident(ctx, id)
	if err != nil || incident.State != "resolved" {
		t.Fatal("flap history lost at restart")
	}
}
func TestFlapUnknownAdministrativeStateDoesNotCount(t *testing.T) {
	s, ctx, now := healthFixture(t)
	start := now.Add(-12 * time.Second)
	for i := 0; i < 5; i++ {
		at := start.Add(time.Duration(i) * 3 * time.Second)
		s.healthClock = func() time.Time { return at }
		port := resourcePort(i%2 == 0)
		if i == 1 || i == 3 {
			port.AdminUp = false
		}
		if err := s.Ingest(ctx, resourceEnvelope(fmt.Sprint("admin-flap", i), at, port)); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.Incidents(ctx, health.Filter{RuleID: "interface.flap"})
	if err != nil || len(items) != 0 {
		t.Fatal("administrative/unknown state counted as physical flap")
	}
}
func TestCollectorLossRecoveryAndNoDownstreamOffline(t *testing.T) {
	s, ctx, now := healthFixture(t)
	s.collectorStartedAt = now.Add(-2 * time.Minute)
	sweep := func(at time.Time) {
		t.Helper()
		if _, err := s.SweepCollectors(ctx, at, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Disconnect(ctx, "agent-1", "session-1"); err != nil {
		t.Fatal(err)
	}
	sweep(now)
	sweep(now.Add(14 * time.Second))
	items, _ := s.Incidents(ctx, health.Filter{ResourceKind: "agent"})
	if len(items) != 0 {
		t.Fatal("short connection interruption created incident")
	}
	sweep(now.Add(15 * time.Second))
	items, err := s.Incidents(ctx, health.Filter{ResourceKind: "agent"})
	if err != nil || len(items) != 1 || items[0].ResourceID != "agent-1" || items[0].DeviceID != "" || items[0].EvidenceStatus != "fresh" {
		t.Fatalf("collector incident: %+v %v", items, err)
	}
	id := items[0].ID
	var reported bool
	if err = s.pool.QueryRow(ctx, `SELECT online FROM devices WHERE agent_id='agent-1'`).Scan(&reported); err != nil || !reported {
		t.Fatal("collector loss fabricated device offline")
	}
	// Fresh repeated transport observations after reconnect, not Hello alone.
	if _, err = s.pool.Exec(ctx, `UPDATE agents SET connected=true,last_seen=$1`, now.Add(16*time.Second)); err != nil {
		t.Fatal(err)
	}
	sweep(now.Add(16 * time.Second))
	if _, err = s.pool.Exec(ctx, `UPDATE agents SET last_seen=$1`, now.Add(22*time.Second)); err != nil {
		t.Fatal(err)
	}
	sweep(now.Add(31 * time.Second))
	incident, err := s.Incident(ctx, id)
	if err != nil || incident.State != "resolved" {
		t.Fatal("sustained collector recovery failed")
	}
	card, err := s.ResourceHealth(ctx, "agent-1", "", "agent", "agent-1")
	if err != nil || len(card) != 1 {
		t.Fatal("agent health card missing")
	}
}
func TestCollectorStartupGraceAndAtomicFailure(t *testing.T) {
	s, ctx, now := healthFixture(t)
	s.collectorStartedAt = now
	if err := s.Disconnect(ctx, "agent-1", "session-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SweepCollectors(ctx, now.Add(30*time.Second), ""); err != nil {
		t.Fatal(err)
	}
	items, _ := s.Incidents(ctx, health.Filter{ResourceKind: "agent"})
	if len(items) != 0 {
		t.Fatal("startup fabricated outage")
	}
	at := now.Add(91 * time.Second)
	if _, err := s.SweepCollectors(ctx, at, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE incident_evidence ADD CONSTRAINT reject_collector CHECK(kind<>'opened')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SweepCollectors(ctx, at.Add(15*time.Second), ""); err == nil {
		t.Fatal("SQL failure missing")
	}
	items, _ = s.Incidents(ctx, health.Filter{ResourceKind: "agent"})
	if len(items) != 0 {
		t.Fatal("partial collector incident persisted")
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE incident_evidence DROP CONSTRAINT reject_collector`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SweepCollectors(ctx, at.Add(15*time.Second), ""); err != nil {
		t.Fatal(err)
	}
	items, _ = s.Incidents(ctx, health.Filter{ResourceKind: "agent"})
	if len(items) != 1 {
		t.Fatal("collector retry not unique")
	}
}
