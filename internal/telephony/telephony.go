// Package telephony describes received SIP OPTIONS observations. It never probes endpoints.
package telephony

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
)

const DefaultIntervalSeconds = 30
const MaxTargets = 256 // Current Agent's transport mapping limit.

func Validate(u *pb.TelephonyUpdate) error {
	if u == nil || len(u.Source) > 128 || len(u.Targets) > MaxTargets || u.ObservedAt != nil && u.ObservedAt.CheckValid() != nil {
		return errors.New("invalid telephony update")
	}
	// Earlier backends acknowledged empty batches. Preserve that behavior without
	// inventing a target or a probe; real Agent batches require source/time below.
	if len(u.Targets) == 0 {
		return nil
	}
	if u.ObservedAt == nil || u.Source == "" {
		return errors.New("telephony batch requires source and observation time")
	}
	seen := map[string]bool{}
	for _, h := range u.Targets {
		if h == nil || h.TargetId == "" || h.Address == "" || h.Source == "" || len(h.TargetId) > 1024 || len(h.Address) > 1024 || len(h.Source) > 1024 || len(h.ErrorCategory) > 1024 || h.ObservedAt == nil || h.ObservedAt.CheckValid() != nil {
			return errors.New("invalid telephony target identity or timestamp")
		}
		if seen[h.TargetId] {
			return errors.New("duplicate target ID in telephony update")
		}
		seen[h.TargetId] = true
		if h.Transport != "udp" && h.Transport != "tcp" && h.Transport != "tls" {
			return errors.New("invalid SIP transport")
		}
		switch h.State {
		case "unknown", "up", "healthy", "degraded", "down":
		default:
			return errors.New("unsupported SIP state")
		}
		if h.ResponseCode != nil && (*h.ResponseCode < 100 || *h.ResponseCode > 699 || !h.Reachable) {
			return errors.New("invalid SIP response code or reachability")
		}
		if h.LatencyMs != nil && (math.IsNaN(*h.LatencyMs) || math.IsInf(*h.LatencyMs, 0) || *h.LatencyMs < 0) {
			return errors.New("SIP latency must be finite and nonnegative")
		}
	}
	return nil
}

type Sample struct {
	ID                  int64     `json:"id"`
	AgentID             string    `json:"agent_id"`
	TargetID            string    `json:"target_id"`
	MessageID           string    `json:"message_id"`
	Address             string    `json:"address"`
	Transport           string    `json:"transport"`
	Source              string    `json:"source"`
	Reachable           bool      `json:"reachable"`
	ResponseCode        *uint32   `json:"response_code"`
	LatencyMS           *float64  `json:"latency_ms"`
	ObservedAt          time.Time `json:"observed_at"`
	ReceivedAt          time.Time `json:"received_at"`
	ConsecutiveFailures uint32    `json:"consecutive_failures"`
	State               string    `json:"state"`
	ErrorCategory       string    `json:"error_category"`
	ClockSkew           bool      `json:"clock_skew"`
}
type Monitoring struct {
	ExpectedIntervalSeconds int    `json:"expected_interval_seconds"`
	StaleAfterSeconds       int    `json:"stale_after_seconds"`
	Origin                  string `json:"origin"`
	DeliveryMode            string `json:"delivery_mode"`
	ApplicationStatus       string `json:"application_status"`
	AgentYAMLFragment       string `json:"agent_yaml_fragment"`
}

func ValidateInterval(seconds int) error {
	if seconds < monitoring.MinIntervalSeconds || seconds > monitoring.MaxIntervalSeconds {
		return errors.New("expected_interval_seconds must be an integer between 3 and 86400")
	}
	return nil
}
func DescribeInterval(target string, seconds int, origin string) Monitoring {
	if origin == "collector_applied" {
		return Monitoring{seconds, monitoring.StaleAfterSeconds(seconds), origin, "managed_collector_stream", "applied", ""}
	}
	return Monitoring{seconds, monitoring.StaleAfterSeconds(seconds), origin, "manual_agent_yaml", "unverified", fmt.Sprintf("telephony:\n  endpoints:\n    - id: %s\n      interval: %ds\n", strconv.Quote(target), seconds)}
}

