package assets

import (
	"errors"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
)

var ErrInvalid = errors.New("invalid asset decision")
var ErrConflict = errors.New("asset decision conflicts with current facts or revision")

type Signature struct {
	Kind, Vendor, Model                        string
	UUIDs, Serials, StrongUUIDs, StrongSerials []string
}
type Comparison struct {
	StrongMatch bool     `json:"strong_match"`
	Matches     []string `json:"matches"`
	Conflicts   []string `json:"conflicts"`
}
type Candidate struct {
	Asset             Asset      `json:"asset"`
	Comparison        Comparison `json:"comparison"`
	BlockedByDecision bool       `json:"blocked_by_decision"`
}
type MergeInput struct {
	TargetID           string `json:"target_id"`
	TargetRevision     int64  `json:"target_revision"`
	SourceID           string `json:"source_id"`
	SourceRevision     int64  `json:"source_revision"`
	CanonicalBindingID string `json:"canonical_binding_id"`
	Reason             string `json:"reason"`
	Confirmed          bool   `json:"confirmed"`
}
type SplitInput struct {
	AssetID            string   `json:"asset_id"`
	Revision           int64    `json:"revision"`
	BindingIDs         []string `json:"binding_ids"`
	CanonicalBindingID string   `json:"canonical_binding_id"`
	Reason             string   `json:"reason"`
	Confirmed          bool     `json:"confirmed"`
}
type Decision struct {
	ID                 string    `json:"id"`
	Operation          string    `json:"operation"`
	ActorID            string    `json:"actor_id"`
	TargetID           string    `json:"target_id"`
	SourceID           string    `json:"source_id"`
	BindingIDs         []string  `json:"binding_ids"`
	CanonicalBindingID string    `json:"canonical_binding_id"`
	Reason             string    `json:"reason"`
	CreatedAt          time.Time `json:"created_at"`
}

func ValidReason(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 512 && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}
func ValidMerge(in MergeInput) bool {
	return in.Confirmed && in.TargetID != "" && in.SourceID != "" && in.TargetID != in.SourceID && in.TargetRevision > 0 && in.SourceRevision > 0 && in.CanonicalBindingID != "" && ValidReason(in.Reason)
}
func ValidSplit(in SplitInput) bool {
	if !in.Confirmed || in.AssetID == "" || in.Revision < 1 || len(in.BindingIDs) == 0 || len(in.BindingIDs) > 64 || in.CanonicalBindingID == "" || !ValidReason(in.Reason) {
		return false
	}
	seen := map[string]bool{}
	canonical := false
	for _, id := range in.BindingIDs {
		if id == "" || seen[id] {
			return false
		}
		seen[id] = true
		canonical = canonical || id == in.CanonicalBindingID
	}
	return canonical
}

func RootSignature(d *pb.Device) Signature {
	s := Signature{Kind: Kind(d), Vendor: strings.ToLower(strings.TrimSpace(d.Vendor)), Model: strings.ToLower(strings.TrimSpace(d.Model))}
	sets := map[string]map[string]bool{"uuid": {}, "serial": {}, "strong_uuid": {}, "strong_serial": {}}
	add := func(kind, value string, strong bool) {
		normalized, ok := NormalizeIdentity(kind, value)
		if !ok {
			return
		}
		key := "serial"
		if kind == "hardware_uuid" {
			key = "uuid"
		}
		sets[key][normalized] = true
		if strong {
			sets["strong_"+key][normalized] = true
		}
	}
	add("serial", d.SerialNumber, false)
	add("hardware_uuid", d.GetHardware().GetHardwareUuid(), false)
	for _, e := range d.IdentityEvidence {
		if e.SubjectId != "" || (e.Type != "serial" && e.Type != "hardware_uuid") {
			continue
		}
		if e.GetSource().GetProvenance() == "simulated" && Provenance(d) != "simulated" {
			continue
		}
		resource := e.GetSource().GetResourceId()
		root := d.GetObservation().GetSource().GetResourceId()
		if resource != "" && resource != root {
			continue
		}
		add(e.Type, e.Value, e.Confidence == "high")
	}
	for key, set := range sets {
		values := []string{}
		for v := range set {
			values = append(values, v)
		}
		sort.Strings(values)
		switch key {
		case "uuid":
			s.UUIDs = values
		case "serial":
			s.Serials = values
		case "strong_uuid":
			s.StrongUUIDs = values
		case "strong_serial":
			s.StrongSerials = values
		}
	}
	return s
}
func overlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}
func Compare(a, b Signature) Comparison {
	c := Comparison{Matches: []string{}, Conflicts: []string{}}
	if a.Kind != b.Kind {
		c.Conflicts = append(c.Conflicts, "different_entity_kind")
	}
	if len(a.UUIDs) > 1 || len(b.UUIDs) > 1 || len(a.Serials) > 1 || len(b.Serials) > 1 {
		c.Conflicts = append(c.Conflicts, "contradictory_identity_within_source")
	}
	if len(a.UUIDs) > 0 && len(b.UUIDs) > 0 && !overlap(a.UUIDs, b.UUIDs) {
		c.Conflicts = append(c.Conflicts, "different_hardware_uuid")
	}
	if len(a.Serials) > 0 && len(b.Serials) > 0 && !overlap(a.Serials, b.Serials) {
		c.Conflicts = append(c.Conflicts, "different_serial")
	}
	if a.Vendor != "" && b.Vendor != "" && a.Vendor != b.Vendor {
		c.Conflicts = append(c.Conflicts, "different_manufacturer")
	}
	if overlap(a.StrongUUIDs, b.StrongUUIDs) {
		c.Matches = append(c.Matches, "high_confidence_root_uuid")
	}
	if a.Vendor != "" && a.Vendor == b.Vendor && overlap(a.StrongSerials, b.StrongSerials) {
		c.Matches = append(c.Matches, "high_confidence_root_serial_and_manufacturer")
	}
	c.StrongMatch = len(c.Matches) > 0 && len(c.Conflicts) == 0 && a.Kind != "unknown" && b.Kind != "unknown"
	return c
}
