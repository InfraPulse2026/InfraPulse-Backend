package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	rm "github.com/InfraPulse2026/InfraPulse-Backend/internal/remediation"
)

// Backend and the independent worker must load the same reviewed local policy.
// Neither policy nor protected-resource lists may be supplied through REST.
func LoadRemediationPolicy() (rm.Policy, error) {
	path := os.Getenv("REMEDIATION_POLICY_FILE")
	if path == "" {
		return rm.Policy{}, nil
	}
	return rm.LoadPolicy(path)
}

func LoadCommandPolicy() (commands.Policy, error) {
	policy := commands.Defaults()
	for key, dest := range map[string]*bool{"COMMANDS_ENABLED": &policy.Enabled, "COMMAND_ALLOW_DISABLE": &policy.AllowDisable, "COMMAND_ALLOW_ROLLBACK": &policy.AllowRollback} {
		if value := os.Getenv(key); value != "" {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return policy, fmt.Errorf("%s must be true or false", key)
			}
			*dest = parsed
		}
	}
	for key, dest := range map[string]*[]string{"COMMAND_PROTECTED_DEVICES": &policy.ProtectedDevices, "COMMAND_PROTECTED_INTERFACES": &policy.ProtectedInterfaces} {
		if value := os.Getenv(key); value != "" {
			for _, part := range strings.Split(value, ",") {
				part = strings.TrimSpace(part)
				if part == "" || len(part) > 128 {
					return policy, fmt.Errorf("invalid %s", key)
				}
				*dest = append(*dest, part)
			}
		}
	}
	return policy, nil
}
