"""Trusted closed Ansible Runner adapter. Input arrives on stdin, never argv.
Only structured metadata goes to stdout. Never print raw Runner stdout/events.
"""
import importlib.metadata
import json
import os
import re
import signal
import resource
import subprocess
import sys
import tempfile
import time

TEMPLATES = {
    "cisco_ios_read_config_v1": ("cisco.ios.ios", "cisco.ios", "show running-config", "show version", "cisco.ios.ios_config"),
    "arista_eos_read_config_v1": ("arista.eos.eos", "arista.eos", "show running-config", "show version", "arista.eos.eos_config"),
    "vyos_read_config_v1": ("vyos.vyos.vyos", "vyos.vyos", "show configuration commands", "show version", None),
}
SAFE_INTERFACE = re.compile(r"^(?:GigabitEthernet|TenGigabitEthernet|TwentyFiveGigE|FortyGigabitEthernet|HundredGigE|FastEthernet|Ethernet)[0-9]+(?:/[0-9]+){0,3}$")
SAFE_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$")


def probe():
    import ansible_runner  # noqa: F401
    versions = {name: importlib.metadata.version(name) for name in ("ansible-core", "ansible-runner", "paramiko")}
    galaxy = subprocess.run(["ansible-galaxy", "collection", "list", "--format", "json"], capture_output=True, timeout=15, check=True)
    collections = json.loads(galaxy.stdout)
    available = {}
    for path, entries in collections.items():
        del path
        available.update(entries)
    ids = [name for name, (_, collection, _, _, _) in TEMPLATES.items() if collection in available and "ansible.netcommon" in available]
    if ids:
        modules = ["ansible.netcommon.cli_command"] + [TEMPLATES[name][0] + "_facts" for name in ids]
        modules += [TEMPLATES[name][4] for name in ids if TEMPLATES[name][4] is not None]
        documentation = subprocess.run(["ansible-doc", "--json", *modules], capture_output=True, timeout=10, check=True)
        if set(modules) - set(json.loads(documentation.stdout)):
            return {"available": False, "error_code": "runner_unavailable"}
    for collection in {TEMPLATES[name][1] for name in ids} | ({"ansible.netcommon"} if ids else set()):
        versions[collection] = available[collection]["version"]
    return {"available": bool(ids), "versions": versions, "templates": ids, "error_code": "" if ids else "runner_unavailable"}