type Health struct {
	Status            string               `json:"status"`
	Reason            string               `json:"reason"`
	AgentConnected    bool                 `json:"agent_connected"`
	ReportedState     *string              `json:"reported_state"`
	ReportedReachable *bool                `json:"reported_reachable"`
	ClockSkew         bool                 `json:"clock_skew"`
	Freshness         monitoring.Freshness `json:"freshness"`
}

func Evaluate(now time.Time, latest, issue *Sample, connected bool, interval int) Health {
	h := Health{Status: "unknown", Reason: "no_qualified_observation", AgentConnected: connected, Freshness: monitoring.Evaluate(now, nil, nil, interval)}
	h.ClockSkew = issue != nil && (latest == nil || !issue.ReceivedAt.Before(latest.ReceivedAt))
	if latest == nil {
		if h.ClockSkew {
			h.Reason = "clock_skew"
		}
		return h
	}
	state, reachable := latest.State, latest.Reachable
	h.ReportedState, h.ReportedReachable = &state, &reachable
	h.Freshness = monitoring.Evaluate(now, &latest.ObservedAt, &latest.ReceivedAt, interval)
	if h.Freshness.Status == "clock_skew" {
		h.ClockSkew = true
		h.Status, h.Reason = "unknown", "clock_skew"
		return h
	}
	if h.Freshness.AgeSeconds != nil {
		seconds := max(int64(0), now.Unix()-latest.ObservedAt.Unix())
		h.Freshness.AgeSeconds = &seconds
	}
	if !connected {
		h.Status, h.Reason = "stale", "agent_disconnected"
		return h
	}
	if h.Freshness.Status != "fresh" {
		h.Status, h.Reason = "stale", "observation_expired"
		return h
	}
	switch state {
	case "up", "healthy":
		if !reachable {
			h.Reason = "inconsistent_report"
			return h
		}
		h.Status, h.Reason = "up", "agent_reported_up"
	case "degraded":
		h.Status, h.Reason = "degraded", "agent_reported_degraded"
	case "down":
		if reachable {
			h.Reason = "inconsistent_report"
			return h
		}
		h.Status, h.Reason = "down", "agent_reported_down"
	default:
		h.Reason = "agent_reported_unknown"
	}
	return h
}

type Target struct {
	AgentID         string     `json:"agent_id"`
	TargetID        string     `json:"target_id"`
	Address         string     `json:"address"`
	Transport       string     `json:"transport"`
	Source          string     `json:"source"`
	FirstReceivedAt time.Time  `json:"first_received_at"`
	LastReceivedAt  time.Time  `json:"last_received_at"`
	Latest          *Sample    `json:"latest"`
	ClockIssue      *Sample    `json:"clock_issue"`
	Health          Health     `json:"health"`
	Monitoring      Monitoring `json:"monitoring"`
}
type Filter struct {
	AgentID, TargetID, Transport, Source string
	Limit, Offset                        int
}
type HistoryFilter struct {
	AgentID, TargetID string
	From, To          *time.Time
	BeforeID          int64
	Limit             int
}
type HistoryPage struct {
	Items        []Sample `json:"items"`
	NextBeforeID *int64   `json:"next_before_id"`
	Limit        int      `json:"limit"`
}
type Summary struct {
	GeneratedAt       time.Time `json:"generated_at"`
	Total             int64     `json:"total"`
	Up                int64     `json:"up"`
	Degraded          int64     `json:"degraded"`
	Down              int64     `json:"down"`
	Stale             int64     `json:"stale"`
	Unknown           int64     `json:"unknown"`
	ClockSkew         int64     `json:"clock_skew"`
	AgentDisconnected int64     `json:"agent_disconnected"`
}
