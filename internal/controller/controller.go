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
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/collectorcontract"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/telephony"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/topology"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Repository interface {
	// Ingest returns nil only after inbox and domain persistence have committed.
	// A legitimate replay must also return nil without repeating domain effects.
	Ingest(context.Context, *pb.AgentEnvelope) error
	Disconnect(context.Context, string, string) error
}

type Controller struct {
	managedConfig bool
	pb.UnimplementedAgentServiceServer
	repo           Repository
	log            *slog.Logger
	mu             sync.Mutex
	active         map[string]bool
	ready          map[string]*readyStream
	slots          chan struct{}
	authorizeAgent func(context.Context, string) error
}

type Option func(*Controller)

func WithAgentAuthorizer(authorize func(context.Context, string) error) Option {
	return func(c *Controller) { c.authorizeAgent = authorize }
}

func New(repo Repository, log *slog.Logger, options ...Option) *Controller {
	c := &Controller{repo: repo, log: log, active: make(map[string]bool), ready: make(map[string]*readyStream), slots: make(chan struct{}, 128)}
	for _, option := range options {
		option(c)
	}
	return c
}

func (c *Controller) authorize(ctx context.Context, id string) error {
	if c.authorizeAgent != nil && c.authorizeAgent(ctx, id) != nil {
		return status.Error(codes.Unauthenticated, "agent certificate authorization failed")
	}
	return nil
}

func (c *Controller) Connect(stream grpc.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error {
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		return status.Error(codes.ResourceExhausted, "controller stream limit reached")
	}
	sender := newStreamSender(stream)
	defer sender.stop(status.Error(codes.Canceled, "agent stream closed"))
	received := receiveMessages(sender.ctx, stream)
	e, err := sender.receive(received)
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
	if err = c.authorize(sender.ctx, e.AgentId); err != nil {
		return err
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
		// Remove readiness before database cleanup, so a closing stream cannot
		// accept a new command while Disconnect is still running.
		sender.stop(status.Error(codes.Canceled, "agent stream closed"))
		c.mu.Lock()
		if current := c.ready[e.AgentId]; current != nil && current.sender == sender && current.sessionID == e.SessionId {
			delete(c.ready, e.AgentId)
		}
		c.mu.Unlock()
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
	if err = c.persist(sender.ctx, e); err != nil {
		return err
	}
	registered = true
	applicationACK := h.ApplicationAck
	managedConfig := c.managedConfig && c.authorizeAgent != nil && verifiedManagedPeer(sender.ctx) && h.ManagedConfigV1 && h.RemoteManagementEnabled && applicationACK
	if err = sender.send(&pb.ControllerEnvelope{Payload: &pb.ControllerEnvelope_HelloAck{HelloAck: &pb.HelloAck{Accepted: true, ProtocolVersion: 1, ApplicationAck: applicationACK, ManagedConfigV1: managedConfig}}}); err != nil {
		return err
	}
	if sender.ctx.Err() != nil {
		return sender.stopError()
	}
	c.mu.Lock()
	c.ready[e.AgentId] = &readyStream{sessionID: e.SessionId, sender: sender, managedConfig: managedConfig}
	c.mu.Unlock()
	c.log.Info("agent connected", "agent_id", e.AgentId, "session_id", e.SessionId, "name", h.Name, "application_ack", applicationACK)
	defer c.log.Info("agent disconnected", "agent_id", e.AgentId)
	sequence := e.Sequence
	for {
		next, err := sender.receive(received)
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
		if err = c.authorize(sender.ctx, next.AgentId); err != nil {
			return err
		}
		if err = c.persist(sender.ctx, next); err != nil {
			return err
		}
		sequence = next.Sequence
		// Ingest has committed, including when this is a legitimate replay.
		// Send failure leaves the durable inbox intact for the next attempt.
		if applicationACK {
			if err = sender.send(&pb.ControllerEnvelope{Payload: &pb.ControllerEnvelope_MessageAck{MessageAck: &pb.MessageACK{MessageId: next.MessageId}}}); err != nil {
				c.log.Debug("message ACK send failed after commit", "agent_id", next.AgentId, "message_id", next.MessageId, "error", err)
				return err
			}
			c.log.Debug("message ACK sent", "agent_id", next.AgentId, "message_id", next.MessageId, "kind", storage.Kind(next))
		}
		if next.GetHeartbeat() != nil {
			c.log.Debug("heartbeat received", "agent_id", e.AgentId, "health", next.GetHeartbeat().Health)
		}
	}
}

func (c *Controller) persist(ctx context.Context, e *pb.AgentEnvelope) error {
	if err := c.repo.Ingest(ctx, e); err != nil {
		if errors.Is(err, storage.ErrMessageConflict) {
			return status.Error(codes.InvalidArgument, "message ID reused with different contents")
		}
		if errors.Is(err, storage.ErrEventConflict) || errors.Is(err, storage.ErrInvalidAvailability) {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		if errors.Is(err, storage.ErrLogConflict) || errors.Is(err, storage.ErrInvalidLogs) || errors.Is(err, storage.ErrInvalidTelephony) {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		c.log.Error("cannot persist agent message", "agent_id", e.AgentId, "message_id", e.MessageId, "kind", storage.Kind(e), "ack_sent", false, "error", err)
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
	if err := collectorcontract.Validate(e); err != nil {
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
	case *pb.AgentEnvelope_Topology:
		if err := topology.Validate(p.Topology); err != nil {
			return err
		}
	case *pb.AgentEnvelope_Logs:
		if err := logbook.Validate(p.Logs); err != nil {
			return err
		}
	case *pb.AgentEnvelope_Telephony:
		if err := telephony.Validate(p.Telephony); err != nil {
			return err
		}
	case *pb.AgentEnvelope_CommandResult:
		if err := commands.ValidateResult(p.CommandResult); err != nil {
			return err
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
	if !m.IsValid() || len(m.GetUnknown()) != 0 {
		return errors.New("payload contains unsupported protobuf fields")
	}
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
