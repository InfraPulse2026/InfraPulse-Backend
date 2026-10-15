// Package correlation contains the deterministic, explainable B12 scoring
// model. It groups already detected signals; it does not prove a root cause.
package correlation

import (
	"errors"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var ErrInvalid = errors.New("invalid correlation request")
var ErrConflict = errors.New("correlation revision or identity conflict")

const Window = 5 * time.Minute
const Threshold = 60

type AssetRole struct {
	AssetID string `json:"asset_id"`
	Role    string `json:"role"`
}

type ServiceInput struct {
	Name             string      `json:"name"`
	Kind             string      `json:"kind"`
	Enabled          bool        `json:"enabled"`
	Assets           []AssetRole `json:"assets"`
	ExpectedRevision int64       `json:"expected_revision"`
	Confirmed        bool        `json:"confirmed"`
	Reason           string      `json:"reason"`
}

type Service struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Kind      string      `json:"kind"`
	Enabled   bool        `json:"enabled"`
	Revision  int64       `json:"revision"`
	Assets    []AssetRole `json:"assets"`
	CreatedBy string      `json:"created_by"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

type Signal struct {
	ID               string         `json:"id"`
	IncidentID       string         `json:"incident_id"`
	Category         string         `json:"category"`
	Status           string         `json:"status"`
	Severity         string         `json:"severity"`
	SourceKind       string         `json:"source_kind"`
	SourceInstanceID *string        `json:"source_instance_id,omitempty"`
	Scope            string         `json:"scope"`
	ObservedAt       time.Time      `json:"observed_at"`
	ReceivedAt       time.Time      `json:"received_at"`
	TimeQuality      string         `json:"time_quality"`
	AssetIDs         []string       `json:"asset_ids"`
	ServiceIDs       []string       `json:"service_ids"`
	SiteIDs          []string       `json:"site_ids"`
	Payload          map[string]any `json:"payload"`
}

type Evidence struct {
	Kind    string         `json:"kind"`
	Score   int            `json:"score"`
	Details map[string]any `json:"details"`
}

type Decision struct {
	Eligible       bool       `json:"eligible"`
	Score          int        `json:"score"`
	Confidence     string     `json:"confidence"`
	TimeConfidence string     `json:"time_confidence"`
	Relation       string     `json:"relation"`
	Evidence       []Evidence `json:"evidence"`
	CausalClaim    bool       `json:"causal_claim"`
}

type Incident struct {
	ID                string     `json:"id"`
	State             string     `json:"state"`
	Severity          string     `json:"severity"`
	Title             string     `json:"title"`
	PrimaryAssetID    *string    `json:"primary_asset_id,omitempty"`
	Confidence        string     `json:"confidence"`
	ConfidenceScore   int        `json:"confidence_score"`
	TimeConfidence    string     `json:"time_confidence"`
	Revision          int64      `json:"revision"`
	OpenedAt          time.Time  `json:"opened_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	ResolvedAt        *time.Time `json:"resolved_at,omitempty"`
	SignalCount       int        `json:"signal_count"`
	ActiveSignalCount int        `json:"active_signal_count"`
	SourceCount       int        `json:"source_count"`
}

type Membership struct {
	Signal   Signal     `json:"signal"`
	Relation string     `json:"relation"`
	Score    int        `json:"score"`
	Evidence []Evidence `json:"evidence"`
	AddedAt  time.Time  `json:"added_at"`
}

type TimelineEvent struct {
	ID          int64     `json:"id"`
	SignalID    *string   `json:"signal_id,omitempty"`
	EventKind   string    `json:"event_kind"`
	OccurredAt  time.Time `json:"occurred_at"`
	RecordedAt  time.Time `json:"recorded_at"`
	TimeQuality string    `json:"time_quality"`
	Summary     string    `json:"summary"`
}

type Case struct {
	Truncated   bool             `json:"truncated"`
	Incident    Incident         `json:"incident"`
	Signals     []Membership     `json:"signals"`
	Evidence    []EvidenceRecord `json:"evidence"`
	Timeline    []TimelineEvent  `json:"timeline"`
	Limitations []string         `json:"limitations"`
}

type EvidenceRecord struct {
	ID            int64  `json:"id"`
	LeftSignalID  string `json:"left_signal_id"`
	RightSignalID string `json:"right_signal_id"`
	Evidence
	CreatedAt time.Time `json:"created_at"`
}

type Filter struct {
	State, Severity, AssetID, ServiceID string
	Limit, Offset                       int
}

