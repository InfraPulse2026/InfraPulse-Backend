package storage

import (
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func p3ArchiveEnvelopes(now time.Time) []*pb.AgentEnvelope {
	return []*pb.AgentEnvelope{
		{AgentId: "agent-1", SessionId: "session-1", MessageId: "heartbeat-p3", Sequence: 2, SentAt: timestamppb.New(now), Payload: &pb.AgentEnvelope_Heartbeat{Heartbeat: &pb.Heartbeat{
			Timestamp: timestamppb.New(now), Health: "ok",
			Queue:       &pb.QueueState{Messages: 1, Bytes: 1024, Mode: "durable", StorageBytes: 4096, OldestPending: timestamppb.New(now.Add(-time.Minute)), Unacked: 1, PendingResults: 1, StorageError: "test-diagnostic"},
			Certificate: &pb.CertificateStatus{ExpiresAt: timestamppb.New(now.Add(time.Hour)), RemainingSeconds: 3600, EnrollmentState: "enrolled", RenewalState: "idle", LastSuccessfulRenewal: timestamppb.New(now.Add(-time.Hour)), LastError: "test-diagnostic"},
		}}},
		{AgentId: "agent-1", SessionId: "session-1", MessageId: "command-result-p3", Sequence: 3, SentAt: timestamppb.New(now), Payload: &pb.AgentEnvelope_CommandResult{CommandResult: &pb.CommandResult{
			CommandId: "command-1", Status: pb.CommandStatus_COMMAND_STATUS_SUCCESS, StartedAt: timestamppb.New(now.Add(-time.Second)), FinishedAt: timestamppb.New(now), SnapshotId: "snapshot-1",
		}}},
	}
}

func TestWireJSONPreservesP3ArchiveFields(t *testing.T) {
	for _, original := range p3ArchiveEnvelopes(time.Now().UTC()) {
		t.Run(Kind(original), func(t *testing.T) {
			// Exercise the real protobuf decode followed by the JSON archive path.
			wire, err := proto.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			decoded := new(pb.AgentEnvelope)
			if err = proto.Unmarshal(wire, decoded); err != nil {
				t.Fatal(err)
			}
			raw, err := wireJSON(decoded)
			if err != nil {
				t.Fatal(err)
			}
			archived := new(pb.AgentEnvelope)
			if err = protojson.Unmarshal(raw, archived); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(original, archived) {
				t.Fatalf("P3 data lost during decode/archive: original=%v archived=%v", original, archived)
			}
		})
	}
}

func TestPostgresP3ArchiveAndReplay(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	for _, original := range p3ArchiveEnvelopes(now) {
		t.Run(Kind(original), func(t *testing.T) {
			if err := s.Ingest(ctx, original); err != nil {
				t.Fatal(err)
			}
			replay := proto.Clone(original).(*pb.AgentEnvelope)
			replay.Sequence += 10
			replay.SentAt = timestamppb.New(now.Add(time.Minute))
			if err := s.Ingest(ctx, replay); err != nil {
				t.Fatalf("archived payload replay rejected: %v", err)
			}
			messageCounts(t, s, original.MessageId, 1, 0)
			var raw []byte
			if err := s.pool.QueryRow(ctx, `SELECT payload FROM agent_messages WHERE agent_id=$1 AND message_id=$2`, original.AgentId, original.MessageId).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			archived := new(pb.AgentEnvelope)
			if err := protojson.Unmarshal(raw, archived); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(original, archived) {
				t.Fatalf("P3 fields lost in PostgreSQL archive: original=%v archived=%v", original, archived)
			}
			changed := proto.Clone(replay).(*pb.AgentEnvelope)
			if h := changed.GetHeartbeat(); h != nil {
				h.Queue.Unacked++
			} else {
				changed.GetCommandResult().SnapshotId = "different-snapshot"
			}
			if err := s.Ingest(ctx, changed); err != ErrMessageConflict {
				t.Fatalf("changed P3 business payload accepted under same ID: %v", err)
			}
		})
	}
}
