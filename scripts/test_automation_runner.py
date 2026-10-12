"""Adapter contract tests with an explicit fake Runner (no hardware claim)."""
import importlib.util
import json
import os
import re
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest.mock import patch

try:
    from jinja2 import Environment, StrictUndefined
except ImportError:
    Environment = None

if sys.platform == "win32":
    sys.modules.setdefault("resource", types.SimpleNamespace(RLIMIT_CORE=0, RLIMIT_FSIZE=1, RLIMIT_AS=2, setrlimit=lambda *_: None))
path = Path(__file__).resolve().parent.parent / "internal" / "automation" / "runner.py"
spec = importlib.util.spec_from_file_location("infrapulse_adapter", path)
adapter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(adapter)


class AdapterTests(unittest.TestCase):
    def execute(self, template, failure=False):
        seen = {}

        def run(**kwargs):
            seen.update(kwargs)
            self.assertTrue(kwargs["quiet"])
            self.assertTrue(kwargs["suppress_output_file"])
            variables = kwargs["inventory"]["all"]["hosts"]["device"]
            self.assertTrue(variables["ansible_host_key_checking"])
            self.assertFalse(variables["ansible_host_key_auto_add"])
            self.assertEqual(variables["ansible_password"], "test-only-password")
            tasks = kwargs["playbook"][0]["tasks"]
            self.assertEqual(len(tasks), 3)
            self.assertFalse(kwargs["playbook"][0]["gather_facts"])
            callback = kwargs["event_handler"]
            if failure:
                self.assertFalse(callback({"event": "runner_on_failed", "event_data": {"res": {"msg": "test-only-password"}}}))
            else:
                for task, result in [
                    ("InfraPulse read configuration", {"stdout": "hostname lab\ninterface Gi0/12\n switchport access vlan 20\n"}),
                    ("InfraPulse read version", {"stdout": "test version"}),
                    ("InfraPulse read facts", {"ansible_facts": {"ansible_net_model": "test-switch", "unknown_password": "secret"}}),
                ]:
                    self.assertFalse(callback({"event": "runner_on_ok", "event_data": {"task": task, "res": result}}))
            return types.SimpleNamespace(rc=1 if failure else 0, status="failed" if failure else "successful")

        data = {"target": {"host": "192.0.2.20", "port": 22, "template_id": template, "ssh_host_key": "ssh-ed25519 test-only-placeholder"}, "credential": {"username": "readonly", "password": "test-only-password"}, "timeout_seconds": 30}
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {"HOME": directory}), patch.object(adapter.os, "getcwd", return_value=directory), patch.dict(sys.modules, {"ansible_runner": types.SimpleNamespace(run=run)}):
            result = adapter.execute(data)
            self.assertEqual(list(Path(directory).glob("private-*")), [])
        return result, seen

    def test_reviewed_templates_return_configuration_and_filtered_facts(self):
        for template in adapter.TEMPLATES:
            with self.subTest(template=template):
                result, seen = self.execute(template)
                self.assertIn("vlan 20", result["configuration"])
                self.assertEqual(result["facts"]["ansible_net_model"], "test-switch")
                self.assertNotIn("unknown_password", result["facts"])
                tasks = seen["playbook"][0]["tasks"]
                self.assertEqual(tasks[0]["ansible.netcommon.cli_command"]["command"], adapter.TEMPLATES[template][2])
                self.assertNotIn("shell", json.dumps(tasks))

    def test_failure_returns_only_fixed_code(self):
        result, _ = self.execute("cisco_ios_read_config_v1", failure=True)
        self.assertEqual(result, {"error_code": "runner_failed"})
        self.assertNotIn("password", json.dumps(result))


