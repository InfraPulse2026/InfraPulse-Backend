package remediation

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPolicyDefaultDenyAndExactAssetBindingPortVLANScope(t *testing.T) {
	action := testAction()
	if err := (Policy{}).Validate(); err != nil || (Policy{}).Authorize("admin-1", action) == nil {
		t.Fatal("default policy grants remediation", err)
	}
	if err := testPolicy().Authorize("operator-1", action); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Policy){
		"wrong_asset":                  func(p *Policy) { p.Rules[0].AssetID = "asset-2" },
		"wrong_binding":                func(p *Policy) { p.Rules[0].BindingID = "binding-2" },
		"same_name_wrong_interface_id": func(p *Policy) { p.Rules[0].InterfaceID = "13" },
		"same_id_wrong_name":           func(p *Policy) { p.Rules[0].Interface = "GigabitEthernet0/13" },
		"unsupported_vendor":           func(p *Policy) { p.Rules[0].Platform = "arista.eos.eos" },
		"unknown_role":                 func(p *Policy) { p.Rules[0].Role = "" },
		"trunk":                        func(p *Policy) { p.Rules[0].Role = "trunk" },
		"uplink":                       func(p *Policy) { p.Rules[0].Role = "uplink" },
		"management":                   func(p *Policy) { p.Rules[0].Role = "management" },
		"protected_admin":              func(p *Policy) { p.Rules[0].Protected = true },
		"forbidden_vlan":               func(p *Policy) { p.Rules[0].AllowedVLANs = []int{40} },
		"management_target_vlan":       func(p *Policy) { p.Rules[0].ManagementVLANs = []int{20} },
		"management_current_vlan":      func(p *Policy) { p.Rules[0].ManagementVLANs = []int{30} },
		"operator_capability_removed":  func(p *Policy) { p.AllowedUsers = []string{"somebody-else"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			policy := testPolicy()
			mutate(&policy)
			if !errors.Is(policy.Authorize("admin-1", action), ErrPolicy) {
				t.Fatal("admin bypassed policy")
			}
		})
	}
	// PolicyDigest may be combined with existing protected-resource policy by
	// storage. Authorize must not compare it to this local policy's digest.
	action.PolicyDigest = strings.Repeat("c", 64)
	if testPolicy().Authorize("operator-1", action) != nil {
		t.Fatal("combined protected-resource policy digest rejected")
	}
}

func TestPolicyValidationRejectsAmbiguousRulesAndInvalidAllowlists(t *testing.T) {
	cases := map[string]func(*Policy){
		"missing_version":              func(p *Policy) { p.Version = "" },
		"missing_users":                func(p *Policy) { p.AllowedUsers = nil },
		"missing_rules":                func(p *Policy) { p.Rules = nil },
		"duplicate_users":              func(p *Policy) { p.AllowedUsers = append(p.AllowedUsers, p.AllowedUsers[0]) },
		"wildcard_users":               func(p *Policy) { p.AllowedUsers = []string{"*"} },
		"duplicate_rule":               func(p *Policy) { p.Rules = append(p.Rules, p.Rules[0]) },
		"no_vlan_allowlist":            func(p *Policy) { p.Rules[0].AllowedVLANs = nil },
		"duplicate_vlan":               func(p *Policy) { p.Rules[0].AllowedVLANs = []int{20, 20} },
		"invalid_vlan":                 func(p *Policy) { p.Rules[0].AllowedVLANs = []int{4095} },
		"negative_vlan":                func(p *Policy) { p.Rules[0].AllowedVLANs = []int{-1} },
		"missing_vlan_not_default_one": func(p *Policy) { p.Rules[0].AllowedVLANs = []int{0} },
		"invalid_management_vlan":      func(p *Policy) { p.Rules[0].ManagementVLANs = []int{0} },
		"management_overlap":           func(p *Policy) { p.Rules[0].ManagementVLANs = []int{20} },
		"wildcard_asset":               func(p *Policy) { p.Rules[0].AssetID = "*" },
		"missing_binding":              func(p *Policy) { p.Rules[0].BindingID = "" },
		"lag_name":                     func(p *Policy) { p.Rules[0].Interface = "Port-channel1" },
		"alias_name":                   func(p *Policy) { p.Rules[0].Interface = "Gi0/12" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			policy := testPolicy()
			mutate(&policy)
			if !errors.Is(policy.Validate(), ErrInvalid) {
				t.Fatal("invalid policy accepted")
			}
		})
	}
}

