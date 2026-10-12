package configdrift

import (
	"strconv"
	"strings"
)

// AccessPort is the relevant state of one canonical physical interface. It
// contains no description, configuration lines or credentials.
type AccessPort struct {
	Name   string `json:"name"`
	VLAN   int    `json:"vlan"`
	Access bool   `json:"access"`
	Safe   bool   `json:"safe"`
}

func PhysicalInterfaceName(name string) bool {
	return len(name) <= 64 && physicalInterface.MatchString(name)
}

// AccessPortState reuses B11.1 normalization and accepts only a deliberately
// narrow Cisco access-port grammar. Unknown, routed, trunk, LAG, voice and
// nested commands fail closed. VLAN 1 is never inferred from missing state.
func AccessPortState(template, configuration, name string) (AccessPort, error) {
	if template != "cisco_ios_read_config_v1" || !PhysicalInterfaceName(name) {
		return AccessPort{}, ErrUnsupported
	}
	normalized, err := normalize(template, configuration)
	if err != nil {
		return AccessPort{}, err
	}
	for _, candidate := range normalized.Interfaces {
		if candidate.Name != name {
			continue
		}
		if !candidate.Access || candidate.VLAN < 1 || candidate.VLAN > 4094 {
			return AccessPort{}, ErrUnsupported
		}
		seen := map[string]bool{}
		indent := -1
		for _, raw := range candidate.Lines {
			if strings.ContainsRune(raw, '\t') {
				return AccessPort{}, ErrUnsupported
			}
			depth := len(raw) - len(strings.TrimLeft(raw, " "))
			if indent < 0 {
				indent = depth
			}
			if depth != indent {
				return AccessPort{}, ErrUnsupported
			}
			line := strings.TrimSpace(raw)
			key := scalar(line)
			valid := false
			switch key {
			case "description":
				valid = len(strings.TrimSpace(strings.TrimPrefix(line, "description "))) > 0
			case "access_vlan":
				n, parseErr := strconv.Atoi(strings.TrimPrefix(line, "switchport access vlan "))
				valid = parseErr == nil && n == candidate.VLAN && line == "switchport access vlan "+strconv.Itoa(n)
			case "switchport_mode":
				valid = line == "switchport mode access"
			case "switchport":
				valid = line == "switchport"
			case "admin_state":
				valid = true
			case "mtu":
				n, parseErr := strconv.Atoi(strings.TrimPrefix(line, "mtu "))
				valid = parseErr == nil && n > 0 && n <= 65535 && line == "mtu "+strconv.Itoa(n)
			case "speed":
				switch strings.TrimPrefix(line, "speed ") {
				case "auto", "10", "100", "1000", "10000", "25000", "40000", "100000":
					valid = true
				}
			case "duplex":
				switch strings.TrimPrefix(line, "duplex ") {
				case "auto", "full", "half":
					valid = true
				}
			}
			if !valid || seen[key] {
				return AccessPort{}, ErrUnsupported
			}
			seen[key] = true
		}
		return AccessPort{Name: name, VLAN: candidate.VLAN, Access: true, Safe: true}, nil
	}
	return AccessPort{}, ErrUnsupported
}