class RestoreAdapterTests(unittest.TestCase):
    def restore(self):
        return {"template_id": adapter.RESTORE_TEMPLATE, "template_digest": adapter.template_digest(), "interface": "GigabitEthernet0/12", "current_vlan": 30, "target_vlan": 20}

    def execute(self, restore=None, failure=None, extra=None):
        seen = {"calls": 0, "writes": 0}

        def run(**kwargs):
            seen["calls"] += 1
            seen["arguments"] = kwargs
            tasks = kwargs["playbook"][0]["tasks"]
            self.assertEqual(len(tasks), 3)
            self.assertEqual(kwargs["playbook"][0]["hosts"], "device")
            self.assertEqual(set(kwargs["inventory"]["all"]["hosts"]), {"device"})
            self.assertNotIn("extravars", kwargs)
            self.assertEqual(tasks[0]["ansible.netcommon.cli_command"], {"command": "show running-config interface GigabitEthernet0/12"})
            self.assertFalse(tasks[0]["changed_when"])
            self.assertIn("ansible.builtin.assert", tasks[1])
            self.assertNotIn("when", tasks[1])  # Assert failure must abort, not skip.
            self.assertTrue(all(task["no_log"] for task in tasks))
            callback = kwargs["event_handler"]
            callback({"event": "runner_on_ok", "event_data": {"task": tasks[0]["name"], "res": {"stdout": "test-only-sensitive-config"}}})
            if failure == "precondition":
                callback({"event": "runner_on_failed", "event_data": {"task": tasks[1]["name"], "res": {"msg": "test-only-password"}}})
                return types.SimpleNamespace(rc=2, status="failed")
            callback({"event": "runner_on_ok", "event_data": {"task": tasks[1]["name"], "res": {}}})
            callback({"event": "runner_on_start", "event_data": {"task": tasks[2]["name"]}})
            seen["writes"] += 1
            event = "runner_on_failed" if failure == "write" else "runner_on_ok"
            callback({"event": event, "event_data": {"task": tasks[2]["name"], "res": {"changed": True, "msg": "test-only-password"}}})
            return types.SimpleNamespace(rc=2 if failure else 0, status="failed" if failure else "successful")

        data = {"target": {"host": "192.0.2.20", "port": 22, "template_id": "cisco_ios_read_config_v1", "ssh_host_key": "ssh-ed25519 test-only-placeholder"}, "credential": {"username": "reviewed", "password": "test-only-password"}, "timeout_seconds": 30, "restore": self.restore() if restore is None else restore}
        if extra:
            data.update(extra)
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {"HOME": directory}), patch.object(adapter.os, "getcwd", return_value=directory), patch.dict(sys.modules, {"ansible_runner": types.SimpleNamespace(run=run)}):
            result = adapter.execute(data)
            self.assertEqual(list(Path(directory).glob("private-*")), [])
        return result, seen

    def test_typed_workflow_is_exact_and_acknowledgement_is_not_verification(self):
        result, seen = self.execute()
        self.assertEqual(result, {"facts": {}})
        self.assertEqual(seen["writes"], 1)
        tasks = seen["arguments"]["playbook"][0]["tasks"]
        self.assertEqual(tasks[2]["cisco.ios.ios_config"], {"parents": ["interface GigabitEthernet0/12"], "lines": ["switchport access vlan 20"], "match": "line", "replace": "line", "backup": False, "save_when": "never"})
        self.assertNotIn("shell", json.dumps(tasks))
        self.assertNotIn("src", tasks[2]["cisco.ios.ios_config"])
        conditions = tasks[1]["ansible.builtin.assert"]["that"]
        self.assertTrue(any("switchport access vlan 30" in expression for expression in conditions))
        self.assertTrue(any("switchport mode access" in expression for expression in conditions))
        self.assertTrue(any("reject('match'" in expression for expression in conditions))
        self.assertTrue(any("unique" in expression for expression in conditions))
        self.assertFalse(any("test-only" in expression for expression in conditions))

    def test_failed_immediate_guard_never_reaches_write(self):
        result, seen = self.execute(failure="precondition")
        self.assertEqual(result, {"error_code": "precondition_failed"})
        self.assertEqual(seen["writes"], 0)
        self.assertNotIn("password", json.dumps(result))

    def test_write_failure_only_returns_fixed_uncertain_error(self):
        result, seen = self.execute(failure="write")
        self.assertEqual(result, {"error_code": "runner_failed"})
        self.assertEqual(seen["writes"], 1)
        self.assertNotIn("password", json.dumps(result))

    def test_invalid_typed_inputs_and_arbitrary_extra_vars_never_call_runner(self):
        cases = [
            {"interface": "Gi0/12"}, {"interface": "Port-channel1"}, {"interface": "GigabitEthernet0/12; reboot"},
            {"template_id": "arbitrary_playbook"}, {"template_digest": "0" * 64}, {"target_vlan": 4095},
            {"current_vlan": 0}, {"target_vlan": True}, {"target_vlan": "20"}, {"target_vlan": 30},
            {"extra_vars": {"command": "reload"}},
        ]
        for values in cases:
            with self.subTest(values=values):
                restore = self.restore()
                restore.update(values)
                result, seen = self.execute(restore=restore)
                self.assertEqual(result, {"error_code": "invalid_result"})
                self.assertEqual(seen["calls"], 0)
        result, seen = self.execute(extra={"extra_vars": {"command": "reload"}})
        self.assertEqual(result, {"error_code": "invalid_result"})
        self.assertEqual(seen["calls"], 0)


