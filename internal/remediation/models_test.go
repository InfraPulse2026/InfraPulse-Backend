package remediation

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testAction() Action {
	digest := strings.Repeat("a", 64)
	return Action{ActionType: ActionType, AssetID: "asset-1", AssetRevision: 2, BindingID: "binding-1", BindingDigest: digest, TargetRevision: 3, TargetDigest: digest, InterfaceID: "12", Interface: "GigabitEthernet0/12", DriftID: 4, BaselineRevision: 5, ExpectedSnapshotID: "snapshot-expected", ActualSnapshotID: "snapshot-actual", ExpectedDigest: digest, ObservedDigest: strings.Repeat("b", 64), CurrentVLAN: 30, TargetVLAN: 20, TemplateID: TemplateID, TemplateDigest: digest, PolicyDigest: digest}
}

func testPolicy() Policy {
	return Policy{Enabled: true, Version: "policy-v1", AllowedUsers: []string{"operator-1", "admin-1"}, Rules: []Rule{{AssetID: "asset-1", BindingID: "binding-1", InterfaceID: "12", Interface: "GigabitEthernet0/12", Platform: Platform, Role: "access", AllowedVLANs: []int{20, 40}, ManagementVLANs: []int{99}}}}
}

func testProposal() Proposal {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	action := testAction()
	return Proposal{ID: "proposal-1", Action: action, Digest: Fingerprint(action), State: "proposed", CreatedBy: "admin-1", CreatedAt: now, ExpiresAt: now.Add(ApprovalTTL), AssetName: "SW-ACCESS-02", Warnings: []string{"device_change_is_not_atomic"}}
}

func TestActionFingerprintStableAndBindsEveryCriticalParameter(t *testing.T) {
	action := testAction()
	if action.Validate() != nil || !ValidDigest(Fingerprint(action)) || Fingerprint(action) != Fingerprint(action) {
		t.Fatal("valid action does not have a stable digest")
	}
	raw, err := json.Marshal(action)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Action
	if err := json.Unmarshal(raw, &decoded); err != nil || Fingerprint(action) != Fingerprint(decoded) {
		t.Fatal("action round trip changed fingerprint", err)
	}
	for i := 0; i < reflect.TypeOf(action).NumField(); i++ {
		field := reflect.TypeOf(action).Field(i)
		t.Run(field.Name, func(t *testing.T) {
			changed := action
			value := reflect.ValueOf(&changed).Elem().Field(i)
			switch value.Kind() {
			case reflect.String:
				value.SetString(value.String() + "x")
			case reflect.Int64, reflect.Int:
				value.SetInt(value.Int() + 1)
			default:
				t.Fatal("new action field needs an immutability test")
			}
			if Fingerprint(action) == Fingerprint(changed) {
				t.Fatal("action parameter was not bound")
			}
			proposal := testProposal()
			proposal.Action = changed
			if !errors.Is(proposal.ValidateBinding(), ErrConflict) {
				t.Fatal("mutated action retained approval binding")
			}
		})
	}
}

