package commands

import "errors"

var ErrDisabled = errors.New("command dispatch is disabled")
var ErrProtected = errors.New("target resource is protected")
var ErrPrecondition = errors.New("fresh inventory, telemetry, capability and expected interface state are required")
var ErrConflict = errors.New("idempotency key reused with different parameters")
var ErrBusy = errors.New("agent already has an active or unresolved command")
var ErrNotFound = errors.New("command not found")

type Policy struct {
	Enabled             bool     `json:"enabled"`
	AllowDisable        bool     `json:"allow_disable"`
	AllowRollback       bool     `json:"allow_rollback"`
	ProtectedDevices    []string `json:"protected_devices"`
	ProtectedInterfaces []string `json:"protected_interfaces"`
}

func Defaults() Policy {
	return Policy{ProtectedDevices: []string{}, ProtectedInterfaces: []string{"lo", "eth0", "ether1"}}
}

type Filter struct {
	AgentID, DeviceID, State string
	Limit, Offset            int
}
type Event struct {
	ID        int64  `json:"id"`
	CommandID string `json:"command_id"`
	State     string `json:"state"`
	Reason    string `json:"reason"`
	At        string `json:"at"`
}

func Active(state string) bool {
	switch state {
	case "queued", "dispatching", "sent", "accepted", "running", "uncertain":
		return true
	}
	return false
}
