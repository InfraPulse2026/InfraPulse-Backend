package configdrift

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

const approvedConfig = "hostname access-switch\ninterface GigabitEthernet0/12\n description Voice access\n switchport mode access\n switchport access vlan 20\n no shutdown\n!\nend\n"

func TestConfigurationDriftVLANAndBoundedProposal(t *testing.T) {
	actual := strings.Replace(approvedConfig, "vlan 20", "vlan 30", 1)
	out := Compare("cisco_ios_read_config_v1", approvedConfig, actual)
	if out.Status != "drift" || out.TotalChanges != 1 || out.Changes[0].Kind != "access_vlan_changed" || *out.Changes[0].ExpectedVLAN != 20 || *out.Changes[0].ActualVLAN != 30 || out.Proposal == nil || !out.Proposal.ExecutionAvailable || !out.Proposal.ApprovalRequired || out.Proposal.Limitation != "requires_enabled_policy_and_explicit_admin_approval" {
		t.Fatal(out)
	}
	for _, config := range []string{strings.Replace(approvedConfig, "switchport mode access", "switchport mode trunk", 1), strings.Replace(approvedConfig, "GigabitEthernet0/12", "Port-Channel1", 1), strings.Replace(approvedConfig, "switchport mode access", "no switchport\n switchport mode access", 1)} {
		out = Compare("cisco_ios_read_config_v1", config, strings.Replace(config, "vlan 20", "vlan 30", 1))
		if out.Proposal != nil {
			t.Fatal("unsafe interface proposal", out)
		}
	}
	actual = "username unrelated secret changed\n" + actual
	out = Compare("cisco_ios_read_config_v1", approvedConfig, actual)
	if out.Proposal != nil || out.TotalChanges != 2 {
		t.Fatal("proposal ignores unrelated drift", out)
	}
}
func TestConfigurationDriftNoiseAndIndependentOrdering(t *testing.T) {
	actual := "Building configuration...\r\nCurrent configuration : 900 bytes\r\n! Last configuration change at 12:00\r\nhostname access-switch\r\ninterface GigabitEthernet0/12\r\n no shutdown\r\n switchport access vlan 20\r\n switchport mode access\r\n description Voice access\r\nend\r\n"
	out := Compare("cisco_ios_read_config_v1", approvedConfig, actual)
	if out.Status != "compliant" || out.ExpectedDigest != out.ActualDigest {
		t.Fatal(out)
	}
	a := "hostname sw\ninterface Ethernet1\n switchport mode access\n switchport access vlan 20\ninterface Ethernet2\n shutdown\n"
	b := "hostname sw\ninterface Ethernet2\n shutdown\ninterface Ethernet1\n switchport access vlan 20\n switchport mode access\n"
	if out = Compare("arista_eos_read_config_v1", a, b); out.Status != "compliant" {
		t.Fatal("interface block reorder", out)
	}
}
func TestConfigurationDriftPreservesOrderAndMasksSecrets(t *testing.T) {
	for _, pair := range [][2]string{
		{"ip access-list extended VOICE\n permit udp any any\n deny ip any any\n", "ip access-list extended VOICE\n deny ip any any\n permit udp any any\n"},
		{"interface Ethernet1\n switchport trunk allowed vlan add 20\n switchport trunk allowed vlan remove 30\n", "interface Ethernet1\n switchport trunk allowed vlan remove 30\n switchport trunk allowed vlan add 20\n"},
		{"interface Ethernet1\n switchport access vlan 20\n switchport access vlan 30\n", "interface Ethernet1\n switchport access vlan 30\n switchport access vlan 20\n"},
	} {
		out := Compare("cisco_ios_read_config_v1", pair[0], pair[1])
		if out.Status != "drift" || out.Proposal != nil {
			t.Fatal("ordered/repeated commands silently normalized", out)
		}
	}
	out := Compare("cisco_ios_read_config_v1", "username owner secret TOP_SECRET_OLD\n", "username owner secret TOP_SECRET_NEW\n")
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "TOP_SECRET") || !out.Changes[0].Redacted {
		t.Fatal("secret leaked through diff", string(raw))
	}
	if out = Compare("vyos_read_config_v1", "set firewall rule 10 action accept\nset firewall rule 20 action drop\n", "set firewall rule 20 action drop\nset firewall rule 10 action accept\n"); out.Status != "drift" {
		t.Fatal("VyOS ordering was assumed harmless")
	}
}
func TestConfigurationDriftLimitsAndUnsupportedAreNotCompliant(t *testing.T) {
	for _, invalid := range []string{"", "Building configuration...\n!\nend\n", "interface Ethernet1\n shutdown\ninterface Ethernet1\n no shutdown\n", strings.Repeat("x", maxLineBytes+1), strings.Repeat("hostname x\n", maxLines+1), "hostname x\x00"} {
		out := Compare("cisco_ios_read_config_v1", invalid, invalid)
		if out.Status != "unknown" || out.Proposal != nil {
			t.Fatal(out)
		}
	}
	var expected, actual strings.Builder
	for i := 0; i < MaxChanges+5; i++ {
		name := strconv.Itoa(i + 1)
		expected.WriteString("interface Ethernet" + name + "\n shutdown\n")
		actual.WriteString("interface Ethernet" + name + "\n no shutdown\n")
	}
	out := Compare("arista_eos_read_config_v1", expected.String(), actual.String())
	if out.Status != "drift" || !out.Truncated || out.TotalChanges != MaxChanges+5 || len(out.Changes) != MaxChanges {
		t.Fatal(out)
	}
}

func TestConfigurationDriftOpaqueBannerDoesNotBecomeCommandsOrNoise(t *testing.T) {
	configuration := "hostname sw\nbanner motd ^C\n! IMPORTANT_BANNER_TEXT\ninterface Ethernet999\n^C\ninterface Ethernet1\n shutdown\n"
	out := Compare("cisco_ios_read_config_v1", configuration, strings.Replace(configuration, "IMPORTANT_BANNER_TEXT", "CHANGED_BANNER_TEXT", 1))
	if out.Status != "drift" || out.TotalChanges != 1 || out.Changes[0].Path != "/global" || out.Proposal != nil {
		t.Fatal("banner content interpreted as comments/commands", out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "BANNER_TEXT") {
		t.Fatal("banner text leaked")
	}
	if out = Compare("cisco_ios_read_config_v1", "banner motd ^C\nunterminated\n", "banner motd ^C\nunterminated\n"); out.Status != "unknown" {
		t.Fatal("incomplete banner normalized", out)
	}
}