func TestActionRejectsUnboundedOrUnsafeParameters(t *testing.T) {
	cases := map[string]func(*Action){
		"unsupported_action":       func(a *Action) { a.ActionType = "restore_running_config" },
		"unknown_template":         func(a *Action) { a.TemplateID = "unknown-template" },
		"arbitrary_playbook":       func(a *Action) { a.TemplateID = "../../playbook.yml" },
		"wildcard_asset":           func(a *Action) { a.AssetID = "*" },
		"host_expression_as_asset": func(a *Action) { a.AssetID = "all:!production" },
		"missing_binding":          func(a *Action) { a.BindingID = "" },
		"missing_expected":         func(a *Action) { a.ExpectedSnapshotID = "" },
		"missing_actual":           func(a *Action) { a.ActualSnapshotID = "" },
		"unknown_asset_revision":   func(a *Action) { a.AssetRevision = 0 },
		"unknown_target_revision":  func(a *Action) { a.TargetRevision = 0 },
		"unknown_baseline":         func(a *Action) { a.BaselineRevision = 0 },
		"missing_drift":            func(a *Action) { a.DriftID = 0 },
		"management":               func(a *Action) { a.Interface = "Management0" },
		"lag":                      func(a *Action) { a.Interface = "Port-channel1" },
		"logical_subinterface":     func(a *Action) { a.Interface = "GigabitEthernet0/12.20" },
		"unresolved_alias":         func(a *Action) { a.Interface = "Gi0/12" },
		"interface_injection":      func(a *Action) { a.Interface = "GigabitEthernet0/12; reboot" },
		"interface_id_injection":   func(a *Action) { a.InterfaceID = "12\nshutdown" },
		"wildcard_interface_id":    func(a *Action) { a.InterfaceID = "*" },
		"vlan_zero":                func(a *Action) { a.TargetVLAN = 0 },
		"negative_vlan":            func(a *Action) { a.TargetVLAN = -1 },
		"vlan_too_high":            func(a *Action) { a.TargetVLAN = 4095 },
		"missing_current_vlan":     func(a *Action) { a.CurrentVLAN = 0 },
		"no_action":                func(a *Action) { a.CurrentVLAN = a.TargetVLAN },
		"malformed_digest":         func(a *Action) { a.ExpectedDigest = "digest" },
		"uppercase_digest":         func(a *Action) { a.TemplateDigest = strings.Repeat("A", 64) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			action := testAction()
			mutate(&action)
			if !errors.Is(action.Validate(), ErrInvalid) {
				t.Fatal("unsafe action accepted")
			}
			if !errors.Is(testPolicy().Authorize("admin-1", action), ErrPolicy) {
				t.Fatal("unsafe action authorized")
			}
		})
	}
}

func TestApprovalRequiresExactFreshOneTimeHumanDecision(t *testing.T) {
	proposal := testProposal()
	input := ApproveInput{Confirmed: true, Digest: proposal.Digest, Reason: "restore approved VLAN"}
	if err := proposal.ValidateApproval("operator-1", input, proposal.CreatedAt.Add(time.Second), testPolicy()); err != nil {
		t.Fatal("valid human approval rejected", err)
	}
	cases := []struct {
		name   string
		mutate func(*Proposal, *ApproveInput, *Policy, *time.Time, *string)
		want   error
	}{
		{"confirmation_missing", func(_ *Proposal, in *ApproveInput, _ *Policy, _ *time.Time, _ *string) { in.Confirmed = false }, ErrInvalid},
		{"wrong_digest", func(_ *Proposal, in *ApproveInput, _ *Policy, _ *time.Time, _ *string) {
			in.Digest = strings.Repeat("b", 64)
		}, ErrConflict},
		{"changed_expected_snapshot", func(p *Proposal, _ *ApproveInput, _ *Policy, _ *time.Time, _ *string) {
			p.Action.ExpectedSnapshotID = "new-expected"
		}, ErrConflict},
		{"changed_actual_configuration", func(p *Proposal, _ *ApproveInput, _ *Policy, _ *time.Time, _ *string) { p.Action.CurrentVLAN = 40 }, ErrConflict},
		{"changed_expected_vlan", func(p *Proposal, _ *ApproveInput, _ *Policy, _ *time.Time, _ *string) { p.Action.TargetVLAN = 40 }, ErrConflict},
		{"expired", func(p *Proposal, _ *ApproveInput, _ *Policy, now *time.Time, _ *string) { *now = p.ExpiresAt }, ErrExpired},
		{"future_proposal", func(p *Proposal, _ *ApproveInput, _ *Policy, now *time.Time, _ *string) {
			*now = p.CreatedAt.Add(-time.Second)
		}, ErrExpired},
		{"approval_transfer", func(p *Proposal, _ *ApproveInput, _ *Policy, _ *time.Time, _ *string) {
			p.ApprovedDigest = strings.Repeat("b", 64)
		}, ErrConflict},
		{"already_approved", func(p *Proposal, _ *ApproveInput, _ *Policy, _ *time.Time, _ *string) { p.State = "approved" }, ErrConflict},
		{"already_queued", func(p *Proposal, _ *ApproveInput, _ *Policy, _ *time.Time, _ *string) { p.JobID = "job-1" }, ErrConflict},
		{"approval_timestamp", func(p *Proposal, _ *ApproveInput, _ *Policy, now *time.Time, _ *string) { p.ApprovedAt = now }, ErrConflict},
		{"rejected", func(p *Proposal, _ *ApproveInput, _ *Policy, _ *time.Time, _ *string) { p.State = "rejected" }, ErrConflict},
		{"stale_drift", func(p *Proposal, _ *ApproveInput, _ *Policy, _ *time.Time, _ *string) { p.State = "stale" }, ErrConflict},
		{"viewer_without_permission", func(_ *Proposal, _ *ApproveInput, _ *Policy, _ *time.Time, user *string) { *user = "viewer-1" }, ErrPolicy},
		{"operator_without_capability", func(_ *Proposal, _ *ApproveInput, policy *Policy, _ *time.Time, _ *string) {
			policy.AllowedUsers = []string{"admin-1"}
		}, ErrPolicy},
		{"policy_disabled", func(_ *Proposal, _ *ApproveInput, policy *Policy, _ *time.Time, _ *string) { policy.Enabled = false }, ErrPolicy},
		{"distinct_approver", func(_ *Proposal, _ *ApproveInput, policy *Policy, _ *time.Time, user *string) {
			policy.RequireDistinctApprover = true
			*user = "admin-1"
		}, ErrPolicy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, in, policy, now, user := testProposal(), input, testPolicy(), proposal.CreatedAt.Add(time.Second), "operator-1"
			tc.mutate(&p, &in, &policy, &now, &user)
			if err := p.ValidateApproval(user, in, now, policy); !errors.Is(err, tc.want) {
				t.Fatalf("wanted %v, got %v", tc.want, err)
			}
		})
	}
}

