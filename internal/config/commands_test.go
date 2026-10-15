package config

import "testing"

func TestCommandPolicyDefaultsAndFlags(t *testing.T) {
	for _, k := range []string{"COMMANDS_ENABLED", "COMMAND_ALLOW_DISABLE", "COMMAND_ALLOW_ROLLBACK", "COMMAND_PROTECTED_DEVICES", "COMMAND_PROTECTED_INTERFACES"} {
		t.Setenv(k, "")
	}
	c, err := Load()
	if err != nil || c.Commands.Enabled || c.Commands.AllowDisable || c.Commands.AllowRollback || len(c.Commands.ProtectedInterfaces) < 3 {
		t.Fatal("unsafe command defaults")
	}
	t.Setenv("COMMANDS_ENABLED", "true")
	t.Setenv("COMMAND_ALLOW_DISABLE", "true")
	t.Setenv("COMMAND_PROTECTED_INTERFACES", "uplink")
	c, err = Load()
	if err != nil || !c.Commands.Enabled || !c.Commands.AllowDisable || len(c.Commands.ProtectedInterfaces) != 4 {
		t.Fatal("explicit flags/protection not applied")
	}
	t.Setenv("COMMANDS_ENABLED", "anything")
	if _, err = Load(); err == nil {
		t.Fatal("invalid flag accepted")
	}
}
