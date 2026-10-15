package reports

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"time"
)

var scopeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type V2Filter struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	AssetID string    `json:"asset_id"`
	SiteID  string    `json:"site_id"`
	Scope   string    `json:"scope"`
	Limit   int       `json:"limit"`
}

func (f V2Filter) Normalize(now time.Time) (V2Filter, error) {
	f.From = f.From.UTC().Truncate(time.Microsecond)
	f.To = f.To.UTC().Truncate(time.Microsecond)
	if f.Scope == "" {
		f.Scope = "real"
	}
	if f.Limit == 0 {
		f.Limit = 50
	}
	if f.From.IsZero() || f.To.IsZero() || f.To.Sub(f.From) < time.Second || f.To.Sub(f.From) > 31*24*time.Hour || f.To.After(now) || f.Limit < 1 || f.Limit > 200 || (f.Scope != "real" && f.Scope != "simulated" && f.Scope != "all") || f.AssetID != "" && !scopeID.MatchString(f.AssetID) || f.SiteID != "" && !scopeID.MatchString(f.SiteID) {
		return f, ErrInvalid
	}
	return f, nil
}
func (f V2Filter) Fingerprint() string {
	raw, _ := json.Marshal(struct {
		Kind   string
		Filter V2Filter
	}{"unified-v2", f})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type V2Stats struct {
	Cases         int64   `json:"overlapping_cases"`
	Signals       int64   `json:"signal_occurrences"`
	Assets        int64   `json:"affected_assets"`
	ActiveAtEnd   int64   `json:"active_cases_at_end"`
	SummedSeconds float64 `json:"summed_case_seconds"`
}
type CategoryGroup struct {
	Category string `json:"category"`
	Cases    int64  `json:"case_occurrences"`
	Signals  int64  `json:"signal_occurrences"`
}
type ChronicProblem struct {
	AssetID             string    `json:"asset_id"`
	Category            string    `json:"category"`
	Scope               string    `json:"scope"`
	SimulationCollector string    `json:"simulation_collector,omitempty"`
	Cases               int64     `json:"case_occurrences"`
	Signals             int64     `json:"signal_occurrences"`
	Sources             int64     `json:"observation_sources"`
	FirstOpened         time.Time `json:"first_opened_at"`
	LastOpened          time.Time `json:"last_opened_at"`
	Rules               []string  `json:"rule_ids"`
	RelatedServices     []string  `json:"related_service_ids"`
}
type SourceGroup struct {
	Kind    string  `json:"source_kind"`
	ID      *string `json:"source_instance_id"`
	Signals int64   `json:"signal_occurrences"`
	Enabled *bool   `json:"enabled_now"`
	Status  *string `json:"recorded_connection_status_now"`
}
type V2Summary struct {
	Version              string           `json:"version"`
	GeneratedAt          time.Time        `json:"generated_at"`
	Filter               V2Filter         `json:"filter"`
	Stats                V2Stats          `json:"stats"`
	Categories           []CategoryGroup  `json:"categories"`
	Chronic              []ChronicProblem `json:"chronic_problems"`
	Sources              []SourceGroup    `json:"sources"`
	ChronicTotal         int64            `json:"chronic_total"`
	SourcesTotal         int64            `json:"sources_total"`
	Truncated            bool             `json:"truncated"`
	PendingNormalization int64            `json:"pending_work_items_global_now"`
	Methodology          []string         `json:"methodology"`
}
type V2Snapshot struct {
	ID        string     `json:"id"`
	CreatedAt time.Time  `json:"created_at"`
	CreatedBy string     `json:"created_by"`
	Filter    V2Filter   `json:"filter"`
	Report    *V2Summary `json:"report,omitempty"`
}

func V2Methodology() []string {
	return []string{
		"period [from,to); one repeatable-read database snapshot",
		"scope and physical site use current reviewed Asset mappings; not historical placement reconstruction",
		"a chronic category needs at least three distinct correlated case episodes for one Asset/category/simulation namespace",
		"several signal sources in one case do not count as several case episodes",
		"case durations use stored lifecycle timestamps; sums may overlap and are not measured downtime or availability",
		"source connection status is current recorded metadata, not historical source uptime",
		"only projected cases/signals appear; global pending normalization is disclosed separately",
		"related services are current reviewed dependencies, not proof of traffic routing",
		"absence of incidents is not evidence of healthy or continuously observed equipment",
		"configuration/topology/log review records are events, not physical recovery or established root cause",
	}
}
