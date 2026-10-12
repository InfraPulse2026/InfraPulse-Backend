// Package configdrift compares approved configuration snapshots without exposing
// arbitrary configuration lines or granting execution privileges.
package configdrift

import (
	"errors"
	au "github.com/InfraPulse2026/InfraPulse-Backend/internal/automation"
	"time"
)

var ErrInvalid = errors.New("invalid configuration drift request")
var ErrConflict = errors.New("configuration baseline conflict")
var ErrUnsupported = errors.New("configuration cannot be safely normalized")

const NormalizerVersion = "infrapulse_config_v1"
const MaxChanges = 200
const DefaultMaxAge = 15 * time.Minute

type BaselineInput struct {
	ExpectedRevision int64  `json:"expected_revision"`
	AssetRevision    int64  `json:"asset_revision"`
	SnapshotID       string `json:"snapshot_id"`
	Confirmed        bool   `json:"confirmed"`
	Reason           string `json:"reason"`
}

func (v BaselineInput) Validate() error {
	if v.ExpectedRevision < 0 || v.AssetRevision < 1 || !au.Identifier(v.SnapshotID) || !v.Confirmed || !au.Reason(v.Reason) {
		return ErrInvalid
	}
	return nil
}

type Baseline struct {
	AssetID          string    `json:"asset_id"`
	Revision         int64     `json:"revision"`
	AssetRevision    int64     `json:"asset_revision"`
	SnapshotID       string    `json:"snapshot_id"`
	ReadTemplateID   string    `json:"read_template_id"`
	Normalizer       string    `json:"normalizer"`
	RawDigest        string    `json:"raw_sha256"`
	NormalizedDigest string    `json:"normalized_sha256"`
	ActorID          string    `json:"actor_id"`
	Reason           string    `json:"reason"`
	CreatedAt        time.Time `json:"created_at"`
}
type Change struct {
	Path         string `json:"path"`
	Kind         string `json:"kind"`
	Interface    string `json:"interface,omitempty"`
	ExpectedVLAN *int   `json:"expected_vlan,omitempty"`
	ActualVLAN   *int   `json:"actual_vlan,omitempty"`
	Redacted     bool   `json:"redacted"`
}
type Proposal struct {
	Action             string `json:"action"`
	Interface          string `json:"interface"`
	ExpectedVLAN       int    `json:"expected_vlan"`
	ActualVLAN         int    `json:"actual_vlan"`
	ApprovalRequired   bool   `json:"approval_required"`
	ExecutionAvailable bool   `json:"execution_available"`
	Limitation         string `json:"limitation"`
}
type Comparison struct {
	Status         string    `json:"status"`
	ErrorCode      string    `json:"error_code"`
	ExpectedDigest string    `json:"expected_normalized_sha256"`
	ActualDigest   string    `json:"actual_normalized_sha256"`
	Changes        []Change  `json:"changes"`
	TotalChanges   int       `json:"total_changes"`
	Truncated      bool      `json:"truncated"`
	Proposal       *Proposal `json:"proposal"`
}
type Service struct {
	AssociationID string `json:"association_id"`
	Kind          string `json:"kind"`
	Relation      string `json:"relation"`
}
type Record struct {
	Comparison
	ID                   int64     `json:"id"`
	AssetID              string    `json:"asset_id"`
	BaselineRevision     int64     `json:"baseline_revision"`
	ActualSnapshotID     string    `json:"actual_snapshot_id"`
	ActualTargetRevision int64     `json:"actual_target_revision"`
	Normalizer           string    `json:"normalizer"`
	ActualObservedAt     time.Time `json:"actual_observed_at"`
	ComparedAt           time.Time `json:"compared_at"`
	RelatedServices      []Service `json:"related_services"`
}
type State struct {
	AssetID       string    `json:"asset_id"`
	Status        string    `json:"status"`
	Reason        string    `json:"reason"`
	Fresh         bool      `json:"fresh"`
	MaxAgeSeconds int       `json:"max_age_seconds"`
	Baseline      *Baseline `json:"baseline"`
	Latest        *Record   `json:"latest"`
}
type CompareInput struct {
	BaselineRevision int64  `json:"baseline_revision"`
	SnapshotID       string `json:"snapshot_id"`
	Reason           string `json:"reason"`
}

func (v CompareInput) Validate() error {
	if v.BaselineRevision < 1 || !au.Identifier(v.SnapshotID) || !au.Reason(v.Reason) {
		return ErrInvalid
	}
	return nil
}