func text(v string, min, max int) bool {
	if !utf8.ValidString(v) || v != strings.TrimSpace(v) || len(v) < min || len(v) > max {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (in ServiceInput) Validate(create bool) error {
	if !text(in.Name, 1, 128) || !text(in.Reason, 3, 1000) || !in.Confirmed || len(in.Assets) < 1 || len(in.Assets) > 256 {
		return ErrInvalid
	}
	if in.Kind != "network" && in.Kind != "voice" && in.Kind != "infrastructure" && in.Kind != "business" {
		return ErrInvalid
	}
	if create && in.ExpectedRevision != 0 || !create && in.ExpectedRevision < 1 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, a := range in.Assets {
		if !text(a.AssetID, 1, 128) || a.Role != "provider" && a.Role != "dependency" && a.Role != "consumer" || seen[a.AssetID] {
			return ErrInvalid
		}
		seen[a.AssetID] = true
	}
	return nil
}

func overlap(a, b []string) []string {
	set := map[string]bool{}
	for _, v := range a {
		set[v] = true
	}
	out := []string{}
	for _, v := range b {
		if set[v] {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func timeConfidence(a, b Signal) string {
	quality := map[string]int{"skewed": 0, "received_only": 1, "estimated": 2, "exact": 3}
	n := min(quality[a.TimeQuality], quality[b.TimeQuality])
	return []string{"low", "low", "medium", "high"}[n]
}

// Score explains every point. Mere temporal proximity, site or source diversity
// can never group unrelated infrastructure without an Asset/service/topology link.
func Score(a, b Signal, topologyNeighbor bool) Decision {
	d := Decision{Confidence: "low", TimeConfidence: timeConfidence(a, b), Relation: "insufficient_evidence", Evidence: []Evidence{}, CausalClaim: false}
	if a.Scope != b.Scope && (a.Scope != "" || b.Scope != "") || a.Status != "active" || b.Status != "active" {
		return d
	}
	if a.Scope != "" && a.Payload["agent_id"] != b.Payload["agent_id"] {
		return d
	}
	delta := a.ObservedAt.Sub(b.ObservedAt)
	if delta < 0 {
		delta = -delta
	}
	if delta > Window {
		return d
	}
	assets := overlap(a.AssetIDs, b.AssetIDs)
	services := overlap(a.ServiceIDs, b.ServiceIDs)
	sites := overlap(a.SiteIDs, b.SiteIDs)
	structural := false
	add := func(kind string, score int, details map[string]any) {
		d.Score += score
		d.Evidence = append(d.Evidence, Evidence{Kind: kind, Score: score, Details: details})
	}
	if len(assets) > 0 {
		structural = true
		add("same_asset", 50, map[string]any{"asset_ids": assets})
		d.Relation = "same_asset"
	}
	if len(services) > 0 {
		structural = true
		add("same_service", 40, map[string]any{"service_ids": services})
		if d.Relation == "insufficient_evidence" {
			d.Relation = "same_service"
		}
	}
	if topologyNeighbor {
		structural = true
		add("topology_neighbor", 40, map[string]any{"fresh": true, "resolution": "high_confidence"})
		if d.Relation == "insufficient_evidence" {
			d.Relation = "topology_neighbor"
		}
	}
	if len(sites) > 0 {
		add("same_site", 10, map[string]any{"site_ids": sites})
	}
	if delta <= Window {
		points := 15
		if d.TimeConfidence == "medium" {
			points = 7
		}
		if d.TimeConfidence == "low" {
			points = 0
		}
		if points > 0 {
			add("temporal_proximity", points, map[string]any{"distance_seconds": delta.Seconds(), "time_confidence": d.TimeConfidence})
		}
	}
	configBefore := a.Category == "configuration" && !a.ObservedAt.After(b.ObservedAt) || b.Category == "configuration" && !b.ObservedAt.After(a.ObservedAt)
	if structural && configBefore && d.TimeConfidence == "high" && delta <= Window {
		add("configuration_precedes", 15, map[string]any{"distance_seconds": delta.Seconds()})
	}
	if len(assets) > 0 && a.Category == b.Category {
		add("recurring_pattern", 5, map[string]any{"category": a.Category})
	}
	sourceA, sourceB := "", ""
	if a.SourceInstanceID != nil {
		sourceA = *a.SourceInstanceID
	}
	if b.SourceInstanceID != nil {
		sourceB = *b.SourceInstanceID
	}
	if a.SourceKind != b.SourceKind || sourceA != "" && sourceB != "" && sourceA != sourceB {
		add("source_diversity", 5, map[string]any{"source_kinds": []string{a.SourceKind, b.SourceKind}, "source_instance_ids": []string{sourceA, sourceB}})
	}
	if d.Score > 100 {
		d.Score = 100
	}
	d.Eligible = structural && d.Score >= Threshold
	if !d.Eligible {
		return d
	}
	d.Confidence = "medium"
	if d.Score >= 80 && d.TimeConfidence == "high" {
		d.Confidence = "high"
	}
	if d.TimeConfidence == "low" {
		d.Confidence = "low"
	}
	return d
}
