package reports

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

var ErrInvalid = errors.New("invalid report period or filter")
var ErrConflict = errors.New("report idempotency key belongs to different request")
var ErrNotFound = errors.New("report snapshot not found")

type Filter struct {
	From         time.Time `json:"from"`
	To           time.Time `json:"to"`
	AgentID      string    `json:"agent_id"`
	Source       string    `json:"source"`
	ResourceKind string    `json:"resource_kind"`
	TopLimit     int       `json:"top_limit"`
}

func (f Filter) Normalize(now time.Time) (Filter, error) {
	f.From = f.From.UTC().Truncate(time.Microsecond)
	f.To = f.To.UTC().Truncate(time.Microsecond)
	if f.TopLimit == 0 {
		f.TopLimit = 20
	}
	if f.From.IsZero() || f.To.IsZero() || f.To.Sub(f.From) < time.Second || f.To.Sub(f.From) > 31*24*time.Hour || f.To.After(now) || f.TopLimit < 1 || f.TopLimit > 100 || len(f.AgentID) > 256 || len(f.Source) > 128 {
		return f, ErrInvalid
	}
	if f.ResourceKind != "" && f.ResourceKind != "device" && f.ResourceKind != "interface" && f.ResourceKind != "sip" && f.ResourceKind != "agent" && f.ResourceKind != "external" {
		return f, ErrInvalid
	}
	return f, nil
}
func (f Filter) Fingerprint() string {
	raw, _ := json.Marshal(f)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

type Stats struct {
	Overlapping           int64    `json:"overlapping_incidents"`
	Opened                int64    `json:"opened_in_period"`
	CarriedIn             int64    `json:"carried_in"`
	Resolved              int64    `json:"resolved_in_period"`
	ActiveAtEnd           int64    `json:"active_at_end"`
	Acknowledged          int64    `json:"acknowledged_in_period"`
	SummedSeconds         float64  `json:"summed_incident_seconds"`
	MeanAckSeconds        *float64 `json:"mean_acknowledgment_delay_seconds"`
	MeanResolutionSeconds *float64 `json:"mean_resolution_delay_seconds"`
	RealEpisodes          int64    `json:"real_source_episodes"`
	SimulatedEpisodes     int64    `json:"simulated_source_episodes"`
}
type RuleGroup struct {
	RuleID    string  `json:"rule_id"`
	Source    string  `json:"source"`
	Simulated bool    `json:"simulated"`
	Count     int64   `json:"overlapping_incidents"`
	Opened    int64   `json:"opened_in_period"`
	Seconds   float64 `json:"summed_incident_seconds"`
}
type ResourceGroup struct {
	AgentID         string  `json:"agent_id"`
	DeviceID        string  `json:"device_id"`
	ResourceKind    string  `json:"resource_kind"`
	ResourceID      string  `json:"resource_id"`
	ResourceContext string  `json:"resource_context"`
	Source          string  `json:"source"`
	Scope           string  `json:"scope"`
	Count           int64   `json:"overlapping_incidents"`
	Seconds         float64 `json:"summed_incident_seconds"`
}
type Recurrence struct {
	ResourceGroup
	RuleID            string    `json:"rule_id"`
	OpenedOccurrences int64     `json:"opened_occurrences"`
	FirstOpened       time.Time `json:"first_opened_at"`
	LastOpened        time.Time `json:"last_opened_at"`
}
type Quality struct {
	FilterScope         string   `json:"filter_scope"`
	TelemetryReceived   int64    `json:"telemetry_samples_received"`
	MissingCPU          int64    `json:"received_samples_without_cpu"`
	FutureClock         int64    `json:"received_samples_with_future_clock"`
	LateSamples         int64    `json:"received_samples_more_than_5m_late"`
	SIPReceived         int64    `json:"sip_samples_received"`
	SIPMissingLatency   int64    `json:"received_sip_samples_without_latency"`
	AvailabilityPercent *float64 `json:"availability_percent"`
	CoverageStatus      string   `json:"coverage_status"`
}
type Summary struct {
	RuleGroupsTruncated bool            `json:"rule_groups_truncated"`
	Version             string          `json:"version"`
	GeneratedAt         time.Time       `json:"generated_at"`
	Filter              Filter          `json:"filter"`
	Stats               Stats           `json:"stats"`
	ByRule              []RuleGroup     `json:"by_rule"`
	TopResources        []ResourceGroup `json:"top_resources"`
	Recurring           []Recurrence    `json:"recurring"`
	Quality             Quality         `json:"quality"`
	Methodology         []string        `json:"methodology"`
}

func Methodology() []string {
	return []string{
		"period is [from,to); report reads one repeatable-read database snapshot",
		"durations use backend incident detection/resolution time, clipped to the requested period",
		"external incidents use reported source problem/recovery times; import may occur later, and these intervals are not measured downtime",
		"summed incident seconds may overlap across rules/resources and are not downtime or availability",
		"active_at_end includes incidents resolved exactly at to; those resolutions belong to the next period",
		"acknowledgment/resolution means use full delay since detection for events inside the requested period",
		"recurrence requires at least two occurrences opened inside the period for the same resource/rule/source/scope/context",
		"simulated sources/sessions remain explicit; absence of incidents does not prove a healthy or observed network",
		"quality counts use received_at; raw missing values may represent unsupported metrics, not collector failures",
		"report contents are detection evidence and do not establish a network root cause",
	}
}

type Snapshot struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
	Filter    Filter    `json:"filter"`
	Report    *Summary  `json:"report,omitempty"`
}
