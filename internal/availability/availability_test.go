package availability

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
)

func inputAt(observed time.Time, kind string, online bool) Input {
	return Input{
		Latest:         &Observation{Kind: kind, Source: "snmp", MessageID: "message-1", EventID: "event-1", TimeBasis: "agent_observed_at", Online: online, ObservedAt: observed, ReceivedAt: observed.Add(time.Second)},
		AgentConnected: true,
		Policy:         monitoring.Defaults(),
		AgentTimeout:   30 * time.Second,
	}
}

func TestNoObservationRemainsUnknown(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, connected := range []bool{true, false} {
		s := Evaluate(now, Input{AgentConnected: connected, Policy: monitoring.Defaults()})
		if s.Status != "unknown" || s.Reason != "no_observation" || s.LastReportedOnline != nil || s.AgentConnected != connected {
			t.Fatalf("no report must not imply offline: %+v", s)
		}
		if s.ObservedAt != nil || s.ReceivedAt != nil || s.ExpiresAt != nil || s.AgeSeconds != nil || s.ClockSkew {
			t.Fatalf("invented evidence: %+v", s)
		}
	}
	in := inputAt(now.Add(time.Minute), "inventory", true)
	in.ClockIssue, in.Latest = in.Latest, nil
	s := Evaluate(now, in)
	if s.Status != "unknown" || s.Reason != "clock_skew" || !s.ClockSkew || s.LastReportedOnline != nil || s.MessageID != "message-1" || s.ExpiresAt != nil {
		t.Fatalf("rejected clock evidence became a report: %+v", s)
	}
}

func TestExplicitReportsAndExpiryBoundary(t *testing.T) {
	observed := time.Date(2026, 10, 7, 12, 0, 0, 123456789, time.FixedZone("agent", 3*3600))
	for _, online := range []bool{true, false} {
		in := inputAt(observed, "telemetry", online)
		fresh := Evaluate(observed.Add(11*time.Second-time.Nanosecond), in)
		wantStatus, wantReason := "offline", "device_reported_offline"
		if online {
			wantStatus, wantReason = "online", "device_reported_online"
		}
		if fresh.Status != wantStatus || fresh.Reason != wantReason || fresh.LastReportedOnline == nil || *fresh.LastReportedOnline != online {
			t.Fatalf("explicit report lost: %+v", fresh)
		}
		if fresh.ExpectedIntervalSeconds != 3 || fresh.StaleAfterSeconds != 11 || fresh.ExpiresAt == nil || !fresh.ExpiresAt.Equal(observed.Add(11*time.Second)) {
			t.Fatalf("wrong telemetry deadline: %+v", fresh)
		}
		if fresh.ObservedAt.Location() != time.UTC || fresh.ReceivedAt.Location() != time.UTC {
			t.Fatalf("timestamps are not UTC: %+v", fresh)
		}
		stale := Evaluate(observed.Add(11*time.Second), in)
		if stale.Status != "stale" || stale.Reason != "observation_expired" || stale.LastReportedOnline == nil || *stale.LastReportedOnline != online {
			t.Fatalf("expiry must preserve the last report: %+v", stale)
		}
		if stale.EvidenceKind != "telemetry" || stale.Source != "snmp" || stale.MessageID != "message-1" || stale.EventID != "event-1" || stale.TimeBasis != "agent_observed_at" {
			t.Fatalf("expiry lost provenance: %+v", stale)
		}
		if in.Latest.ObservedAt.Location() == time.UTC || in.Latest.Online != online {
			t.Fatal("evaluation mutated its input")
		}
	}
}

func TestCadenceFollowsEvidenceAndPolicy(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		kind     string
		interval int
		deadline int
	}{
		{"telemetry", 3, 11},
		{"inventory", 300, 630},
		{"event_online", 300, 630},
		{"event_offline", 300, 630},
		{"legacy", 300, 630},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			in := inputAt(now, tc.kind, true)
			s := Evaluate(now.Add(12*time.Second), in)
			if s.ExpectedIntervalSeconds != tc.interval || s.StaleAfterSeconds != tc.deadline {
				t.Fatalf("wrong cadence: %+v", s)
			}
			in.Policy.TelemetryIntervalSeconds = 36000
			in.Policy.InventoryIntervalSeconds = 86400
			s = Evaluate(now.Add(25*time.Hour), in)
			if tc.kind == "telemetry" {
				if s.Status != "stale" || s.StaleAfterSeconds != monitoring.StaleAfterSeconds(36000) {
					t.Fatalf("telemetry policy was ignored: %+v", s)
				}
			} else if s.Status != "online" || s.StaleAfterSeconds != monitoring.StaleAfterSeconds(86400) {
				t.Fatalf("daily reports expired early: %+v", s)
			}
		})
	}
	// Event cadence must use the slower interval even when telemetry is slower.
	in := inputAt(now, "event_offline", false)
	in.Policy.TelemetryIntervalSeconds, in.Policy.InventoryIntervalSeconds = 60, 3
	s := Evaluate(now.Add(time.Minute), in)
	if s.ExpectedIntervalSeconds != 60 || s.Status != "offline" {
		t.Fatalf("event freshness only used inventory: %+v", s)
	}
}

