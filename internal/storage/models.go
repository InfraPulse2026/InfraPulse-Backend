package storage

import (
	"encoding/json"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"time"
)

type Agent struct {
	AgentID         string          `json:"agent_id"`
	SessionID       string          `json:"session_id"`
	Name            string          `json:"name"`
	Version         string          `json:"version"`
	ProtocolVersion int             `json:"protocol_version"`
	Connected       bool            `json:"connected"`
	LastSeen        time.Time       `json:"last_seen"`
	Heartbeat       json.RawMessage `json:"heartbeat"`
}

type Device struct {
	AgentID            string               `json:"agent_id"`
	DeviceID           string               `json:"device_id"`
	Name               string               `json:"name"`
	Hostname           string               `json:"hostname"`
	Vendor             string               `json:"vendor"`
	Model              string               `json:"model"`
	Type               string               `json:"type"`
	ManagementAddress  string               `json:"management_address"`
	Source             string               `json:"source"`
	Online             bool                 `json:"online"`
	AgentConnected     bool                 `json:"agent_connected"`
	LastSeen           time.Time            `json:"last_seen"`
	ReceivedAt         time.Time            `json:"received_at"`
	Inventory          json.RawMessage      `json:"inventory"`
	Monitoring         monitoring.Settings  `json:"monitoring"`
	TelemetryFreshness monitoring.Freshness `json:"telemetry_freshness"`
}

type Telemetry struct {
	ID               int64           `json:"id"`
	AgentID          string          `json:"agent_id"`
	DeviceID         string          `json:"device_id"`
	ObservedAt       time.Time       `json:"observed_at"`
	CollectedAt      time.Time       `json:"collected_at"`
	ReceivedAt       time.Time       `json:"received_at"`
	Source           string          `json:"source"`
	CPUUsagePercent  *float64        `json:"cpu_usage_percent"`
	MemoryTotalBytes *string         `json:"memory_total_bytes"`
	MemoryUsedBytes  *string         `json:"memory_used_bytes"`
	Sample           json.RawMessage `json:"sample"`
}
