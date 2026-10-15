package health

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"strings"
	"time"
)

type LogEvidence struct {
	PatternID    string     `json:"pattern_id,omitempty"`
	AuditSuccess *bool      `json:"audit_success,omitempty"`
	ProcessHash  string     `json:"process_hash"`
	RecordID     int64      `json:"record_id"`
	BindingID    string     `json:"binding_id"`
	SourceIDHash string     `json:"source_id_hash"`
	Process      string     `json:"process"`
	Severity     *uint32    `json:"severity"`
	EventAt      time.Time  `json:"event_at"`
	ReceivedAt   time.Time  `json:"received_at"`
	WindowStart  time.Time  `json:"window_start"`
	EventCount   int64      `json:"event_count"`
	TimeQuality  string     `json:"time_quality"`
	Lifecycle    string     `json:"lifecycle"`
	ReviewedAt   *time.Time `json:"reviewed_at,omitempty"`
	ReviewedBy   string     `json:"reviewed_by,omitempty"`
	ReviewReason string     `json:"review_reason,omitempty"`
}

type LogReview struct {
	Confirmed          bool   `json:"confirmed"`
	ExpectedEventCount int64  `json:"expected_event_count"`
	Reason             string `json:"reason"`
}

func (r LogReview) Validate() error {
	if !r.Confirmed || r.ExpectedEventCount < 1 || len(strings.TrimSpace(r.Reason)) < 5 || len(r.Reason) > 512 || strings.ContainsRune(r.Reason, 0) {
		return errors.New("confirmed, positive expected_event_count and reason of 5..512 bytes required")
	}
	return nil
}

// A severe log is an immutable reported event, never proof of device failure.
// Old original timestamps cannot be refreshed by a later file reread.
func LogEventSeverity(now time.Time, r logbook.Record) (string, string) {
	if LogEventRule(r) == "" {
		return "", ""
	}
	for _, at := range []time.Time{r.EventAt, r.ObservedAt, r.ReceivedAt} {
		if at.IsZero() || at.Before(time.Unix(0, 0)) || at.After(now.Add(30*time.Second)) {
			return "", ""
		}
	}
	if r.ReceivedAt.Sub(r.EventAt) > 300*time.Second || r.ReceivedAt.Sub(r.ObservedAt) > 300*time.Second || r.EventAt.After(r.ReceivedAt.Add(30*time.Second)) || r.ObservedAt.After(r.ReceivedAt.Add(30*time.Second)) {
		return "", ""
	}
	quality := "estimated"
	if len(r.Observation) > 0 {
		var meta struct {
			Time struct {
				ClockQuality    string     `json:"clock_quality"`
				SourceTimestamp *time.Time `json:"source_timestamp"`
			} `json:"time"`
		}
		if json.Unmarshal(r.Observation, &meta) != nil {
			return "", ""
		}
		if meta.Time.ClockQuality == "degraded" || meta.Time.ClockQuality == "unsynced" {
			return "", ""
		}
		if at := meta.Time.SourceTimestamp; at != nil && (at.After(now.Add(30*time.Second)) || r.ReceivedAt.Sub(*at) > 300*time.Second || at.After(r.ReceivedAt.Add(30*time.Second))) {
			return "", ""
		}
		if meta.Time.ClockQuality == "synced" && r.TimeBasis == "observed_at" {
			quality = "exact"
		}
	}
	if r.Severity != nil && *r.Severity <= 2 || LogEventRule(r) == "log.kernel_oom_kill" {
		return "critical", quality
	}
	return "warning", quality
}
func LogWindowKey(binding, scope string, r logbook.Record) (string, time.Time) {
	window := r.EventAt.UTC().Truncate(5 * time.Minute)
	parts := []string{binding, scope, r.Source, r.SourceID, r.Process, window.Format(time.RFC3339)}
	if rule := LogEventRule(r); rule != "" && rule != "log.reported_severity" {
		parts = append(parts, rule)
	}
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), window
}

type LogIncidentRecord struct {
	LogID     int64     `json:"log_id"`
	BindingID string    `json:"binding_id"`
	EventAt   time.Time `json:"event_at"`
	Severity  *uint32   `json:"severity"`
}

func LogIdentityHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Explicit severe logs take precedence: one record produces at most one signal.
// A failed audit operation is an event requiring review, not proof of intrusion.
func LogEventRule(r logbook.Record) string {
	if r.Severity != nil && *r.Severity <= 3 {
		return "log.reported_severity"
	}
	if r.Audit != nil && r.Audit.Success != nil && !*r.Audit.Success {
		return "audit.reported_failure"
	}
	return LogPatternRule(r)
}