func TestAgentLivenessUsesExactTimeoutCeiling(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, kind := range []string{"hello", "heartbeat"} {
		for _, timeout := range []time.Duration{time.Nanosecond, time.Second, 25*time.Second + time.Nanosecond, 30 * time.Second} {
			in := inputAt(now, kind, true)
			in.AgentTimeout = timeout
			seconds := int(timeout / time.Second)
			if timeout%time.Second != 0 {
				seconds++
			}
			deadline := now.Add(time.Duration(seconds) * time.Second)
			s := Evaluate(deadline.Add(-time.Nanosecond), in)
			if s.Status != "online" || s.StaleAfterSeconds != seconds || s.ExpectedIntervalSeconds != 0 || !s.ExpiresAt.Equal(deadline) {
				t.Fatalf("kind %s timeout %s got %+v", kind, timeout, s)
			}
			if kind == "heartbeat" && s.Reason != "agent_heartbeat" {
				t.Fatalf("heartbeat reason: %+v", s)
			}
			s = Evaluate(deadline, in)
			if s.Status != "stale" || s.Reason != "observation_expired" {
				t.Fatalf("kind %s deadline must expire exactly: %+v", kind, s)
			}
		}
	}
}

func TestDisconnectionAndClockSkewAreSeparateFromOffline(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	in := inputAt(now, "inventory", false)
	in.AgentConnected = false
	s := Evaluate(now, in)
	if s.Status != "stale" || s.Reason != "agent_disconnected" || s.LastReportedOnline == nil || *s.LastReportedOnline {
		t.Fatalf("disconnection obscured the report: %+v", s)
	}
	in.Latest.ObservedAt = now.Add(30*time.Second + time.Nanosecond)
	s = Evaluate(now, in)
	if s.Status != "stale" || s.Reason != "clock_skew" || !s.ClockSkew || s.AgeSeconds != nil {
		t.Fatalf("future timestamps need clock-skew precedence: %+v", s)
	}
	in.AgentConnected = true
	in.Latest.ObservedAt = now.Add(30 * time.Second)
	s = Evaluate(now, in)
	if s.Status != "offline" || s.ClockSkew || s.AgeSeconds == nil || *s.AgeSeconds != 0 {
		t.Fatalf("30-second tolerance rejected: %+v", s)
	}
	in.Latest.ObservedAt = now
	for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Second} {
		in.ClockIssue = &Observation{ReceivedAt: in.Latest.ReceivedAt.Add(offset)}
		s = Evaluate(now, in)
		if s.Status != "offline" || s.ClockSkew != (offset >= 0) || s.MessageID != "message-1" {
			t.Fatalf("clock issue offset %s changed valid evidence: %+v", offset, s)
		}
	}
}

func TestAncientAgesAndUpperExpiryRemainRepresentable(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	ancient := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	s := Evaluate(now, inputAt(ancient, "inventory", true))
	if s.Status != "stale" || s.AgeSeconds == nil || *s.AgeSeconds != now.Unix()-ancient.Unix() {
		t.Fatalf("age saturated at time.Duration's limit: %+v", s)
	}
	last := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	nearLast := last.Add(-time.Second)
	s = Evaluate(nearLast, inputAt(nearLast, "inventory", true))
	if s.Status != "online" || s.ExpiresAt == nil || !s.ExpiresAt.Equal(last) {
		t.Fatalf("expiry crossed JSON's year limit: %+v", s)
	}
	if _, err := json.Marshal(s); err != nil {
		t.Fatalf("state is not JSON representable: %v", err)
	}
}

func TestPriorityPreservesNegativeEvidenceAndFastPositiveCadence(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		online bool
		want   int
	}{
		{"heartbeat", true, 50},
		{"event_offline", false, 40},
		{"inventory", false, 30},
		{"legacy", false, 25},
		{"telemetry", true, 20},
		{"event_online", true, 10},
		{"inventory", true, 0},
		{"hello", true, 0},
		{"legacy", true, 0},
	} {
		if got := Priority(tc.kind, tc.online); got != tc.want {
			t.Errorf("Priority(%q, %t) = %d, want %d", tc.kind, tc.online, got, tc.want)
		}
	}
	if Priority("inventory", false) <= Priority("telemetry", true) || Priority("telemetry", true) <= Priority("event_online", true) {
		t.Fatal("ties can erase offline evidence or stretch telemetry freshness")
	}
}

func TestPublicJSONKeepsUnknownValuesNull(t *testing.T) {
	data, err := json.Marshal(State{Status: "unknown", Reason: "no_observation"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"last_reported_online", "observed_at", "received_at", "expires_at", "age_seconds"} {
		if string(fields[key]) != "null" {
			t.Fatalf("unknown %s must be explicit null: %s", key, data)
		}
	}
	if _, exists := fields["source"]; exists {
		t.Fatalf("empty provenance must be omitted: %s", data)
	}
	for _, value := range []any{Change{}, Summary{}} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		fields = nil
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		for key := range fields {
			for _, char := range key {
				if char >= 'A' && char <= 'Z' {
					t.Fatalf("public key is not snake_case: %s", key)
				}
			}
		}
	}
}
