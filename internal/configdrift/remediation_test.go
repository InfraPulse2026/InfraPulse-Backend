package configdrift

import "testing"

func TestRemediationPolicyAndApprovalValidation(t *testing.T) {
	valid := PolicyInput{Enabled: true, AllowedVLANs: []int{20}, ProtectedInterfaces: []string{"GigabitEthernet0/1"}, MaxEvidenceAgeSeconds: 900, Reason: "reviewed policy"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []PolicyInput{
		{Enabled: true, MaxEvidenceAgeSeconds: 900, Reason: "missing allowlist"},
		{Enabled: true, AllowedVLANs: []int{20, 20}, MaxEvidenceAgeSeconds: 900, Reason: "duplicate VLAN"},
		{Enabled: true, AllowedVLANs: []int{4095}, MaxEvidenceAgeSeconds: 900, Reason: "invalid VLAN"},
		{Enabled: true, AllowedVLANs: []int{20}, ProtectedInterfaces: []string{"Gi1", "Gi1"}, MaxEvidenceAgeSeconds: 900, Reason: "duplicate interface"},
	} {
		if bad.Validate() == nil {
			t.Fatal("invalid policy accepted", bad)
		}
	}
	if (RemediationInput{DriftResultID: 1, PolicyRevision: 1, Reason: "reviewed request"}).Validate() != nil {
		t.Fatal("valid request rejected")
	}
	if (ApprovalInput{ExpectedRevision: 1, Confirmed: true, TimeoutSeconds: 90, Reason: "reviewed approval"}).Validate() != nil {
		t.Fatal("valid approval rejected")
	}
	if (ApprovalInput{ExpectedRevision: 1, Confirmed: false, TimeoutSeconds: 90, Reason: "not confirmed"}).Validate() == nil {
		t.Fatal("unconfirmed approval accepted")
	}
	if (CancelInput{ExpectedRevision: 1, Reason: "cancel reviewed request"}).Validate() != nil {
		t.Fatal("valid cancellation rejected")
	}
}
