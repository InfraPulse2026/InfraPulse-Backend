package controller

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCertificateRejectionPrecedesRegistration(t *testing.T) {
	repo := &memoryRepository{}
	client := testClient(t, repo, WithAgentAuthorizer(func(context.Context, string) error { return errors.New("wrong certificate") }))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.Send(testHello()); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected certificate rejection, got %v", err)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.messages) != 0 || repo.disconnected {
		t.Fatal("unauthorized identity persisted")
	}
}

func TestCertificateRecheckedBeforeHeartbeatPersistence(t *testing.T) {
	var checks atomic.Int32
	repo := &memoryRepository{}
	client := testClient(t, repo, WithAgentAuthorizer(func(context.Context, string) error {
		if checks.Add(1) > 1 {
			return errors.New("expired certificate")
		}
		return nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.Send(testHello()); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); err != nil {
		t.Fatal(err)
	}
	if err = stream.Send(heartbeat()); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected expiry rejection, got %v", err)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.messages) != 1 {
		t.Fatal("unauthorized heartbeat persisted")
	}
}

func TestExpiredAuthorizationPreventsCommandSend(t *testing.T) {
	var denied atomic.Bool
	c, client := commandTestClient(t, &memoryRepository{}, WithAgentAuthorizer(func(context.Context, string) error {
		if denied.Load() {
			return errors.New("expired")
		}
		return nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.Send(ackHello()); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); err != nil {
		t.Fatal(err)
	}
	awaitSession(t, c, true)
	denied.Store(true)
	if attempted, err := c.SendCommand(ctx, "agent-1", "session-1", testCommand("expired-peer")); attempted || status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthorized command attempted=%v err=%v", attempted, err)
	}
	if _, err = stream.Recv(); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("stream not closed: %v", err)
	}
}
