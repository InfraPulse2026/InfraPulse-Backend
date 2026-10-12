// Package assets describes the global inventory layer above source-owned devices.
package assets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type Asset struct {
	Revision           int64     `json:"revision"`
	MergedIntoID       *string   `json:"merged_into_id"`
	ID                 string    `json:"id"`
	Kind               string    `json:"kind"`
	CanonicalName      string    `json:"canonical_name"`
	Vendor             string    `json:"vendor"`
	Model              string    `json:"model"`
	Serial             string    `json:"serial"`
	HardwareRevision   string    `json:"hardware_revision"`
	SoftwareVersion    string    `json:"software_version"`
	FirmwareVersion    string    `json:"firmware_version"`
	LifecycleStatus    string    `json:"lifecycle_status"`
	ResolutionStatus   string    `json:"resolution_status"`
	Scope              string    `json:"scope"`
	CanonicalBindingID string    `json:"canonical_binding_id"`
	SourceCount        int       `json:"source_count"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type Binding struct {
	ID               string          `json:"id"`
	AssetID          string          `json:"asset_id"`
	SourceInstanceID string          `json:"source_instance_id"`
	SourceType       string          `json:"source_type"`
	SourceName       string          `json:"source_name"`
	ExternalType     string          `json:"external_type"`
	ExternalID       string          `json:"external_id"`
	Confidence       string          `json:"confidence"`
	Provenance       string          `json:"provenance"`
	Scope            string          `json:"scope"`
	LegacyAgentID    *string         `json:"legacy_agent_id"`
	LegacyDeviceID   *string         `json:"legacy_device_id"`
	FirstSeen        time.Time       `json:"first_seen"`
	LastSeen         time.Time       `json:"last_seen"`
	InventoryAt      time.Time       `json:"inventory_at"`
	LatestFactID     int64           `json:"latest_fact_id"`
	Inventory        json.RawMessage `json:"inventory,omitempty"`
	ProviderState    json.RawMessage `json:"provider_state,omitempty"`
}

type FactVersion struct {
	ID            int64           `json:"id"`
	BindingID     string          `json:"binding_id"`
	AssetID       string          `json:"asset_id"`
	ObservedAt    time.Time       `json:"observed_at"`
	ReceivedAt    time.Time       `json:"received_at"`
	Origin        string          `json:"origin"`
	OriginRef     string          `json:"origin_ref"`
	Current       bool            `json:"current"`
	Inventory     json.RawMessage `json:"inventory"`
	ProviderState json.RawMessage `json:"provider_state"`
}

type Identity struct {
	ID              int64           `json:"id"`
	BindingID       string          `json:"binding_id"`
	FactID          int64           `json:"fact_id"`
	Kind            string          `json:"kind"`
	Value           string          `json:"value"`
	NormalizedValue string          `json:"normalized_value"`
	Confidence      string          `json:"confidence"`
	SubjectID       string          `json:"subject_id"`
	Usable          bool            `json:"usable"`
	Current         bool            `json:"current"`
	Evidence        json.RawMessage `json:"evidence,omitempty"`
	ObservedAt      time.Time       `json:"observed_at"`
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func NormalizeIdentity(kind, value string) (string, bool) {
	v := strings.TrimSpace(value)
	switch kind {
	case "hardware_uuid":
		v = strings.ToLower(v)
		return v, uuidPattern.MatchString(v) && v != "00000000-0000-0000-0000-000000000000" && v != "ffffffff-ffff-ffff-ffff-ffffffffffff"
	case "serial":
		v = strings.ToUpper(v)
		switch v {
		case "", "UNKNOWN", "N/A", "NA", "NONE", "0", "00000000", "TO BE FILLED BY O.E.M.", "[REDACTED]", "<REDACTED>", "REDACTED", "***":
			return v, false
		}
		return v, len(v) >= 3 && len(v) <= 256 && strings.Trim(v, "0") != "" && v != "DEFAULT STRING" && v != "SYSTEM SERIAL NUMBER"
	case "mac":
		mac, err := net.ParseMAC(v)
		if err != nil {
			return v, false
		}
		return mac.String(), len(mac) == 6 && mac[0]&1 == 0 && mac.String() != "00:00:00:00:00:00"
	case "management_ip":
		ip, err := netip.ParseAddr(v)
		if err != nil {
			return v, false
		}
		return ip.Unmap().String(), ip.IsValid() && !ip.IsUnspecified()
	case "hostname", "sys_name", "manufacturer", "model":
		return strings.ToLower(v), v != ""
	default:
		return v, v != ""
	}
}

func Kind(d *pb.Device) string {
	switch strings.ToLower(strings.ReplaceAll(d.GetHardware().GetResourceType(), "_", "")) {
	case "system", "computersystem":
		return "system"
	case "manager", "bmc":
		return "bmc"
	case "chassis":
		return "chassis"
	case "component", "processor", "memory", "drive", "sensor", "fan", "power", "powersupply", "storage", "storagecontroller", "networkinterface", "networkadapter", "networkport", "ethernetinterface", "pciedevice", "pciefunction":
		return "component"
	}
	if d.GetHardware().GetResourceType() != "" {
		return "unknown"
	}
	switch d.Type {
	case pb.DeviceType_DEVICE_TYPE_ROUTER, pb.DeviceType_DEVICE_TYPE_SWITCH, pb.DeviceType_DEVICE_TYPE_FIREWALL, pb.DeviceType_DEVICE_TYPE_GATEWAY, pb.DeviceType_DEVICE_TYPE_ACCESS_POINT, pb.DeviceType_DEVICE_TYPE_VOICE_GATEWAY:
		return "network_device"
	case pb.DeviceType_DEVICE_TYPE_SERVER, pb.DeviceType_DEVICE_TYPE_WORKSTATION, pb.DeviceType_DEVICE_TYPE_HYPERVISOR, pb.DeviceType_DEVICE_TYPE_PBX:
		return "host"
	default:
		return "unknown"
	}
}

func Provenance(d *pb.Device) string {
	if d.Source == "simulated" || d.GetObservation().GetSource().GetProvenance() == "simulated" {
		return "simulated"
	}
	switch p := d.GetObservation().GetSource().GetProvenance(); p {
	case "manual", "external":
		return p
	}
	return "real"
}

// Fingerprint ignores observation clocks and uptime but retains business facts,
// health/power, identities, resource subjects and boot identity. The original
// full snapshot is kept separately. A return A->B->A creates a new history entry.
func Fingerprint(d *pb.Device) (string, error) {
	copy := proto.Clone(d).(*pb.Device)
	copy.LastSeen = nil
	copy.UptimeSeconds = 0
	if copy.Observation != nil {
		copy.Observation.Time = nil
	}
	for _, e := range append(copy.IdentityEvidence, copy.ExtendedIdentityEvidence...) {
		e.Time = nil
	}
	for _, component := range copy.Components {
		if component.Observation != nil {
			component.Observation.Time = nil
		}
	}
	raw, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(copy)
	if err != nil {
		return "", err
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err = decoder.Decode(&decoded); err != nil {
		return "", err
	}
	raw, err = json.Marshal(decoded)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func Identities(d *pb.Device) []Identity {
	items := []Identity{}
	seen := map[string]bool{}
	add := func(kind, value, confidence, subject string, evidence json.RawMessage) {
		if value == "" {
			return
		}
		normalized, usable := NormalizeIdentity(kind, value)
		key := kind + "\x00" + value + "\x00" + subject + "\x00" + confidence + "\x00" + string(evidence)
		if seen[key] {
			return
		}
		seen[key] = true
		items = append(items, Identity{Kind: kind, Value: value, NormalizedValue: normalized, Confidence: confidence, SubjectID: subject, Usable: usable, Evidence: evidence})
	}
	for _, e := range append(append([]*pb.IdentityEvidence{}, d.IdentityEvidence...), d.ExtendedIdentityEvidence...) {
		raw, _ := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(e)
		confidence := e.Confidence
		if confidence == "" {
			confidence = "unresolved"
		}
		subject := e.SubjectId
		// Phase6 root facts can explicitly name the owner resource. Component,
		// interface and remote-neighbor facts never become root identifiers.
		ownerKind := Kind(d)
		rootKind := e.SubjectKind == "network_device" && ownerKind == "network_device" || e.SubjectKind == "computer_system" && (ownerKind == "system" || ownerKind == "host") || e.SubjectKind == "chassis" && ownerKind == "chassis" || e.SubjectKind == "manager" && ownerKind == "bmc" || e.SubjectKind == "component" && ownerKind == "component"
		if rootKind && (subject == d.Id || e.Source != nil && e.Source.ResourceId != "" && subject == e.Source.ResourceId) {
			subject = ""
		}
		before := len(items)
		add(e.Type, e.Value, confidence, subject, raw)
		if len(items) > before && (e.Normalization == "invalid" || e.Normalization == "rejected" || e.SubjectKind != "" && !rootKind && subject == "") {
			items[len(items)-1].Usable = false
		}
	}
	// Legacy fields are evidence of observation, not an invented high-confidence claim.
	add("serial", d.SerialNumber, "unresolved", "", nil)
	add("hardware_uuid", d.GetHardware().GetHardwareUuid(), "unresolved", "", nil)
	add("hostname", d.Hostname, "low", "", nil)
	add("management_ip", d.ManagementAddress, "low", "", nil)
	add("manufacturer", d.Vendor, "medium", "", nil)
	add("model", d.Model, "medium", "", nil)
	for _, c := range d.Components {
		if c.Id == "" {
			continue
		}
		add("serial", c.SerialNumber, "unresolved", c.Id, nil)
		add("hardware_uuid", c.GetHardware().GetHardwareUuid(), "unresolved", c.Id, nil)
	}
	return items
}
