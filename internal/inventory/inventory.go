// Package inventory builds a provenance-preserving view of source-owned facts.
// It never infers a physical identity or placement from names or LLDP neighbors.
package inventory

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"google.golang.org/protobuf/encoding/protojson"
)

type Snapshot struct {
	BindingID     string
	SourceID      string
	SourceType    string
	SourceName    string
	Provenance    string
	FactID        int64
	ObservedAt    time.Time
	ReceivedAt    time.Time
	SourceEnabled bool
	ProviderState json.RawMessage
	Device        *pb.Device
}
type Claim struct {
	Value              json.RawMessage `json:"value"`
	BindingID          string          `json:"binding_id"`
	SourceID           string          `json:"source_instance_id"`
	SourceType         string          `json:"source_type"`
	CollectorProtocol  string          `json:"collector_protocol"`
	SourceName         string          `json:"source_name"`
	Provenance         string          `json:"provenance"`
	FactID             int64           `json:"fact_id"`
	ObservedAt         time.Time       `json:"observed_at"`
	SnapshotObservedAt time.Time       `json:"snapshot_observed_at"`
	ReceivedAt         time.Time       `json:"fact_received_at"`
	SourceEnabled      bool            `json:"source_enabled"`
	Confidence         string          `json:"confidence"`
	Method             string          `json:"method"`
	Usable             bool            `json:"usable"`
	ClockQuality       string          `json:"clock_quality"`
	Evidence           json.RawMessage `json:"evidence,omitempty"`
	normalized         string
}
type Attribute struct {
	Status   string          `json:"status"`
	Value    json.RawMessage `json:"value"`
	Selected *Claim          `json:"selected"`
	Conflict bool            `json:"conflict"`
	Claims   []Claim         `json:"claims"`
}
type Summary struct {
	AssetID       string               `json:"asset_id"`
	AssetRevision int64                `json:"asset_revision"`
	Status        string               `json:"status"`
	Missing       []string             `json:"missing"`
	Attributes    map[string]Attribute `json:"attributes"`
	SourceCount   int                  `json:"source_count"`
}

var fields = []string{"name", "hostname", "vendor", "model", "serial", "hardware_uuid", "hardware_revision", "os", "distribution", "kernel", "architecture", "software_version", "firmware_version", "management_ips", "management_interfaces", "components", "networks", "source_location", "system_location", "hardware"}

// Only these already-normalized Zabbix fields are exposed; arbitrary provider
// state is not copied into inventory or treated as a credential/endpoint.
type managementInterface struct {
	ID        string `json:"interfaceid"`
	Type      string `json:"type"`
	Main      string `json:"main"`
	UseIP     string `json:"useip"`
	IP        string `json:"ip"`
	DNS       string `json:"dns"`
	Port      string `json:"port"`
	Available string `json:"available"`
}

func rank(s string) int {
	switch s {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	}
	return 0
}

