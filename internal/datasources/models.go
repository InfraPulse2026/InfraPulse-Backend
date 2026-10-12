// Package datasources defines source instances independently of Collector devices.
package datasources

import (
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/externalmonitor"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var ErrInvalid = errors.New("invalid data source")
var ErrNotFound = errors.New("data source not found")
var ErrConflict = errors.New("data source revision conflict")
var ErrKeyUnavailable = errors.New("data source credential encryption is not configured")
var ErrUnsupported = errors.New("operation is not supported by this source adapter")

type Settings struct {
	TimeoutSeconds int                 `json:"timeout_seconds"`
	TLSCARef       string              `json:"tls_ca_ref,omitempty"`
	Zabbix         *ZabbixSettings     `json:"zabbix,omitempty"`
	Prometheus     *prommodel.Settings `json:"prometheus,omitempty"`
}

type ZabbixSettings struct {
	HostIDs           []string          `json:"host_ids"`
	PageSize          int               `json:"page_size"`
	DeviceType        string            `json:"device_type"`
	VerifiedInventory bool              `json:"verified_inventory"`
	Monitoring        *ZabbixMonitoring `json:"monitoring,omitempty"`
}

type ZabbixMonitoring struct {
	Enabled              bool                      `json:"enabled"`
	EventPageSize        int                       `json:"event_page_size"`
	EventLookbackSeconds int                       `json:"event_lookback_seconds"`
	ItemMappings         []externalmonitor.Mapping `json:"item_mappings"`
}

func (z ZabbixSettings) Normalize() (ZabbixSettings, error) {
	if z.PageSize == 0 {
		z.PageSize = 20
	}
	if z.PageSize < 1 || z.PageSize > 50 || len(z.HostIDs) > 200 {
		return z, ErrInvalid
	}
	if z.DeviceType == "" {
		z.DeviceType = "unknown"
	}
	switch z.DeviceType {
	case "unknown", "server", "workstation", "router", "switch", "firewall", "gateway", "access_point", "voice_gateway", "pbx", "hypervisor", "system", "bmc", "chassis", "component":
	default:
		return z, ErrInvalid
	}
	z.HostIDs = append([]string{}, z.HostIDs...)
	seen := map[string]bool{}
	for _, id := range z.HostIDs {
		n, err := strconv.ParseUint(id, 10, 64)
		if err != nil || n == 0 || strconv.FormatUint(n, 10) != id || seen[id] {
			return z, ErrInvalid
		}
		seen[id] = true
	}
	sort.Slice(z.HostIDs, func(i, j int) bool {
		a, _ := strconv.ParseUint(z.HostIDs[i], 10, 64)
		b, _ := strconv.ParseUint(z.HostIDs[j], 10, 64)
		return a < b
	})
	if z.Monitoring != nil {
		m := *z.Monitoring
		if m.EventPageSize == 0 {
			m.EventPageSize = 20
		}
		if m.EventLookbackSeconds == 0 {
			m.EventLookbackSeconds = 86400
		}
		if m.EventPageSize < 1 || m.EventPageSize > 100 || m.EventLookbackSeconds < 300 || m.EventLookbackSeconds > 2592000 || len(m.ItemMappings) > 100 {
			return z, ErrInvalid
		}
		m.ItemMappings = append([]externalmonitor.Mapping{}, m.ItemMappings...)
		seenItems := map[string]bool{}
		for i, v := range m.ItemMappings {
			normal, err := v.Normalize()
			if err != nil || !seen[v.HostID] || seenItems[v.ItemID] {
				return z, ErrInvalid
			}
			m.ItemMappings[i] = normal
			seenItems[v.ItemID] = true
		}
		z.Monitoring = &m
	}
	return z, nil
}

// Input deliberately accepts no credentials inside an endpoint or arbitrary configuration.
// Credential=nil retains the existing secret; an empty string explicitly removes it.
type Input struct {
	Type                string   `json:"type"`
	Name                string   `json:"name"`
	Endpoint            string   `json:"endpoint"`
	ConnectionRef       string   `json:"connection_ref"`
	Enabled             bool     `json:"enabled"`
	SyncIntervalSeconds int      `json:"sync_interval_seconds"`
	Configuration       Settings `json:"configuration"`
	Credential          *string  `json:"credential,omitempty"`
	Revision            int64    `json:"revision"`
}

type Source struct {
	ID                  string     `json:"id"`
	Type                string     `json:"type"`
	Name                string     `json:"name"`
	Endpoint            string     `json:"endpoint"`
	ConnectionRef       string     `json:"connection_ref"`
	Enabled             bool       `json:"enabled"`
	SyncIntervalSeconds int        `json:"sync_interval_seconds"`
	Configuration       Settings   `json:"configuration"`
	Revision            int64      `json:"revision"`
	HasCredential       bool       `json:"has_credential"`
	ConnectionStatus    string     `json:"connection_status"`
	LastSuccessfulSync  *time.Time `json:"last_successful_sync"`
	LastError           string     `json:"last_error"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	Capabilities        []string   `json:"capabilities"`
}

func safeText(s string, max int) bool {
	if len(s) > max {
		return false
	}
	return !strings.ContainsFunc(s, unicode.IsControl)
}

func (in Input) Normalize() (Input, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || !safeText(in.Name, 128) || in.Revision < 0 {
		return in, ErrInvalid
	}
	if in.Credential != nil && (len(*in.Credential) > 16384 || strings.ContainsRune(*in.Credential, '\x00')) {
		return in, ErrInvalid
	}
	if in.SyncIntervalSeconds < 3 || in.SyncIntervalSeconds > 86400 {
		return in, ErrInvalid
	}
	if in.Configuration.TimeoutSeconds == 0 {
		in.Configuration.TimeoutSeconds = 15
	}
	if in.Configuration.TimeoutSeconds < 1 || in.Configuration.TimeoutSeconds > 120 {
		return in, ErrInvalid
	}
	if in.Configuration.TLSCARef != "" && !caRefPattern.MatchString(in.Configuration.TLSCARef) {
		return in, ErrInvalid
	}
	if in.Configuration.Zabbix != nil {
		if in.Type != "ZABBIX" {
			return in, ErrInvalid
		}
		z, err := in.Configuration.Zabbix.Normalize()
		if err != nil {
			return in, err
		}
		in.Configuration.Zabbix = &z
	}
	if in.Configuration.Prometheus != nil {
		if in.Type != "PROMETHEUS" {
			return in, ErrInvalid
		}
		p, err := in.Configuration.Prometheus.Normalize()
		if err != nil {
			return in, ErrInvalid
		}
		in.Configuration.Prometheus = &p
	}
	switch in.Type {
	case "COLLECTOR":
		if in.Endpoint != "" || in.ConnectionRef == "" || !safeText(in.ConnectionRef, 256) || in.Configuration.TLSCARef != "" || (in.Credential != nil && *in.Credential != "") {
			return in, ErrInvalid
		}
	case "ZABBIX", "PROMETHEUS", "ANSIBLE":
		if in.ConnectionRef != "" || !safeText(in.Endpoint, 2048) {
			return in, ErrInvalid
		}
		u, _, err := parseEndpoint(in.Endpoint)
		if err != nil {
			return in, ErrInvalid
		}
		in.Endpoint = u.String()
	default:
		return in, ErrInvalid
	}
	return in, nil
}

// Adapter operations are enabled only once an implementation is registered.
// Collector uses its existing authenticated gRPC stream rather than polling.
func Capabilities(kind string) []string {
	if kind == "COLLECTOR" {
		return []string{"stream_status"}
	}
	return []string{}
}
