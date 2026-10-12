package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func healthEnvelope(id string, index int, at time.Time, value float64, source string) *pb.AgentEnvelope {
	return &pb.AgentEnvelope{AgentId: "agent-1", SessionId: "session-1", MessageId: id, Sequence: uint64(index + 2), SentAt: timestamppb.Now(), Payload: &pb.AgentEnvelope_Telemetry{Telemetry: &pb.TelemetryBatch{Samples: []*pb.TelemetrySample{{DeviceId: "shared-router", Source: source, BootId: "boot-1", ObservedAt: timestamppb.New(at), CollectedAt: timestamppb.Now(), Cpu: &pb.CPUStats{UsagePercent: &value}}}}}}
}
func healthFixture(t *testing.T) (*Store, context.Context, time.Time) {
	t.Helper()
	s := integrationStore(t)
	_, p := adminFixture(t, s)
	ctx := access.WithPrincipal(t.Context(), p)
	at := time.Now().UTC().Add(-time.Second)
	if err := s.Ingest(ctx, registration("agent-1", at)); err != nil {
		t.Fatal(err)
	}
	return s, ctx, at
}
func TestHealthIncidentLifecycleDedupAndNoFalseRecovery(t *testing.T) {
	s, ctx, now := healthFixture(t)
	var replay *pb.AgentEnvelope
	for i, v := range []float64{81, 82, 83} {
		envelope := healthEnvelope(fmt.Sprint("high", i), i, now.Add(time.Duration(i-2)*3*time.Second), v, "linux")
		if i == 2 {
			replay = envelope
		}
		if err := s.Ingest(ctx, envelope); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.Incidents(ctx, health.Filter{})
	if err != nil || len(list) != 1 || list[0].State != "open" || list[0].RuleID != "cpu.sustained" {
		t.Fatalf("incidents=%+v err=%v", list, err)
	}
	id := list[0].ID
	if err = s.Ingest(ctx, replay); err != nil {
		t.Fatal(err)
	}
	evidence, _ := s.IncidentEvidence(ctx, id)
	if len(evidence) != 1 {
		t.Fatal("duplicate trigger evidence")
	}
	if _, err = s.AcknowledgeIncident(context.Background(), id); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("anonymous acknowledge allowed")
	}
	for i := 0; i < 2; i++ {
		if _, err = s.AcknowledgeIncident(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	transitions, _ := s.IncidentTransitions(ctx, id)
	if len(transitions) != 2 {
		t.Fatal("acknowledgment not idempotent")
	}
	// Historical sample must not replace the current incident or create an event.
	if err = s.Ingest(ctx, healthEnvelope("late-low", 30, now.Add(-30*time.Second), 0, "linux")); err != nil {
		t.Fatal(err)
	}
	record, _ := s.Incident(ctx, id)
	if record.State != "acknowledged" {
		t.Fatal("late packet closed incident")
	}
	// Future timestamps cannot manufacture recovery.
	for i, v := range []float64{50, 50, 50} {
		at := now.Add(time.Duration(i+1) * 3 * time.Second)
		envelope := healthEnvelope(fmt.Sprint("future-low", i), 40+i, at, v, "linux")
		if err = s.Ingest(ctx, envelope); err != nil {
			t.Fatal(err)
		}
	}
	record, _ = s.Incident(ctx, id)
	if record.State != "acknowledged" {
		t.Fatal("future metrics falsely recovered incident")
	}
}

func TestHealthRecurrenceAndSourceIsolation(t *testing.T) {
	s, ctx, now := healthFixture(t)
	start := now.Add(-24 * time.Second)
	for i, value := range []float64{85, 85, 85, 50, 50, 50, 85, 85, 85} {
		at := start.Add(time.Duration(i) * 3 * time.Second)
		s.healthClock = func() time.Time { return at }
		if err := s.Ingest(ctx, healthEnvelope(fmt.Sprint("occurrence", i), i, at, value, "linux")); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.Incidents(ctx, health.Filter{RuleID: "cpu.sustained"})
	if err != nil || len(items) != 2 {
		t.Fatalf("recurrence=%+v err=%v", items, err)
	}
	counts := map[string]int{}
	for _, item := range items {
		counts[item.State]++
		if item.Occurrence < 1 || item.Occurrence > 2 {
			t.Fatal("wrong occurrence")
		}
	}
	if counts["resolved"] != 1 || counts["open"] != 1 {
		t.Fatal("recurrence reused closed incident")
	}
	// Split high points across two sources: neither may satisfy 3-point sustain.
	s2, ctx2, at := healthFixture(t)
	for i := 0; i < 4; i++ {
		source := "remote"
		if i%2 == 0 {
			source = "linux"
		}
		if err := s2.Ingest(ctx2, healthEnvelope(fmt.Sprint("source", i), i, at.Add(time.Duration(i-3)*3*time.Second), 90, source)); err != nil {
			t.Fatal(err)
		}
	}
	items, err = s2.Incidents(ctx2, health.Filter{})
	if err != nil || len(items) != 0 {
		t.Fatal("sources mixed into false sustain")
	}
}
func TestHealthRecoveryAndEarlyForecastAtomicProjection(t *testing.T) {
	s, ctx, now := healthFixture(t)
	// Entire scenario is in the past; latest sample is fresh at query time.
	start := now.Add(-30 * time.Second)
	for i, v := range []float64{85, 85, 85, 50, 50, 50} {
		at := start.Add(time.Duration(i) * 6 * time.Second)
		s.healthClock = func() time.Time { return at }
		if err := s.Ingest(ctx, healthEnvelope(fmt.Sprint("recovery", i), i, at, v, "linux")); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.Incidents(ctx, health.Filter{RuleID: "cpu.sustained"})
	if err != nil || len(list) != 1 || list[0].State != "resolved" {
		t.Fatalf("stable recovery failed: %+v %v", list, err)
	}
	evidence, _ := s.IncidentEvidence(ctx, list[0].ID)
	if len(evidence) != 2 || evidence[0].Kind != "recovered" {
		t.Fatal("recovery evidence missing")
	}
	if _, err = s.AcknowledgeIncident(ctx, list[0].ID); !errors.Is(err, health.ErrConflict) {
		t.Fatal("resolved incident acknowledged")
	}
	// A different source is its own continuity/fingerprint, not pooled data.
	for i, v := range []float64{50, 55, 60, 65, 70, 75} {
		at := start.Add(time.Duration(i) * 6 * time.Second)
		s.healthClock = func() time.Time { return at }
		if err := s.Ingest(ctx, healthEnvelope(fmt.Sprint("trend", i), 20+i, at, v, "remote")); err != nil {
			t.Fatal(err)
		}
	}
	list, err = s.Incidents(ctx, health.Filter{RuleID: "cpu.trend"})
	if err != nil || len(list) != 1 || list[0].Signal.Value == nil || *list[0].Signal.Value >= 80 {
		t.Fatal("early forecast not persisted")
	}
	card, err := s.DeviceHealth(ctx, "agent-1", "shared-router")
	if err != nil || len(card) != 6 {
		t.Fatalf("health source projections=%d err=%v", len(card), err)
	}
	if err = s.Disconnect(ctx, "agent-1", "session-1"); err != nil {
		t.Fatal(err)
	}
	record, err := s.Incident(ctx, list[0].ID)
	if err != nil || record.State != "open" || record.EvidenceStatus != "collector_unavailable" {
		t.Fatal("collector loss misrepresented as recovery")
	}
}
func TestHealthProjectionSQLFailureDoesNotACKOrPartiallyPersist(t *testing.T) {
	s, ctx, now := healthFixture(t)
	for i := 0; i < 2; i++ {
		if err := s.Ingest(ctx, healthEnvelope(fmt.Sprint("atomic", i), i, now.Add(time.Duration(i-2)*3*time.Second), 85, "linux")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE incident_evidence ADD CONSTRAINT reject_fixture CHECK(kind<>'opened')`); err != nil {
		t.Fatal(err)
	}
	third := healthEnvelope("atomic2", 2, now, 85, "linux")
	if err := s.Ingest(ctx, third); err == nil {
		t.Fatal("SQL fixture failure missing")
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM agent_messages WHERE message_id='atomic2'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed incident projection inbox committed")
	}
	items, _ := s.Incidents(ctx, health.Filter{})
	if len(items) != 0 {
		t.Fatal("partial incident committed")
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE incident_evidence DROP CONSTRAINT reject_fixture`); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, third); err != nil {
		t.Fatal(err)
	}
	items, _ = s.Incidents(ctx, health.Filter{})
	if len(items) != 1 {
		t.Fatal("retry did not create exactly one incident")
	}
}
