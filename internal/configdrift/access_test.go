package configdrift

import (
	"errors"
	"strings"
	"testing"
)

func TestAccessPortStateUsesB11NormalizerAndExactCanonicalPhysicalPort(t *testing.T) {
	configuration := approvedConfig
	port, err := AccessPortState("cisco_ios_read_config_v1", configuration, "GigabitEthernet0/12")
	if err != nil || port.Name != "GigabitEthernet0/12" || port.VLAN != 20 || !port.Safe || !port.Access {
		t.Fatal(port, err)
	}
	configuration = strings.Replace(configuration, " no shutdown", " switchport\n mtu 1500\n speed 1000\n duplex full\n no shutdown", 1)
	if _, err := AccessPortState("cisco_ios_read_config_v1", configuration, "GigabitEthernet0/12"); err != nil {
		t.Fatal("known scalar access settings rejected", err)
	}
	if out := Compare("cisco_ios_read_config_v1", configuration, strings.Replace(configuration, "vlan 20", "vlan 30", 1)); out.Proposal == nil || out.Status != "drift" {
		t.Fatal("existing B11.1 proposal changed", out)
	}
}

func TestAccessPortStateFailsClosedOnUnsafeAndUnknownConfiguration(t *testing.T) {
	cases := map[string]string{
		"missing_mode":                strings.Replace(approvedConfig, " switchport mode access\n", "", 1),
		"missing_vlan_no_default_one": strings.Replace(approvedConfig, " switchport access vlan 20\n", "", 1),
		"zero_vlan":                   strings.Replace(approvedConfig, "vlan 20", "vlan 0", 1),
		"invalid_vlan":                strings.Replace(approvedConfig, "vlan 20", "vlan 4095", 1),
		"noncanonical_vlan":           strings.Replace(approvedConfig, "vlan 20", "vlan 020", 1),
		"trunk":                       strings.Replace(approvedConfig, "mode access", "mode trunk", 1),
		"unknown_mode":                strings.Replace(approvedConfig, "mode access", "mode dynamic auto", 1),
		"routed":                      strings.Replace(approvedConfig, " no shutdown", " no switchport\n no shutdown", 1),
		"lag_member":                  strings.Replace(approvedConfig, " no shutdown", " channel-group 1 mode active\n no shutdown", 1),
		"voice_vlan":                  strings.Replace(approvedConfig, " no shutdown", " switchport voice vlan 40\n no shutdown", 1),
		"trunk_allowed":               strings.Replace(approvedConfig, " no shutdown", " switchport trunk allowed vlan 20\n no shutdown", 1),
		"protected":                   strings.Replace(approvedConfig, " no shutdown", " switchport protected\n no shutdown", 1),
		"ip_address":                  strings.Replace(approvedConfig, " no shutdown", " ip address 192.0.2.1 255.255.255.0\n no shutdown", 1),
		"unknown_command":             strings.Replace(approvedConfig, " no shutdown", " mystery critical-setting\n no shutdown", 1),
		"nested_configuration":        strings.Replace(approvedConfig, " no shutdown", "  no shutdown", 1),
		"duplicate_mode":              strings.Replace(approvedConfig, " no shutdown", " switchport mode access\n no shutdown", 1),
		"duplicate_vlan":              strings.Replace(approvedConfig, " no shutdown", " switchport access vlan 20\n no shutdown", 1),
		"duplicate_description":       strings.Replace(approvedConfig, " no shutdown", " description another role\n no shutdown", 1),
		"duplicate_admin_state":       strings.Replace(approvedConfig, " no shutdown", " shutdown\n no shutdown", 1),
		"duplicate_interface":         approvedConfig + "interface GigabitEthernet0/12\n switchport mode access\n switchport access vlan 20\n",
		"interface_range":             approvedConfig + "interface range GigabitEthernet0/1-24\n switchport access vlan 20\n",
		"invalid_speed":               strings.Replace(approvedConfig, " no shutdown", " speed invalid\n no shutdown", 1),
		"invalid_duplex":              strings.Replace(approvedConfig, " no shutdown", " duplex invalid\n no shutdown", 1),
		"invalid_mtu":                 strings.Replace(approvedConfig, " no shutdown", " mtu -1\n no shutdown", 1),
	}
	for name, configuration := range cases {
		t.Run(name, func(t *testing.T) {
			port, err := AccessPortState("cisco_ios_read_config_v1", configuration, "GigabitEthernet0/12")
			if !errors.Is(err, ErrUnsupported) || port.Safe || port.VLAN != 0 {
				t.Fatal("unsafe port accepted", port, err)
			}
		})
	}
}

func TestAccessPortStateRejectsMissingLogicalAliasedAndUnsupportedTargets(t *testing.T) {
	for _, name := range []string{"GigabitEthernet0/13", "Gi0/12", "Port-channel1", "Management0", "Vlan20", "GigabitEthernet0/12.20", "GigabitEthernet0/12;reboot", "*"} {
		if _, err := AccessPortState("cisco_ios_read_config_v1", approvedConfig, name); !errors.Is(err, ErrUnsupported) {
			t.Fatal("unsafe interface accepted", name, err)
		}
	}
	for _, template := range []string{"arista_eos_read_config_v1", "vyos_read_config_v1", "unknown", "../../playbook.yml"} {
		if _, err := AccessPortState(template, approvedConfig, "GigabitEthernet0/12"); !errors.Is(err, ErrUnsupported) {
			t.Fatal("unsupported platform accepted", template, err)
		}
	}
	// Banner content cannot manufacture a usable physical interface.
	configuration := "hostname sw\nbanner motd ^C\ninterface GigabitEthernet0/12\n switchport mode access\n switchport access vlan 20\n^C\n"
	if _, err := AccessPortState("cisco_ios_read_config_v1", configuration, "GigabitEthernet0/12"); !errors.Is(err, ErrUnsupported) {
		t.Fatal("opaque banner parsed as a target")
	}
}
