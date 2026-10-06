package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TEST_DATABASE_URL must point to an expendable test database. Each run creates
// and drops its own schema; it never truncates shared tables.
func integrationStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("infrapulse_test_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		_ = admin.Close(ctx)
		t.Fatal(err)
	}
	poolCfg, err := parseTestPool(url, schema)
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{pool: poolCfg, timeout: 90 * time.Second, defaults: monitoring.Defaults()}
	t.Cleanup(func() {
		s.Close()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		_ = admin.Close(ctx)
	})
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func registration(agent string, now time.Time) *pb.AgentEnvelope {
	return &pb.AgentEnvelope{AgentId: agent, SessionId: "session-1", MessageId: "hello", Sequence: 1, SentAt: timestamppb.New(now), Payload: &pb.AgentEnvelope_Hello{Hello: &pb.AgentHello{
		AgentId: agent, SessionId: "session-1", ProtocolVersion: 1, Name: agent, StartedAt: timestamppb.New(now), Device: &pb.Device{Id: "shared-router", Name: "Router", Vendor: "MikroTik", Online: true, Source: "simulated", LastSeen: timestamppb.New(now)},
	}}}
}
func sampleEnvelope(now time.Time) *pb.AgentEnvelope {
	zero := 0.0
	return &pb.AgentEnvelope{AgentId: "agent-1", SessionId: "session-1", MessageId: "telemetry", Sequence: 2, SentAt: timestamppb.New(now), Payload: &pb.AgentEnvelope_Telemetry{Telemetry: &pb.TelemetryBatch{Samples: []*pb.TelemetrySample{{
		DeviceId: "shared-router", ObservedAt: timestamppb.New(now), CollectedAt: timestamppb.New(now), Source: "simulated", Cpu: &pb.CPUStats{UsagePercent: &zero}, Memory: &pb.MemoryStats{TotalBytes: ^uint64(0), UsedBytes: 1024},
	}}}}}
}

