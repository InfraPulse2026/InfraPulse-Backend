package assets

import (
	"encoding/json"
	"strings"
	"time"
	"unicode"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
)

// Observation is an internal adapter output, not an HTTP publishing endpoint.
// Source instance is supplied by the fenced job, never by this record.
type Observation struct {
	ExternalType  string
	ExternalID    string
	ObservedAt    time.Time
	Provenance    string
	Scope         string
	Device        *pb.Device
	ProviderState json.RawMessage
}

func ValidObservation(o Observation) bool {
	if len(o.ProviderState) > 0 {
		var object map[string]json.RawMessage
		if len(o.ProviderState) > 256<<10 || json.Unmarshal(o.ProviderState, &object) != nil || object == nil {
			return false
		}
	}
	if o.Device == nil || o.ExternalID == "" || o.Device.Id != o.ExternalID || len(o.ExternalID) > 256 || strings.ContainsFunc(o.ExternalID, unicode.IsControl) || o.ObservedAt.IsZero() || o.ObservedAt.Year() < 1970 || o.ObservedAt.Year() > 9999 {
		return false
	}
	for _, component := range o.Device.Components {
		if component == nil {
			return false
		}
	}
	for _, identity := range o.Device.IdentityEvidence {
		if identity == nil || identity.Source == nil || identity.Source.Type == "" || identity.Value == "" || len(identity.Value) > 4096 || strings.ContainsFunc(identity.Value, unicode.IsControl) {
			return false
		}
		switch identity.Confidence {
		case "high", "medium", "low", "unresolved":
		default:
			return false
		}
	}
	switch o.ExternalType {
	case "device", "host", "system", "bmc", "chassis", "component":
	default:
		return false
	}
	switch o.Provenance {
	case "external", "manual", "real":
		if o.Scope != "" || Provenance(o.Device) == "simulated" {
			return false
		}
	case "simulated":
		if o.Scope == "" || len(o.Scope) > 128 || strings.ContainsFunc(o.Scope, unicode.IsControl) {
			return false
		}
	default:
		return false
	}
	return len(o.Device.ProtoReflect().GetUnknown()) == 0
}

// AutoCompatible adds corroboration beyond the review-only comparison. Hosts
// require UUID plus serial/manufacturer; physical network devices may instead
// use a high serial namespace plus an exactly matching nonempty model.
func AutoCompatible(a, b Signature) bool {
	if !Compare(a, b).StrongMatch || a.Vendor == "" || a.Vendor != b.Vendor || len(a.Serials) != 1 || len(b.Serials) != 1 || !overlap(a.Serials, b.Serials) {
		return false
	}
	if overlap(a.StrongUUIDs, b.StrongUUIDs) {
		return true
	}
	return a.Kind == "network_device" && a.Model != "" && a.Model == b.Model && overlap(a.StrongSerials, b.StrongSerials)
}

type ResolutionEvent struct {
	ID                int64       `json:"id"`
	BindingID         string      `json:"binding_id"`
	SourceAssetID     string      `json:"source_asset_id"`
	TargetAssetID     *string     `json:"target_asset_id"`
	FactID            int64       `json:"fact_id"`
	TargetFactIDs     []int64     `json:"target_fact_ids"`
	SourceObservedAt  time.Time   `json:"source_observed_at"`
	TargetObservedAts []time.Time `json:"target_observed_ats"`
	Code              string      `json:"code"`
	CreatedAt         time.Time   `json:"created_at"`
}
type BackfillResult struct {
	Scanned   int   `json:"scanned"`
	Processed int   `json:"processed"`
	Failed    int   `json:"failed"`
	Remaining int64 `json:"remaining"`
}
