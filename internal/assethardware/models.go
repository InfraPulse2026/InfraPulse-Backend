// Package assethardware preserves source-owned hardware observations without
// assigning physical sensor identity from names, paths, or numeric values.
package assethardware

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var ErrInvalid = errors.New("invalid hardware filter")
var ErrLimit = errors.New("hardware projection exceeds bounds")

type Filter struct {
	SourceID, Kind               string
	IncludeRelated               bool
	Limit, Offset, MaxAgeSeconds int
}
type HistoryFilter struct {
	SourceID, ObservationKey string
	IncludeRelated           bool
	AfterID                  int64
	Limit, MaxAgeSeconds     int
}
type Observation struct {
	DeviceID         string          `json:"device_id"`
	Kind             string          `json:"kind"`
	ResourceID       string          `json:"resource_id"`
	SubjectID        string          `json:"subject_id"`
	Name             string          `json:"name"`
	Unit             string          `json:"unit"`
	Value            *float64        `json:"value"`
	SourceType       string          `json:"source_type"`
	Provenance       string          `json:"provenance"`
	SourceRef        json.RawMessage `json:"source_ref"`
	ObservationTime  json.RawMessage `json:"observation_time"`
	ObservedAt       time.Time       `json:"observed_at"`
	CollectedAt      *time.Time      `json:"collected_at"`
	SourceTimestamp  *time.Time      `json:"source_timestamp"`
	ClockQuality     string          `json:"clock_quality"`
	DataQuality      string          `json:"data_quality"`
	Payload          json.RawMessage `json:"payload"`
	ResourceType     string          `json:"resource_type"`
	PowerState       string          `json:"power_state"`
	Health           string          `json:"health"`
	HealthRollup     string          `json:"health_rollup"`
	OperationalState string          `json:"operational_state"`
	namespace        string
}
type Item struct {
	Observation
	ID                     int64     `json:"id"`
	ObservationKey         string    `json:"observation_key"`
	MessageID              string    `json:"message_id"`
	AssetID                string    `json:"asset_id"`
	BindingID              string    `json:"binding_id"`
	SourceID               string    `json:"source_instance_id"`
	SourceName             string    `json:"source_name"`
	Scope                  string    `json:"scope"`
	ReceivedAt             time.Time `json:"received_at"`
	FreshnessMaxAgeSeconds int       `json:"freshness_max_age_seconds"`
	Freshness              string    `json:"freshness"`
	SourceEnabled          bool      `json:"source_enabled"`
	AgentConnected         bool      `json:"agent_connected"`
	Related                bool      `json:"related"`
	RelationIDs            []string  `json:"relation_ids"`
}
type Page struct {
	AssetID            string    `json:"asset_id"`
	GeneratedAt        time.Time `json:"generated_at"`
	Items              []Item    `json:"items"`
	Limit              int       `json:"limit"`
	Offset             int       `json:"offset"`
	HasMore            bool      `json:"has_more"`
	RelationsTruncated bool      `json:"relations_truncated"`
	MaxAgeSeconds      int       `json:"max_age_seconds"`
}
type HistoryPage struct {
	AssetID            string    `json:"asset_id"`
	GeneratedAt        time.Time `json:"generated_at"`
	Items              []Item    `json:"items"`
	Limit              int       `json:"limit"`
	NextAfterID        int64     `json:"next_after_id"`
	HasMore            bool      `json:"has_more"`
	RelationsTruncated bool      `json:"relations_truncated"`
	MaxAgeSeconds      int       `json:"max_age_seconds"`
}

