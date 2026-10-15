package health

import (
	"encoding/json"
	"errors"
	"time"
)

var ErrConflict = errors.New("incident is already resolved")

type Incident struct {
	SourceInstanceID *string    `json:"source_instance_id,omitempty"`
	ExternalEventID  *string    `json:"external_event_id,omitempty"`
	ID               string     `json:"id"`
	AgentID          string     `json:"agent_id"`
	DeviceID         string     `json:"device_id"`
	ResourceKind     string     `json:"resource_kind"`
	ResourceID       string     `json:"resource_id"`
	ResourceContext  string     `json:"resource_context"`
	Source           string     `json:"source"`
	Scope            string     `json:"scope"`
	RuleID           string     `json:"rule_id"`
	Occurrence       int        `json:"occurrence"`
	State            string     `json:"state"`
	Severity         string     `json:"severity"`
	OpenedAt         time.Time  `json:"opened_at"`
	LastObservedAt   time.Time  `json:"last_observed_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	AcknowledgedAt   *time.Time `json:"acknowledged_at"`
	AcknowledgedBy   *string    `json:"acknowledged_by"`
	ResolvedAt       *time.Time `json:"resolved_at"`
	Signal           Assessment `json:"signal"`
	EvidenceStatus   string     `json:"evidence_status"`
}
type Transition struct {
	ID      int64     `json:"id"`
	State   string    `json:"state"`
	Reason  string    `json:"reason"`
	At      time.Time `json:"at"`
	ActorID *string   `json:"actor_id"`
}
type Evidence struct {
	ID         int64           `json:"id"`
	Kind       string          `json:"kind"`
	ObservedAt time.Time       `json:"observed_at"`
	At         time.Time       `json:"at"`
	Assessment json.RawMessage `json:"assessment"`
}
type Observation struct {
	ResourceKind    string     `json:"resource_kind"`
	ResourceID      string     `json:"resource_id"`
	ResourceContext string     `json:"resource_context"`
	Current         bool       `json:"current"`
	AgentID         string     `json:"agent_id"`
	DeviceID        string     `json:"device_id"`
	Source          string     `json:"source"`
	Scope           string     `json:"scope"`
	Assessment      Assessment `json:"assessment"`
	EvaluatedAt     time.Time  `json:"evaluated_at"`
}
type Filter struct {
	AgentID, DeviceID, State, RuleID, Source string
	ResourceKind, ResourceID                 string
	Limit, Offset                            int
}