func TestRequestInputValidation(t *testing.T) {
	if (CreateInput{DriftID: 1, Reason: "create proposal"}).Validate() != nil || (RejectInput{Reason: "do not modify this port"}).Validate() != nil {
		t.Fatal("valid request rejected")
	}
	for _, reason := range []string{"", "ab", "unsafe\nreason", "secret\x00reason", strings.Repeat("x", 513)} {
		if (CreateInput{DriftID: 1, Reason: reason}).Validate() == nil || (RejectInput{Reason: reason}).Validate() == nil || (ApproveInput{Confirmed: true, Digest: strings.Repeat("a", 64), Reason: reason}).Validate() == nil {
			t.Fatal("invalid reason accepted")
		}
	}
	if (CreateInput{DriftID: 0, Reason: "create proposal"}).Validate() == nil {
		t.Fatal("missing registered drift accepted")
	}
}

func TestReconcileInputRequiresExplicitReadOnlySnapshotDecision(t *testing.T) {
	if (ReconcileInput{SnapshotID: "snapshot-read-only", Confirmed: true, Reason: "review unknown execution evidence"}).Validate() != nil {
		t.Fatal("valid reconciliation rejected")
	}
	for _, input := range []ReconcileInput{
		{SnapshotID: "", Confirmed: true, Reason: "review evidence"},
		{SnapshotID: "../snapshot", Confirmed: true, Reason: "review evidence"},
		{SnapshotID: "*", Confirmed: true, Reason: "review evidence"},
		{SnapshotID: "snapshot-read-only", Confirmed: false, Reason: "review evidence"},
		{SnapshotID: "snapshot-read-only", Confirmed: true, Reason: ""},
	} {
		if !errors.Is(input.Validate(), ErrInvalid) {
			t.Fatal("unsafe reconciliation accepted", input)
		}
	}
}
