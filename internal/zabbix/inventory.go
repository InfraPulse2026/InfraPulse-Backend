package zabbix

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type cursor struct {
	HostOffset     int                    `json:"host_offset"`
	APIVersion     string                 `json:"api_version"`
	HistoryFrom    int64                  `json:"history_from,omitempty"`
	EventWindows   map[string]eventWindow `json:"event_windows,omitempty"`
	ActiveAfter    map[string]string      `json:"active_after,omitempty"`
	ReconcileAfter string                 `json:"reconcile_after,omitempty"`
}
type host struct {
	ID                string          `json:"hostid"`
	Host              string          `json:"host"`
	Name              string          `json:"name"`
	Status            string          `json:"status"`
	MaintenanceStatus string          `json:"maintenance_status"`
	MaintenanceType   string          `json:"maintenance_type"`
	ActiveAvailable   string          `json:"active_available,omitempty"`
	Inventory         json.RawMessage `json:"inventory"`
	Interfaces        []hostInterface `json:"interfaces"`
	Groups            []hostGroup     `json:"groups"`
	HostGroups        []hostGroup     `json:"hostgroups"`
	Tags              []hostTag       `json:"tags"`
}
type hostInterface struct {
	ID        string `json:"interfaceid"`
	Type      string `json:"type"`
	Main      string `json:"main"`
	UseIP     string `json:"useip"`
	IP        string `json:"ip"`
	DNS       string `json:"dns"`
	Port      string `json:"port"`
	Available string `json:"available"`
}
type hostGroup struct {
	ID   string `json:"groupid"`
	Name string `json:"name"`
}
type hostTag struct {
	Tag   string `json:"tag"`
	Value string `json:"value"`
}
type providerState struct {
	APIVersion        string          `json:"api_version"`
	Status            string          `json:"status"`
	MonitoringEnabled bool            `json:"monitoring_enabled"`
	MaintenanceStatus string          `json:"maintenance_status"`
	MaintenanceType   string          `json:"maintenance_type"`
	ActiveAvailable   string          `json:"active_available,omitempty"`
	Interfaces        []hostInterface `json:"interfaces"`
	Groups            []hostGroup     `json:"groups"`
	Tags              []hostTag       `json:"tags"`
}

func (c *Connector) sync(ctx context.Context, request datasources.Request) (datasources.Result, error) {
	if request.Source.Configuration.Zabbix == nil {
		return datasources.Result{}, datasources.Failure{Code: "configuration_required"}
	}
	settings, err := request.Source.Configuration.Zabbix.Normalize()
	if err != nil || len(settings.HostIDs) == 0 {
		return datasources.Result{}, datasources.Failure{Code: "configuration_required"}
	}
	position := cursor{}
	if len(request.Cursor) != 0 && string(request.Cursor) != "{}" && (json.Unmarshal(request.Cursor, &position) != nil || position.HostOffset < 0 || position.HostOffset >= len(settings.HostIDs)) {
		return datasources.Result{}, datasources.Failure{Code: "invalid_response"}
	}
	rpc, err := c.connect(ctx, request)
	if err != nil {
		return datasources.Result{}, err
	}
	defer rpc.http.Close()
	if position.APIVersion != rpc.version {
		position.HostOffset = 0
	}
	pageSize := settings.PageSize
	if settings.Monitoring != nil && settings.Monitoring.Enabled {
		pageSize = min(pageSize, 5)
	}
	end := min(position.HostOffset+pageSize, len(settings.HostIDs))
	selected := settings.HostIDs[position.HostOffset:end]
	output := []string{"hostid", "host", "name", "status", "maintenance_status", "maintenance_type"}
	if !rpc.legacy {
		output = append(output, "active_available")
	}
	params := map[string]any{
		"hostids": selected, "output": output,
		"selectInventory":  []string{"vendor", "model", "serialno_a", "os", "hw_arch", "software", "location"},
		"selectInterfaces": []string{"interfaceid", "type", "main", "useip", "ip", "dns", "port", "available"},
		"selectTags":       []string{"tag", "value"},
	}
	if rpc.legacy {
		params["selectGroups"] = []string{"groupid", "name"}
	} else {
		params["selectHostGroups"] = []string{"groupid", "name"}
	}
	var hosts []host
	if err = rpc.call(ctx, "host.get", params, true, &hosts); err != nil {
		return datasources.Result{}, err
	}
	if len(hosts) != len(selected) {
		return datasources.Result{}, datasources.Failure{Code: "selected_hosts_unavailable"}
	}
	byID := map[string]host{}
	allowed := map[string]bool{}
	for _, id := range selected {
		allowed[id] = true
	}
	for _, h := range hosts {
		if !allowed[h.ID] {
			return datasources.Result{}, datasources.Failure{Code: "invalid_response"}
		}
		if _, duplicate := byID[h.ID]; duplicate {
			return datasources.Result{}, datasources.Failure{Code: "invalid_response"}
		}
		byID[h.ID] = h
	}
	observed := time.Now().UTC()
	result := datasources.Result{Observations: []assets.Observation{}}
	for _, id := range selected {
		observation, err := normalizeHost(byID[id], request.Source.ID, rpc.version, settings, observed, request.Credential)
		if err != nil {
			return datasources.Result{}, err
		}
		result.Observations = append(result.Observations, observation)
	}
	if settings.Monitoring != nil && settings.Monitoring.Enabled {
		result.Monitoring, err = rpc.monitor(ctx, request, settings, selected, &position, observed)
		if err != nil {
			return datasources.Result{}, err
		}
	}
	if end == len(settings.HostIDs) {
		end = 0
	}
	position.HostOffset = end
	position.APIVersion = rpc.version
	result.Cursor, err = json.Marshal(position)
	return result, err
}

