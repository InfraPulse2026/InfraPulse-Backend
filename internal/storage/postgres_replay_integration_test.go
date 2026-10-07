package storage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func messageCounts(t *testing.T, s *Store, messageID string, wantMessages, wantSamples int) {
	t.Helper()
	var messages, samples int
	if err := s.pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM agent_messages WHERE agent_id='agent-1' AND message_id=$1),
		(SELECT count(*) FROM telemetry WHERE agent_id='agent-1' AND message_id=$1)`, messageID).Scan(&messages, &samples); err != nil {
		t.Fatal(err)
	}
	if messages != wantMessages || samples != wantSamples {
		t.Fatalf("message %q: inbox=%d samples=%d, want inbox=%d samples=%d", messageID, messages, samples, wantMessages, wantSamples)
	}
}

func TestPostgresReplayAcrossTransportSessionsAndStoreRestart(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	original := sampleEnvelope(now)
	if err := s.Ingest(ctx, original); err != nil {
		t.Fatal(err)
	}
	messageCounts(t, s, original.MessageId, 1, 1)

	// The stored full envelope uses the existing, pre-ACK schema. Retrying a
	// logical message must not require a fingerprint column or backfill.
	replay := proto.Clone(original).(*pb.AgentEnvelope)
	replay.Sequence = 90
	replay.SentAt = timestamppb.New(now.Add(time.Minute))
	if _, err := s.pool.Exec(ctx, `UPDATE agent_sessions SET last_seen=now()-interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, replay); err != nil {
		t.Fatalf("new transmission attempt rejected: %v", err)
	}
	messageCounts(t, s, original.MessageId, 1, 1)
	var recent bool
	if err := s.pool.QueryRow(ctx, `SELECT last_seen>now()-interval '1 minute' FROM agent_sessions WHERE agent_id=$1 AND session_id=$2`, replay.AgentId, replay.SessionId).Scan(&recent); err != nil || !recent {
		t.Fatalf("accepted replay did not refresh session: %t %v", recent, err)
	}

	// An agent process restart registers a new session before recovering the
	// old logical message. Session tracking and dedup are independent.
	hello := registration("agent-1", now.Add(2*time.Minute))
	hello.SessionId = "session-2"
	hello.GetHello().SessionId = hello.SessionId
	hello.MessageId = "hello-session-2"
	if err := s.Ingest(ctx, hello); err != nil {
		t.Fatal(err)
	}
	replay.SessionId = hello.SessionId
	replay.Sequence = 2
	replay.SentAt = timestamppb.New(now.Add(3 * time.Minute))
	if err := s.Ingest(ctx, replay); err != nil {
		t.Fatalf("new-session replay rejected: %v", err)
	}
	messageCounts(t, s, original.MessageId, 1, 1)

	// Open a fresh pool and run the real startup migrations against the same
	// PostgreSQL schema. Dedup state must survive the backend's runtime state.
	pool, err := pgxpool.NewWithConfig(ctx, s.pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	s.pool.Close()
	s.pool = pool
	if err = s.Migrate(ctx); err != nil {
		t.Fatalf("startup with existing message rows: %v", err)
	}
	hello.MessageId = "hello-backend-restart"
	hello.Sequence = 3
	hello.SentAt = timestamppb.New(now.Add(4 * time.Minute))
	if err = s.Ingest(ctx, hello); err != nil {
		t.Fatal(err)
	}
	replay.Sequence = 4
	replay.SentAt = timestamppb.New(now.Add(5 * time.Minute))
	if err = s.Ingest(ctx, replay); err != nil {
		t.Fatalf("persistent dedup replay rejected: %v", err)
	}
	messageCounts(t, s, original.MessageId, 1, 1)
	var session, sequence string
	if err = s.pool.QueryRow(ctx, `SELECT session_id,sequence::text FROM agent_messages WHERE agent_id=$1 AND message_id=$2`, original.AgentId, original.MessageId).Scan(&session, &sequence); err != nil || session != original.SessionId || sequence != "2" {
		t.Fatalf("original inbox archive changed during replay: session=%q sequence=%q err=%v", session, sequence, err)
	}
	var sessions int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM agent_sessions WHERE agent_id=$1`, original.AgentId).Scan(&sessions); err != nil || sessions != 2 {
		t.Fatalf("session history lost: %d %v", sessions, err)
	}
}

func TestPostgresReplayRejectsChangedBusinessContents(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	original := sampleEnvelope(now)
	if err := s.Ingest(ctx, original); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*pb.AgentEnvelope)
	}{
		{"source", func(e *pb.AgentEnvelope) { e.GetTelemetry().Samples[0].Source = "different" }},
		{"business timestamp", func(e *pb.AgentEnvelope) {
			e.GetTelemetry().Samples[0].ObservedAt = timestamppb.New(now.Add(time.Second))
		}},
		{"optional presence", func(e *pb.AgentEnvelope) { e.GetTelemetry().Samples[0].Cpu.UsagePercent = nil }},
		{"payload kind", func(e *pb.AgentEnvelope) {
			e.Payload = &pb.AgentEnvelope_Heartbeat{Heartbeat: &pb.Heartbeat{Timestamp: timestamppb.New(now), Health: "ok"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := proto.Clone(original).(*pb.AgentEnvelope)
			changed.Sequence = 10
			changed.SentAt = timestamppb.New(now.Add(time.Minute))
			tc.change(changed)
			if err := s.Ingest(ctx, changed); !errors.Is(err, ErrMessageConflict) {
				t.Fatalf("expected conflicting ID reuse, got %v", err)
			}
			messageCounts(t, s, original.MessageId, 1, 1)
		})
	}
}

func TestPostgresConcurrentReplayHasOneDomainEffect(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	original := sampleEnvelope(now)
	const attempts = 8
	errors := make(chan error, attempts)
	var workers sync.WaitGroup
	for i := range attempts {
		workers.Go(func() {
			replay := proto.Clone(original).(*pb.AgentEnvelope)
			replay.Sequence = uint64(i + 2)
			replay.SentAt = timestamppb.New(now.Add(time.Duration(i) * time.Second))
			errors <- s.Ingest(ctx, replay)
		})
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Errorf("concurrent replay rejected: %v", err)
		}
	}
	messageCounts(t, s, original.MessageId, 1, 1)
}

func TestPostgresRollbackAllowsReplayAfterRecovery(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE telemetry ADD CONSTRAINT reject_replay_source CHECK(source <> 'reject-until-recovery')`); err != nil {
		t.Fatal(err)
	}
	e := sampleEnvelope(now)
	e.MessageId = "recoverable-batch"
	e.GetTelemetry().Samples[0].DeviceId = "new-device"
	second := proto.Clone(e.GetTelemetry().Samples[0]).(*pb.TelemetrySample)
	second.Source = "reject-until-recovery"
	e.GetTelemetry().Samples = append(e.GetTelemetry().Samples, second)
	if err := s.Ingest(ctx, e); err == nil {
		t.Fatal("failed domain transaction accepted")
	}
	messageCounts(t, s, e.MessageId, 0, 0)
	if _, err := s.Device(ctx, e.AgentId, "new-device"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("device projection survived rollback: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE telemetry DROP CONSTRAINT reject_replay_source`); err != nil {
		t.Fatal(err)
	}
	e.Sequence = 3
	e.SentAt = timestamppb.New(now.Add(time.Minute))
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatalf("message could not recover after DB failure: %v", err)
	}
	messageCounts(t, s, e.MessageId, 1, 2)
}

