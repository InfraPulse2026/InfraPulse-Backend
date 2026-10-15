// Package externalmonitor carries normalized monitoring facts from read-only
// source adapters. It never creates Agent identities or executes remediation.
package externalmonitor

import (
	"errors"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var ErrInvalid = errors.New("invalid external monitoring data")
var decimal = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]{1,3})?$`)
var Units = map[string]string{"cpu.utilization": "percent", "memory.utilization": "percent", "disk.utilization": "percent", "memory.used": "bytes", "memory.total": "bytes", "disk.free": "bytes", "network.rx": "bytes_per_second", "network.tx": "bytes_per_second", "network.errors": "per_second", "latency": "seconds", "availability": "boolean"}

func ValidID(id string) bool {
	n, err := strconv.ParseUint(id, 10, 64)
	return err == nil && n > 0 && strconv.FormatUint(n, 10) == id
}

type Mapping struct {
	HostID        string `json:"host_id"`
	ItemID        string `json:"item_id"`
	Metric        string `json:"metric"`
	SourceUnit    string `json:"source_unit"`
	Unit          string `json:"unit"`
	Multiplier    string `json:"multiplier"`
	MaxAgeSeconds int    `json:"max_age_seconds"`
}

func (m Mapping) Normalize() (Mapping, error) {
	if !ValidID(m.HostID) || !ValidID(m.ItemID) || Units[m.Metric] == "" || m.Unit != Units[m.Metric] || len(m.SourceUnit) > 32 || strings.ContainsAny(m.SourceUnit, "\r\n\x00") {
		return m, ErrInvalid
	}
	if m.Multiplier == "" {
		m.Multiplier = "1"
	}
	v, ok := Number(m.Multiplier)
	if !ok || v.Sign() <= 0 || v.Cmp(big.NewRat(1000000000, 1)) > 0 {
		return m, ErrInvalid
	}
	if m.MaxAgeSeconds == 0 {
		m.MaxAgeSeconds = 300
	}
	if m.MaxAgeSeconds < 3 || m.MaxAgeSeconds > 86400 {
		return m, ErrInvalid
	}
	return m, nil
}
func Number(value string) (*big.Rat, bool) {
	if len(value) > 256 || !decimal.MatchString(value) {
		return nil, false
	}
	v, ok := new(big.Rat).SetString(value)
	if !ok {
		return nil, false
	}
	if v.Num().BitLen() > 2048 || v.Denom().BitLen() > 2048 {
		return nil, false
	}
	return v, true
}
func NormalizeValue(raw string, m Mapping) (string, bool) {
	v, ok := Number(raw)
	if !ok || v.Sign() < 0 {
		return "", false
	}
	scale, ok := Number(m.Multiplier)
	if !ok {
		return "", false
	}
	v.Mul(v, scale)
	if m.Unit == "percent" && v.Cmp(big.NewRat(100, 1)) > 0 || m.Unit == "boolean" && v.Cmp(big.NewRat(0, 1)) != 0 && v.Cmp(big.NewRat(1, 1)) != 0 {
		return "", false
	}
	if v.IsInt() {
		return v.Num().String(), true
	}
	return strings.TrimRight(strings.TrimRight(v.FloatString(12), "0"), "."), true
}

type Measurement struct {
	MappingID     string     `json:"mapping_id,omitempty"`
	TimeSemantics string     `json:"time_semantics,omitempty"`
	ScrapedAt     *time.Time `json:"scraped_at,omitempty"`
	HostID        string     `json:"host_id"`
	ItemID        string     `json:"item_id"`
	Name          string     `json:"name"`
	Metric        string     `json:"metric"`
	Unit          string     `json:"unit"`
	SourceUnit    string     `json:"source_unit"`
	Multiplier    string     `json:"multiplier"`
	Value         *string    `json:"value"`
	RawValue      string     `json:"raw_value"`
	ObservedAt    *time.Time `json:"observed_at"`
	Nanoseconds   int        `json:"nanoseconds"`
	Quality       string     `json:"quality"`
	MaxAgeSeconds int        `json:"max_age_seconds"`
}
type Signal struct {
	EventID         string     `json:"event_id"`
	TriggerID       string     `json:"trigger_id"`
	HostIDs         []string   `json:"host_ids"`
	Name            string     `json:"name"`
	TriggerName     string     `json:"trigger_name"`
	TriggerStatus   string     `json:"trigger_status"`
	Severity        int        `json:"severity"`
	Acknowledged    bool       `json:"acknowledged"`
	Suppressed      bool       `json:"suppressed"`
	ObservedAt      time.Time  `json:"observed_at"`
	Nanoseconds     int        `json:"nanoseconds"`
	RecoveryEventID string     `json:"recovery_event_id"`
	RecoveredAt     *time.Time `json:"recovered_at"`
	State           string     `json:"state"`
	MappingStatus   string     `json:"mapping_status"`
}
type Pending struct {
	EventID   string
	TriggerID string
	HostIDs   []string
}
type Batch struct {
	Measurements []Measurement
	Signals      []Signal
}
type MetricRecord struct {
	Measurement
	SourceInstanceID string    `json:"source_instance_id"`
	SourceRevision   int64     `json:"source_revision"`
	SourceType       string    `json:"source_type"`
	AssetID          string    `json:"asset_id"`
	ReceivedAt       time.Time `json:"received_at"`
	LastSeen         time.Time `json:"last_seen"`
}
type SignalRecord struct {
	Signal
	SourceInstanceID string    `json:"source_instance_id"`
	SourceRevision   int64     `json:"source_revision"`
	SourceType       string    `json:"source_type"`
	IncidentID       string    `json:"incident_id"`
	AssetIDs         []string  `json:"asset_ids"`
	FirstSeen        time.Time `json:"first_seen"`
	LastSeen         time.Time `json:"last_seen"`
	Freshness        string    `json:"freshness"`
}
type Filter struct {
	AssetID, SourceID, State, ItemID string
	Limit, Offset                    int
}
type SignalVersion struct {
	ID             int64     `json:"id"`
	SourceRevision int64     `json:"source_revision"`
	Signal         Signal    `json:"signal"`
	ReceivedAt     time.Time `json:"received_at"`
}