func TestPostgresAtomicityDeduplicationAndOrdering(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	e := sampleEnvelope(now.Add(time.Second))
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	items, err := s.Metrics(ctx, "agent-1", "shared-router", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].CPUUsagePercent == nil || *items[0].CPUUsagePercent != 0 || items[0].MemoryTotalBytes == nil || *items[0].MemoryTotalBytes != "18446744073709551615" {
		t.Fatalf("incorrect zero/uint64/dedup semantics: %+v", items)
	}
	changed := proto.Clone(e).(*pb.AgentEnvelope)
	changed.GetTelemetry().Samples[0].Source = "changed"
	if err = s.Ingest(ctx, changed); !errors.Is(err, ErrMessageConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	old := proto.Clone(e).(*pb.AgentEnvelope)
	old.MessageId = "older"
	old.Sequence = 3
	old.GetTelemetry().Samples[0].ObservedAt = timestamppb.New(now.Add(-time.Hour))
	if err = s.Ingest(ctx, old); err != nil {
		t.Fatal(err)
	}
	latest, err := s.Metrics(ctx, "agent-1", "shared-router", 1)
	if err != nil || latest[0].ObservedAt.Before(now) {
		t.Fatalf("latest metric regressed: %v %v", latest, err)
	}
	d, err := s.Device(ctx, "agent-1", "shared-router")
	if err != nil || d.Name != "Router" || d.Vendor != "MikroTik" {
		t.Fatalf("telemetry overwrote inventory: %+v %v", d, err)
	}
	if err = s.Ingest(ctx, registration("agent-2", now)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Device(ctx, "", "shared-router"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("expected ambiguous device, got %v", err)
	}
	// Force a DB constraint failure in the second sample: the first sample and
	// message row must both roll back, leaving no partially accepted batch.
	if _, err = s.pool.Exec(ctx, `ALTER TABLE telemetry ADD CONSTRAINT reject_test_source CHECK(source <> 'test-reject')`); err != nil {
		t.Fatal(err)
	}
	bad := proto.Clone(e).(*pb.AgentEnvelope)
	bad.MessageId = "bad-batch"
	bad.Sequence = 4
	bad.GetTelemetry().Samples = append(bad.GetTelemetry().Samples, proto.Clone(bad.GetTelemetry().Samples[0]).(*pb.TelemetrySample))
	bad.GetTelemetry().Samples[1].Source = "test-reject"
	if err = s.Ingest(ctx, bad); err == nil {
		t.Fatal("constraint failure accepted")
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM agent_messages WHERE message_id='bad-batch'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial batch persisted: %d %v", count, err)
	}
	if err = s.Disconnect(ctx, "agent-1", "wrong-session"); err != nil {
		t.Fatal(err)
	}
	agents, err := s.Agents(ctx)
	if err != nil || !agents[0].Connected {
		t.Fatalf("wrong session disconnected agent: %v %v", agents, err)
	}
	if err = s.Disconnect(ctx, "agent-1", "session-1"); err != nil {
		t.Fatal(err)
	}
	agents, err = s.Agents(ctx)
	if err != nil || agents[0].Connected {
		t.Fatalf("disconnect failed: %v %v", agents, err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatalf("repeated migration: %v", err)
	}
	agents, err = s.Agents(ctx)
	if err != nil || agents[1].Connected {
		t.Fatalf("startup failed to reset stale connections: %v %v", agents, err)
	}
}

func TestPostgresHeartbeatSessionsAndTimeout(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	e := registration("agent-1", now)
	e.GetHello().Device.Online = false
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	h := &pb.AgentEnvelope{AgentId: e.AgentId, SessionId: e.SessionId, MessageId: "heartbeat", Sequence: 2, SentAt: timestamppb.New(now), Payload: &pb.AgentEnvelope_Heartbeat{Heartbeat: &pb.Heartbeat{Timestamp: timestamppb.New(now.Add(time.Second)), Health: "ok"}}}
	if err := s.Ingest(ctx, h); err != nil {
		t.Fatal(err)
	}
	d, err := s.Device(ctx, "agent-1", "shared-router")
	if err != nil || !d.Online || !d.AgentConnected {
		t.Fatalf("heartbeat did not restore local device: %v %v", d, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE agents SET last_seen=now()-interval '2 minutes'`); err != nil {
		t.Fatal(err)
	}
	agents, err := s.Agents(ctx)
	if err != nil || agents[0].Connected {
		t.Fatalf("expired agent still connected: %v %v", agents, err)
	}
	e2 := registration("agent-1", now)
	e2.SessionId = "session-2"
	e2.GetHello().SessionId = e2.SessionId
	e2.MessageId = "hello-2"
	if err = s.Disconnect(ctx, e.AgentId, e.SessionId); err != nil {
		t.Fatal(err)
	}
	if err = s.Ingest(ctx, e2); err != nil {
		t.Fatal(err)
	}
	if err = s.Disconnect(ctx, e.AgentId, e.SessionId); err != nil {
		t.Fatal(err)
	}
	agents, err = s.Agents(ctx)
	if err != nil || !agents[0].Connected || agents[0].SessionID != "session-2" {
		t.Fatalf("old disconnect affected new session: %v %v", agents, err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM agent_sessions`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("session history missing: %d %v", count, err)
	}
}

func TestMonitoringPoliciesPersistAndKeepHeartbeatIndependent(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, agent := range []string{"agent-1", "agent-2"} {
		if err := s.Ingest(ctx, registration(agent, now)); err != nil {
			t.Fatal(err)
		}
	}
	e := sampleEnvelope(now.Add(-5 * time.Hour))
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	d, err := s.Device(ctx, "agent-1", "shared-router")
	if err != nil || d.TelemetryFreshness.Status != "stale" {
		t.Fatalf("fast profile freshness: %+v %v", d, err)
	}
	slow := monitoring.Policy{TelemetryIntervalSeconds: 86400, InventoryIntervalSeconds: 86400, TopologyIntervalSeconds: 86400}
	if err = s.UpdateMonitoring(ctx, "agent-1", "shared-router", slow); err != nil {
		t.Fatal(err)
	}
	d, err = s.Device(ctx, "agent-1", "shared-router")
	if err != nil || d.TelemetryFreshness.Status != "fresh" || !d.AgentConnected || d.Monitoring.ApplicationStatus != "unverified" {
		t.Fatalf("slow profile broke freshness/connection: %+v %v", d, err)
	}
	d2, err := s.Device(ctx, "agent-2", "shared-router")
	if err != nil || d2.Monitoring.TelemetryIntervalSeconds != 3 || d2.TelemetryFreshness.Status != "no_data" {
		t.Fatalf("policy leaked to another agent: %+v %v", d2, err)
	}
	// Heartbeat and inventory do not make an old telemetry sample fresh.
	if err = s.UpdateMonitoring(ctx, "agent-1", "shared-router", monitoring.Defaults()); err != nil {
		t.Fatal(err)
	}
	h := &pb.AgentEnvelope{AgentId: "agent-1", SessionId: "session-1", MessageId: "hb-now", Sequence: 3, SentAt: timestamppb.New(now), Payload: &pb.AgentEnvelope_Heartbeat{Heartbeat: &pb.Heartbeat{Timestamp: timestamppb.New(now), Health: "ok"}}}
	if err = s.Ingest(ctx, h); err != nil {
		t.Fatal(err)
	}
	d, err = s.Device(ctx, "agent-1", "shared-router")
	if err != nil || !d.AgentConnected || d.TelemetryFreshness.Status != "stale" {
		t.Fatalf("heartbeat masked stale metrics: %+v %v", d, err)
	}
	if err = s.UpdateMonitoring(ctx, "agent-1", "shared-router", slow); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	d, err = s.Device(ctx, "agent-1", "shared-router")
	if err != nil || d.Monitoring.TelemetryIntervalSeconds != 86400 {
		t.Fatalf("migration lost saved policy: %+v %v", d, err)
	}
	if err = s.UpdateMonitoring(ctx, "agent-1", "missing", slow); err != ErrNotFound {
		t.Fatalf("expected not found, got %v", err)
	}
	if err = s.UpdateMonitoring(ctx, "agent-1", "shared-router", monitoring.Policy{TelemetryIntervalSeconds: 1, InventoryIntervalSeconds: 300, TopologyIntervalSeconds: 300}); err == nil {
		t.Fatal("invalid policy persisted")
	}
}
