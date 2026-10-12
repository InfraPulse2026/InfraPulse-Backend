package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

func commandTestClient(t *testing.T, repo Repository, options ...Option) (*Controller, pb.AgentServiceClient) {
	t.Helper()
	c := New(repo, slog.New(slog.NewTextHandler(io.Discard, nil)), options...)
	listener := bufconn.Listen(4 << 20)
	server := grpc.NewServer()
	pb.RegisterAgentServiceServer(server, c)
	go func() { _ = server.Serve(listener) }()
	conn, err := grpc.NewClient("passthrough:///commands", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); server.Stop(); _ = listener.Close() })
	return c, pb.NewAgentServiceClient(conn)
}

func awaitSession(t *testing.T, c *Controller, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for c.HasSession("agent-1", "session-1") != want {
		if time.Now().After(deadline) {
			t.Fatalf("matching ready session did not become %v", want)
		}
		time.Sleep(time.Millisecond)
	}
}

func testCommand(id string) *pb.Command {
	expected := true
	return commands.Build(id, "key-"+id, commands.Request{AgentID: "agent-1", DeviceID: "device-1", InterfaceID: "interface-1", Operation: "disable", ExpectedAdminUp: &expected, TimeoutSeconds: 10, Confirmed: true}, time.Now())
}

func TestCommandDispatchAfterHelloOnlyToMatchingSession(t *testing.T) {
	c, client := commandTestClient(t, &memoryRepository{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if attempted, err := c.SendCommand(ctx, "agent-1", "session-1", testCommand("before-hello")); attempted || err == nil {
		t.Fatalf("unregistered stream received command: attempted=%v err=%v", attempted, err)
	}
	if err := stream.Send(ackHello()); err != nil {
		t.Fatal(err)
	}
	if ack, err := stream.Recv(); err != nil || !ack.GetHelloAck().GetAccepted() {
		t.Fatalf("hello failed: %v %v", ack, err)
	}
	awaitSession(t, c, true)
	for _, target := range [][2]string{{"agent-1", "old-session"}, {"other-agent", "session-1"}} {
		if attempted, err := c.SendCommand(ctx, target[0], target[1], testCommand("wrong-session")); attempted || err == nil {
			t.Fatalf("wrong session send: attempted=%v err=%v", attempted, err)
		}
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if attempted, err := c.SendCommand(canceled, "agent-1", "session-1", testCommand("canceled")); attempted || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled dispatch: attempted=%v err=%v", attempted, err)
	}
	command := testCommand("command-1")
	if attempted, err := c.SendCommand(ctx, "agent-1", "session-1", command); !attempted || err != nil {
		t.Fatalf("ready dispatch failed: attempted=%v err=%v", attempted, err)
	}
	e, err := stream.Recv()
	if err != nil || !proto.Equal(command, e.GetCommand()) {
		t.Fatalf("wrong command wire payload: %v %v", e, err)
	}
	_ = stream.CloseSend()
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("close: %v", err)
	}
	awaitSession(t, c, false)
	if attempted, err := c.SendCommand(ctx, "agent-1", "session-1", command); attempted || err == nil {
		t.Fatalf("closed session dispatch: attempted=%v err=%v", attempted, err)
	}
}

func TestACKAndCommandsShareOneStreamingSender(t *testing.T) {
	repo := &memoryRepository{}
	c, client := commandTestClient(t, repo)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Send(ackHello())
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	awaitSession(t, c, true)
	const count = 12
	var wg sync.WaitGroup
	failures := make(chan error, count+1)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if attempted, err := c.SendCommand(ctx, "agent-1", "session-1", testCommand("command")); !attempted || err != nil {
				failures <- err
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < count; i++ {
			e := heartbeat()
			e.Sequence = uint64(i + 2)
			e.MessageId = time.Now().Format("150405.000000000")
			if err := stream.Send(e); err != nil {
				failures <- err
				return
			}
		}
	}()
	acks, receivedCommands := 0, 0
	for i := 0; i < 2*count; i++ {
		e, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case e.GetMessageAck() != nil:
			acks++
			repo.mu.Lock()
			persisted := len(repo.messages)
			repo.mu.Unlock()
			if persisted < acks+1 {
				t.Fatal("ACK preceded commit")
			}
		case e.GetCommand() != nil:
			receivedCommands++
		default:
			t.Fatalf("unexpected payload: %v", e)
		}
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Errorf("dispatch failed: %v", err)
	}
	if acks != count || receivedCommands != count {
		t.Fatalf("acks=%d commands=%d", acks, receivedCommands)
	}
}

