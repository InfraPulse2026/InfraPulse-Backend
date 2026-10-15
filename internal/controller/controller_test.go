package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type memoryRepository struct {
	mu           sync.Mutex
	messages     []*pb.AgentEnvelope
	disconnected bool
	fail         error
}

func (m *memoryRepository) Ingest(_ context.Context, e *pb.AgentEnvelope) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.messages = append(m.messages, proto.Clone(e).(*pb.AgentEnvelope))
	return nil
}
func (m *memoryRepository) Disconnect(context.Context, string, string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.disconnected = true
	return nil
}

func testHello() *pb.AgentEnvelope {
	return &pb.AgentEnvelope{MessageId: "hello-1", AgentId: "agent-1", SessionId: "session-1", Sequence: 1, SentAt: timestamppb.Now(), Payload: &pb.AgentEnvelope_Hello{Hello: &pb.AgentHello{
		AgentId: "agent-1", SessionId: "session-1", ProtocolVersion: 1, StartedAt: timestamppb.Now(), Device: &pb.Device{Id: "device-1"},
	}}}
}
func heartbeat() *pb.AgentEnvelope {
	return &pb.AgentEnvelope{MessageId: "heartbeat-2", AgentId: "agent-1", SessionId: "session-1", Sequence: 2, SentAt: timestamppb.Now(), Payload: &pb.AgentEnvelope_Heartbeat{Heartbeat: &pb.Heartbeat{Timestamp: timestamppb.Now(), Health: "healthy"}}}
}
func testClient(t *testing.T, repo Repository, options ...Option) pb.AgentServiceClient {
	t.Helper()
	listener := bufconn.Listen(4 << 20)
	server := grpc.NewServer()
	pb.RegisterAgentServiceServer(server, New(repo, slog.New(slog.NewTextHandler(io.Discard, nil)), options...))
	go func() { _ = server.Serve(listener) }()
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); server.Stop(); _ = listener.Close() })
	return pb.NewAgentServiceClient(conn)
}

func TestHelloHeartbeatAndCleanDisconnect(t *testing.T) {
	repo := &memoryRepository{}
	client := testClient(t, repo)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.Send(testHello()); err != nil {
		t.Fatal(err)
	}
	ack, err := stream.Recv()
	if err != nil || !ack.GetHelloAck().GetAccepted() || ack.GetHelloAck().GetProtocolVersion() != 1 {
		t.Fatalf("bad ack: %v %v", ack, err)
	}
	if err = stream.Send(heartbeat()); err != nil {
		t.Fatal(err)
	}
	if err = stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.messages) != 2 || !repo.disconnected {
		t.Fatalf("messages=%d disconnected=%v", len(repo.messages), repo.disconnected)
	}
}

func TestRejectInvalidHandshake(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*pb.AgentEnvelope)
		code   codes.Code
	}{
		{"wrong-version", func(e *pb.AgentEnvelope) { e.GetHello().ProtocolVersion = 2 }, codes.FailedPrecondition},
		{"mismatched-agent", func(e *pb.AgentEnvelope) { e.GetHello().AgentId = "other" }, codes.FailedPrecondition},
		{"missing-device", func(e *pb.AgentEnvelope) { e.GetHello().Device = nil }, codes.InvalidArgument},
		{"missing-time", func(e *pb.AgentEnvelope) { e.SentAt = nil }, codes.InvalidArgument},
		{"missing-payload", func(e *pb.AgentEnvelope) { e.Payload = nil }, codes.InvalidArgument},
		{"invalid-time", func(e *pb.AgentEnvelope) { e.SentAt = &timestamppb.Timestamp{Seconds: math.MaxInt64} }, codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &memoryRepository{}
			client := testClient(t, repo)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			stream, err := client.Connect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			e := testHello()
			tc.change(e)
			_ = stream.Send(e)
			_, err = stream.Recv()
			if status.Code(err) != tc.code {
				t.Fatalf("got %v", err)
			}
			repo.mu.Lock()
			defer repo.mu.Unlock()
			if len(repo.messages) != 0 {
				t.Fatal("invalid hello persisted")
			}
		})
	}
}

func TestRejectStreamIdentityAndSequence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*pb.AgentEnvelope)
	}{
		{"sequence", func(e *pb.AgentEnvelope) { e.Sequence = 1 }},
		{"session", func(e *pb.AgentEnvelope) { e.SessionId = "other" }},
		{"agent", func(e *pb.AgentEnvelope) { e.AgentId = "other" }},
		{"nonfinite", func(e *pb.AgentEnvelope) { e.GetHeartbeat().UptimeSeconds = math.NaN() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &memoryRepository{}
			client := testClient(t, repo)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			stream, err := client.Connect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_ = stream.Send(testHello())
			if _, err = stream.Recv(); err != nil {
				t.Fatal(err)
			}
			e := heartbeat()
			tc.change(e)
			_ = stream.Send(e)
			_, err = stream.Recv()
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("got %v", err)
			}
			repo.mu.Lock()
			defer repo.mu.Unlock()
			if len(repo.messages) != 1 {
				t.Fatal("invalid message persisted")
			}
		})
	}
}

func TestDatabaseFailureDoesNotAcceptHello(t *testing.T) {
	repo := &memoryRepository{fail: errors.New("database offline")}
	client := testClient(t, repo)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Send(testHello())
	ack, err := stream.Recv()
	if ack != nil || status.Code(err) != codes.Unavailable {
		t.Fatalf("hello acknowledged despite failure: %v %v", ack, err)
	}
}

func TestDuplicateActiveAgentRejected(t *testing.T) {
	client := testClient(t, &memoryRepository{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Send(testHello())
	if _, err = first.Recv(); err != nil {
		t.Fatal(err)
	}
	second, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = second.Send(testHello())
	_, err = second.Recv()
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("got %v", err)
	}
}
