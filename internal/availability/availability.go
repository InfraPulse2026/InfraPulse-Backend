// Package availability derives device availability from explicit observations.
// Missing reports and agent disconnection make evidence stale; neither implies
// that the device reported itself offline.
package availability

import (
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
)

type Observation struct {
	Kind, Source, MessageID, EventID, TimeBasis string
	Online                                      bool
	ObservedAt, ReceivedAt                      time.Time
}

type Input struct {
	Latest         *Observation
	ClockIssue     *Observation
	AgentConnected bool
	Policy         monitoring.Policy
	AgentTimeout   time.Duration
}

type State struct {
	Status                  string     `json:"status"`
	Reason                  string     `json:"reason"`
	LastReportedOnline      *bool      `json:"last_reported_online"`
	AgentConnected          bool       `json:"agent_connected"`
	EvidenceKind            string     `json:"evidence_kind,omitempty"`
	Source                  string     `json:"source,omitempty"`
	MessageID               string     `json:"message_id,omitempty"`
	EventID                 string     `json:"event_id,omitempty"`
	TimeBasis               string     `json:"time_basis,omitempty"`
	ObservedAt              *time.Time `json:"observed_at"`
	ReceivedAt              *time.Time `json:"received_at"`
	ExpiresAt               *time.Time `json:"expires_at"`
	AgeSeconds              *int64     `json:"age_seconds"`
	ExpectedIntervalSeconds int        `json:"expected_interval_seconds"`
	StaleAfterSeconds       int        `json:"stale_after_seconds"`
	ClockSkew               bool       `json:"clock_skew"`
}

type Change struct {
	ID         int64     `json:"id"`
	AgentID    string    `json:"agent_id"`
	DeviceID   string    `json:"device_id"`
	Kind       string    `json:"kind"`
	Source     string    `json:"source"`
	MessageID  string    `json:"message_id"`
	EventID    string    `json:"event_id"`
	TimeBasis  string    `json:"time_basis"`
	FromOnline *bool     `json:"from_online"`
	ToOnline   bool      `json:"to_online"`
	ObservedAt time.Time `json:"observed_at"`
	ReceivedAt time.Time `json:"received_at"`
}

type Summary struct {
	GeneratedAt       time.Time `json:"generated_at"`
	Total             int64     `json:"total"`
	Online            int64     `json:"online"`
	Offline           int64     `json:"offline"`
	Stale             int64     `json:"stale"`
	Unknown           int64     `json:"unknown"`
	ClockSkew         int64     `json:"clock_skew"`
	AgentDisconnected int64     `json:"agent_disconnected"`
}

// Evaluate uses the configured cadence for the observation that actually won
// selection. Policy is expected to have passed monitoring.Policy.Validate.
func Evaluate(now time.Time, in Input) State {
	s := State{Status: "unknown", Reason: "no_observation", AgentConnected: in.AgentConnected}
	if in.Latest == nil {
		if in.ClockIssue != nil {
			s.Reason = "clock_skew"
			s.ClockSkew = true
			setProvenance(&s, in.ClockIssue)
		}
		return s
	}

	o := in.Latest
	online := o.Online
	s.LastReportedOnline = &online
	setProvenance(&s, o)
	s.ExpectedIntervalSeconds, s.StaleAfterSeconds = freshness(o.Kind, in)
	expires := expiry(o.ObservedAt, s.StaleAfterSeconds)
	s.ExpiresAt = &expires
	s.ClockSkew = in.ClockIssue != nil && !in.ClockIssue.ReceivedAt.Before(o.ReceivedAt)

	// Compare instants before computing age: time.Sub saturates over centuries
	// and would give misleading ages for valid, very old persisted observations.
	if o.ObservedAt.After(now.Add(30 * time.Second)) {
		s.Status, s.Reason, s.ClockSkew = "stale", "clock_skew", true
		return s
	}
	age := max(int64(0), now.Unix()-o.ObservedAt.Unix())
	s.AgeSeconds = &age
	s.Status = "stale"
	switch {
	case !in.AgentConnected:
		s.Reason = "agent_disconnected"
	case !now.Before(expires):
		s.Reason = "observation_expired"
	case o.Online:
		s.Status, s.Reason = "online", "device_reported_online"
		if o.Kind == "heartbeat" {
			s.Reason = "agent_heartbeat"
		}
	default:
		s.Status, s.Reason = "offline", "device_reported_offline"
	}
	return s
}

func setProvenance(s *State, o *Observation) {
	s.EvidenceKind, s.Source = o.Kind, o.Source
	s.MessageID, s.EventID, s.TimeBasis = o.MessageID, o.EventID, o.TimeBasis
	observed, received := o.ObservedAt.UTC(), o.ReceivedAt.UTC()
	s.ObservedAt, s.ReceivedAt = &observed, &received
}

func freshness(kind string, in Input) (interval, staleAfter int) {
	switch kind {
	case "hello", "heartbeat":
		// Duration arithmetic preserves the exact ceiling, including fractional
		// seconds and durations too large to represent safely as float64 nanos.
		seconds := in.AgentTimeout / time.Second
		if in.AgentTimeout%time.Second > 0 {
			seconds++
		}
		return 0, max(0, int(seconds))
	case "telemetry":
		interval = in.Policy.TelemetryIntervalSeconds
	case "inventory":
		interval = in.Policy.InventoryIntervalSeconds
	default:
		interval = max(in.Policy.TelemetryIntervalSeconds, in.Policy.InventoryIntervalSeconds)
	}
	return interval, monitoring.StaleAfterSeconds(interval)
}

func expiry(observed time.Time, seconds int) time.Time {
	observed = observed.UTC()
	// JSON time encoding only supports years 0 through 9999. Clamp at its last
	// instant rather than producing an unencodable time near the upper bound.
	last := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	if observed.After(last) || int64(seconds) > last.Unix()-observed.Unix() {
		return last
	}
	return time.Unix(observed.Unix()+int64(seconds), int64(observed.Nanosecond())).UTC()
}

// Priority breaks ties at the same observation timestamp. Negative inventory
// and events outrank positive reports; positive telemetry outranks an online
// event so its faster cadence remains attached to the selected evidence.
func Priority(kind string, online bool) int {
	switch kind {
	case "heartbeat":
		return 50
	case "event_online", "event_offline":
		if !online {
			return 40
		}
		return 10
	case "inventory":
		if !online {
			return 30
		}
	case "legacy":
		if !online {
			return 25
		}
	case "telemetry":
		return 20
	}
	return 0
}
