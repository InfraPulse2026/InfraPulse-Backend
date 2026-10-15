package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	au "github.com/InfraPulse2026/InfraPulse-Backend/internal/automation"
	cd "github.com/InfraPulse2026/InfraPulse-Backend/internal/configdrift"
)

func remediationFixture(t *testing.T) (*Store, context.Context, au.Target, cd.Record) {
	t.Helper()
	s, ctx, _, target := automationFixture(t)
	expected := createDriftSnapshot(t, s, ctx, target, driftApproved, "remediation-expected")
	approveDriftBaseline(t, s, ctx, target, expected)
	createDriftSnapshot(t, s, ctx, target, strings.Replace(driftApproved, "vlan 20", "vlan 30", 1), "remediation-drift")
	state, err := s.ConfigurationDrift(ctx, target.AssetID)
	if err != nil || state.Latest == nil || state.Latest.Proposal == nil {
		t.Fatal(state, err)
	}
	return s, ctx, target, *state.Latest
}

func TestConfigurationRemediationPolicyApprovalExecutionAndVerification(t *testing.T) {
	s, ctx, target, drift := remediationFixture(t)
	policy, err := s.PutConfigurationRemediationPolicy(ctx, target.AssetID, cd.PolicyInput{ExpectedRevision: 0, Enabled: true, AllowedVLANs: []int{20, 40}, ProtectedInterfaces: []string{"GigabitEthernet0/1"}, MaxEvidenceAgeSeconds: 900, Reason: "approved lab remediation boundary"})
	if err != nil || policy.Revision != 1 {
		t.Fatal(policy, err)
	}
	in := cd.RemediationInput{DriftResultID: drift.ID, PolicyRevision: policy.Revision, Reason: "restore reviewed access VLAN"}
	request, replay, err := s.CreateConfigurationRemediation(ctx, target.AssetID, in, "remediation-request-one")
	if err != nil || replay || request.State != "pending_approval" || request.ExpectedVLAN != 20 || request.ActualVLAN != 30 {
		t.Fatal(request, replay, err)
	}
	again, replay, err := s.CreateConfigurationRemediation(ctx, target.AssetID, in, "remediation-request-one")
	if err != nil || !replay || again.ID != request.ID {
		t.Fatal("request replay", again, replay, err)
	}
	approved, replay, err := s.ApproveConfigurationRemediation(ctx, request.ID, cd.ApprovalInput{ExpectedRevision: request.Revision, Confirmed: true, TimeoutSeconds: 90, Reason: "approved after checking port and VLAN"}, "remediation-approve-one")
	if err != nil || replay || approved.State != "queued" || approved.AutomationJobID == "" {
		t.Fatal(approved, replay, err)
	}
	claimed, err := s.ClaimAutomation(ctx, "worker_one")
	if err != nil || claimed.ID != approved.AutomationJobID || claimed.Target.Operation == nil || claimed.Target.Operation.RemediationID != request.ID || claimed.Target.Operation.Interface != "GigabitEthernet0/12" || claimed.Target.Operation.VLAN != 20 {
		t.Fatal("closed operation was not claimed", claimed, err)
	}
	if err = s.FinishAutomation(ctx, claimed, "succeeded", au.Result{Configuration: driftApproved, Facts: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	finished, err := s.ConfigurationRemediation(ctx, request.ID)
	if err != nil || finished.State != "verified" || finished.ErrorCode != "" || finished.VerificationID == 0 {
		t.Fatal(finished, err)
	}
	state, err := s.ConfigurationDrift(ctx, target.AssetID)
	if err != nil || state.Status != "compliant" || state.Latest == nil || state.Latest.ID != finished.VerificationID {
		t.Fatal("post-change recollection was not verified", state, err)
	}
}

func TestConfigurationRemediationRejectsPolicyChangeProtectedAndStaleEvidence(t *testing.T) {
	for _, scenario := range []string{"policy_changed", "policy_changed_after_approval", "protected", "stale"} {
		t.Run(scenario, func(t *testing.T) {
			s, ctx, target, drift := remediationFixture(t)
			protected := []string{}
			if scenario == "protected" {
				protected = []string{"GigabitEthernet0/12"}
			}
			policy, err := s.PutConfigurationRemediationPolicy(ctx, target.AssetID, cd.PolicyInput{ExpectedRevision: 0, Enabled: true, AllowedVLANs: []int{20}, ProtectedInterfaces: protected, MaxEvidenceAgeSeconds: 900, Reason: "reviewed remediation boundary"})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "stale" {
				if _, err = s.pool.Exec(ctx, `UPDATE configuration_drift_results SET actual_observed_at=now()-interval '1 hour' WHERE id=$1`, drift.ID); err != nil {
					t.Fatal(err)
				}
			}
			request, _, err := s.CreateConfigurationRemediation(ctx, target.AssetID, cd.RemediationInput{DriftResultID: drift.ID, PolicyRevision: policy.Revision, Reason: "request bounded repair"}, "request-"+scenario)
			if scenario == "protected" || scenario == "stale" {
				if !errors.Is(err, cd.ErrConflict) {
					t.Fatal("unsafe request accepted", request, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "policy_changed_after_approval" {
				approved, _, e := s.ApproveConfigurationRemediation(ctx, request.ID, cd.ApprovalInput{ExpectedRevision: request.Revision, Confirmed: true, TimeoutSeconds: 90, Reason: "approve before policy is frozen"}, "approve-before-policy-change")
				if e != nil || approved.State != "queued" {
					t.Fatal(approved, e)
				}
			}
			_, err = s.PutConfigurationRemediationPolicy(ctx, target.AssetID, cd.PolicyInput{ExpectedRevision: policy.Revision, Enabled: false, AllowedVLANs: []int{20}, MaxEvidenceAgeSeconds: 900, Reason: "freeze remediation before execution"})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "policy_changed_after_approval" {
				claimed, e := s.ClaimAutomation(ctx, "worker_one")
				if e != nil || claimed.ID != "" {
					t.Fatal("worker executed after policy change", claimed, e)
				}
				failed, e := s.ConfigurationRemediation(ctx, request.ID)
				if e != nil || failed.State != "failed" || failed.ErrorCode != "authorization_or_target_changed" {
					t.Fatal(failed, e)
				}
				return
			}
			if _, _, err = s.ApproveConfigurationRemediation(ctx, request.ID, cd.ApprovalInput{ExpectedRevision: request.Revision, Confirmed: true, TimeoutSeconds: 90, Reason: "must fail after policy change"}, "approve-policy-changed"); !errors.Is(err, cd.ErrConflict) {
				t.Fatal("changed policy accepted", err)
			}
		})
	}
}

func TestConfigurationRemediationCancellationReleasesAsset(t *testing.T) {
	s, ctx, target, drift := remediationFixture(t)
	policy, err := s.PutConfigurationRemediationPolicy(ctx, target.AssetID, cd.PolicyInput{ExpectedRevision: 0, Enabled: true, AllowedVLANs: []int{20}, MaxEvidenceAgeSeconds: 900, Reason: "reviewed cancellation policy"})
	if err != nil {
		t.Fatal(err)
	}
	request, _, err := s.CreateConfigurationRemediation(ctx, target.AssetID, cd.RemediationInput{DriftResultID: drift.ID, PolicyRevision: policy.Revision, Reason: "request to be cancelled"}, "cancel-request-one")
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.CancelConfigurationRemediation(ctx, request.ID, cd.CancelInput{ExpectedRevision: request.Revision, Reason: "maintenance window was withdrawn"})
	if err != nil || cancelled.State != "cancelled" || cancelled.ErrorCode != "cancelled" {
		t.Fatal(cancelled, err)
	}
	second, _, err := s.CreateConfigurationRemediation(ctx, target.AssetID, cd.RemediationInput{DriftResultID: drift.ID, PolicyRevision: policy.Revision, Reason: "replacement reviewed request"}, "cancel-request-two")
	if err != nil || second.State != "pending_approval" {
		t.Fatal("cancelled request still blocks Asset", second, err)
	}
}
