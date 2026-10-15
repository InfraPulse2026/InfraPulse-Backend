"""Adapter contract tests with an explicit fake Runner (no hardware claim)."""
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest.mock import patch

if sys.platform == "win32":
    sys.modules.setdefault("resource", types.SimpleNamespace(RLIMIT_CORE=0, RLIMIT_FSIZE=1, RLIMIT_AS=2, setrlimit=lambda *_: None))
path = Path(__file__).resolve().parent.parent / "internal" / "automation" / "runner.py"
spec = importlib.util.spec_from_file_location("infrapulse_adapter", path)
adapter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(adapter)


class AdapterTests(unittest.TestCase):
    def execute(self, template, failure=False, operation=None):
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
            self.assertEqual(len(tasks), 4 if operation else 3)
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
        if operation is not None:
            data["target"]["operation"] = operation
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

    def test_remediation_is_one_fixed_vendor_module_then_recollection(self):
        operation = {"type": "restore_access_vlan", "remediation_id": "remediation_one", "interface": "GigabitEthernet0/12", "vlan": 20}
        result, seen = self.execute("cisco_ios_read_config_v1", operation=operation)
        self.assertIn("configuration", result)
        task = seen["playbook"][0]["tasks"][0]
        self.assertEqual(task["name"], "InfraPulse approved access VLAN restoration")
        self.assertEqual(task["cisco.ios.ios_config"]["parents"], ["interface GigabitEthernet0/12"])
        self.assertEqual(task["cisco.ios.ios_config"]["lines"][-1], "switchport access vlan 20")
        self.assertEqual(task["cisco.ios.ios_config"]["save_when"], "modified")

    def test_remediation_rejects_unreviewed_shape_and_vyos(self):
        bad = {"type": "restore_access_vlan", "remediation_id": "remediation_one", "interface": "GigabitEthernet0/12", "vlan": 20, "commands": ["reload"]}
        result, _ = self.execute("cisco_ios_read_config_v1", operation=bad)
        self.assertEqual(result, {"error_code": "destination_denied"})
        good = {"type": "restore_access_vlan", "remediation_id": "remediation_one", "interface": "Ethernet1", "vlan": 20}
        result, _ = self.execute("vyos_read_config_v1", operation=good)
        self.assertEqual(result, {"error_code": "destination_denied"})

    def test_failure_returns_only_fixed_code(self):
        result, _ = self.execute("cisco_ios_read_config_v1", failure=True)
        self.assertEqual(result, {"error_code": "runner_failed"})
        self.assertNotIn("password", json.dumps(result))


if __name__ == "__main__":
    unittest.main()