@unittest.skipUnless(Environment is not None, "install optional Jinja2 test dependency to evaluate the immediate Ansible assertion grammar")
class RestoreGuardExpressionTests(unittest.TestCase):
    def allowed(self, configuration):
        # Evaluate the generated trusted assert expressions with Jinja's real
        # parser; only Ansible's regex filters/tests are supplied by this fixture.
        environment = Environment(undefined=StrictUndefined)
        environment.filters["regex_findall"] = lambda value, pattern: re.findall(pattern, value)
        environment.tests["match"] = lambda value, pattern: re.match(pattern, value) is not None
        restore = {"template_id": adapter.RESTORE_TEMPLATE, "template_digest": adapter.template_digest(), "interface": "GigabitEthernet0/12", "current_vlan": 30, "target_vlan": 20}
        conditions = adapter.restore_tasks(restore)[1]["ansible.builtin.assert"]["that"]
        variables = {"infrapulse_port_state": types.SimpleNamespace(stdout=configuration)}
        return all(environment.compile_expression(expression)(**variables) for expression in conditions)

    def test_exact_safe_port_and_uniform_indentation_are_allowed(self):
        for spaces in (" ", "  ", "    "):
            config = f"Building configuration...\nCurrent configuration : 120 bytes\n!\ninterface GigabitEthernet0/12\n{spaces}description Lab endpoint\n{spaces}switchport\n{spaces}switchport mode access\n{spaces}switchport access vlan 30\n{spaces}mtu 1500\n{spaces}speed auto\n{spaces}duplex full\n{spaces}no shutdown\n!\nend\n"
            self.assertTrue(self.allowed(config))
            self.assertTrue(self.allowed(config.replace("\n", "\r\n")))

    def test_changed_unknown_ambiguous_or_protected_grammar_is_rejected(self):
        base = "interface GigabitEthernet0/12\n switchport mode access\n switchport access vlan 30\n"
        cases = {
            "changed_vlan": base.replace("vlan 30", "vlan 40"),
            "missing_vlan": base.replace(" switchport access vlan 30\n", ""),
            "unknown_mode": base.replace(" switchport mode access\n", ""),
            "trunk": base.replace("mode access", "mode trunk"),
            "trunk_setting": base + " switchport trunk allowed vlan 20,30\n",
            "lag": base + " channel-group 1 mode active\n",
            "routed": base + " no switchport\n",
            "voice": base + " switchport voice vlan 40\n",
            "management": base + " ip address 192.0.2.20 255.255.255.0\n",
            "nested": base + "  duplex full\n",
            "tab": base + "\tduplex full\n",
            "duplicate_vlan": base + " switchport access vlan 30\n",
            "duplicate_mode": base + " switchport mode access\n",
            "duplicate_description": base + " description A\n description B\n",
            "conflicting_admin_state": base + " shutdown\n no shutdown\n",
            "unknown_command": base + " service-policy input CRITICAL\n",
            "invalid_mtu": base + " mtu 0\n",
            "oversized_mtu": base + " mtu 65536\n",
            "unknown_speed": base + " speed unknown\n",
            "unknown_duplex": base + " duplex unknown\n",
            "other_interface": base + "interface GigabitEthernet0/13\n switchport mode access\n switchport access vlan 30\n",
            "alias_header": base.replace("GigabitEthernet0/12", "Gi0/12"),
        }
        for name, configuration in cases.items():
            with self.subTest(name=name):
                self.assertFalse(self.allowed(configuration))


if __name__ == "__main__":
    unittest.main()
