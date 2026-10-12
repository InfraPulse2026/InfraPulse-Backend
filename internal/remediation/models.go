// Package remediation binds a single reviewed configuration action to a human
// approval. It does not execute commands or own a parallel job engine.
package remediation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	au "github.com/InfraPulse2026/InfraPulse-Backend/internal/automation"
	cd "github.com/InfraPulse2026/InfraPulse-Backend/internal/configdrift"
)

var ErrInvalid = errors.New("invalid remediation action")
var ErrConflict = errors.New("remediation action or state conflict")
var ErrPolicy = errors.New("remediation policy denied")
var ErrExpired = errors.New("remediation approval expired")
var ErrUnavailable = errors.New("remediation execution unavailable")

const ActionType = "restore_access_vlan"
const TemplateID = "cisco_ios_restore_access_vlan_v1"
const Platform = "cisco.ios.ios"
const MaxPending = 100
const MaxConcurrent = 4
const ApprovalTTL = 5 * time.Minute
const QueueTTL = time.Minute
const ExecutionTimeout = 120 * time.Second
const GatherTimeout = 30 * time.Second

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var interfaceIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_./:-]{0,127}$`)

func ValidDigest(value string) bool { return digestPattern.MatchString(value) }

// Action contains only the immutable parameters approved by the human. Host,
// credentials and complete configuration remain in their existing secure stores;
// target/binding digests bind their security-relevant identity without exposing it.
type Action struct {
	ActionType         string `json:"action_type"`
	AssetID            string `json:"asset_id"`
	AssetRevision      int64  `json:"asset_revision"`
	BindingID          string `json:"binding_id"`
	BindingDigest      string `json:"binding_digest"`
	TargetRevision     int64  `json:"target_revision"`
	TargetDigest       string `json:"target_digest"`
	InterfaceID        string `json:"interface_id"`
	Interface          string `json:"interface"`
	DriftID            int64  `json:"drift_id"`
	BaselineRevision   int64  `json:"baseline_revision"`
	ExpectedSnapshotID string `json:"expected_snapshot_id"`
	ActualSnapshotID   string `json:"actual_snapshot_id"`
	ExpectedDigest     string `json:"expected_digest"`
	ObservedDigest     string `json:"observed_digest"`
	CurrentVLAN        int    `json:"current_vlan"`
	TargetVLAN         int    `json:"target_vlan"`
	TemplateID         string `json:"template_id"`
	TemplateDigest     string `json:"template_digest"`
	PolicyDigest       string `json:"policy_digest"`
}

func (a Action) Validate() error {
	if a.ActionType != ActionType || a.TemplateID != TemplateID || !au.Identifier(a.AssetID) || !au.Identifier(a.BindingID) || !au.Identifier(a.ExpectedSnapshotID) || !au.Identifier(a.ActualSnapshotID) || !interfaceIDPattern.MatchString(a.InterfaceID) || !cd.PhysicalInterfaceName(a.Interface) || a.AssetRevision < 1 || a.TargetRevision < 1 || a.DriftID < 1 || a.BaselineRevision < 1 || a.CurrentVLAN < 1 || a.CurrentVLAN > 4094 || a.TargetVLAN < 1 || a.TargetVLAN > 4094 || a.CurrentVLAN == a.TargetVLAN {
		return ErrInvalid
	}
	for _, value := range []string{a.BindingDigest, a.TargetDigest, a.ExpectedDigest, a.ObservedDigest, a.TemplateDigest, a.PolicyDigest} {
		if !ValidDigest(value) {
			return ErrInvalid
		}
	}
	return nil
}

// Fingerprint uses a versioned, fixed-field JSON representation. It is independent
// of timestamps, approval reasons and map iteration, and covers every action field.
func Fingerprint(a Action) string {
	raw, _ := json.Marshal(struct {
		Version int    `json:"version"`
		Action  Action `json:"action"`
	}{Version: 1, Action: a})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type Proposal struct {
	ID                 string        `json:"id"`
	Action             Action        `json:"action"`
	Digest             string        `json:"digest"`
	State              string        `json:"state"`
	AssetName          string        `json:"asset_name"`
	ActualObservedAt   time.Time     `json:"actual_observed_at"`
	ExpectedObservedAt time.Time     `json:"expected_observed_at"`
	Warnings           []string      `json:"warnings"`
	CreatedBy          string        `json:"created_by"`
	CreatedAt          time.Time     `json:"created_at"`
	ExpiresAt          time.Time     `json:"expires_at"`
	ApprovedBy         string        `json:"approved_by,omitempty"`
	ApprovedAt         *time.Time    `json:"approved_at,omitempty"`
	ApprovedDigest     string        `json:"approved_digest,omitempty"`
	JobID              string        `json:"job_id,omitempty"`
	BeforeSnapshotID   string        `json:"before_snapshot_id,omitempty"`
	AfterSnapshotID    string        `json:"after_snapshot_id,omitempty"`
	Verification       *Verification `json:"verification,omitempty"`
	ErrorCode          string        `json:"error_code,omitempty"`
}

type Verification struct {
	Status     string        `json:"status"`
	Comparison cd.Comparison `json:"comparison"`
	VerifiedAt time.Time     `json:"verified_at"`
	DriftID    int64         `json:"drift_id"`
}

type HistoryEvent struct {
	ID        int64     `json:"id"`
	ActorID   string    `json:"actor_id"`
	Action    string    `json:"action"`
	Outcome   string    `json:"outcome"`
	Resource  string    `json:"resource"`
	RequestID string    `json:"request_id"`
	At        time.Time `json:"at"`
}

type CreateInput struct {
	DriftID int64  `json:"drift_id"`
	Reason  string `json:"reason"`
}

func (v CreateInput) Validate() error {
	if v.DriftID < 1 || !au.Reason(v.Reason) {
		return ErrInvalid
	}
	return nil
}

type ApproveInput struct {
	Confirmed bool   `json:"confirmed"`
	Digest    string `json:"digest"`
	Reason    string `json:"reason"`
}

func (v ApproveInput) Validate() error {
	if !v.Confirmed || !ValidDigest(v.Digest) || !au.Reason(v.Reason) {
		return ErrInvalid
	}
	return nil
}

type RejectInput struct {
	Reason string `json:"reason"`
}

func (v RejectInput) Validate() error {
	if !au.Reason(v.Reason) {
		return ErrInvalid
	}
	return nil
}

// ReconcileInput confirms a separately gathered, read-only snapshot after an
// unknown execution. It cannot queue or replay a write operation.
type ReconcileInput struct {
	SnapshotID string `json:"snapshot_id"`
	Confirmed  bool   `json:"confirmed"`
	Reason     string `json:"reason"`
}

func (v ReconcileInput) Validate() error {
	if !au.Identifier(v.SnapshotID) || !v.Confirmed || !au.Reason(v.Reason) {
		return ErrInvalid
	}
	return nil
}

// ValidateBinding rejects stored action mutations, including mutations made after
// approval. Persistence owns the immutable-row constraint and atomic transitions.
func (p Proposal) ValidateBinding() error {
	if !au.Identifier(p.ID) || !au.Identifier(p.CreatedBy) || p.Action.Validate() != nil || p.Digest != Fingerprint(p.Action) || p.CreatedAt.IsZero() || !p.ExpiresAt.After(p.CreatedAt) || p.ExpiresAt.Sub(p.CreatedAt) > ApprovalTTL {
		return ErrConflict
	}
	if p.ApprovedDigest != "" && p.ApprovedDigest != p.Digest {
		return ErrConflict
	}
	return nil
}

// ValidateApproval validates a fresh one-time approval. The storage transaction
// handles the idempotent response for an already queued action by the same user.
func (p Proposal) ValidateApproval(user string, input ApproveInput, now time.Time, policy Policy) error {
	if input.Validate() != nil || !au.Identifier(user) {
		return ErrInvalid
	}
	if p.ValidateBinding() != nil || input.Digest != p.Digest || p.State != "proposed" || p.ApprovedAt != nil || p.ApprovedBy != "" || p.ApprovedDigest != "" || p.JobID != "" {
		return ErrConflict
	}
	if !now.Before(p.ExpiresAt) || now.Before(p.CreatedAt) {
		return ErrExpired
	}
	if policy.RequireDistinctApprover && strings.EqualFold(user, p.CreatedBy) {
		return ErrPolicy
	}
	return policy.Authorize(user, p.Action)
}