def execute(data):
    import ansible_runner
    target = data["target"]
    credential = data["credential"]
    platform, collection, config_command, version_command, write_module = TEMPLATES[target["template_id"]]
    del collection
    cancelled = [False]
    signal.signal(signal.SIGTERM, lambda *_: cancelled.__setitem__(0, True))
    signal.signal(signal.SIGINT, lambda *_: cancelled.__setitem__(0, True))
    timeout = data["timeout_seconds"]
    deadline = time.monotonic() + timeout
    result = {}
    failed = [False]

    # Results are consumed in memory; the handler suppresses event persistence
    # and Runner suppresses stdout artifacts. Temporary files have private modes.
    def event_handler(event):
        if event.get("event") in ("runner_on_failed", "runner_on_unreachable"):
            failed[0] = True
        if event.get("event") == "runner_on_ok":
            event_data = event.get("event_data", {})
            if event_data.get("task") == "InfraPulse read configuration":
                result["configuration"] = event_data.get("res", {}).get("stdout", "")
            elif event_data.get("task") == "InfraPulse read version":
                result["facts"] = {"version_output": event_data.get("res", {}).get("stdout", ""), "platform": platform}
            elif event_data.get("task") == "InfraPulse read facts":
                facts = event_data.get("res", {}).get("ansible_facts", {})
                result.setdefault("facts", {}).update({key: facts[key] for key in (
                    "ansible_net_hostname", "ansible_net_model", "ansible_net_serialnum", "ansible_net_version") if key in facts})
        return False  # Do not persist event files with sensitive results.

    with tempfile.TemporaryDirectory(prefix="private-", dir=os.getcwd()) as private:
        os.chmod(private, 0o700)
        known_hosts = os.path.join(private, "known_hosts")
        host = target["host"]
        # Paramiko uses plain host for port 22 and [host]:port otherwise.
        host_entry = host if target["port"] == 22 else f"[{host}]:{target['port']}"
        with open(known_hosts, "w", encoding="utf-8") as file:
            file.write(f"{host_entry} {target['ssh_host_key']}\n")
        os.chmod(known_hosts, 0o600)
        # Paramiko reads HOME/.ssh/known_hosts, with no automatic trust.
        ssh_dir = os.path.join(os.environ["HOME"], ".ssh")
        os.makedirs(ssh_dir, mode=0o700, exist_ok=True)
        import shutil
        shutil.copyfile(known_hosts, os.path.join(ssh_dir, "known_hosts"))
        variables = {
            "ansible_host": host,
            "ansible_port": target["port"],
            "ansible_user": credential["username"],
            "ansible_password": credential["password"],
            "ansible_connection": "ansible.netcommon.network_cli",
            "ansible_network_os": platform,
            "ansible_network_cli_ssh_type": "paramiko",
            "ansible_host_key_checking": True,
            "ansible_host_key_auto_add": False,
            "ansible_command_timeout": min(30, timeout),
            "ansible_connect_timeout": min(15, timeout),
            "ansible_persistent_log_messages": False,
        }
        if credential.get("enable_password"):
            variables.update(ansible_become=True, ansible_become_method="enable", ansible_become_password=credential["enable_password"])
        inventory = {"all": {"hosts": {"device": variables}}}
        tasks = []
        operation = target.get("operation")
        if operation is not None:
            if (not isinstance(operation, dict) or write_module is None or operation.get("type") != "restore_access_vlan" or
                    not isinstance(operation.get("remediation_id"), str) or not SAFE_ID.fullmatch(operation["remediation_id"]) or
                    not SAFE_INTERFACE.fullmatch(operation.get("interface", "")) or
                    not isinstance(operation.get("vlan"), int) or not 1 <= operation["vlan"] <= 4094 or
                    set(operation) != {"type", "remediation_id", "interface", "vlan"}):
                return {"error_code": "destination_denied"}
            tasks.append({
                "name": "InfraPulse approved access VLAN restoration",
                write_module: {
                    "parents": [f"interface {operation['interface']}"],
                    "lines": ["switchport", "switchport mode access", f"switchport access vlan {operation['vlan']}"],
                    "match": "line",
                    "replace": "line",
                    "save_when": "modified",
                },
            })
        tasks.extend([
            {"name": "InfraPulse read configuration", "ansible.netcommon.cli_command": {"command": config_command}, "changed_when": False},
            {"name": "InfraPulse read version", "ansible.netcommon.cli_command": {"command": version_command}, "changed_when": False},
            {"name": "InfraPulse read facts", platform.rsplit(".", 1)[0] + "." + platform.rsplit(".", 1)[1] + "_facts": {"gather_subset": ["min"]}, "changed_when": False},
        ])
        runner = ansible_runner.run(
            private_data_dir=private,
            playbook=[{"name": "InfraPulse reviewed read-only template", "hosts": "device", "gather_facts": False, "tasks": tasks}],
            inventory=inventory,
            quiet=True,
            suppress_output_file=True,
            event_handler=event_handler,
            cancel_callback=lambda: cancelled[0] or time.monotonic() >= deadline,
            envvars={"ANSIBLE_HOST_KEY_CHECKING": "True", "ANSIBLE_PERSISTENT_LOG_MESSAGES": "False", "ANSIBLE_RETRY_FILES_ENABLED": "False"},
        )
        if cancelled[0] or time.monotonic() >= deadline:
            return {"error_code": "cancelled" if cancelled[0] else "timed_out"}
        if failed[0] or runner.rc != 0 or runner.status != "successful" or not result.get("configuration") or "facts" not in result:
            return {"error_code": "runner_failed"}
        if len(result["configuration"].encode("utf-8")) > 4194304 or len(json.dumps(result["facts"]).encode("utf-8")) > 131072:
            return {"error_code": "invalid_result"}
        return result


def main():
    os.umask(0o077)
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    resource.setrlimit(resource.RLIMIT_FSIZE, (33554432, 33554432))
    resource.setrlimit(resource.RLIMIT_AS, (1073741824, 1073741824))
    try:
        data = json.loads(sys.stdin.buffer.read(65537))
        result = probe() if data.get("probe") is True else execute(data)
        json.dump(result, sys.stdout, ensure_ascii=True)
    except Exception:
        json.dump({"error_code": "runner_unavailable" if "data" in locals() and data.get("probe") else "runner_failed"}, sys.stdout)


if __name__ == "__main__":
    main()