// fakeDispatchStream mimics gRPC canceling the transport context when Connect
// returns. Its blocked Send/Recv must both exit after that cancellation.
type fakeDispatchStream struct {
	ctx         context.Context
	cancel      context.CancelFunc
	recv        chan *pb.AgentEnvelope
	sent        chan *pb.ControllerEnvelope
	block       func(*pb.ControllerEnvelope) bool
	sendError   error
	entered     chan struct{}
	sendExited  chan struct{}
	recvExited  chan struct{}
	once        sync.Once
	activeSends atomic.Int32
	concurrent  atomic.Bool
}

func newFakeDispatchStream() *fakeDispatchStream {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeDispatchStream{ctx: ctx, cancel: cancel, recv: make(chan *pb.AgentEnvelope, 8), sent: make(chan *pb.ControllerEnvelope, 32), entered: make(chan struct{}), sendExited: make(chan struct{}), recvExited: make(chan struct{})}
}

func (f *fakeDispatchStream) Context() context.Context     { return f.ctx }
func (f *fakeDispatchStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeDispatchStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeDispatchStream) SetTrailer(metadata.MD)       {}
func (f *fakeDispatchStream) SendMsg(any) error            { return nil }
func (f *fakeDispatchStream) RecvMsg(any) error            { return nil }
func (f *fakeDispatchStream) Send(e *pb.ControllerEnvelope) error {
	if f.activeSends.Add(1) != 1 {
		f.concurrent.Store(true)
	}
	defer f.activeSends.Add(-1)
	if f.block != nil && f.block(e) {
		f.once.Do(func() { close(f.entered) })
		<-f.ctx.Done()
		close(f.sendExited)
		return f.ctx.Err()
	}
	if e.GetCommand() != nil && f.sendError != nil {
		return f.sendError
	}
	select {
	case f.sent <- proto.Clone(e).(*pb.ControllerEnvelope):
		return nil
	case <-f.ctx.Done():
		return f.ctx.Err()
	}
}
func (f *fakeDispatchStream) Recv() (*pb.AgentEnvelope, error) {
	select {
	case e := <-f.recv:
		return e, nil
	case <-f.ctx.Done():
		close(f.recvExited)
		return nil, f.ctx.Err()
	}
}

func runFakeConnection(t *testing.T, stream *fakeDispatchStream) (*Controller, <-chan error) {
	t.Helper()
	c := New(&memoryRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan error, 1)
	go func() {
		err := c.Connect(stream)
		stream.cancel() // Real gRPC does this after its RPC handler returns.
		done <- err
	}()
	t.Cleanup(stream.cancel)
	stream.recv <- ackHello()
	select {
	case e := <-stream.sent:
		if e.GetHelloAck() == nil {
			t.Fatalf("expected HelloAck: %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("hello not sent")
	}
	awaitSession(t, c, true)
	return c, done
}

func awaitClosed(t *testing.T, channel <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(time.Second):
		t.Fatalf("%s goroutine did not exit", name)
	}
}

func TestCommandDeadlineReleasesCallerAndStopsRPC(t *testing.T) {
	stream := newFakeDispatchStream()
	stream.block = func(e *pb.ControllerEnvelope) bool { return e.GetCommand() != nil }
	c, done := runFakeConnection(t, stream)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	attempted, err := c.SendCommand(ctx, "agent-1", "session-1", testCommand("blocked"))
	if !attempted || !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("blocked dispatch was not bounded: attempted=%v err=%v duration=%v", attempted, err, time.Since(started))
	}
	select {
	case err := <-done:
		if status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("wrong stream cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Connect did not stop after timed-out Send")
	}
	awaitClosed(t, stream.sendExited, "Send")
	awaitClosed(t, stream.recvExited, "Recv")
	awaitSession(t, c, false)
	if stream.concurrent.Load() {
		t.Fatal("concurrent Send calls")
	}
}