func Build(a assets.Asset, snapshots []Snapshot) Summary {
	out := Summary{AssetID: a.ID, AssetRevision: a.Revision, Status: "unknown", Missing: []string{}, SourceCount: len(snapshots), Attributes: map[string]Attribute{}}
	for _, f := range fields {
		out.Attributes[f] = Attribute{Status: "unknown", Claims: []Claim{}}
	}
	for _, s := range snapshots {
		d := s.Device
		if d == nil {
			continue
		}
		identities := assets.Identities(d)
		rejected := map[string]bool{}
		for _, i := range identities {
			if i.SubjectID == "" && len(i.Evidence) > 0 && !i.Usable {
				rejected[i.Kind+":"+i.NormalizedValue] = true
			}
		}
		add := func(field string, value any, confidence, method, normalized string, usable bool, evidence json.RawMessage) {
			raw, err := json.Marshal(value)
			if err != nil {
				return
			}
			observed := s.ObservedAt
			if at := d.GetObservation().GetTime().GetObservedAt(); at != nil && at.IsValid() {
				observed = at.AsTime()
			}
			c := Claim{Value: raw, BindingID: s.BindingID, SourceID: s.SourceID, SourceType: s.SourceType, CollectorProtocol: d.Source, SourceName: s.SourceName, Provenance: s.Provenance, FactID: s.FactID, ObservedAt: observed, SnapshotObservedAt: s.ObservedAt, ReceivedAt: s.ReceivedAt, SourceEnabled: s.SourceEnabled, Confidence: confidence, Method: method, Usable: usable, ClockQuality: d.GetObservation().GetTime().GetClockQuality(), Evidence: evidence, normalized: normalized}
			attr := out.Attributes[field]
			attr.Claims = append(attr.Claims, c)
			out.Attributes[field] = attr
		}
		text := func(field, value, confidence string) {
			value = strings.TrimSpace(value)
			if value == "" {
				return
			}
			norm := value
			usable := true
			switch field {
			case "serial", "hardware_uuid":
				norm, usable = assets.NormalizeIdentity(field, value)
				usable = usable && !rejected[field+":"+norm]
			case "vendor", "model", "hostname":
				norm = strings.ToLower(value)
			}
			add(field, value, confidence, "device_field", norm, usable, nil)
		}
		text("name", d.Name, "low")
		text("hostname", d.Hostname, "low")
		text("vendor", d.Vendor, "medium")
		text("model", d.Model, "medium")
		text("serial", d.SerialNumber, "unresolved")
		text("hardware_uuid", d.GetHardware().GetHardwareUuid(), "unresolved")
		for f, v := range map[string]string{"hardware_revision": d.HardwareRevision, "os": d.Os, "distribution": d.Distribution, "kernel": d.Kernel, "architecture": d.Architecture, "software_version": d.SoftwareVersion, "firmware_version": d.FirmwareVersion, "system_location": d.SystemLocation} {
			text(f, v, "medium")
		}
		ips := map[string]bool{}
		for _, v := range append([]string{d.ManagementAddress}, d.GetHardware().GetNetworkAddresses()...) {
			if norm, ok := assets.NormalizeIdentity("management_ip", v); ok {
				ips[norm] = true
			}
		}
		if s.SourceType == "ZABBIX" {
			var provider struct {
				Interfaces []managementInterface `json:"interfaces"`
			}
			if json.Unmarshal(s.ProviderState, &provider) == nil && len(provider.Interfaces) > 0 && len(provider.Interfaces) <= 128 {
				for _, i := range provider.Interfaces {
					if i.UseIP == "1" {
						if ip, ok := assets.NormalizeIdentity("management_ip", i.IP); ok {
							ips[ip] = true
						}
					}
				}
				raw, _ := json.Marshal(provider.Interfaces)
				add("management_interfaces", provider.Interfaces, "medium", "provider_interfaces", string(raw), true, nil)
			}
		}
		for _, i := range identities {
			if i.SubjectID != "" || len(i.Evidence) == 0 {
				continue
			}
			field := i.Kind
			if field == "manufacturer" {
				field = "vendor"
			}
			switch field {
			case "serial", "hardware_uuid", "vendor", "model", "hostname":
				add(field, i.Value, i.Confidence, "identity_evidence", i.NormalizedValue, i.Usable, i.Evidence)
				var e pb.IdentityEvidence
				if protojson.Unmarshal(i.Evidence, &e) == nil && e.Time != nil {
					attr := out.Attributes[field]
					c := &attr.Claims[len(attr.Claims)-1]
					if at := e.Time.ObservedAt; at != nil && at.IsValid() {
						c.ObservedAt = at.AsTime()
					}
					c.ClockQuality = e.Time.ClockQuality
					out.Attributes[field] = attr
				}
			case "management_ip":
				if i.Usable {
					ips[i.NormalizedValue] = true
				}
			}
		}
		ipList := []string{}
		for ip := range ips {
			ipList = append(ipList, ip)
		}
		sort.Strings(ipList)
		if len(ipList) > 0 {
			add("management_ips", ipList, "low", "device_addresses", strings.Join(ipList, ","), true, nil)
		}
		// Components and network objects stay scoped to their binding. Identical
		// component names in different sources do not imply identical physical parts.
		marshalObject := func(field string, value any) {
			raw, err := json.Marshal(value)
			if err == nil {
				add(field, json.RawMessage(raw), "medium", "device_field", string(raw), true, nil)
			}
		}
		if len(d.Components) > 0 {
			raw := []json.RawMessage{}
			for _, c := range d.Components {
				v, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(c)
				raw = append(raw, v)
			}
			marshalObject("components", raw)
		}
		if len(d.Networks) > 0 {
			raw := []json.RawMessage{}
			for _, n := range d.Networks {
				v, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(n)
				raw = append(raw, v)
			}
			marshalObject("networks", raw)
		}
		if d.Location != nil {
			v, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(d.Location)
			if string(v) != "{}" {
				marshalObject("source_location", json.RawMessage(v))
			}
		}
		if d.Hardware != nil {
			v, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(d.Hardware)
			if string(v) != "{}" {
				marshalObject("hardware", json.RawMessage(v))
			}
		}
	}
	populated, conflict := false, false
	for field, attr := range out.Attributes {
		sort.SliceStable(attr.Claims, func(i, j int) bool {
			l, r := attr.Claims[i], attr.Claims[j]
			if l.Usable != r.Usable {
				return l.Usable
			}
			if rank(l.Confidence) != rank(r.Confidence) {
				return rank(l.Confidence) > rank(r.Confidence)
			}
			if (l.BindingID == a.CanonicalBindingID) != (r.BindingID == a.CanonicalBindingID) {
				return l.BindingID == a.CanonicalBindingID
			}
			if l.BindingID != r.BindingID {
				return l.BindingID < r.BindingID
			}
			return l.Method < r.Method
		})
		values, top := map[string]bool{}, map[string]bool{}
		best := -1
		for _, c := range attr.Claims {
			if !c.Usable {
				continue
			}
			values[c.normalized] = true
			if best < 0 {
				best = rank(c.Confidence)
			}
			if rank(c.Confidence) == best {
				top[c.normalized] = true
			}
		}
		// Multi-valued inventories are additive evidence, not scalar conflicts.
		scalar := field != "management_ips" && field != "management_interfaces" && field != "components" && field != "networks" && field != "hardware"
		attr.Conflict = scalar && len(values) > 1
		if len(values) > 0 {
			populated = true
			attr.Status = "observed"
			if scalar && len(top) == 1 {
				v := attr.Claims[0]
				attr.Selected = &v
				attr.Value = v.Value
			}
			if !scalar {
				attr.Status = "source_scoped"
			}
			if field == "management_ips" {
				ips := map[string]bool{}
				for _, c := range attr.Claims {
					var list []string
					if c.Usable && json.Unmarshal(c.Value, &list) == nil {
						for _, ip := range list {
							ips[ip] = true
						}
					}
				}
				list := []string{}
				for ip := range ips {
					list = append(list, ip)
				}
				sort.Strings(list)
				attr.Value, _ = json.Marshal(list)
				attr.Status = "observed"
			}
			if attr.Conflict {
				attr.Status = "conflict"
				conflict = true
			}
		}
		out.Attributes[field] = attr
	}
	for _, f := range []string{"vendor", "model", "serial"} {
		if out.Attributes[f].Selected == nil {
			out.Missing = append(out.Missing, f)
		}
	}
	if populated {
		out.Status = "partial"
	}
	if populated && len(out.Missing) == 0 {
		out.Status = "complete"
	}
	if conflict {
		out.Status = "conflict"
	}
	return out
}
