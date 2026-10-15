package storage

import (
	"fmt"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"google.golang.org/protobuf/types/known/timestamppb"
	"testing"
	"time"
)

func TestHardwareIncidentRecoveryReplayScopeAndCorrelation(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Add(-40 * time.Second).Truncate(time.Microsecond)
	for _, agent := range []string{"hw-inc-a", "hw-inc-b"} {
		if err := s.Ingest(ctx, registration(agent, now)); err != nil {
			t.Fatal(err)
		}
		d := &pb.Device{Id: "system", Name: "System", Source: "redfish", Observation: assetHardwareMeta(agent, "target", "/Systems/1", now), Hardware: &pb.HardwareInventory{ResourceType: "System", PowerState: "Off", Status: &pb.HardwareStatus{Health: "ok"}}}
		if err := s.Ingest(ctx, assetInventory(agent, "session-1", "inventory", now, d)); err != nil {
			t.Fatal(err)
		}
	}
	process := func() {
		t.Helper()
		if _, err := s.ProcessCorrelation(ctx); err != nil {
			t.Fatal(err)
		}
	}
	process()
	send := func(n int, status string, quality string, sourceAt *time.Time) {
		t.Helper()
		e := assetHardwareTelemetry("hw-inc-a", "system", fmt.Sprintf("health-%d", n), "target", now.Add(time.Duration(n)*time.Second), 0)
		sample := e.GetTelemetry().Samples[0]
		sample.HardwareHealth[0].Status.Health = status
		sample.Observation.Time.ClockQuality = quality
		if sourceAt != nil {
			sample.Observation.Time.SourceTimestamp = timestamppb.New(*sourceAt)
		}
		if err := s.Ingest(ctx, e); err != nil {
			t.Fatal(err)
		}
		process()
	}
	list := func(state string) []health.Incident {
		t.Helper()
		v, err := s.Incidents(ctx, health.Filter{ResourceKind: "hardware", AgentID: "hw-inc-a", State: state})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	send(1, "critical", "unknown", nil)
	open := list("open")
	if len(open) != 1 || open[0].Signal.Hardware == nil || open[0].Severity != "critical" {
		t.Fatal("missing vendor problem", open)
	}
	var cases, members int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM normalized_signals WHERE category='hardware'`).Scan(&members); err != nil || members != 1 {
		t.Fatal("hardware not normalized", members, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM correlated_incidents`).Scan(&cases); err != nil || cases != 1 {
		t.Fatal("correlation missing", cases, err)
	}
	foreign, err := s.Incidents(ctx, health.Filter{ResourceKind: "hardware", AgentID: "hw-inc-b"})
	if err != nil || len(foreign) != 0 {
		t.Fatal("foreign incident", foreign, err)
	}
	send(2, "ok", "unknown", nil)
	send(3, "ok", "unknown", nil)
	if len(list("open")) != 1 {
		t.Fatal("premature recovery")
	}
	send(4, "unknown", "unknown", nil)
	if v := list("open"); len(v) != 1 || v[0].EvidenceStatus != "unknown" {
		t.Fatal("unknown vendor status remained fresh", v)
	}
	send(5, "ok", "unknown", nil)
	send(6, "ok", "unknown", nil)
	if len(list("open")) != 1 {
		t.Fatal("unknown bridged recovery")
	}
	send(7, "ok", "unknown", nil)
	resolved := list("resolved")
	if len(resolved) != 1 || resolved[0].Signal.Hardware.RecoveryCount != 3 || len(resolved[0].Signal.ResourceEvidence) != 3 {
		t.Fatal("no stable recovery", resolved)
	}
	process()
	if len(list("resolved")) != 1 {
		t.Fatal("replay changed occurrence")
	}
	send(8, "warning", "unknown", nil)
	if v := list("open"); len(v) != 1 || v[0].Occurrence != 2 {
		t.Fatal("recurrence missing", v)
	}
	send(9, "ok", "unsynced", nil)
	if v := list("open"); len(v) != 1 || v[0].EvidenceStatus != "clock_degraded" {
		t.Fatal("invalid clock recovered", v)
	}
	readingAt := now.Add(10 * time.Second)
	send(10, "ok", "unknown", &readingAt)
	send(11, "ok", "unknown", &readingAt)
	send(12, "ok", "unknown", &readingAt)
	if len(list("open")) != 1 {
		t.Fatal("same source reading counted repeatedly")
	}
	send(500, "critical", "unknown", nil)
	send(13, "ok", "unknown", nil)
	send(14, "ok", "unknown", nil)
	if len(list("open")) != 1 {
		t.Fatal("future sample established recovery")
	}
	send(15, "ok", "unknown", nil)
	if len(list("open")) != 0 || len(list("resolved")) != 2 {
		t.Fatal("future timestamp poisoned recovery cursor")
	}
	send(16, "critical", "unknown", nil)
	if _, err = s.pool.Exec(ctx, `UPDATE agents SET connected=false WHERE agent_id='hw-inc-a'`); err != nil {
		t.Fatal(err)
	}
	if v := list("open"); len(v) != 1 || v[0].EvidenceStatus != "source_unavailable" {
		t.Fatal("disconnect recovered or remained fresh", v)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE asset_source_bindings SET inventory=jsonb_set(inventory,'{observation,source,target_id}','"replacement-target"') WHERE legacy_agent_id='hw-inc-a' AND external_id='system'`); err != nil {
		t.Fatal(err)
	}
	if v := list("open"); len(v) != 1 || v[0].EvidenceStatus != "missing" {
		t.Fatal("old target still provided evidence", v)
	}
}

func TestHardwareIncidentWorkerAtomicRetry(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Add(-5 * time.Second)
	if err := s.Ingest(ctx, registration("hw-retry", now)); err != nil {
		t.Fatal(err)
	}
	d := &pb.Device{Id: "system", Source: "redfish", Observation: assetHardwareMeta("hw-retry", "target", "/Systems/1", now)}
	if err := s.Ingest(ctx, assetInventory("hw-retry", "session-1", "inventory", now, d)); err != nil {
		t.Fatal(err)
	}
	e := assetHardwareTelemetry("hw-retry", "system", "critical", "target", now, 0)
	e.GetTelemetry().Samples[0].HardwareHealth[0].Status.Health = "critical"
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE hardware_signal_state ADD CONSTRAINT force_worker_failure CHECK(recovery_count=99)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessHardwareSignals(ctx); err == nil {
		t.Fatal("expected SQL failure")
	}
	var pending, incidents int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM hardware_signal_pending`).Scan(&pending); err != nil || pending == 0 {
		t.Fatal("lost pending samples", pending, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM incidents WHERE resource_kind='hardware'`).Scan(&incidents); err != nil || incidents != 0 {
		t.Fatal("partial transaction", incidents, err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE hardware_signal_state DROP CONSTRAINT force_worker_failure`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessCorrelation(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM incidents WHERE resource_kind='hardware'`).Scan(&incidents); err != nil || incidents != 1 {
		t.Fatal("retry lost incident", incidents, err)
	}
}