func TestCancelWaitingForSenderDoesNotSendOrInterruptOtherRequest(t *testing.T) {
	stream := newFakeDispatchStream()
	stream.block = func(e *pb.ControllerEnvelope) bool { return e.GetCommand() != nil }
	c, done := runFakeConnection(t, stream)
	firstCtx, firstCancel := context.WithCancel(context.Background())
	defer firstCancel()
	first := make(chan sendOutcome, 1)
	go func() {
		attempted, err := c.SendCommand(firstCtx, "agent-1", "session-1", testCommand("first"))
		first <- sendOutcome{attempted: attempted, err: err}
	}()
	awaitClosed(t, stream.entered, "first command entry")
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer secondCancel()
	if attempted, err := c.SendCommand(secondCtx, "agent-1", "session-1", testCommand("second")); attempted || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting command sent: attempted=%v err=%v", attempted, err)
	}
	if !c.HasSession("agent-1", "session-1") || stream.concurrent.Load() {
		t.Fatal("canceling waiter interrupted an in-progress send")
	}
	firstCancel()
	select {
	case result := <-first:
		if !result.attempted || !errors.Is(result.err, context.Canceled) {
			t.Fatalf("first result: %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("first command caller stuck")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Connect stuck")
	}
	awaitClosed(t, stream.sendExited, "Send")
	awaitClosed(t, stream.recvExited, "Recv")
}

func TestTransportSendFailureIsAttemptedAndInvalidatesSession(t *testing.T) {
	stream := newFakeDispatchStream()
	stream.sendError = status.Error(codes.Unavailable, "transport gone")
	c, done := runFakeConnection(t, stream)
	attempted, err := c.SendCommand(context.Background(), "agent-1", "session-1", testCommand("failed"))
	if !attempted || status.Code(err) != codes.Unavailable {
		t.Fatalf("failed Send must remain attempted: %v %v", attempted, err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("failed stream did not close")
	}
	awaitSession(t, c, false)
	awaitClosed(t, stream.recvExited, "Recv")
}

func TestBlockedACKUsesBoundedSendAndClosesRPC(t *testing.T) {
	stream := newFakeDispatchStream()
	stream.block = func(e *pb.ControllerEnvelope) bool { return e.GetMessageAck() != nil }
	c, done := runFakeConnection(t, stream)
	stream.recv <- heartbeat()
	awaitClosed(t, stream.entered, "ACK entry")
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) && status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("wrong bounded ACK error: %v", err)
		}
	case <-time.After(streamSendTimeout + time.Second):
		t.Fatal("blocked ACK never closed RPC")
	}
	awaitClosed(t, stream.sendExited, "ACK Send")
	awaitClosed(t, stream.recvExited, "Recv")
	awaitSession(t, c, false)
}

func TestCommandResultValidationAllowsLegacyButRejectsMissingID(t *testing.T) {
	e := heartbeat()
	e.Payload = &pb.AgentEnvelope_CommandResult{CommandResult: &pb.CommandResult{CommandId: "legacy-command"}}
	if err := validate(e); err != nil {
		t.Fatalf("legacy unspecified result rejected: %v", err)
	}
	e.GetCommandResult().CommandId = ""
	if validate(e) == nil {
		t.Fatal("result without command ID accepted")
	}
}

func TestReadyRegistryWaitsUntilHelloAckFinishes(t *testing.T) {
	stream := newFakeDispatchStream()
	stream.block = func(e *pb.ControllerEnvelope) bool { return e.GetHelloAck() != nil }
	c := New(&memoryRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan error, 1)
	go func() {
		err := c.Connect(stream)
		stream.cancel()
		done <- err
	}()
	t.Cleanup(stream.cancel)
	stream.recv <- ackHello()
	awaitClosed(t, stream.entered, "HelloAck Send entry")
	if c.HasSession("agent-1", "session-1") {
		t.Fatal("stream registered before HelloAck was sent")
	}
	if attempted, err := c.SendCommand(context.Background(), "agent-1", "session-1", testCommand("too-early")); attempted || err == nil {
		t.Fatalf("command bypassed blocked HelloAck: attempted=%v err=%v", attempted, err)
	}
	stream.cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled handshake did not finish")
	}
	awaitClosed(t, stream.sendExited, "HelloAck Send")
	awaitClosed(t, stream.recvExited, "Recv")
}