func TestPostgresCommitFailureAllowsReplayAfterRecovery(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	// A deferred trigger runs at COMMIT, after both inbox and telemetry inserts
	// have succeeded. Ingest must not report durable acceptance at this point.
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_test_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'test commit failure' USING ERRCODE='23514'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `CREATE CONSTRAINT TRIGGER reject_test_commit
		AFTER INSERT ON agent_messages DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE FUNCTION reject_test_commit()`); err != nil {
		t.Fatal(err)
	}
	e := sampleEnvelope(now)
	e.MessageId = "commit-failure"
	if err := s.Ingest(ctx, e); err == nil {
		t.Fatal("failed COMMIT reported successful acceptance")
	}
	messageCounts(t, s, e.MessageId, 0, 0)
	if _, err := s.pool.Exec(ctx, `DROP TRIGGER reject_test_commit ON agent_messages`); err != nil {
		t.Fatal(err)
	}
	e.Sequence = 3
	e.SentAt = timestamppb.New(now.Add(time.Minute))
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatalf("message could not recover after COMMIT failure: %v", err)
	}
	messageCounts(t, s, e.MessageId, 1, 1)
}

func TestPostgresInventoryReplayKeepsOneArchive(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	e := &pb.AgentEnvelope{AgentId: "agent-1", SessionId: "session-1", MessageId: "inventory", Sequence: 2, SentAt: timestamppb.New(now), Payload: &pb.AgentEnvelope_Inventory{Inventory: &pb.InventoryUpdate{
		ObservedAt: timestamppb.New(now), Devices: []*pb.Device{{Id: "shared-router", Name: "Updated router", Vendor: "MikroTik", Online: true, LastSeen: timestamppb.New(now), Source: "simulated"}},
	}}}
	for i := range 2 {
		e.Sequence = uint64(i + 2)
		e.SentAt = timestamppb.New(now.Add(time.Duration(i) * time.Second))
		if err := s.Ingest(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	messageCounts(t, s, e.MessageId, 1, 0)
	d, err := s.Device(ctx, e.AgentId, "shared-router")
	if err != nil || d.Name != "Updated router" {
		t.Fatalf("inventory regression: %+v %v", d, err)
	}
	changed := proto.Clone(e).(*pb.AgentEnvelope)
	changed.GetInventory().Devices[0].Name = "Conflicting router"
	if err := s.Ingest(ctx, changed); !errors.Is(err, ErrMessageConflict) {
		t.Fatalf("changed inventory accepted under same ID: %v", err)
	}
}
