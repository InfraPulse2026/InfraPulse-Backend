package controller

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func ackHello() *pb.AgentEnvelope {
	e := testHello()
	e.GetHello().ApplicationAck = true
	return e
}

func TestApplicationACKForEveryAcceptedKind(t *testing.T) {
	repo := &memoryRepository{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := testClient(t, repo).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.Send(ackHello()); err != nil {
		t.Fatal(err)
	}
	ack, err := stream.Recv()
	if err != nil || !ack.GetHelloAck().GetAccepted() || ack.GetHelloAck().GetProtocolVersion() != 1 || !ack.GetHelloAck().GetApplicationAck() {
		t.Fatalf("ACK negotiation failed: %v %v", ack, err)
	}
	now := timestamppb.Now()
	messages := []*pb.AgentEnvelope{
		heartbeat(),
		{Payload: &pb.AgentEnvelope_Telemetry{Telemetry: &pb.TelemetryBatch{Samples: []*pb.TelemetrySample{{DeviceId: "device-1", Source: "linux", ObservedAt: now, CollectedAt: now}}}}},
		{Payload: &pb.AgentEnvelope_Inventory{Inventory: &pb.InventoryUpdate{ObservedAt: now, Devices: []*pb.Device{{Id: "device-1"}}}}},
		{Payload: &pb.AgentEnvelope_Topology{Topology: &pb.TopologyUpdate{DeviceId: "device-1", ObservedAt: now}}},
		{Payload: &pb.AgentEnvelope_Logs{Logs: &pb.LogBatch{ObservedAt: now}}},
		{Payload: &pb.AgentEnvelope_Events{Events: &pb.EventBatch{Events: []*pb.Event{{Id: "event-1"}}}}},
		{Payload: &pb.AgentEnvelope_Telephony{Telephony: &pb.TelephonyUpdate{ObservedAt: now}}},
		{Payload: &pb.AgentEnvelope_CommandResult{CommandResult: &pb.CommandResult{CommandId: "command-1", SnapshotId: "snapshot-1"}}},
		{Payload: &pb.AgentEnvelope_Error{Error: &pb.ErrorReport{Component: "collector", Timestamp: now}}},
	}
	for i, e := range messages {
		e.MessageId = storage.Kind(e)
		e.AgentId, e.SessionId, e.Sequence, e.SentAt = "agent-1", "session-1", uint64(i+2), now
		if err = stream.Send(e); err != nil {
			t.Fatal(err)
		}
		ack, err = stream.Recv()
		if err != nil || ack.GetMessageAck().GetMessageId() != e.MessageId {
			t.Fatalf("%s not acknowledged: %v %v", storage.Kind(e), ack, err)
		}
		repo.mu.Lock()
		persisted := len(repo.messages)
		repo.mu.Unlock()
		if persisted != i+2 {
			t.Fatalf("ACK preceded persistence: messages=%d", persisted)
		}
	}
	if err = stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

type gatedRepository struct {
	memoryRepository
	entered chan struct{}
	commit  chan struct{}
}

func (r *gatedRepository) Ingest(ctx context.Context, e *pb.AgentEnvelope) error {
	if e.GetHello() == nil {
		close(r.entered)
		select {
		case <-r.commit:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.memoryRepository.Ingest(ctx, e)
}

func TestMessageACKWaitsForSuccessfulIngestReturn(t *testing.T) {
	repo := &gatedRepository{entered: make(chan struct{}), commit: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := testClient(t, repo).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Send(ackHello())
	if _, err = stream.Recv(); err != nil {
		t.Fatal(err)
	}
	_ = stream.Send(heartbeat())
	select {
	case <-repo.entered:
	case <-ctx.Done():
		t.Fatal("Ingest did not start")
	}
	result := make(chan error, 1)
	go func() {
		ack, err := stream.Recv()
		if err == nil && ack.GetMessageAck().GetMessageId() != "heartbeat-2" {
			err = errors.New("wrong MessageACK")
		}
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("ACK before commit: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(repo.commit)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("no ACK after commit")
	}
}

func TestRejectedMessageGetsNoACK(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
		change  func(*pb.AgentEnvelope)
		code    codes.Code
	}{
		{name: "database rollback", failure: errors.New("database failure"), code: codes.Unavailable},
		{name: "conflicting reuse", failure: storage.ErrMessageConflict, code: codes.InvalidArgument},
		{name: "invalid payload", change: func(e *pb.AgentEnvelope) { e.GetHeartbeat().Timestamp = nil }, code: codes.InvalidArgument},
		{name: "unknown payload", change: func(e *pb.AgentEnvelope) {
			e.Payload = nil
			e.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 99, protowire.BytesType), nil))
		}, code: codes.InvalidArgument},
		{name: "unknown nested fields", change: func(e *pb.AgentEnvelope) {
			e.GetHeartbeat().ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
		}, code: codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &memoryRepository{}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			stream, err := testClient(t, repo).Connect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_ = stream.Send(ackHello())
			if _, err = stream.Recv(); err != nil {
				t.Fatal(err)
			}
			repo.mu.Lock()
			repo.fail = tc.failure
			repo.mu.Unlock()
			e := heartbeat()
			if tc.change != nil {
				tc.change(e)
			}
			_ = stream.Send(e)
			ack, err := stream.Recv()
			if ack != nil || status.Code(err) != tc.code {
				t.Fatalf("rejected message acknowledged: %v %v", ack, err)
			}
			repo.mu.Lock()
			defer repo.mu.Unlock()
			if len(repo.messages) != 1 {
				t.Fatal("rejected message persisted")
			}
		})
	}
}
