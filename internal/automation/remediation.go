package automation

import "regexp"

const RestoreTemplateID = "cisco_ios_restore_access_vlan_v1"

// The template is separate from the public read-only template lookup. Ordinary
// B10 jobs cannot select a write workflow by supplying its ID.
func RestoreTemplate() Template {
	return Template{ID: RestoreTemplateID, Platform: "cisco.ios.ios", Collection: "cisco.ios", ReadOnly: false}
}

// Pin the exact compiled adapter artifact, including its precondition and
// reviewed module arguments. Updating that artifact invalidates old approval.
func RestoreTemplateDigest() string { return Digest(string(runnerScript)) }

type RestoreAccessVLAN struct {
	TemplateID     string `json:"template_id"`
	TemplateDigest string `json:"template_digest"`
	Interface      string `json:"interface"`
	CurrentVLAN    int    `json:"current_vlan"`
	TargetVLAN     int    `json:"target_vlan"`
}

var restorePhysicalInterface = regexp.MustCompile(`^(?:GigabitEthernet|TenGigabitEthernet|TwentyFiveGigE|FortyGigabitEthernet|HundredGigE|FastEthernet|Ethernet)[0-9]+(?:/[0-9]+){0,3}$`)

func (v RestoreAccessVLAN) Validate() error {
	if v.TemplateID != RestoreTemplateID || v.TemplateDigest != RestoreTemplateDigest() || len(v.Interface) > 64 || !restorePhysicalInterface.MatchString(v.Interface) || v.CurrentVLAN < 1 || v.CurrentVLAN > 4094 || v.TargetVLAN < 1 || v.TargetVLAN > 4094 || v.CurrentVLAN == v.TargetVLAN {
		return ErrInvalid
	}
	return nil
}
