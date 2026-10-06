package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"sync"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Repository interface {
	Ingest(context.Context, *pb.AgentEnvelope) error
	Disconnect(context.Context, string, string) error
}

type Controller struct {
	pb.UnimplementedAgentServiceServer
	repo   Repository
	log    *slog.Logger
	mu     sync.Mutex
	active map[string]bool
	slots  chan struct{}
}

func New(repo Repository, log *slog.Logger) *Controller {
	return &Controller{repo: repo, log: log, active: make(map[string]bool), slots: make(chan struct{}, 128)}
}

func (c *Controller) Connect(stream grpc.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error {
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		return status.Error(codes.ResourceExhausted, "controller stream limit reached")
	}
	e, err := stream.Recv()
	if err != nil {
		return err
	}
	if err = validate(e); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	h := e.GetHello()
	if h == nil || h.ProtocolVersion != 1 || h.AgentId != e.AgentId || h.SessionId != e.SessionId {
		return status.Error(codes.FailedPrecondition, "first message must be a matching protocol-v1 hello")
	}
	c.mu.Lock()
	if c.active[e.AgentId] {
		c.mu.Unlock()
		return status.Error(codes.AlreadyExists, "agent already has an active stream; retry after it closes")
	}
	c.active[e.AgentId] = true
	c.mu.Unlock()
	registered := false
	defer func() {
		if registered {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := c.repo.Disconnect(ctx, e.AgentId, e.SessionId); err != nil {
				c.log.Error("cannot mark agent disconnected", "agent_id", e.AgentId, "error", err)
			}
			cancel()
		}
		c.mu.Lock()
		delete(c.active, e.AgentId)
		c.mu.Unlock()
	}()
	if err = c.persist(stream.Context(), e); err != nil {
		return err
	}
	registered = true
	if err = stream.Send(&pb.ControllerEnvelope{Payload: &pb.ControllerEnvelope_HelloAck{HelloAck: &pb.HelloAck{Accepted: true, ProtocolVersion: 1}}}); err != nil {
		return err
	}
	c.log.Info("agent connected", "agent_id", e.AgentId, "session_id", e.SessionId, "name", h.Name)
	defer c.log.Info("agent disconnected", "agent_id", e.AgentId)
	sequence := e.Sequence
	for {
		next, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err = validate(next); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		if next.GetHello() != nil || next.AgentId != e.AgentId || next.SessionId != e.SessionId || next.Sequence <= sequence {
			return status.Error(codes.InvalidArgument, "invalid stream identity or sequence")
		}
		if err = c.persist(stream.Context(), next); err != nil {
			return err
		}
		sequence = next.Sequence
		if next.GetHeartbeat() != nil {
			c.log.Info("heartbeat received", "agent_id", e.AgentId, "health", next.GetHeartbeat().Health)
		}
	}
}

func (c *Controller) persist(ctx context.Context, e *pb.AgentEnvelope) error {
	if err := c.repo.Ingest(ctx, e); err != nil {
		if errors.Is(err, storage.ErrMessageConflict) {
			return status.Error(codes.InvalidArgument, "message ID reused with different contents")
		}
		c.log.Error("cannot persist agent message", "agent_id", e.AgentId, "kind", storage.Kind(e), "error", err)
		return status.Error(codes.Unavailable, "cannot persist message; reconnect and retry")
	}
	return nil
}

func validate(e *pb.AgentEnvelope) error {
	if e == nil || e.MessageId == "" || e.AgentId == "" || e.SessionId == "" || e.Sequence == 0 || e.SentAt == nil || storage.Kind(e) == "" {
		return errors.New("envelope requires IDs, positive sequence, sent_at and a payload")
	}
	if len(e.AgentId) > 256 || len(e.SessionId) > 256 || len(e.MessageId) > 256 {
		return errors.New("envelope ID exceeds 256 bytes")
	}
	if err := validateFields(e.ProtoReflect()); err != nil {
		return err
	}
	switch p := e.Payload.(type) {
	case *pb.AgentEnvelope_Hello:
		if p.Hello == nil || p.Hello.Device == nil || p.Hello.Device.Id == "" || p.Hello.StartedAt == nil {
			return errors.New("hello requires device ID and started_at")
		}
	case *pb.AgentEnvelope_Heartbeat:
		if p.Heartbeat == nil || p.Heartbeat.Timestamp == nil {
			return errors.New("heartbeat requires timestamp")
		}
	case *pb.AgentEnvelope_Inventory:
		if p.Inventory == nil || p.Inventory.ObservedAt == nil || len(p.Inventory.Devices) > 1024 {
			return errors.New("invalid inventory batch")
		}
		for _, d := range p.Inventory.Devices {
			if d == nil || d.Id == "" {
				return errors.New("inventory requires device IDs")
			}
		}
	case *pb.AgentEnvelope_Telemetry:
		if p.Telemetry == nil || len(p.Telemetry.Samples) == 0 || len(p.Telemetry.Samples) > 1024 {
			return errors.New("invalid telemetry batch")
		}
		for _, t := range p.Telemetry.Samples {
			if t == nil || t.DeviceId == "" || t.Source == "" || t.ObservedAt == nil || t.CollectedAt == nil {
				return errors.New("telemetry requires device_id, source and timestamps")
			}
		}
	}
	return nil
}

// Walk all supplied fields so malformed timestamps or NaN values in optional
// payloads cannot silently enter PostgreSQL or become invalid JSON for frontend.
func validateFields(m protoreflect.Message) error {
	if m.Descriptor().FullName() == "google.protobuf.Timestamp" {
		ts := m.Interface().(*timestamppb.Timestamp)
		if err := ts.CheckValid(); err != nil {
			return errors.New("invalid protobuf timestamp")
		}
	}
	var failure error
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		check := func(value protoreflect.Value) {
			if fd.Kind() == protoreflect.MessageKind {
				failure = validateFields(value.Message())
			}
			if fd.Kind() == protoreflect.DoubleKind || fd.Kind() == protoreflect.FloatKind {
				if math.IsNaN(value.Float()) || math.IsInf(value.Float(), 0) {
					failure = errors.New("metric values must be finite")
				}
			}
		}
		if fd.IsList() {
			for i := 0; i < v.List().Len() && failure == nil; i++ {
				check(v.List().Get(i))
			}
		} else {
			check(v)
		}
		return failure == nil
	})
	return failure
}