func Defaults(f Filter) (Filter, error) {
	if f.Limit == 0 {
		f.Limit = 50
	}
	if f.MaxAgeSeconds == 0 {
		f.MaxAgeSeconds = 300
	}
	if !validSource(f.SourceID) || !validKind(f.Kind) || f.Limit < 1 || f.Limit > 200 || f.Offset < 0 || f.Offset > 100000 || f.MaxAgeSeconds < 3 || f.MaxAgeSeconds > 86400 {
		return f, ErrInvalid
	}
	return f, nil
}
func HistoryDefaults(f HistoryFilter) (HistoryFilter, error) {
	if f.Limit == 0 {
		f.Limit = 50
	}
	if f.MaxAgeSeconds == 0 {
		f.MaxAgeSeconds = 300
	}
	if !validSource(f.SourceID) || f.AfterID < 0 || f.Limit < 1 || f.Limit > 200 || f.MaxAgeSeconds < 3 || f.MaxAgeSeconds > 86400 || f.ObservationKey != "" && (len(f.ObservationKey) != 64 || strings.ContainsAny(f.ObservationKey, "/\\")) {
		return f, ErrInvalid
	}
	if f.ObservationKey != "" {
		if _, e := hex.DecodeString(f.ObservationKey); e != nil {
			return f, ErrInvalid
		}
	}
	return f, nil
}
func validSource(v string) bool { return len(v) <= 128 && !strings.ContainsAny(v, "\x00\r\n") }
func validKind(v string) bool {
	return v == "" || v == "inventory" || v == "component" || v == "health" || v == "metric"
}
func Freshness(o Observation, received, now time.Time, maxAge int, enabled, connected bool) string {
	if o.DataQuality != "valid" {
		return o.DataQuality
	}
	if o.ObservedAt.After(now.Add(30*time.Second)) || received.After(now.Add(30*time.Second)) || o.CollectedAt != nil && o.CollectedAt.After(now.Add(30*time.Second)) || o.SourceTimestamp != nil && o.SourceTimestamp.After(now.Add(30*time.Second)) {
		return "clock_skew"
	}
	if o.ClockQuality == "degraded" || o.ClockQuality == "unsynced" {
		return "clock_degraded"
	}
	if !enabled || !connected {
		return "source_unavailable"
	}
	if now.Sub(o.ObservedAt) > time.Duration(maxAge)*time.Second {
		return "stale"
	}
	if o.SourceTimestamp != nil && now.Sub(*o.SourceTimestamp) > time.Duration(maxAge)*time.Second {
		return "source_stale"
	}
	return "fresh"
}
func (o Observation) Key(binding string) string {
	raw, _ := json.Marshal([]string{binding, o.namespace, o.ResourceID, o.SubjectID, o.Kind, o.Name, o.Unit})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func rawProto(m proto.Message) json.RawMessage {
	if m == nil || !m.ProtoReflect().IsValid() {
		return json.RawMessage(`null`)
	}
	raw, _ := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(m)
	return raw
}
func observation(device, kind, resource, subject, name, source string, meta, fallback *pb.ObservationMetadata, at time.Time) Observation {
	if meta == nil {
		meta = fallback
	}
	src := meta.GetSource()
	if src == nil {
		src = &pb.SourceRef{Type: source}
	}
	o := Observation{DeviceID: device, Kind: kind, ResourceID: resource, SubjectID: subject, Name: name, SourceType: src.Type, Provenance: src.Provenance, SourceRef: rawProto(src), ObservationTime: rawProto(meta.GetTime()), ObservedAt: at.UTC(), ClockQuality: meta.GetTime().GetClockQuality(), DataQuality: "valid"}
	if o.ResourceID == "" {
		o.ResourceID = src.ResourceId
	}
	if o.Provenance == "" {
		o.Provenance = "real"
	}
	if source == "simulated" || fallback.GetSource().GetProvenance() == "simulated" {
		o.Provenance = "simulated"
	}
	t := meta.GetTime()
	if t.GetObservedAt() != nil {
		o.ObservedAt = t.ObservedAt.AsTime().UTC()
	}
	if t.GetCollectedAt() != nil {
		v := t.CollectedAt.AsTime().UTC()
		o.CollectedAt = &v
	}
	if t.GetSourceTimestamp() != nil {
		v := t.SourceTimestamp.AsTime().UTC()
		o.SourceTimestamp = &v
	}
	raw, _ := json.Marshal([]string{src.Type, src.CollectorId, src.TargetId, src.Protocol, src.InstanceId, src.Provenance})
	o.namespace = string(raw)
	return o
}

// Extract is bounded by the enclosing Collector contract. Missing optional
// numbers remain absent in protobuf JSON; a metric with value zero is measured.
func Extract(e *pb.AgentEnvelope) ([]Observation, error) {
	out := []Observation{}
	add := func(o Observation) error {
		if len(out) >= 12000 || len(o.Payload) > 1<<20 {
			return ErrLimit
		}
		out = append(out, o)
		return nil
	}
	var devices []*pb.Device
	var at time.Time
	if h := e.GetHello(); h != nil {
		devices = []*pb.Device{h.Device}
		at = e.GetSentAt().AsTime()
		if h.GetDevice().GetLastSeen() != nil {
			at = h.Device.LastSeen.AsTime()
		}
	}
	if i := e.GetInventory(); i != nil {
		devices = i.Devices
		at = i.ObservedAt.AsTime()
	}
	for _, d := range devices {
		if d == nil {
			continue
		}
		if d.Hardware != nil {
			o := observation(d.Id, "inventory", "", d.Id, d.Name, d.Source, d.Observation, nil, at)
			o.ResourceType = d.Hardware.ResourceType
			o.PowerState = d.Hardware.PowerState
			setStatus(&o, d.Hardware.Status)
			o.Payload, _ = json.Marshal(struct {
				Hardware   json.RawMessage `json:"hardware"`
				Vendor     string          `json:"vendor"`
				Model      string          `json:"model"`
				Serial     string          `json:"serial_number"`
				Firmware   string          `json:"firmware_version"`
				Software   string          `json:"software_version"`
				Identities json.RawMessage `json:"identity_evidence"`
				Extended   json.RawMessage `json:"extended_identity_evidence"`
			}{rawProto(d.Hardware), d.Vendor, d.Model, d.SerialNumber, d.FirmwareVersion, d.SoftwareVersion, identityJSON(d.IdentityEvidence), identityJSON(d.ExtendedIdentityEvidence)})
			if err := add(o); err != nil {
				return nil, err
			}
		}
		for _, c := range d.Components {
			if c == nil {
				continue
			}
			o := observation(d.Id, "component", "", c.Id, c.Name, d.Source, c.Observation, d.Observation, at)
			o.ResourceType = c.GetHardware().GetResourceType()
			o.PowerState = c.GetHardware().GetPowerState()
			setStatus(&o, c.GetHardware().GetStatus())
			o.Payload = rawProto(c)
			if err := add(o); err != nil {
				return nil, err
			}
		}
	}
	for _, s := range e.GetTelemetry().GetSamples() {
		if s == nil {
			continue
		}
		at = s.GetObservedAt().AsTime()
		for _, h := range s.HardwareHealth {
			if h == nil {
				continue
			}
			o := observation(s.DeviceId, "health", h.ResourceId, h.Id, h.Name, s.Source, h.Observation, s.Observation, at)
			setStatus(&o, h.Status)
			o.Payload = rawProto(h)
			if err := add(o); err != nil {
				return nil, err
			}
		}
		for _, m := range s.Metrics {
			if m == nil || !strings.HasPrefix(m.Name, "hardware.") && m.ResourceId == "" && m.ComponentId == "" && m.Status == nil {
				continue
			}
			o := observation(s.DeviceId, "metric", m.ResourceId, m.ComponentId, m.Name, s.Source, m.Observation, s.Observation, at)
			setStatus(&o, m.Status)
			o.Unit = m.Unit
			o.Payload = rawProto(m)
			if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
				o.DataQuality = "non_finite"
			} else {
				v := m.Value
				o.Value = &v
			}
			if err := add(o); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
func identityJSON(v []*pb.IdentityEvidence) json.RawMessage {
	out := []json.RawMessage{}
	for _, e := range v {
		out = append(out, rawProto(e))
	}
	raw, _ := json.Marshal(out)
	return raw
}

func setStatus(o *Observation, s *pb.HardwareStatus) {
	o.Health = s.GetHealth()
	o.HealthRollup = s.GetHealthRollup()
	o.OperationalState = s.GetState()
}

// CompatibleSource never matches a supplied target against an unrelated
// inventory namespace. Legacy absence is retained as an explicit unknown source.
func CompatibleSource(o Observation, d *pb.Device) bool {
	known := d.GetObservation().GetSource()
	if known == nil {
		return true
	}
	var observed pb.SourceRef
	if protojson.Unmarshal(o.SourceRef, &observed) != nil {
		return false
	}
	return observed.Type == known.Type && observed.TargetId == known.TargetId && observed.Protocol == known.Protocol && observed.InstanceId == known.InstanceId
}
