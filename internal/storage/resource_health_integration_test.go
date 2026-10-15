package storage

import (
	"fmt"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func resourcePort(up bool) *pb.Interface {
	speed := uint64(8_000_000)
	state := "up"
	if !up {
		state = "down"
	}
	return &pb.Interface{Id: "port-1", Name: "port-1", AdminUp: true, Metadata: &pb.InterfaceMetadata{AdminStatus: true}, OperState: state, SpeedBitsPerSecond: &speed, Rates: &pb.InterfaceRates{}}
}
func resourceEnvelope(id string, at time.Time, port *pb.Interface) *pb.AgentEnvelope {
	e := healthEnvelope(id, 1, at, 10, "linux")
	e.GetTelemetry().Samples[0].Interfaces = []*pb.Interface{port}
	return e
}
func resourceSIPEnvelope(id string, at time.Time, address, state string, latency *float64) *pb.AgentEnvelope {
	h := &pb.TelephonyHealth{TargetId: "sip-1", Address: address, Transport: "udp", Source: "sip-options", State: state, ObservedAt: timestamppb.New(at), LatencyMs: latency}
	if state != "down" {
		h.Reachable = true
		code := uint32(200)
		if state == "degraded" {
			code = 401
		}
		h.ResponseCode = &code
	}
	return &pb.AgentEnvelope{AgentId: "agent-1", SessionId: "session-1", MessageId: id, Sequence: 2, SentAt: timestamppb.Now(), Payload: &pb.AgentEnvelope_Telephony{Telephony: &pb.TelephonyUpdate{Source: "sip-options", ObservedAt: timestamppb.New(at), Targets: []*pb.TelephonyHealth{h}}}}
}
func TestResourceInterfaceLifecycleAndCard(t *testing.T) {
	s, ctx, now := healthFixture(t)
	start := now.Add(-15 * time.Second)
	var last *pb.AgentEnvelope
	for i := 0; i < 3; i++ {
		at := start.Add(time.Duration(i) * 3 * time.Second)
		s.healthClock = func() time.Time { return at }
		last = resourceEnvelope(fmt.Sprint("port-down", i), at, resourcePort(false))
		if err := s.Ingest(ctx, last); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.Incidents(ctx, health.Filter{ResourceKind: "interface", ResourceID: "port-1"})
	if err != nil || len(items) != 1 || items[0].ResourceKind != "interface" || items[0].DeviceID != "shared-router" {
		t.Fatalf("interface incident: %+v %v", items, err)
	}
	id := items[0].ID
	if err = s.Ingest(ctx, last); err != nil {
		t.Fatal(err)
	}
	evidence, _ := s.IncidentEvidence(ctx, id)
	if len(evidence) != 1 {
		t.Fatal("replay duplicated interface evidence")
	}
	for i := 0; i < 3; i++ {
		at := start.Add(time.Duration(i+3) * 3 * time.Second)
		s.healthClock = func() time.Time { return at }
		if err := s.Ingest(ctx, resourceEnvelope(fmt.Sprint("port-up", i), at, resourcePort(true))); err != nil {
			t.Fatal(err)
		}
	}
	item, err := s.Incident(ctx, id)
	if err != nil || item.State != "resolved" {
		t.Fatal("port did not stably recover")
	}
	card, err := s.ResourceHealth(ctx, "agent-1", "shared-router", "interface", "port-1")
	if err != nil || len(card) != 5 {
		t.Fatalf("port card=%d err=%v", len(card), err)
	}
	device, err := s.DeviceHealth(ctx, "agent-1", "shared-router")
	if err != nil || len(device) != 8 {
		t.Fatalf("device misses interface components: %d %v", len(device), err)
	}
}
func TestResourceSIPSeparateTargetLifecycleAndEndpointChange(t *testing.T) {
	s, ctx, now := healthFixture(t)
	s.telephonyInterval = 3
	start := now.Add(-24 * time.Second)
	for i := 0; i < 3; i++ {
		at := start.Add(time.Duration(i) * 3 * time.Second)
		s.healthClock = func() time.Time { return at }
		if err := s.Ingest(ctx, resourceSIPEnvelope(fmt.Sprint("sip-down", i), at, "sip:original", "down", nil)); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.Incidents(ctx, health.Filter{ResourceKind: "sip"})
	if err != nil || len(items) != 1 || items[0].DeviceID != "" || items[0].ResourceID != "sip-1" {
		t.Fatalf("SIP incident=%+v err=%v", items, err)
	}
	original := items[0].ID
	// Healthy NEW endpoint cannot recover an incident on the previous endpoint.
	low := 50.0
	for i := 0; i < 3; i++ {
		at := start.Add(time.Duration(i+3) * 3 * time.Second)
		s.healthClock = func() time.Time { return at }
		if err := s.Ingest(ctx, resourceSIPEnvelope(fmt.Sprint("sip-new", i), at, "sip:replacement", "up", &low)); err != nil {
			t.Fatal(err)
		}
	}
	old, err := s.Incident(ctx, original)
	if err != nil || old.State != "open" || old.EvidenceStatus != "stale" {
		t.Fatal("new endpoint recovered/misrepresented old SIP incident")
	}
	// Returning to the original endpoint needs its own new continuous evidence.
	for i := 0; i < 3; i++ {
		at := start.Add(time.Duration(i+6) * 3 * time.Second)
		s.healthClock = func() time.Time { return at }
		if err := s.Ingest(ctx, resourceSIPEnvelope(fmt.Sprint("sip-recover", i), at, "sip:original", "up", &low)); err != nil {
			t.Fatal(err)
		}
	}
	old, err = s.Incident(ctx, original)
	if err != nil || old.State != "resolved" {
		t.Fatal("same endpoint stable recovery missing")
	}
	card, err := s.ResourceHealth(ctx, "agent-1", "", "sip", "sip-1")
	if err != nil || len(card) != 4 {
		t.Fatalf("SIP context card=%d err=%v", len(card), err)
	}
	var deviceCount int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM devices`).Scan(&deviceCount); err != nil || deviceCount != 1 {
		t.Fatal("SIP target created a fake device")
	}
}
func TestResourceProjectionAtomicFailureAndRetry(t *testing.T) {
	s, ctx, now := healthFixture(t)
	for i := 0; i < 2; i++ {
		if err := s.Ingest(ctx, resourceEnvelope(fmt.Sprint("atomic-port", i), now.Add(time.Duration(i-2)*3*time.Second), resourcePort(false))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE incident_evidence ADD CONSTRAINT reject_port_fixture CHECK(kind<>'opened')`); err != nil {
		t.Fatal(err)
	}
	last := resourceEnvelope("atomic-port2", now, resourcePort(false))
	if err := s.Ingest(ctx, last); err == nil {
		t.Fatal("SQL failure missing")
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM agent_messages WHERE message_id='atomic-port2'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("resource SQL failure inbox committed")
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE incident_evidence DROP CONSTRAINT reject_port_fixture`); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, last); err != nil {
		t.Fatal(err)
	}
	items, _ := s.Incidents(ctx, health.Filter{ResourceKind: "interface"})
	if len(items) != 1 {
		t.Fatal("retry did not create exactly one port incident")
	}
}
