package remediation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	au "github.com/InfraPulse2026/InfraPulse-Backend/internal/automation"
	cd "github.com/InfraPulse2026/InfraPulse-Backend/internal/configdrift"
)

const MaxPolicyFileBytes = 64 << 10

// Policy is an explicit local capability. Neither admin nor an operator role
// alone grants it. Registry identity, fresh role/protection and template checks
// are also required by storage at proposal, approval, queue and worker start.
type Policy struct {
	Enabled                 bool     `json:"enabled"`
	Version                 string   `json:"version"`
	AllowedUsers            []string `json:"allowed_users"`
	RequireDistinctApprover bool     `json:"require_distinct_approver"`
	Rules                   []Rule   `json:"rules"`
}

type Rule struct {
	AssetID         string `json:"asset_id"`
	BindingID       string `json:"binding_id"`
	InterfaceID     string `json:"interface_id"`
	Interface       string `json:"interface"`
	Platform        string `json:"platform"`
	Role            string `json:"role"`
	Protected       bool   `json:"protected"`
	AllowedVLANs    []int  `json:"allowed_vlans"`
	ManagementVLANs []int  `json:"management_vlans"`
}

func validVLANList(values []int) bool {
	if len(values) > 4094 {
		return false
	}
	seen := map[int]bool{}
	for _, value := range values {
		if value < 1 || value > 4094 || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func (p Policy) Validate() error {
	if len(p.AllowedUsers) > 1000 || len(p.Rules) > 1000 || (p.Enabled && (!au.Identifier(p.Version) || len(p.AllowedUsers) == 0 || len(p.Rules) == 0)) || (p.Version != "" && !au.Identifier(p.Version)) {
		return ErrInvalid
	}
	users := map[string]bool{}
	for _, user := range p.AllowedUsers {
		if !au.Identifier(user) || users[user] {
			return ErrInvalid
		}
		users[user] = true
	}
	rules := map[string]bool{}
	for _, rule := range p.Rules {
		key := rule.AssetID + "\x00" + rule.BindingID + "\x00" + rule.InterfaceID
		if !au.Identifier(rule.AssetID) || !au.Identifier(rule.BindingID) || !interfaceIDPattern.MatchString(rule.InterfaceID) || !cd.PhysicalInterfaceName(rule.Interface) || rule.Platform != Platform || rule.Role != "access" || len(rule.AllowedVLANs) == 0 || !validVLANList(rule.AllowedVLANs) || !validVLANList(rule.ManagementVLANs) || rules[key] {
			return ErrInvalid
		}
		rules[key] = true
		for _, vlan := range rule.AllowedVLANs {
			if containsVLAN(rule.ManagementVLANs, vlan) {
				return ErrInvalid
			}
		}
	}
	return nil
}

// Digest canonicalizes copied slices. Reordering equivalent allowlists does not
// invalidate approvals, but every security-relevant policy change does.
func (p Policy) Digest() string {
	copy := p
	copy.AllowedUsers = append([]string{}, p.AllowedUsers...)
	copy.Rules = append([]Rule{}, p.Rules...)
	sort.Strings(copy.AllowedUsers)
	for i := range copy.Rules {
		copy.Rules[i].AllowedVLANs = append([]int{}, p.Rules[i].AllowedVLANs...)
		copy.Rules[i].ManagementVLANs = append([]int{}, p.Rules[i].ManagementVLANs...)
		sort.Ints(copy.Rules[i].AllowedVLANs)
		sort.Ints(copy.Rules[i].ManagementVLANs)
	}
	sort.Slice(copy.Rules, func(i, j int) bool {
		a, b := copy.Rules[i], copy.Rules[j]
		return a.AssetID+"\x00"+a.BindingID+"\x00"+a.InterfaceID < b.AssetID+"\x00"+b.BindingID+"\x00"+b.InterfaceID
	})
	raw, _ := json.Marshal(copy)
	return au.Digest(string(raw))
}

func containsVLAN(values []int, vlan int) bool {
	for _, value := range values {
		if value == vlan {
			return true
		}
	}
	return false
}

func (p Policy) Authorize(user string, action Action) error {
	if !p.Enabled || p.Validate() != nil || action.Validate() != nil {
		return ErrPolicy
	}
	allowed := false
	for _, candidate := range p.AllowedUsers {
		if candidate == user {
			allowed = true
			break
		}
	}
	if !allowed {
		return ErrPolicy
	}
	for _, rule := range p.Rules {
		if rule.AssetID == action.AssetID && rule.BindingID == action.BindingID && rule.InterfaceID == action.InterfaceID && rule.Interface == action.Interface && rule.Platform == Platform && rule.Role == "access" && !rule.Protected && containsVLAN(rule.AllowedVLANs, action.TargetVLAN) && !containsVLAN(rule.ManagementVLANs, action.TargetVLAN) && !containsVLAN(rule.ManagementVLANs, action.CurrentVLAN) {
			return nil
		}
	}
	return ErrPolicy
}

// LoadPolicy reads only a bounded, regular local file. Unknown fields and a
// second JSON value are errors; an absent file is never treated as permission.
func LoadPolicy(path string) (Policy, error) {
	if !filepath.IsAbs(path) {
		return Policy{}, fmt.Errorf("%w: policy path must be absolute", ErrInvalid)
	}
	file, err := os.Open(path)
	if err != nil {
		return Policy{}, fmt.Errorf("%w: policy file unavailable", ErrInvalid)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxPolicyFileBytes {
		return Policy{}, fmt.Errorf("%w: policy file must be regular and bounded", ErrInvalid)
	}
	raw, err := io.ReadAll(io.LimitReader(file, MaxPolicyFileBytes+1))
	if err != nil || len(raw) > MaxPolicyFileBytes {
		return Policy{}, fmt.Errorf("%w: policy file exceeds size limit", ErrInvalid)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Policy{}, fmt.Errorf("%w: policy must be a JSON object", ErrInvalid)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var policy Policy
	if err := decoder.Decode(&policy); err != nil {
		return Policy{}, fmt.Errorf("%w: invalid policy JSON", ErrInvalid)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Policy{}, fmt.Errorf("%w: trailing policy JSON", ErrInvalid)
	}
	if err := policy.Validate(); err != nil {
		return Policy{}, err
	}
	return policy, nil
}
