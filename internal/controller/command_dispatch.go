package controller

import (
	"context"
	"errors"
	"sync"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const streamSendTimeout = 5 * time.Second

type readyStream struct {
	managedConfig bool
	sessionID     string
	sender        *streamSender
}

// DisconnectAgent stops readiness immediately after an administrator revokes
// a certificate. A reconnect is independently checked against the registry.
func (c *Controller) DisconnectAgent(agentID string) {
	c.mu.Lock()
	s := c.ready[agentID]
	c.mu.Unlock()
	if s != nil {
		s.sender.stop(status.Error(codes.Unauthenticated, "agent certificate revoked"))
	}
}

// HasSession reports only streams whose registration and HelloAck completed.
// The session check prevents a reconnect from receiving an older session's job.
func (c *Controller) HasSession(agentID, sessionID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.ready[agentID]
	return s != nil && s.sessionID == sessionID && s.sender.ctx.Err() == nil
}

// SendCommand distinguishes a request never handed to Send from a potentially
// delivered command. A started send with an error must not be retried blindly.
func (c *Controller) SendCommand(ctx context.Context, agentID, sessionID string, command *pb.Command) (attempted bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if command == nil {
		return false, status.Error(codes.InvalidArgument, "command is missing")
	}
	c.mu.Lock()
	s := c.ready[agentID]
	if s == nil || s.sessionID != sessionID || s.sender.ctx.Err() != nil {
		c.mu.Unlock()
		return false, status.Error(codes.Unavailable, "matching agent session is not ready")
	}
	c.mu.Unlock()
	if err := c.authorize(s.sender.ctx, agentID); err != nil {
		s.sender.stop(err)
		return false, err
	}
	// An asynchronous transport call must not observe the caller mutating its
	// protobuf while the request waits for the shared sender.
	e := &pb.ControllerEnvelope{Payload: &pb.ControllerEnvelope_Command{Command: proto.Clone(command).(*pb.Command)}}
	return s.sender.sendContext(ctx, e)
}

// All controller messages use one gate. A timed-out caller may leave one Send
// pending until gRPC cancels the RPC; the gate remains held until it finishes.
// This prevents another sender from overlapping that pending call.
type streamSender struct {
	stream   grpc.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]
	ctx      context.Context
	cancel   context.CancelFunc
	gate     chan struct{}
	stopOnce sync.Once
	stopMu   sync.Mutex
	stopErr  error
}

func newStreamSender(stream grpc.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) *streamSender {
	ctx, cancel := context.WithCancel(stream.Context())
	s := &streamSender{stream: stream, ctx: ctx, cancel: cancel, gate: make(chan struct{}, 1)}
	s.gate <- struct{}{}
	return s
}

func (s *streamSender) stop(err error) {
	s.stopOnce.Do(func() {
		if err == nil {
			err = status.Error(codes.Canceled, "agent stream closed")
		}
		s.stopMu.Lock()
		s.stopErr = err
		s.stopMu.Unlock()
		s.cancel()
	})
}

func (s *streamSender) stopError() error {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	if s.stopErr != nil {
		return s.stopErr
	}
	return status.FromContextError(s.ctx.Err()).Err()
}

func (s *streamSender) send(e *pb.ControllerEnvelope) error {
	_, err := s.sendContext(s.ctx, e)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err).Err()
	}
	return err
}

type sendAttempt struct {
	mu       sync.Mutex
	started  bool
	canceled bool
}

func (a *sendAttempt) cancelAttempt() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.canceled = true
	return a.started
}

type sendOutcome struct {
	attempted bool
	err       error
}

func (s *streamSender) sendContext(parent context.Context, e *pb.ControllerEnvelope) (bool, error) {
	ctx, cancel := context.WithTimeout(parent, streamSendTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-s.ctx.Done():
		return false, s.stopError()
	case <-s.gate:
	}
	a := &sendAttempt{}
	done := make(chan sendOutcome, 1)
	go func() {
		defer func() { s.gate <- struct{}{} }()
		a.mu.Lock()
		if a.canceled || ctx.Err() != nil || s.ctx.Err() != nil {
			a.mu.Unlock()
			err := ctx.Err()
			if err == nil {
				err = s.stopError()
			}
			done <- sendOutcome{err: err}
			return
		}
		// This mutex makes cancellation before invocation irrevocable: if the
		// caller reports attempted=false, this goroutine cannot later send.
		a.started = true
		a.mu.Unlock()
		err := s.stream.Send(e)
		if err != nil {
			s.stop(err)
		}
		done <- sendOutcome{attempted: true, err: err}
	}()
	select {
	case result := <-done:
		return result.attempted, result.err
	case <-ctx.Done():
		attempted := a.cancelAttempt()
		if attempted {
			// Connect observes this local cancellation and returns. gRPC then
			// cancels its stream context, releasing a flow-control-blocked Send.
			s.stop(status.FromContextError(ctx.Err()).Err())
		}
		return attempted, ctx.Err()
	case <-s.ctx.Done():
		return a.cancelAttempt(), s.stopError()
	}
}

type receiveOutcome struct {
	message *pb.AgentEnvelope
	err     error
}

// Exactly one goroutine calls Recv. The handler can select local cancellation;
// after its return, gRPC cancels and releases a pending transport Recv.
func receiveMessages(ctx context.Context, stream grpc.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) <-chan receiveOutcome {
	received := make(chan receiveOutcome, 1)
	go func() {
		for {
			message, err := stream.Recv()
			select {
			case received <- receiveOutcome{message: message, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return received
}

func (s *streamSender) receive(received <-chan receiveOutcome) (*pb.AgentEnvelope, error) {
	if s.ctx.Err() != nil {
		return nil, s.stopError()
	}
	select {
	case <-s.ctx.Done():
		return nil, s.stopError()
	case result := <-received:
		return result.message, result.err
	}
}
