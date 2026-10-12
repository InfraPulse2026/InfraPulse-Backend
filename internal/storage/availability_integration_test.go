package storage

import (
	"errors"
	"fmt"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/availability"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func availabilityBool(value bool) *bool { return &value }

func availabilityInventory(messageID, deviceID string, observed time.Time, lastSeen *time.Time, online bool) *pb.AgentEnvelope {
	d := &pb.Device{Id: deviceID, Name: deviceID, Source: "remote", Online: online}
	if lastSeen != nil {
		d.LastSeen = timestamppb.New(*lastSeen)
	}
	return &pb.AgentEnvelope{AgentId: "agent-1", SessionId: "session-1", MessageId: messageID, Sequence: 2,
		SentAt: timestamppb.New(observed), Payload: &pb.AgentEnvelope_Inventory{Inventory: &pb.InventoryUpdate{
			ObservedAt: timestamppb.New(observed), Devices: []*pb.Device{d},
		}}}
}

func availabilityTelemetry(messageID, deviceID string, observed time.Time) *pb.AgentEnvelope {
	e := sampleEnvelope(observed)
	e.MessageId = messageID
	e.GetTelemetry().Samples[0].DeviceId = deviceID
	e.GetTelemetry().Samples[0].Source = "remote"
	return e
}

func availabilityEvent(messageID, eventID, deviceID, eventType, source string, observed time.Time) *pb.AgentEnvelope {
	return &pb.AgentEnvelope{AgentId: "agent-1", SessionId: "session-1", MessageId: messageID, Sequence: 3,
		SentAt: timestamppb.New(observed), Payload: &pb.AgentEnvelope_Events{Events: &pb.EventBatch{Events: []*pb.Event{{
			Id: eventID, DeviceId: deviceID, Type: eventType, Source: source, Timestamp: timestamppb.New(observed),
		}}}}}
}

func ingestAvailability(t *testing.T, s *Store, e *pb.AgentEnvelope) {
	t.Helper()
	if err := s.Ingest(t.Context(), e); err != nil {
		t.Fatalf("ingest %q: %v", e.MessageId, err)
	}
}

func assertAvailability(t *testing.T, s *Store, agentID, deviceID, status string, reported *bool) Device {
	t.Helper()
	d, err := s.Device(t.Context(), agentID, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Availability.Status != status || d.Online != (status == "online") {
		t.Fatalf("%s/%s availability/compatibility online disagree: %+v", agentID, deviceID, d)
	}
	for _, value := range []*bool{d.ReportedOnline, d.Availability.LastReportedOnline} {
		if (value == nil) != (reported == nil) || value != nil && *value != *reported {
			t.Fatalf("%s/%s lost reported state: %+v, want %v", agentID, deviceID, d, reported)
		}
	}
	return d
}

func availabilityHistory(t *testing.T, s *Store, agentID, deviceID string) []availability.Change {
	t.Helper()
	changes, err := s.AvailabilityChanges(t.Context(), agentID, deviceID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	return changes
}

func TestAvailabilityPostgresPolicyCadenceDoesNotRefreshEvidence(t *testing.T) {
	s := integrationStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ingestAvailability(t, s, registration("agent-1", now))
	old := now.Add(-5 * time.Hour)
	ingestAvailability(t, s, availabilityTelemetry("old-remote-metrics", "remote-router", old))
	d := assertAvailability(t, s, "agent-1", "remote-router", "stale", availabilityBool(true))
	if d.Availability.Reason != "observation_expired" || d.Availability.ExpectedIntervalSeconds != 3 || d.Availability.StaleAfterSeconds != 11 {
		t.Fatalf("three-second policy did not expire old evidence: %+v", d.Availability)
	}
	history := availabilityHistory(t, s, "agent-1", "remote-router")
	ingestAvailability(t, s, availabilityEvent("old-offline-event", "old-offline-id", "offline-router", "device.offline", "remote", old))
	assertAvailability(t, s, "agent-1", "offline-router", "stale", availabilityBool(false))
	slow := monitoring.Policy{TelemetryIntervalSeconds: 86400, InventoryIntervalSeconds: 86400, TopologyIntervalSeconds: 86400}
	if err := s.UpdateMonitoring(t.Context(), "agent-1", "remote-router", slow); err != nil {
		t.Fatal(err)
	}
	d = assertAvailability(t, s, "agent-1", "remote-router", "online", availabilityBool(true))
	if d.Availability.ExpectedIntervalSeconds != 86400 || d.Availability.StaleAfterSeconds != 172830 ||
		d.Availability.ObservedAt == nil || !d.Availability.ObservedAt.Equal(old) ||
		d.Availability.ExpiresAt == nil || !d.Availability.ExpiresAt.Equal(old.Add(172830*time.Second)) {
		t.Fatalf("daily policy lost the original observation/cadence: %+v", d.Availability)
	}
	// Event cadence uses the slower configured observation cadence, even if
	// the inventory cadence alone is short.
	eventPolicy := monitoring.Policy{TelemetryIntervalSeconds: 86400, InventoryIntervalSeconds: 3, TopologyIntervalSeconds: 3}
	if err := s.UpdateMonitoring(t.Context(), "agent-1", "offline-router", eventPolicy); err != nil {
		t.Fatal(err)
	}
	d = assertAvailability(t, s, "agent-1", "offline-router", "offline", availabilityBool(false))
	if d.Availability.ExpectedIntervalSeconds != 86400 || d.Availability.ObservedAt == nil || !d.Availability.ObservedAt.Equal(old) {
		t.Fatalf("negative event did not use the slow observation cadence: %+v", d.Availability)
	}
	if err := s.UpdateMonitoring(t.Context(), "agent-1", "remote-router", monitoring.Defaults()); err != nil {
		t.Fatal(err)
	}
	assertAvailability(t, s, "agent-1", "remote-router", "stale", availabilityBool(true))
	if got := availabilityHistory(t, s, "agent-1", "remote-router"); len(got) != len(history) {
		t.Fatalf("policy changes invented reported transitions: before=%+v after=%+v", history, got)
	}
	if got := availabilityHistory(t, s, "agent-1", "offline-router"); len(got) != 1 || got[0].ToOnline {
		t.Fatalf("stale/offline policy evaluation invented reported transitions: %+v", got)
	}
}

func TestAvailabilityPostgresInventoryUsesQualifiedObservationAndInventoryCadence(t *testing.T) {
	s := integrationStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ingestAvailability(t, s, registration("agent-1", now))
	old := now.Add(-time.Hour)
	ingestAvailability(t, s, availabilityInventory("old-device-observation", "remote-router", now, &old, true))
	d := assertAvailability(t, s, "agent-1", "remote-router", "stale", availabilityBool(true))
	if d.Availability.ObservedAt == nil || !d.Availability.ObservedAt.Equal(old) || d.Availability.ExpectedIntervalSeconds != 300 {
		t.Fatalf("batch receipt refreshed an old device observation: %+v", d.Availability)
	}
	policy := monitoring.Policy{TelemetryIntervalSeconds: 86400, InventoryIntervalSeconds: 300, TopologyIntervalSeconds: 300}
	if err := s.UpdateMonitoring(t.Context(), "agent-1", "remote-router", policy); err != nil {
		t.Fatal(err)
	}
	assertAvailability(t, s, "agent-1", "remote-router", "stale", availabilityBool(true))
	// A proto3 false with no observation time is unknown, including when it
	// appears in a newer inventory batch after an established online report.
	ingestAvailability(t, s, availabilityInventory("unspecified-online", "remote-router", now.Add(time.Second), nil, false))
	assertAvailability(t, s, "agent-1", "remote-router", "stale", availabilityBool(true))
	ingestAvailability(t, s, availabilityInventory("positive-fallback", "fallback-router", now, nil, true))
	d = assertAvailability(t, s, "agent-1", "fallback-router", "online", availabilityBool(true))
	if d.Availability.ObservedAt == nil || !d.Availability.ObservedAt.Equal(now) {
		t.Fatalf("positive inventory without last_seen did not use batch time: %+v", d.Availability)
	}
	// The timestamp on an offline inventory record identifies a qualified
	// report; the actual negative observation is the inventory batch time.
	ingestAvailability(t, s, availabilityInventory("qualified-offline", "remote-router", now, &old, false))
	d = assertAvailability(t, s, "agent-1", "remote-router", "offline", availabilityBool(false))
	if d.Availability.ObservedAt == nil || !d.Availability.ObservedAt.Equal(now) || d.Availability.ExpectedIntervalSeconds != 300 {
		t.Fatalf("offline inventory used the old positive timestamp: %+v", d.Availability)
	}
}

func TestAvailabilityPostgresExplicitOfflineRecoveryAndCompressedHistory(t *testing.T) {
	s := integrationStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ingestAvailability(t, s, registration("agent-1", now))
	base := now.Add(-5 * time.Second)
	ingestAvailability(t, s, availabilityInventory("first-online", "remote-router", base, &base, true))
	ingestAvailability(t, s, availabilityTelemetry("still-online", "remote-router", base.Add(time.Second)))
	ingestAvailability(t, s, availabilityEvent("first-offline", "offline-1", "remote-router", "device.offline", "remote", base.Add(2*time.Second)))
	d := assertAvailability(t, s, "agent-1", "remote-router", "offline", availabilityBool(false))
	if d.Availability.EvidenceKind != "event_offline" || d.Availability.EventID != "offline-1" || d.Availability.ExpectedIntervalSeconds != 300 {
		t.Fatalf("offline event provenance/cadence missing: %+v", d.Availability)
	}
	ingestAvailability(t, s, availabilityEvent("still-offline", "offline-2", "remote-router", "device.offline", "simulated", base.Add(3*time.Second)))
	ingestAvailability(t, s, availabilityTelemetry("recovery", "remote-router", base.Add(4*time.Second)))
	d = assertAvailability(t, s, "agent-1", "remote-router", "online", availabilityBool(true))
	if d.Availability.EvidenceKind != "telemetry" || d.Availability.MessageID != "recovery" || d.Availability.ExpectedIntervalSeconds != 3 {
		t.Fatalf("telemetry did not restore fresh positive evidence: %+v", d.Availability)
	}
	changes := availabilityHistory(t, s, "agent-1", "remote-router")
	if len(changes) != 3 || !changes[0].ToOnline || changes[0].FromOnline == nil || *changes[0].FromOnline ||
		changes[1].ToOnline || changes[1].FromOnline == nil || !*changes[1].FromOnline ||
		!changes[2].ToOnline || changes[2].FromOnline != nil {
		t.Fatalf("history did not compress repeated reports into true/false/true: %+v", changes)
	}
	if !changes[0].ObservedAt.Equal(base.Add(4*time.Second)) || !changes[1].ObservedAt.Equal(base.Add(2*time.Second)) || !changes[2].ObservedAt.Equal(base) {
		t.Fatalf("history changed reported transition timestamps: %+v", changes)
	}
	page, err := s.AvailabilityChanges(t.Context(), "agent-1", "remote-router", 1, 1)
	if err != nil || len(page) != 1 || page[0].ID != changes[1].ID {
		t.Fatalf("history pagination: %+v %v", page, err)
	}
}

func TestAvailabilityPostgresNegativeEventWinsEqualTimestampInEitherArrivalOrder(t *testing.T) {
	for _, eventFirst := range []bool{false, true} {
		name := "inventory-first"
		if eventFirst {
			name = "event-first"
		}
		t.Run(name, func(t *testing.T) {
			s := integrationStore(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			ingestAvailability(t, s, registration("agent-1", now))
			seen := now.Add(-2 * time.Second)
			inventory := availabilityInventory("same-time-inventory", "remote-router", seen, &seen, true)
			event := availabilityEvent("same-time-offline", "offline-1", "remote-router", "device.offline", "remote", seen)
			if eventFirst {
				ingestAvailability(t, s, event)
				ingestAvailability(t, s, inventory)
			} else {
				ingestAvailability(t, s, inventory)
				ingestAvailability(t, s, event)
			}
			d := assertAvailability(t, s, "agent-1", "remote-router", "offline", availabilityBool(false))
			if d.Availability.EventID != "offline-1" || d.Availability.EvidenceKind != "event_offline" {
				t.Fatalf("equal-time inventory overrode an explicit offline event: %+v", d.Availability)
			}
			ingestAvailability(t, s, availabilityTelemetry("older-metrics", "remote-router", seen.Add(-time.Second)))
			assertAvailability(t, s, "agent-1", "remote-router", "offline", availabilityBool(false))
			ingestAvailability(t, s, availabilityTelemetry("newer-metrics", "remote-router", seen.Add(time.Second)))
			assertAvailability(t, s, "agent-1", "remote-router", "online", availabilityBool(true))
		})
	}
}

func TestAvailabilityPostgresCollectorErrorsAndTopologyDoNotChangeDeviceHealth(t *testing.T) {
	s := integrationStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ingestAvailability(t, s, registration("agent-1", now))
	seen := now.Add(-2 * time.Second)
	ingestAvailability(t, s, availabilityInventory("remote-online", "remote-router", seen, &seen, true))
	before := assertAvailability(t, s, "agent-1", "remote-router", "online", availabilityBool(true))
	for _, tc := range []struct{ id, eventType, source string }{
		{"interface", "interface.down", "remote"},
		{"telephony", "telephony.down", "simulated"},
		{"collector", "collector.error", "remote"},
		{"local-offline", "device.offline", "local"},
		{"collector-offline", "device.offline", "collector"},
		{"empty-source", "device.offline", ""},
	} {
		ingestAvailability(t, s, availabilityEvent("ignored-"+tc.id, tc.id, "remote-router", tc.eventType, tc.source, now))
	}
	graph := topologyEnvelope("remote-topology", now)
	graph.GetTopology().DeviceId = "remote-router"
	graph.GetTopology().Observations[0].LocalDeviceId = "remote-router"
	ingestAvailability(t, s, graph)
	ingestAvailability(t, s, &pb.AgentEnvelope{AgentId: "agent-1", SessionId: "session-1", MessageId: "collector-error-report", Sequence: 4,
		SentAt: timestamppb.New(now), Payload: &pb.AgentEnvelope_Error{Error: &pb.ErrorReport{
			Component: "remote.collector", Code: "timeout", Message: "poll failed", Timestamp: timestamppb.New(now),
		}}})
	after := assertAvailability(t, s, "agent-1", "remote-router", "online", availabilityBool(true))
	if after.Availability.MessageID != before.Availability.MessageID || after.Availability.ObservedAt == nil || !after.Availability.ObservedAt.Equal(seen) ||
		after.Availability.ReceivedAt == nil || before.Availability.ReceivedAt == nil || !after.Availability.ReceivedAt.Equal(*before.Availability.ReceivedAt) ||
		len(availabilityHistory(t, s, "agent-1", "remote-router")) != 1 {
		t.Fatalf("non-device events or topology changed health evidence: before=%+v after=%+v", before.Availability, after.Availability)
	}
}

func TestAvailabilityPostgresMissingDataAndDisconnectNeverReportOffline(t *testing.T) {
	s := integrationStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ingestAvailability(t, s, registration("agent-1", now))
	old := now.Add(-time.Hour)
	ingestAvailability(t, s, availabilityTelemetry("old-evidence", "remote-router", old))
	ingestAvailability(t, s, availabilityInventory("unknown-device", "unknown-router", now, nil, false))
	assertAvailability(t, s, "agent-1", "remote-router", "stale", availabilityBool(true))
	assertAvailability(t, s, "agent-1", "unknown-router", "unknown", nil)
	empty := availabilityInventory("empty-inventory", "unused", now, nil, false)
	empty.GetInventory().Devices = nil
	ingestAvailability(t, s, empty)
	assertAvailability(t, s, "agent-1", "remote-router", "stale", availabilityBool(true))
	changes := availabilityHistory(t, s, "agent-1", "remote-router")
	if err := s.Disconnect(t.Context(), "agent-1", "session-1"); err != nil {
		t.Fatal(err)
	}
	d := assertAvailability(t, s, "agent-1", "remote-router", "stale", availabilityBool(true))
	if d.Availability.Reason != "agent_disconnected" || d.AgentConnected {
		t.Fatalf("disconnect did not make evidence stale: %+v", d)
	}
	assertAvailability(t, s, "agent-1", "unknown-router", "unknown", nil)
	if got := availabilityHistory(t, s, "agent-1", "remote-router"); len(got) != len(changes) || len(got) != 1 || !got[0].ToOnline {
		t.Fatalf("expiry/disconnect created a false offline transition: %+v", got)
	}
	if got := availabilityHistory(t, s, "agent-1", "unknown-router"); len(got) != 0 {
		t.Fatalf("unknown proto default created reported transitions: %+v", got)
	}
}

func TestAvailabilityPostgresHeartbeatUsesReceiptAndOnlyRestoresLocalDevice(t *testing.T) {
	s := integrationStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	hello := registration("agent-1", now)
	hello.GetHello().Device.Online = false
	ingestAvailability(t, s, hello)
	ingestAvailability(t, s, availabilityEvent("remote-offline", "offline-1", "remote-router", "device.offline", "remote", now))
	before := time.Now().UTC().Add(-time.Second)
	ingestAvailability(t, s, &pb.AgentEnvelope{AgentId: "agent-1", SessionId: "session-1", MessageId: "skewed-heartbeat", Sequence: 4,
		SentAt: timestamppb.New(now), Payload: &pb.AgentEnvelope_Heartbeat{Heartbeat: &pb.Heartbeat{
			Timestamp: timestamppb.New(now.Add(3 * time.Hour)), Health: "ok",
		}}})
	d := assertAvailability(t, s, "agent-1", "shared-router", "online", availabilityBool(true))
	if d.Availability.EvidenceKind != "heartbeat" || d.Availability.Reason != "agent_heartbeat" || d.Availability.ClockSkew ||
		d.Availability.ObservedAt == nil || d.Availability.ObservedAt.Before(before) || d.Availability.ObservedAt.After(time.Now().UTC()) ||
		d.Availability.StaleAfterSeconds != 90 {
		t.Fatalf("heartbeat trusted the Agent clock instead of server receipt: %+v", d.Availability)
	}
	assertAvailability(t, s, "agent-1", "remote-router", "offline", availabilityBool(false))
}

func TestAvailabilityPostgresFutureAndOutOfOrderReportsCannotPoisonRecovery(t *testing.T) {
	s := integrationStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ingestAvailability(t, s, registration("agent-1", now))
	seen := now.Add(-3 * time.Second)
	ingestAvailability(t, s, availabilityInventory("valid-online", "remote-router", seen, &seen, true))
	ingestAvailability(t, s, availabilityEvent("future-offline", "future-event", "remote-router", "device.offline", "remote", now.Add(time.Hour)))
	d := assertAvailability(t, s, "agent-1", "remote-router", "online", availabilityBool(true))
	if !d.Availability.ClockSkew || d.Availability.ObservedAt == nil || !d.Availability.ObservedAt.Equal(seen) {
		t.Fatalf("future report replaced valid current evidence: %+v", d.Availability)
	}
	ingestAvailability(t, s, availabilityEvent("older-offline", "old-event", "remote-router", "device.offline", "remote", seen.Add(-time.Hour)))
	assertAvailability(t, s, "agent-1", "remote-router", "online", availabilityBool(true))
	recovered := seen.Add(time.Second)
	ingestAvailability(t, s, availabilityTelemetry("clock-corrected", "remote-router", recovered))
	d = assertAvailability(t, s, "agent-1", "remote-router", "online", availabilityBool(true))
	if d.Availability.ClockSkew || d.Availability.ObservedAt == nil || !d.Availability.ObservedAt.Equal(recovered) || d.LastSeen.After(now.Add(30*time.Second)) {
		t.Fatalf("valid observation could not recover from future evidence: %+v", d)
	}
	if got := availabilityHistory(t, s, "agent-1", "remote-router"); len(got) != 1 || !got[0].ToOnline {
		t.Fatalf("future/old negative reports invented current transitions: %+v", got)
	}
	ingestAvailability(t, s, availabilityTelemetry("future-only", "future-router", now.Add(time.Hour)))
	d = assertAvailability(t, s, "agent-1", "future-router", "unknown", nil)
	if !d.Availability.ClockSkew || d.Availability.Reason != "clock_skew" {
		t.Fatalf("future-only evidence became healthy: %+v", d.Availability)
	}
	ingestAvailability(t, s, availabilityTelemetry("future-router-corrected", "future-router", seen))
	d = assertAvailability(t, s, "agent-1", "future-router", "online", availabilityBool(true))
	if d.Availability.ClockSkew {
		t.Fatalf("future-only device did not recover after clock correction: %+v", d.Availability)
	}
}

func TestAvailabilityPostgresEventReplayAcrossEnvelopeIDsDoesNotRefreshEvidence(t *testing.T) {
	s := integrationStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ingestAvailability(t, s, registration("agent-1", now))
	original := availabilityEvent("original-event-envelope", "stable-event", "remote-router", "device.offline", "remote", now.Add(-time.Second))
	ingestAvailability(t, s, original)
	before := assertAvailability(t, s, "agent-1", "remote-router", "offline", availabilityBool(false))
	replay := proto.Clone(original).(*pb.AgentEnvelope)
	replay.MessageId = "another-event-envelope"
	replay.Sequence = 99
	replay.SentAt = timestamppb.New(now.Add(time.Minute))
	ingestAvailability(t, s, replay)
	after := assertAvailability(t, s, "agent-1", "remote-router", "offline", availabilityBool(false))
	if before.Availability.ReceivedAt == nil || after.Availability.ReceivedAt == nil || !before.Availability.ReceivedAt.Equal(*after.Availability.ReceivedAt) ||
		after.Availability.MessageID != original.MessageId || len(availabilityHistory(t, s, "agent-1", "remote-router")) != 1 {
		t.Fatalf("stable event ID replay refreshed/duplicated evidence: before=%+v after=%+v", before.Availability, after.Availability)
	}
	var inbox, eventIDs int
	if err := s.pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM agent_messages WHERE agent_id='agent-1' AND message_id IN ('original-event-envelope','another-event-envelope')),
		(SELECT count(*) FROM availability_event_ids WHERE agent_id='agent-1' AND event_id='stable-event')`).Scan(&inbox, &eventIDs); err != nil || inbox != 2 || eventIDs != 1 {
		t.Fatalf("accepted replay archive/dedup semantics: inbox=%d event IDs=%d err=%v", inbox, eventIDs, err)
	}
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertAvailability(t, s, "agent-1", "remote-router", "stale", availabilityBool(false))
	hello := registration("agent-1", now)
	hello.SessionId = "session-2"
	hello.GetHello().SessionId = "session-2"
	hello.MessageId = "hello-after-restart"
	ingestAvailability(t, s, hello)
	replay.MessageId = "event-replay-after-restart"
	replay.SessionId = "session-2"
	ingestAvailability(t, s, replay)
	after = assertAvailability(t, s, "agent-1", "remote-router", "offline", availabilityBool(false))
	if after.Availability.ReceivedAt == nil || !before.Availability.ReceivedAt.Equal(*after.Availability.ReceivedAt) ||
		len(availabilityHistory(t, s, "agent-1", "remote-router")) != 1 {
		t.Fatalf("restart/session replay duplicated or refreshed remote evidence: %+v", after.Availability)
	}
	// Event identity belongs to the Agent, so two Agents may use the same ID.
	ingestAvailability(t, s, registration("agent-2", now))
	other := proto.Clone(original).(*pb.AgentEnvelope)
	other.AgentId = "agent-2"
	ingestAvailability(t, s, other)
	assertAvailability(t, s, "agent-2", "remote-router", "offline", availabilityBool(false))
}

func TestAvailabilityPostgresRepeatedReportsKeepCompactCurrentProjection(t *testing.T) {
	s := integrationStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ingestAvailability(t, s, registration("agent-1", now))
	const reports = 12
	base := now.Add(-2 * time.Second)
	for i := range reports {
		ingestAvailability(t, s, availabilityTelemetry(fmt.Sprintf("online-report-%d", i), "remote-router", base.Add(time.Duration(i)*time.Microsecond)))
	}
	d := assertAvailability(t, s, "agent-1", "remote-router", "online", availabilityBool(true))
	if d.Availability.ObservedAt == nil || !d.Availability.ObservedAt.Equal(base.Add((reports-1)*time.Microsecond)) {
		t.Fatalf("compact projection lost the latest report: %+v", d.Availability)
	}
	var sources, current, changes, samples int
	if err := s.pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM availability_sources WHERE agent_id='agent-1' AND device_id='remote-router'),
		(SELECT count(*) FROM device_availability_current WHERE agent_id='agent-1' AND device_id='remote-router'),
		(SELECT count(*) FROM device_availability_changes WHERE agent_id='agent-1' AND device_id='remote-router'),
		(SELECT count(*) FROM telemetry WHERE agent_id='agent-1' AND device_id='remote-router')`).Scan(&sources, &current, &changes, &samples); err != nil ||
		sources != 1 || current != 1 || changes != 1 || samples != reports {
		t.Fatalf("repeated reports grew current/history or lost raw archive: sources=%d current=%d changes=%d samples=%d err=%v", sources, current, changes, samples, err)
	}
}

func TestAvailabilityPostgresConflictingEventIDRollsBackEntireBatch(t *testing.T) {
	s := integrationStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ingestAvailability(t, s, registration("agent-1", now))
	original := availabilityEvent("original-event", "stable-event", "remote-router", "device.offline", "remote", now.Add(-time.Second))
	ingestAvailability(t, s, original)
	bad := availabilityEvent("conflicting-event-batch", "new-event", "must-rollback", "device.online", "remote", now)
	conflict := proto.Clone(original.GetEvents().Events[0]).(*pb.Event)
	conflict.Type = "device.online"
	bad.GetEvents().Events = append(bad.GetEvents().Events, conflict)
	if err := s.Ingest(t.Context(), bad); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("conflicting event ID accepted: %v", err)
	}
	assertAvailability(t, s, "agent-1", "remote-router", "offline", availabilityBool(false))
	if _, err := s.Device(t.Context(), "agent-1", "must-rollback"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("earlier event in rejected batch left a device projection: %v", err)
	}
	var messages, eventIDs, changes int
	if err := s.pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM agent_messages WHERE agent_id='agent-1' AND message_id='conflicting-event-batch'),
		(SELECT count(*) FROM availability_event_ids WHERE agent_id='agent-1' AND event_id='new-event'),
		(SELECT count(*) FROM device_availability_changes WHERE agent_id='agent-1' AND device_id='must-rollback')`).Scan(&messages, &eventIDs, &changes); err != nil || messages != 0 || eventIDs != 0 || changes != 0 {
		t.Fatalf("conflicting batch partially committed: inbox=%d event IDs=%d changes=%d err=%v", messages, eventIDs, changes, err)
	}
	if got := availabilityHistory(t, s, "agent-1", "remote-router"); len(got) != 1 || got[0].ToOnline {
		t.Fatalf("conflicting event changed reported history: %+v", got)
	}
}

func TestAvailabilityPostgresBeforeInventoryTopologyPlaceholderStaysUnknown(t *testing.T) {
	s := integrationStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ingestAvailability(t, s, registration("agent-1", now))
	early := topologyEnvelope("early-topology", now)
	early.GetTopology().DeviceId = "before-inventory"
	early.GetTopology().Observations[0].LocalDeviceId = "before-inventory"
	ingestAvailability(t, s, early)
	d := assertAvailability(t, s, "agent-1", "before-inventory", "unknown", nil)
	if d.Availability.Reason != "no_observation" || d.Availability.ObservedAt != nil || len(availabilityHistory(t, s, "agent-1", "before-inventory")) != 0 {
		t.Fatalf("topology placeholder invented an offline observation: %+v", d.Availability)
	}
	ingestAvailability(t, s, availabilityInventory("default-false-inventory", "before-inventory", now, nil, false))
	assertAvailability(t, s, "agent-1", "before-inventory", "unknown", nil)
	ingestAvailability(t, s, availabilityTelemetry("first-health-evidence", "before-inventory", now))
	assertAvailability(t, s, "agent-1", "before-inventory", "online", availabilityBool(true))
}

func TestAvailabilityPostgresOldInventoryCannotRefreshLastSeen(t *testing.T) {
	s := integrationStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ingestAvailability(t, s, registration("agent-1", now))
	seen := now.Add(-2 * time.Second)
	current := availabilityInventory("current-inventory", "remote-router", seen, &seen, true)
	current.GetInventory().Devices[0].Name = "Current name"
	ingestAvailability(t, s, current)
	before := assertAvailability(t, s, "agent-1", "remote-router", "online", availabilityBool(true))
	futureLastSeen := now.Add(2 * time.Hour)
	old := availabilityInventory("old-inventory", "remote-router", now.Add(-time.Hour), &futureLastSeen, true)
	old.GetInventory().Devices[0].Name = "Old name"
	ingestAvailability(t, s, old)
	after := assertAvailability(t, s, "agent-1", "remote-router", "online", availabilityBool(true))
	if after.Name != "Current name" || !after.LastSeen.Equal(before.LastSeen) || after.Availability.ObservedAt == nil || !after.Availability.ObservedAt.Equal(seen) {
		t.Fatalf("old inventory refreshed latest observation/last_seen: before=%+v after=%+v", before, after)
	}
	// A recent batch carrying an old positive device LastSeen must likewise
	// leave the positive observation at its original business timestamp.
	olderDeviceSeen := now.Add(-2 * time.Hour)
	ingestAvailability(t, s, availabilityInventory("new-batch-old-device", "remote-router", now, &olderDeviceSeen, true))
	after = assertAvailability(t, s, "agent-1", "remote-router", "online", availabilityBool(true))
	if !after.LastSeen.Equal(before.LastSeen) || after.Availability.ObservedAt == nil || !after.Availability.ObservedAt.Equal(seen) {
		t.Fatalf("new batch refreshed an old positive device observation: %+v", after)
	}
}

func assertAvailabilitySummaryMatchesCards(t *testing.T, s *Store, agentID string) availability.Summary {
	t.Helper()
	summary, err := s.AvailabilitySummary(t.Context(), agentID)
	if err != nil {
		t.Fatal(err)
	}
	devices, err := s.Devices(t.Context(), agentID, "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	var clockSkew int64
	for _, d := range devices {
		counts[d.Availability.Status]++
		if d.Availability.ClockSkew {
			clockSkew++
		}
		if d.Online != (d.Availability.Status == "online") {
			t.Fatalf("card compatibility field disagrees: %+v", d)
		}
	}
	if summary.Total != int64(len(devices)) || summary.Online != counts["online"] || summary.Offline != counts["offline"] ||
		summary.Stale != counts["stale"] || summary.Unknown != counts["unknown"] || summary.ClockSkew != clockSkew ||
		summary.Total != summary.Online+summary.Offline+summary.Stale+summary.Unknown {
		t.Fatalf("summary/card state divergence: summary=%+v counts=%+v clock skew=%d", summary, counts, clockSkew)
	}
	return summary
}

func TestAvailabilityPostgresSummaryMatchesCardsAndAgentScope(t *testing.T) {
	s := integrationStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ingestAvailability(t, s, registration("agent-1", now))
	ingestAvailability(t, s, availabilityInventory("online-inventory", "online-router", now, &now, true))
	ingestAvailability(t, s, availabilityEvent("offline-event", "offline-id", "offline-router", "device.offline", "remote", now))
	ingestAvailability(t, s, availabilityTelemetry("expired-metrics", "stale-router", now.Add(-time.Hour)))
	ingestAvailability(t, s, availabilityInventory("unspecified-inventory", "unknown-router", now, nil, false))
	summary := assertAvailabilitySummaryMatchesCards(t, s, "agent-1")
	if summary.Total != 5 || summary.Online != 2 || summary.Offline != 1 || summary.Stale != 1 || summary.Unknown != 1 {
		t.Fatalf("summary did not preserve all four states: %+v", summary)
	}
	ingestAvailability(t, s, registration("agent-2", now))
	other := availabilityTelemetry("other-agent-metrics", "online-router", now)
	other.AgentId = "agent-2"
	ingestAvailability(t, s, other)
	summary = assertAvailabilitySummaryMatchesCards(t, s, "agent-1")
	if summary.Total != 5 {
		t.Fatalf("another Agent leaked into scoped summary: %+v", summary)
	}
	all := assertAvailabilitySummaryMatchesCards(t, s, "")
	if all.Total != 7 || all.Online != 4 {
		t.Fatalf("global summary omitted Agent-scoped devices: %+v", all)
	}
	if err := s.Disconnect(t.Context(), "agent-1", "session-1"); err != nil {
		t.Fatal(err)
	}
	summary = assertAvailabilitySummaryMatchesCards(t, s, "agent-1")
	if summary.Online != 0 || summary.Offline != 0 || summary.Stale != 4 || summary.Unknown != 1 || summary.AgentDisconnected != 5 {
		t.Fatalf("disconnect summary reported devices offline: %+v", summary)
	}
}

func TestAvailabilityPostgresLegacyMigrationPreservesPositiveBusinessObservationTime(t *testing.T) {
	s := integrationStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ingestAvailability(t, s, registration("agent-1", now))
	lastSeen := now.Add(-2 * time.Hour)
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO devices
		(agent_id,device_id,name,source,online,state_at,inventory_at,last_seen,inventory)
		VALUES('agent-1','legacy-router','Legacy router','simulated',true,$1,$1,$2,
		jsonb_build_object('online',true,'source','simulated','last_seen',$2::timestamptz))`, now, lastSeen); err != nil {
		t.Fatal(err)
	}
	// integrationStore gives this test its own PostgreSQL schema. Remove only
	// migration 5's projections to exercise the real upgrade from legacy rows.
	if _, err := s.pool.Exec(t.Context(), `DROP TABLE availability_sources,
		device_availability_current,device_availability_changes,availability_event_ids;
		DELETE FROM schema_migrations WHERE version=5`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE agents SET connected=true,last_seen=now() WHERE agent_id='agent-1'`); err != nil {
		t.Fatal(err)
	}
	d := assertAvailability(t, s, "agent-1", "legacy-router", "stale", availabilityBool(true))
	if d.Availability.EvidenceKind != "legacy" || d.Availability.Reason != "observation_expired" ||
		d.Availability.ObservedAt == nil || !d.Availability.ObservedAt.Equal(lastSeen) {
		t.Fatalf("legacy migration refreshed an old positive observation with inventory batch time: %+v", d.Availability)
	}
	newer := now.Add(-time.Hour)
	ingestAvailability(t, s, availabilityTelemetry("newer-than-legacy-observation", "legacy-router", newer))
	d = assertAvailability(t, s, "agent-1", "legacy-router", "stale", availabilityBool(true))
	if d.Availability.EvidenceKind != "telemetry" || d.Availability.MessageID != "newer-than-legacy-observation" ||
		d.Availability.ObservedAt == nil || !d.Availability.ObservedAt.Equal(newer) {
		t.Fatalf("legacy batch timestamp blocked a newer business observation: %+v", d.Availability)
	}
}

func TestAvailabilityPostgresLaterClockAnomalyUsesLatestReceiptDespiteLowerFutureTime(t *testing.T) {
	s := integrationStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ingestAvailability(t, s, registration("agent-1", now))
	ingestAvailability(t, s, availabilityTelemetry("valid-observation", "remote-router", now))
	ingestAvailability(t, s, availabilityTelemetry("first-clock-anomaly", "remote-router", now.Add(2*time.Hour)))
	d := assertAvailability(t, s, "agent-1", "remote-router", "online", availabilityBool(true))
	if !d.Availability.ClockSkew {
		t.Fatalf("first future observation did not expose the clock issue: %+v", d.Availability)
	}
	corrected := now.Add(time.Second)
	ingestAvailability(t, s, availabilityTelemetry("clock-corrected-again", "remote-router", corrected))
	d = assertAvailability(t, s, "agent-1", "remote-router", "online", availabilityBool(true))
	if d.Availability.ClockSkew {
		t.Fatalf("valid receipt did not clear the earlier clock issue: %+v", d.Availability)
	}
	laterAnomaly := now.Add(time.Hour)
	ingestAvailability(t, s, availabilityTelemetry("latest-clock-anomaly", "remote-router", laterAnomaly))
	d = assertAvailability(t, s, "agent-1", "remote-router", "online", availabilityBool(true))
	if !d.Availability.ClockSkew || d.Availability.MessageID != "clock-corrected-again" ||
		d.Availability.ObservedAt == nil || !d.Availability.ObservedAt.Equal(corrected) {
		t.Fatalf("lower future timestamp hid the latest clock issue or replaced valid evidence: %+v", d.Availability)
	}
	var observed time.Time
	var messageID string
	if err := s.pool.QueryRow(t.Context(), `SELECT observed_at,message_id FROM availability_sources
		WHERE agent_id='agent-1' AND device_id='remote-router' AND kind='telemetry' AND clock_skew`).Scan(&observed, &messageID); err != nil ||
		!observed.Equal(laterAnomaly) || messageID != "latest-clock-anomaly" {
		t.Fatalf("clock issue slot kept an older receipt: observed=%v message=%q err=%v", observed, messageID, err)
	}
	if got := availabilityHistory(t, s, "agent-1", "remote-router"); len(got) != 1 || !got[0].ToOnline {
		t.Fatalf("clock anomalies invented reported availability transitions: %+v", got)
	}
}

func TestAvailabilityPostgresTopologyNodeAndDeviceCardShareAvailability(t *testing.T) {
	s := integrationStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ingestAvailability(t, s, registration("agent-1", now))
	seen := now.Add(-3 * time.Second)
	inventory := availabilityInventory("map-remote-inventory", "remote-switch", seen, &seen, true)
	inventory.GetInventory().Devices[0].Hostname = "switch"
	inventory.GetInventory().Devices[0].ChassisId = "00:01:02:03:04:05"
	ingestAvailability(t, s, inventory)
	ingestAvailability(t, s, topologyEnvelope("map-known-remote-link", now))
	checkAgreement := func(status string, reported bool) {
		t.Helper()
		card := assertAvailability(t, s, "agent-1", "remote-switch", status, availabilityBool(reported))
		graph, err := s.Topology(t.Context(), "agent-1", "shared-router", false, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(graph.Edges) != 1 || graph.Edges[0].Resolution.Method != "chassis_mac" {
			t.Fatalf("known remote inventory did not resolve the topology link: %+v", graph)
		}
		for _, node := range graph.Nodes {
			if node.AgentID != "agent-1" || node.DeviceID != "remote-switch" {
				continue
			}
			if !node.Known || node.Availability == nil || node.Availability.Status != card.Availability.Status ||
				node.Availability.Reason != card.Availability.Reason || node.Availability.EvidenceKind != card.Availability.EvidenceKind ||
				node.Availability.EventID != card.Availability.EventID || node.Availability.AgentConnected != card.Availability.AgentConnected ||
				node.ReportedOnline == nil || *node.ReportedOnline != *card.ReportedOnline ||
				node.Availability.LastReportedOnline == nil || *node.Availability.LastReportedOnline != *card.Availability.LastReportedOnline ||
				node.AgentConnected == nil || *node.AgentConnected != card.AgentConnected {
				t.Fatalf("map/card availability diverged after %s: node=%+v card=%+v", status, node, card)
			}
			return
		}
		t.Fatalf("known remote node missing from the scoped map: %+v", graph.Nodes)
	}
	checkAgreement("online", true)
	ingestAvailability(t, s, availabilityEvent("map-offline-event", "map-offline-id", "remote-switch", "device.offline", "remote", now.Add(-2*time.Second)))
	checkAgreement("offline", false)
	ingestAvailability(t, s, availabilityEvent("map-recovery-event", "map-recovery-id", "remote-switch", "device.online", "remote", now.Add(-time.Second)))
	checkAgreement("online", true)
	if err := s.Disconnect(t.Context(), "agent-1", "session-1"); err != nil {
		t.Fatal(err)
	}
	checkAgreement("stale", true)
}