// Free-text fields are bounded and redacted; they are never trusted as a type
// or a strong identity. This is defense in depth, not exhaustive anonymization.
func clean(s string, maxBytes int) (string, error) {
	if len(s) > maxBytes {
		return "", datasources.Failure{Code: "invalid_response"}
	}
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
	return logbook.SanitizeText(s), nil
}
func oneOf(value string, allowed ...string) bool {
	for _, a := range allowed {
		if value == a {
			return true
		}
	}
	return false
}
func canonicalID(value string) bool {
	n, err := strconv.ParseUint(value, 10, 64)
	return err == nil && n != 0 && strconv.FormatUint(n, 10) == value
}

func normalizeHost(h host, sourceID, version string, settings datasources.ZabbixSettings, observed time.Time, credential []byte) (assets.Observation, error) {
	fail := datasources.Failure{Code: "invalid_response"}
	clean := func(s string, max int) (string, error) {
		value, err := clean(s, max)
		if len(credential) > 0 {
			value = strings.ReplaceAll(value, string(credential), "[REDACTED]")
		}
		return value, err
	}
	if !canonicalID(h.ID) || !oneOf(h.Status, "0", "1") || !oneOf(h.MaintenanceStatus, "0", "1") || !oneOf(h.MaintenanceType, "0", "1") || !oneOf(h.ActiveAvailable, "", "0", "1", "2") || len(h.Interfaces) > 128 || len(h.Groups) > 128 || len(h.HostGroups) > 128 || len(h.Tags) > 128 {
		return assets.Observation{}, fail
	}
	var inv map[string]string
	if len(h.Inventory) > 0 && string(h.Inventory) != "[]" && string(h.Inventory) != "null" {
		if json.Unmarshal(h.Inventory, &inv) != nil {
			return assets.Observation{}, fail
		}
	}
	values := []*string{&h.Host, &h.Name}
	for _, p := range values {
		v, err := clean(*p, 256)
		if err != nil {
			return assets.Observation{}, err
		}
		*p = v
	}
	for key, value := range inv {
		v, err := clean(value, 1024)
		if err != nil {
			return assets.Observation{}, err
		}
		inv[key] = v
	}
	for _, key := range []string{"vendor", "model", "serialno_a"} {
		if len(inv[key]) > 256 {
			return assets.Observation{}, fail
		}
	}
	groups := h.HostGroups
	if strings.HasPrefix(version, "6.") {
		groups = h.Groups
	}
	for i := range groups {
		if !canonicalID(groups[i].ID) {
			return assets.Observation{}, fail
		}
		name, err := clean(groups[i].Name, 256)
		if err != nil {
			return assets.Observation{}, err
		}
		groups[i].Name = name
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	for i := range h.Tags {
		tag, err := clean(h.Tags[i].Tag, 256)
		if err != nil {
			return assets.Observation{}, err
		}
		value, err := clean(h.Tags[i].Value, 256)
		if err != nil {
			return assets.Observation{}, err
		}
		if logbook.SanitizeText(tag+"="+value) != tag+"="+value {
			value = "[REDACTED]"
		}
		h.Tags[i] = hostTag{Tag: tag, Value: value}
	}
	sort.Slice(h.Tags, func(i, j int) bool {
		if h.Tags[i].Tag != h.Tags[j].Tag {
			return h.Tags[i].Tag < h.Tags[j].Tag
		}
		return h.Tags[i].Value < h.Tags[j].Value
	})
	for i := range h.Interfaces {
		it := &h.Interfaces[i]
		if !canonicalID(it.ID) || !oneOf(it.Type, "1", "2", "3", "4") || !oneOf(it.Main, "0", "1") || !oneOf(it.UseIP, "0", "1") || !oneOf(it.Available, "0", "1", "2") {
			return assets.Observation{}, fail
		}
		for _, p := range []*string{&it.IP, &it.DNS, &it.Port} {
			v, err := clean(*p, 256)
			if err != nil {
				return assets.Observation{}, err
			}
			*p = v
		}
	}
	sort.Slice(h.Interfaces, func(i, j int) bool {
		if h.Interfaces[i].Main != h.Interfaces[j].Main {
			return h.Interfaces[i].Main > h.Interfaces[j].Main
		}
		return h.Interfaces[i].ID < h.Interfaces[j].ID
	})
	management := ""
	if len(h.Interfaces) > 0 {
		it := h.Interfaces[0]
		management = it.DNS
		if it.UseIP == "1" {
			management = it.IP
		}
	}
	types := map[string]pb.DeviceType{"unknown": pb.DeviceType_DEVICE_TYPE_UNKNOWN, "server": pb.DeviceType_DEVICE_TYPE_SERVER, "workstation": pb.DeviceType_DEVICE_TYPE_WORKSTATION, "router": pb.DeviceType_DEVICE_TYPE_ROUTER, "switch": pb.DeviceType_DEVICE_TYPE_SWITCH, "firewall": pb.DeviceType_DEVICE_TYPE_FIREWALL, "gateway": pb.DeviceType_DEVICE_TYPE_GATEWAY, "access_point": pb.DeviceType_DEVICE_TYPE_ACCESS_POINT, "voice_gateway": pb.DeviceType_DEVICE_TYPE_VOICE_GATEWAY, "pbx": pb.DeviceType_DEVICE_TYPE_PBX, "hypervisor": pb.DeviceType_DEVICE_TYPE_HYPERVISOR}
	kind, ok := types[settings.DeviceType]
	if !ok {
		kind = pb.DeviceType_DEVICE_TYPE_UNKNOWN
	}
	source := &pb.SourceRef{Type: "zabbix", InstanceId: sourceID, TargetId: h.ID, ResourceId: "/hosts/" + h.ID, Protocol: "jsonrpc", Provenance: "external"}
	d := &pb.Device{Id: h.ID, Name: h.Name, Hostname: h.Host, Vendor: inv["vendor"], Model: inv["model"], SerialNumber: inv["serialno_a"], SoftwareVersion: inv["software"], Os: inv["os"], Architecture: inv["hw_arch"], SystemLocation: inv["location"], ManagementAddress: management, Type: kind, Source: "zabbix", Observation: &pb.ObservationMetadata{Source: source, Time: &pb.ObservationTime{ObservedAt: timestamppb.New(observed), CollectedAt: timestamppb.New(observed), ClockQuality: "unknown"}}}
	if d.Name == "" {
		d.Name = d.Hostname
	}
	if d.Name == "" {
		d.Name = h.ID
	}
	resourceTypes := map[string]string{"system": "system", "bmc": "manager", "chassis": "chassis", "component": "component"}
	if resource := resourceTypes[settings.DeviceType]; resource != "" {
		d.Hardware = &pb.HardwareInventory{ResourceType: resource}
	}
	serialConfidence := "medium"
	if settings.VerifiedInventory {
		serialConfidence = "high"
	}
	if d.SerialNumber != "" {
		d.IdentityEvidence = append(d.IdentityEvidence, &pb.IdentityEvidence{Type: "serial", Value: d.SerialNumber, Confidence: serialConfidence, Source: source})
	}
	// Host status and interface availability are source facts, not physical online/power/health.
	state, err := json.Marshal(providerState{APIVersion: version, Status: h.Status, MonitoringEnabled: h.Status == "0", MaintenanceStatus: h.MaintenanceStatus, MaintenanceType: h.MaintenanceType, ActiveAvailable: h.ActiveAvailable, Interfaces: h.Interfaces, Groups: groups, Tags: h.Tags})
	if err != nil {
		return assets.Observation{}, err
	}
	return assets.Observation{ExternalType: "host", ExternalID: h.ID, ObservedAt: observed, Provenance: "external", Device: d, ProviderState: state}, nil
}
