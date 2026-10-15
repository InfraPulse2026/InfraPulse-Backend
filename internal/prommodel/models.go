// Package prommodel defines the source-owned Prometheus target/query contract.
package prommodel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/externalmonitor"
)

var ErrInvalid = errors.New("invalid Prometheus input")
var ErrNoData = errors.New("Prometheus target data is unavailable")
var ErrHistory = errors.New("Prometheus history is queried at the source")
var name = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var metricName = regexp.MustCompile(`^[A-Za-z_:][A-Za-z0-9_:]*$`)
var mappingID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)
var secretLabel = regexp.MustCompile(`(?i)(password|passwd|token|secret|authorization|credential|community|private.?key)`)
var targetID = regexp.MustCompile(`^prom_target_[a-f0-9]{64}$`)
var sampleID = regexp.MustCompile(`^prom_metric_[a-f0-9]{64}$`)

func Text(s string, n int) bool {
	return len(s) <= n && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}
func LabelName(s string) bool {
	return len(s) <= 64 && name.MatchString(s) && !strings.HasPrefix(s, "__") && !secretLabel.MatchString(s)
}
func TargetIDValid(s string) bool { return targetID.MatchString(s) }
func MetricIDValid(s string) bool { return sampleID.MatchString(s) }

type Mapping struct {
	ID             string `json:"id"`
	Metric         string `json:"metric"`
	Unit           string `json:"unit"`
	Expression     string `json:"expression"`
	Multiplier     string `json:"multiplier"`
	MaxAgeSeconds  int    `json:"max_age_seconds"`
	MetadataMetric string `json:"metadata_metric,omitempty"`
}
type Settings struct {
	AlertsEnabled          bool              `json:"alerts_enabled"`
	ScrapePools            []string          `json:"scrape_pools"`
	LabelMatch             map[string]string `json:"label_match"`
	IdentityLabels         []string          `json:"identity_labels"`
	PageSize               int               `json:"page_size"`
	MaxTargets             int               `json:"max_targets"`
	AuthMode               string            `json:"auth_mode"`
	Username               string            `json:"username,omitempty"`
	DeviceType             string            `json:"device_type"`
	Profile                string            `json:"profile"`
	HostnameLabel          string            `json:"hostname_label,omitempty"`
	ManufacturerLabel      string            `json:"manufacturer_label,omitempty"`
	ModelLabel             string            `json:"model_label,omitempty"`
	SerialLabel            string            `json:"serial_label,omitempty"`
	UUIDLabel              string            `json:"uuid_label,omitempty"`
	VerifiedIdentityLabels bool              `json:"verified_identity_labels"`
	Mappings               []Mapping         `json:"mappings"`
}

func (s Settings) Normalize() (Settings, error) {
	if s.PageSize == 0 {
		s.PageSize = 5
	}
	if s.MaxTargets == 0 {
		s.MaxTargets = 200
	}
	if s.PageSize < 1 || s.PageSize > 5 || s.MaxTargets < 1 || s.MaxTargets > 200 || len(s.ScrapePools) > 8 || len(s.LabelMatch) > 16 || len(s.IdentityLabels) > 8 || len(s.Mappings) > 16 {
		return s, ErrInvalid
	}
	if s.AuthMode == "" {
		s.AuthMode = "none"
	}
	switch s.AuthMode {
	case "none", "bearer":
		if s.Username != "" {
			return s, ErrInvalid
		}
	case "basic":
		if s.Username == "" || !Text(s.Username, 256) || strings.Contains(s.Username, ":") {
			return s, ErrInvalid
		}
	default:
		return s, ErrInvalid
	}
	if s.DeviceType == "" {
		s.DeviceType = "unknown"
	}
	switch s.DeviceType {
	case "unknown", "server", "router", "switch", "firewall", "hypervisor":
	default:
		return s, ErrInvalid
	}
	if s.Profile == "" {
		s.Profile = "custom"
	}
	if s.Profile != "custom" && s.Profile != "node_exporter" {
		return s, ErrInvalid
	}
	if s.Profile == "node_exporter" && s.DeviceType == "unknown" {
		s.DeviceType = "server"
	}
	seen := map[string]bool{}
	s.ScrapePools = append([]string{}, s.ScrapePools...)
	for _, p := range s.ScrapePools {
		if p == "" || !Text(p, 128) || seen[p] {
			return s, ErrInvalid
		}
		seen[p] = true
	}
	sort.Strings(s.ScrapePools)
	if len(s.IdentityLabels) == 0 {
		s.IdentityLabels = []string{"job", "instance"}
	}
	s.IdentityLabels = append([]string{}, s.IdentityLabels...)
	seen = map[string]bool{}
	for _, k := range s.IdentityLabels {
		if !LabelName(k) || seen[k] {
			return s, ErrInvalid
		}
		seen[k] = true
	}
	if !seen["job"] || !seen["instance"] {
		return s, ErrInvalid
	}
	sort.Strings(s.IdentityLabels)
	for k, v := range s.LabelMatch {
		if !LabelName(k) || !Text(v, 512) {
			return s, ErrInvalid
		}
	}
	for _, k := range []string{s.HostnameLabel, s.ManufacturerLabel, s.ModelLabel, s.SerialLabel, s.UUIDLabel} {
		if k != "" && !LabelName(k) {
			return s, ErrInvalid
		}
	}
	s.Mappings = append([]Mapping{}, s.Mappings...)
	if s.Profile == "node_exporter" && len(s.Mappings) == 0 {
		s.Mappings = NodeExporterMappings()
	}
	seen = map[string]bool{}
	for i, m := range s.Mappings {
		if !mappingID.MatchString(m.ID) || seen[m.ID] || externalmonitor.Units[m.Metric] == "" || m.Unit != externalmonitor.Units[m.Metric] || !Text(m.Expression, 4096) || m.Expression == "" || m.MetadataMetric != "" && !metricName.MatchString(m.MetadataMetric) {
			return s, ErrInvalid
		}
		seen[m.ID] = true
		base, err := (externalmonitor.Mapping{HostID: "1", ItemID: "1", Metric: m.Metric, Unit: m.Unit, Multiplier: m.Multiplier, MaxAgeSeconds: m.MaxAgeSeconds}).Normalize()
		if err != nil {
			return s, ErrInvalid
		}
		m.Multiplier = base.Multiplier
		m.MaxAgeSeconds = base.MaxAgeSeconds
		if _, err = ScopedExpression(m.Expression, map[string]string{"job": "validation", "instance": "validation"}); err != nil {
			return s, ErrInvalid
		}
		s.Mappings[i] = m
	}
	return s, nil
}

