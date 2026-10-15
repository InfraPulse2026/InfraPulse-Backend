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

type PolicyInput struct {
	ExpectedRevision      int64    `json:"expected_revision"`
	Enabled               bool     `json:"enabled"`
	AllowedVLANs          []int    `json:"allowed_vlans"`
	ProtectedInterfaces   []string `json:"protected_interfaces"`
	MaxEvidenceAgeSeconds int      `json:"max_evidence_age_seconds"`
	Reason                string   `json:"reason"`
}

func (v PolicyInput) Validate() error {
	if v.ExpectedRevision < 0 || v.ExpectedRevision == int64(^uint64(0)>>1) || len(v.AllowedVLANs) > 256 || len(v.ProtectedInterfaces) > 256 || v.MaxEvidenceAgeSeconds < 60 || v.MaxEvidenceAgeSeconds > 86400 || !au.Reason(v.Reason) {
		return ErrInvalid
	}
	seenVLAN := map[int]bool{}
	for _, vlan := range v.AllowedVLANs {
		if vlan < 1 || vlan > 4094 || seenVLAN[vlan] {
			return ErrInvalid
		}
		seenVLAN[vlan] = true
	}
	seenInterface := map[string]bool{}
	for _, name := range v.ProtectedInterfaces {
		if len(name) < 1 || len(name) > 64 || seenInterface[name] {
			return ErrInvalid
		}
		seenInterface[name] = true
	}
	if v.Enabled && len(v.AllowedVLANs) == 0 {
		return ErrInvalid
	}
	return nil
}

type Policy struct {
	AssetID  string `json:"asset_id"`
	Revision int64  `json:"revision"`
	PolicyInput
	ActorID   string    `json:"actor_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

type RemediationInput struct {
	DriftResultID  int64  `json:"drift_result_id"`
	PolicyRevision int64  `json:"policy_revision"`
	Reason         string `json:"reason"`
}

func (v RemediationInput) Validate() error {
	if v.DriftResultID < 1 || v.PolicyRevision < 1 || !au.Reason(v.Reason) {
		return ErrInvalid
	}
	return nil
}

type ApprovalInput struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Confirmed        bool   `json:"confirmed"`
	TimeoutSeconds   int    `json:"timeout_seconds"`
	Reason           string `json:"reason"`
}

type CancelInput struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason"`
}

func (v CancelInput) Validate() error {
	if v.ExpectedRevision < 1 || !au.Reason(v.Reason) {
		return ErrInvalid
	}
	return nil
}

func (v ApprovalInput) Validate() error {
	if v.ExpectedRevision < 1 || !v.Confirmed || v.TimeoutSeconds < 30 || v.TimeoutSeconds > 600 || !au.Reason(v.Reason) {
		return ErrInvalid
	}
	return nil
}

type Remediation struct {
	ID               string     `json:"id"`
	AssetID          string     `json:"asset_id"`
	Revision         int64      `json:"revision"`
	DriftResultID    int64      `json:"drift_result_id"`
	BaselineRevision int64      `json:"baseline_revision"`
	ActualSnapshotID string     `json:"actual_snapshot_id"`
	PolicyRevision   int64      `json:"policy_revision"`
	Action           string     `json:"action"`
	Interface        string     `json:"interface"`
	ExpectedVLAN     int        `json:"expected_vlan"`
	ActualVLAN       int        `json:"actual_vlan"`
	State            string     `json:"state"`
	ErrorCode        string     `json:"error_code"`
	RequesterID      string     `json:"requester_id"`
	ApproverID       string     `json:"approver_id,omitempty"`
	Reason           string     `json:"reason"`
	ApprovalReason   string     `json:"approval_reason,omitempty"`
	AutomationJobID  string     `json:"automation_job_id,omitempty"`
	VerificationID   int64      `json:"verification_drift_result_id,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	ApprovedAt       *time.Time `json:"approved_at,omitempty"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
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
