package configdrift

import (
	"encoding/json"
	au "github.com/InfraPulse2026/InfraPulse-Backend/internal/automation"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxLines = 50000
const maxLineBytes = 16384

type block struct {
	Name   string   `json:"name"`
	Lines  []string `json:"lines"`
	VLAN   int      `json:"-"`
	Access bool     `json:"-"`
}
type normalized struct {
	Globals    []string `json:"globals"`
	Interfaces []block  `json:"interfaces"`
}

var interfaceName = regexp.MustCompile(`^(?:Ethernet|FastEthernet|GigabitEthernet|TenGigabitEthernet|TwentyFiveGigE|FortyGigabitEthernet|HundredGigE|Port-channel|Port-Channel|Vlan|Loopback|Tunnel|Management|mgmt|Bundle-Ether|Null|Gi|Te|Fa|Eth|Po)[0-9]+(?:[/.:][0-9]+){0,5}$`)
var physicalInterface = regexp.MustCompile(`^(?:GigabitEthernet|TenGigabitEthernet|TwentyFiveGigE|FortyGigabitEthernet|HundredGigE|FastEthernet|Ethernet)[0-9]+(?:/[0-9]+){0,3}$`)
var byteHeader = regexp.MustCompile(`^Current configuration\s*:\s*[0-9]+ bytes$`)
var bannerHeader = regexp.MustCompile(`^banner (?:motd|login|exec|incoming|slip-ppp) (.+)$`)

func noise(line, template string) bool {
	if strings.HasPrefix(line, "!") {
		return true
	}
	if template == "cisco_ios_read_config_v1" {
		return line == "Building configuration..." || byteHeader.MatchString(line) || line == "end"
	}
	if template == "arista_eos_read_config_v1" {
		return line == "end"
	}
	return false
}

// Only unique flat scalar commands are order independent. ACLs, nested modes,
// unknown commands and repeated scalar commands retain their original order.
func scalar(line string) string {
	switch {
	case strings.HasPrefix(line, "description "):
		return "description"
	case strings.HasPrefix(line, "switchport access vlan "):
		return "access_vlan"
	case strings.HasPrefix(line, "switchport mode "):
		return "switchport_mode"
	case line == "shutdown" || line == "no shutdown":
		return "admin_state"
	case line == "switchport" || line == "no switchport":
		return "switchport"
	case strings.HasPrefix(line, "mtu "):
		return "mtu"
	case strings.HasPrefix(line, "speed "):
		return "speed"
	case strings.HasPrefix(line, "duplex "):
		return "duplex"
	default:
		return ""
	}
}
func finishBlock(b *block) {
	seen := map[string]bool{}
	flat := true
	indent := -1
	vlanCount := 0
	modeCount := 0
	routed := false
	for _, raw := range b.Lines {
		trimmed := strings.TrimSpace(raw)
		depth := len(raw) - len(strings.TrimLeft(raw, " \t"))
		if indent < 0 {
			indent = depth
		}
		if depth != indent {
			flat = false
		}
		key := scalar(trimmed)
		if key == "" || seen[key] {
			flat = false
		}
		seen[key] = true
		if strings.HasPrefix(trimmed, "switchport access vlan ") {
			vlanCount++
			n, err := strconv.Atoi(strings.TrimPrefix(trimmed, "switchport access vlan "))
			if err == nil && n >= 1 && n <= 4094 {
				b.VLAN = n
			} else {
				b.VLAN = 0
			}
		}
		if strings.HasPrefix(trimmed, "switchport mode ") {
			modeCount++
			b.Access = trimmed == "switchport mode access"
		}
		if trimmed == "no switchport" {
			routed = true
		}
	}
	if vlanCount != 1 {
		b.VLAN = 0
	}
	if modeCount != 1 {
		b.Access = false
	}
	if routed {
		b.Access = false
	}
	if flat {
		for i := range b.Lines {
			b.Lines[i] = strings.TrimSpace(b.Lines[i])
		}
		sort.Strings(b.Lines)
	}
}
func normalize(template, configuration string) (normalized, error) {
	n := normalized{Globals: []string{}, Interfaces: []block{}}
	if _, ok := au.Lookup(template); !ok || len(configuration) > au.MaxConfiguration || !utf8.ValidString(configuration) || strings.ContainsRune(configuration, 0) || strings.TrimSpace(configuration) == "" {
		return n, ErrUnsupported
	}
	lines := strings.Split(strings.ReplaceAll(configuration, "\r\n", "\n"), "\n")
	if len(lines) > maxLines {
		return n, ErrUnsupported
	}
	// VyOS is intentionally conservative: rule/list ordering is preserved.
	if template == "vyos_read_config_v1" {
		for _, line := range lines {
			if len(line) > maxLineBytes {
				return n, ErrUnsupported
			}
			line = strings.TrimRight(line, " \t\r")
			if strings.TrimSpace(line) != "" {
				n.Globals = append(n.Globals, line)
			}
		}
		return n, nil
	}
	names := map[string]bool{}
	current := -1
	opaqueDelimiter := ""
	for _, raw := range lines {
		if len(raw) > maxLineBytes {
			return n, ErrUnsupported
		}
		raw = strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimSpace(raw)
		if opaqueDelimiter != "" {
			n.Globals = append(n.Globals, raw)
			if strings.HasSuffix(trimmed, opaqueDelimiter) {
				opaqueDelimiter = ""
			}
			continue
		}
		if strings.HasPrefix(trimmed, "banner ") {
			match := bannerHeader.FindStringSubmatch(trimmed)
			if len(match) != 2 {
				return n, ErrUnsupported
			}
			tail := match[1]
			if tail[0] < 33 || tail[0] > 126 {
				return n, ErrUnsupported
			}
			delimiter := tail[:1]
			if tail[0] == '^' {
				if len(tail) < 2 {
					return n, ErrUnsupported
				}
				delimiter = tail[:2]
			}
			n.Globals = append(n.Globals, raw)
			current = -1
			if !strings.Contains(tail[len(delimiter):], delimiter) {
				opaqueDelimiter = delimiter
			}
			continue
		}
		if trimmed == "" || noise(trimmed, template) {
			continue
		}
		indented := raw != strings.TrimLeft(raw, " \t")
		if !indented && strings.HasPrefix(raw, "interface ") {
			name := strings.TrimPrefix(raw, "interface ")
			if len(name) > 64 || !interfaceName.MatchString(name) || names[name] {
				return n, ErrUnsupported
			}
			names[name] = true
			n.Interfaces = append(n.Interfaces, block{Name: name, Lines: []string{}})
			current = len(n.Interfaces) - 1
			continue
		}
		if !indented {
			current = -1
		}
		if current >= 0 {
			n.Interfaces[current].Lines = append(n.Interfaces[current].Lines, raw)
		} else {
			n.Globals = append(n.Globals, raw)
		}
	}
	if opaqueDelimiter != "" {
		return n, ErrUnsupported
	}
	for i := range n.Interfaces {
		finishBlock(&n.Interfaces[i])
	}
	sort.Slice(n.Interfaces, func(i, j int) bool { return n.Interfaces[i].Name < n.Interfaces[j].Name })
	// Do not normalize executable reset/multi-interface commands as running state.
	for _, line := range n.Globals {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "default interface ") || strings.HasPrefix(trimmed, "no interface ") || strings.HasPrefix(trimmed, "interface range ") {
			return n, ErrUnsupported
		}
	}
	if len(n.Globals) == 0 && len(n.Interfaces) == 0 {
		return n, ErrUnsupported
	}
	return n, nil
}
func digest(n normalized) string { raw, _ := json.Marshal(n); return au.Digest(string(raw)) }
func Digest(template, configuration string) (string, error) {
	n, err := normalize(template, configuration)
	if err != nil {
		return "", err
	}
	return digest(n), nil
}
func withoutVLAN(lines []string) []string {
	out := []string{}
	for _, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "switchport access vlan ") {
			out = append(out, line)
		}
	}
	return out
}
func equal(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func Compare(template, expected, actual string) Comparison {
	out := Comparison{Status: "unknown", Changes: []Change{}, ErrorCode: "normalization_unsupported"}
	e, err := normalize(template, expected)
	if err != nil {
		return out
	}
	a, err := normalize(template, actual)
	if err != nil {
		return out
	}
	out.ExpectedDigest = digest(e)
	out.ActualDigest = digest(a)
	out.ErrorCode = ""
	out.Status = "compliant"
	add := func(c Change) {
		out.TotalChanges++
		if len(out.Changes) < MaxChanges {
			out.Changes = append(out.Changes, c)
		} else {
			out.Truncated = true
		}
	}
	if !equal(e.Globals, a.Globals) {
		add(Change{Path: "/global", Kind: "changed", Redacted: true})
	}
	expectedBlocks := map[string]block{}
	actualBlocks := map[string]block{}
	names := map[string]bool{}
	for _, b := range e.Interfaces {
		expectedBlocks[b.Name] = b
		names[b.Name] = true
	}
	for _, b := range a.Interfaces {
		actualBlocks[b.Name] = b
		names[b.Name] = true
	}
	sorted := []string{}
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		eb, existsE := expectedBlocks[name]
		ab, existsA := actualBlocks[name]
		if existsE && existsA && equal(eb.Lines, ab.Lines) {
			continue
		}
		change := Change{Path: "/interfaces/" + name, Interface: name, Kind: "changed", Redacted: true}
		if !existsE {
			change.Kind = "added"
		} else if !existsA {
			change.Kind = "removed"
		} else if eb.VLAN != 0 && ab.VLAN != 0 && eb.VLAN != ab.VLAN && equal(withoutVLAN(eb.Lines), withoutVLAN(ab.Lines)) {
			old, new := eb.VLAN, ab.VLAN
			change.Kind = "access_vlan_changed"
			change.ExpectedVLAN = &old
			change.ActualVLAN = &new
			change.Redacted = false
		}
		add(change)
	}
	if out.TotalChanges > 0 {
		out.Status = "drift"
	}
	if out.TotalChanges == 1 && len(out.Changes) == 1 && out.Changes[0].Kind == "access_vlan_changed" {
		change := out.Changes[0]
		eb := expectedBlocks[change.Interface]
		ab := actualBlocks[change.Interface]
		if eb.Access && ab.Access && physicalInterface.MatchString(change.Interface) {
			out.Proposal = &Proposal{Action: "restore_access_vlan", Interface: change.Interface, ExpectedVLAN: eb.VLAN, ActualVLAN: ab.VLAN, ApprovalRequired: true, ExecutionAvailable: false, Limitation: "execution_requires_B11_2_policy_approval_and_live_verification"}
		}
	}
	return out
}