type Target struct {
	ID             string            `json:"id"`
	ScrapePool     string            `json:"scrape_pool"`
	Labels         map[string]string `json:"labels"`
	Identity       map[string]string `json:"identity"`
	Health         string            `json:"health"`
	LastScrape     *time.Time        `json:"last_scrape"`
	ScrapeInterval string            `json:"scrape_interval"`
	SourceRevision int64             `json:"source_revision"`
	APIVersion     string            `json:"api_version"`
}

func (s Settings) Selected(pool string, labels map[string]string) bool {
	found := false
	for _, p := range s.ScrapePools {
		if pool == p {
			found = true
		}
	}
	if !found {
		return false
	}
	for k, v := range s.LabelMatch {
		if labels[k] != v {
			return false
		}
	}
	return true
}
func Hash(prefix string, value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return prefix + hex.EncodeToString(sum[:])
}
func TargetID(pool string, identity map[string]string) string {
	return Hash("prom_target_", struct {
		Pool   string
		Labels map[string]string
	}{pool, identity})
}
func MetricID(target, mapping string) string { return Hash("prom_metric_", []string{target, mapping}) }

type Batch struct {
	Targets      []Target
	Measurements []externalmonitor.Measurement
	Alerts       []Alert
}

// Only allowlisted display fields are retained; full raw labels are hashed by
// the connector for deduplication and are never exported to storage or REST.
type Alert struct {
	LabelFingerprint string    `json:"label_fingerprint,omitempty"`
	Fingerprint      string    `json:"fingerprint"`
	TargetID         string    `json:"target_id"`
	Name             string    `json:"name"`
	Severity         string    `json:"severity"`
	ActiveAt         time.Time `json:"active_at"`
}
type Query struct {
	AssetID, SourceID, TargetID, MappingID, Kind string
	From, To                                     time.Time
	StepSeconds                                  int
}

func (q Query) Validate(now time.Time) error {
	if q.AssetID == "" || !Text(q.AssetID, 256) || q.SourceID == "" || !Text(q.SourceID, 256) || !TargetIDValid(q.TargetID) || !mappingID.MatchString(q.MappingID) {
		return ErrInvalid
	}
	switch q.Kind {
	case "instant", "metadata":
		return nil
	case "range":
		if q.From.IsZero() || q.To.IsZero() || q.From.Year() < 1970 || q.To.After(now.Add(30*time.Second)) || q.To.Before(q.From) || q.To.Sub(q.From) > 14*24*time.Hour || q.StepSeconds < 3 || q.StepSeconds > 86400 || int64(q.To.Sub(q.From)/time.Second)/int64(q.StepSeconds)+1 > 3000 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

type Point struct {
	At      time.Time `json:"at"`
	Value   *string   `json:"value"`
	Quality string    `json:"quality"`
}
type Metadata struct {
	Metric string `json:"metric"`
	Type   string `json:"type"`
	Unit   string `json:"unit"`
	Help   string `json:"help"`
}
type QueryResult struct {
	SourceID       string     `json:"source_instance_id"`
	SourceRevision int64      `json:"source_revision"`
	AssetID        string     `json:"asset_id"`
	TargetID       string     `json:"target_id"`
	MappingID      string     `json:"mapping_id"`
	Metric         string     `json:"metric"`
	Unit           string     `json:"unit"`
	Kind           string     `json:"kind"`
	Quality        string     `json:"quality"`
	TimeSemantics  string     `json:"time_semantics"`
	Points         []Point    `json:"points"`
	Metadata       []Metadata `json:"metadata"`
}