func TestPolicyDigestCanonicalAndDoesNotMutateAllowlists(t *testing.T) {
	policy := testPolicy()
	policy.Rules = append(policy.Rules, Rule{AssetID: "asset-2", BindingID: "binding-2", InterfaceID: "13", Interface: "GigabitEthernet0/13", Platform: Platform, Role: "access", AllowedVLANs: []int{40, 20}, ManagementVLANs: []int{100, 99}})
	before, _ := json.Marshal(policy)
	first := policy.Digest()
	if !ValidDigest(first) {
		t.Fatal("invalid policy digest")
	}
	after, _ := json.Marshal(policy)
	if string(before) != string(after) {
		t.Fatal("digest mutated source policy")
	}
	var reordered Policy
	if err := json.Unmarshal(before, &reordered); err != nil {
		t.Fatal(err)
	}
	reordered.AllowedUsers[0], reordered.AllowedUsers[1] = reordered.AllowedUsers[1], reordered.AllowedUsers[0]
	reordered.Rules[0], reordered.Rules[1] = reordered.Rules[1], reordered.Rules[0]
	reordered.Rules[0].AllowedVLANs = []int{20, 40}
	reordered.Rules[0].ManagementVLANs = []int{99, 100}
	if reordered.Digest() != first {
		t.Fatal("equivalent allowlist order changed policy digest")
	}
	for _, mutate := range []func(*Policy){
		func(p *Policy) { p.Enabled = false },
		func(p *Policy) { p.Version = "policy-v2" },
		func(p *Policy) { p.RequireDistinctApprover = true },
		func(p *Policy) { p.AllowedUsers = []string{"admin-1"} },
		func(p *Policy) { p.Rules[0].Protected = true },
		func(p *Policy) { p.Rules[0].AllowedVLANs = []int{20} },
		func(p *Policy) { p.Rules[0].ManagementVLANs = []int{98} },
	} {
		var changed Policy
		_ = json.Unmarshal(before, &changed)
		mutate(&changed)
		if changed.Digest() == first {
			t.Fatal("changed policy retained approval digest")
		}
	}
	if (Policy{}).Digest() != (Policy{AllowedUsers: []string{}, Rules: []Rule{}}).Digest() {
		t.Fatal("equivalent disabled policies have different digest")
	}
}

func TestLoadPolicyStrictBoundedLocalJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remediation-policy.json")
	write := func(raw []byte) {
		t.Helper()
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	write(raw)
	loaded, err := LoadPolicy(path)
	if err != nil || !reflect.DeepEqual(loaded, testPolicy()) {
		t.Fatal("valid policy did not load", err)
	}
	for name, invalid := range map[string][]byte{
		"unknown_field":         []byte(`{"enabled":false,"execute_shell":true}`),
		"unknown_rule_field":    []byte(strings.Replace(string(raw), `"role":"access"`, `"role":"access","extra_vars":{}`, 1)),
		"trailing_json":         append(append([]byte{}, raw...), []byte(` {}`)...),
		"malformed":             []byte(`{"enabled":`),
		"array":                 []byte(`[]`),
		"null":                  []byte(`null`),
		"oversized":             []byte(strings.Repeat(" ", MaxPolicyFileBytes+1)),
		"enabled_no_capability": []byte(`{"enabled":true}`),
	} {
		t.Run(name, func(t *testing.T) {
			write(invalid)
			if _, err := LoadPolicy(path); !errors.Is(err, ErrInvalid) {
				t.Fatal("invalid policy loaded", err)
			}
		})
	}
	if _, err := LoadPolicy("relative-policy.json"); !errors.Is(err, ErrInvalid) {
		t.Fatal("relative path accepted", err)
	}
	if _, err := LoadPolicy(t.TempDir()); !errors.Is(err, ErrInvalid) {
		t.Fatal("directory accepted", err)
	}
	if _, err := LoadPolicy(filepath.Join(t.TempDir(), "absent")); !errors.Is(err, ErrInvalid) {
		t.Fatal("missing policy grants permission", err)
	}
}
