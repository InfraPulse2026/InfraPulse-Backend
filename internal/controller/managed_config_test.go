package controller

import (
	"context"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"testing"
	"time"
)

func TestManagedConfigNeverNegotiatesOnPlaintextEvenWithAuthorizer(t *testing.T) {
	c, client := commandTestClient(t, &memoryRepository{}, WithManagedConfiguration(true), WithAgentAuthorizer(func(context.Context, string) error { return nil }))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hello := testHello()
	hello.GetHello().ApplicationAck = true
	hello.GetHello().ManagedConfigV1 = true
	hello.GetHello().RemoteManagementEnabled = true
	if err = stream.Send(hello); err != nil {
		t.Fatal(err)
	}
	ack, err := stream.Recv()
	if err != nil || ack.GetHelloAck() == nil || ack.GetHelloAck().ManagedConfigV1 {
		t.Fatal("plaintext managed negotiation", ack, err)
	}
	awaitSession(t, c, true)
	if c.HasManagedSession("agent-1", "session-1") {
		t.Fatal("plaintext ready for management")
	}
	snapshot := &pb.DesiredConfigSnapshot{CollectorId: "agent-1", SessionId: "session-1", Revision: 1}
	if attempted, err := c.SendDesiredConfig(ctx, "agent-1", "session-1", snapshot); attempted || err == nil {
		t.Fatal("unnegotiated config sent", err)
	}
}
