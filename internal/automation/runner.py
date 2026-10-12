"""Trusted closed Ansible Runner adapter. Input arrives on stdin, never argv.
Only structured metadata goes to stdout. Never print raw Runner stdout/events.
"""
import importlib.metadata
import hashlib
import json
import os
import signal
import resource
import re
import subprocess
import sys
import tempfile
import time
from pathlib import Path

TEMPLATES = {
    "cisco_ios_read_config_v1": ("cisco.ios.ios", "cisco.ios", "show running-config", "show version"),
    "arista_eos_read_config_v1": ("arista.eos.eos", "arista.eos", "show running-config", "show version"),
    "vyos_read_config_v1": ("vyos.vyos.vyos", "vyos.vyos", "show configuration commands", "show version"),
}

RESTORE_TEMPLATE = "cisco_ios_restore_access_vlan_v1"
PHYSICAL_INTERFACE = re.compile(r"(?:GigabitEthernet|TenGigabitEthernet|TwentyFiveGigE|FortyGigabitEthernet|HundredGigE|FastEthernet|Ethernet)[0-9]+(?:/[0-9]+){0,3}\Z")


def template_digest():
    return hashlib.sha256(Path(__file__).read_bytes()).hexdigest()


def restore_tasks(restore):
    # No user-supplied command, path, module argument, inventory or extra_vars.
    if not isinstance(restore, dict) or set(restore) != {"template_id", "template_digest", "interface", "current_vlan", "target_vlan"}:
        raise ValueError("invalid typed restoration")
    interface = restore["interface"]
    current = restore["current_vlan"]
    target = restore["target_vlan"]
    if restore["template_id"] != RESTORE_TEMPLATE or restore["template_digest"] != template_digest() or not isinstance(interface, str) or len(interface) > 64 or not PHYSICAL_INTERFACE.fullmatch(interface) or type(current) is not int or type(target) is not int or not 1 <= current <= 4094 or not 1 <= target <= 4094 or current == target:
        raise ValueError("invalid typed restoration")
    # Fail closed on any unknown/repeated/nested setting. This intentionally
    # supports a narrow flat Cisco access-port grammar, matching backend policy.
    allowed = (
        "description .+|switchport|switchport mode access|switchport access vlan [0-9]+|"
        "shutdown|no shutdown|mtu [0-9]+|speed (?:auto|10|100|1000|10000|25000|40000|100000)|duplex (?:auto|full|half)"
    )
    output_line = rf"^(?:interface {interface}| +(?:{allowed})|!.*|Building configuration\.\.\.|Current configuration\s*:\s*[0-9]+ bytes|end)?$"
    def count(pattern):
        return f"(infrapulse_port_state.stdout | regex_findall({pattern!r}) | length)"
    conditions = [
        "infrapulse_port_state.stdout is string",
        f"{count(rf'(?m)^interface {interface}\r?$')} == 1",
        f"{count(r'(?m)^ +switchport mode access\r?$')} == 1",
        f"{count(r'(?m)^ +switchport mode [^\r\n]+\r?$')} == 1",
        f"{count(rf'(?m)^ +switchport access vlan {current}\r?$')} == 1",
        f"{count(r'(?m)^ +switchport access vlan [^\r\n]+\r?$')} == 1",
        f"(infrapulse_port_state.stdout.splitlines() | reject('match', {output_line!r}) | list | length) == 0",
        r"(infrapulse_port_state.stdout | regex_findall('(?m)^( +)\\S') | unique | list | length) == 1",
    ]
    for pattern in (r"(?m)^ +description [^\r\n]+\r?$", r"(?m)^ +switchport\r?$", r"(?m)^ +(?:no )?shutdown\r?$", r"(?m)^ +mtu [0-9]+\r?$", r"(?m)^ +speed [^\r\n]+\r?$", r"(?m)^ +duplex [^\r\n]+\r?$"):
        conditions.append(f"{count(pattern)} <= 1")
    mtu = r"(?m)^ +mtu ([0-9]+)\r?$"
    conditions.append(f"{count(mtu)} == 0 or (1 <= (infrapulse_port_state.stdout | regex_findall({mtu!r}) | first | int) <= 65535)")
    return [
        {"name": "InfraPulse fresh approved interface", "ansible.netcommon.cli_command": {"command": f"show running-config interface {interface}"}, "register": "infrapulse_port_state", "changed_when": False, "no_log": True},
        {"name": "InfraPulse validate approved access port", "ansible.builtin.assert": {"that": conditions, "fail_msg": "approved access-port precondition changed", "quiet": True}, "no_log": True},
        {"name": "InfraPulse restore approved access VLAN", "cisco.ios.ios_config": {"parents": [f"interface {interface}"], "lines": [f"switchport access vlan {target}"], "match": "line", "replace": "line", "backup": False, "save_when": "never"}, "no_log": True},
    ]


def probe():
    import ansible_runner  # noqa: F401
    versions = {name: importlib.metadata.version(name) for name in ("ansible-core", "ansible-runner", "paramiko")}
    galaxy = subprocess.run(["ansible-galaxy", "collection", "list", "--format", "json"], capture_output=True, timeout=15, check=True)
    collections = json.loads(galaxy.stdout)
    available = {}
    for path, entries in collections.items():
        del path
        available.update(entries)
    ids = [name for name, (_, collection, _, _) in TEMPLATES.items() if collection in available and "ansible.netcommon" in available]
    if ids:
        modules = ["ansible.netcommon.cli_command"] + [TEMPLATES[name][0] + "_facts" for name in ids]
        if "cisco_ios_read_config_v1" in ids:
            modules.append("cisco.ios.ios_config")
        documentation = subprocess.run(["ansible-doc", "--json", *modules], capture_output=True, timeout=10, check=True)
        if set(modules) - set(json.loads(documentation.stdout)):
            return {"available": False, "error_code": "runner_unavailable"}
    for collection in {TEMPLATES[name][1] for name in ids} | ({"ansible.netcommon"} if ids else set()):
        versions[collection] = available[collection]["version"]
    versions["restore_template_sha256"] = template_digest()
    advertised = ids + ([RESTORE_TEMPLATE] if "cisco_ios_read_config_v1" in ids else [])
    return {"available": bool(ids), "versions": versions, "templates": advertised, "error_code": "" if ids else "runner_unavailable"}


def execute(data):
    import ansible_runner
    if not isinstance(data, dict) or set(data) - {"target", "credential", "timeout_seconds", "restore"}:
        return {"error_code": "invalid_result"}
    target = data["target"]
    credential = data["credential"]
    platform, collection, config_command, version_command = TEMPLATES[target["template_id"]]
    del collection
    cancelled = [False]
    signal.signal(signal.SIGTERM, lambda *_: cancelled.__setitem__(0, True))
    signal.signal(signal.SIGINT, lambda *_: cancelled.__setitem__(0, True))
    timeout = data["timeout_seconds"]
    deadline = time.monotonic() + timeout
    result = {}
    failed = [False]
    restore = data.get("restore")
    write_started = [False]
    precondition_failed = [False]
    if restore is not None and target["template_id"] != "cisco_ios_read_config_v1":
        return {"error_code": "invalid_result"}
    try:
        write_tasks = restore_tasks(restore) if restore is not None else None
    except (ValueError, TypeError):
        return {"error_code": "invalid_result"}

    # Results are consumed in memory; the handler suppresses event persistence
    # and Runner suppresses stdout artifacts. Temporary files have private modes.
    def event_handler(event):
        task = event.get("event_data", {}).get("task")
        if event.get("event") == "runner_on_start" and task == "InfraPulse restore approved access VLAN":
            write_started[0] = True
        if event.get("event") in ("runner_on_failed", "runner_on_unreachable"):
            failed[0] = True
            if task == "InfraPulse validate approved access port" and not write_started[0]:
                precondition_failed[0] = True
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
        tasks = [
            {"name": "InfraPulse read configuration", "ansible.netcommon.cli_command": {"command": config_command}, "changed_when": False},
            {"name": "InfraPulse read version", "ansible.netcommon.cli_command": {"command": version_command}, "changed_when": False},
            {"name": "InfraPulse read facts", platform.rsplit(".", 1)[0] + "." + platform.rsplit(".", 1)[1] + "_facts": {"gather_subset": ["min"]}, "changed_when": False},
        ]
        if write_tasks is not None:
            tasks = write_tasks
        runner = ansible_runner.run(
            private_data_dir=private,
            playbook=[{"name": "InfraPulse approved access VLAN template" if restore is not None else "InfraPulse reviewed read-only template", "hosts": "device", "gather_facts": False, "tasks": tasks}],
            inventory=inventory,
            quiet=True,
            suppress_output_file=True,
            event_handler=event_handler,
            cancel_callback=lambda: cancelled[0] or time.monotonic() >= deadline,
            envvars={"ANSIBLE_HOST_KEY_CHECKING": "True", "ANSIBLE_PERSISTENT_LOG_MESSAGES": "False", "ANSIBLE_RETRY_FILES_ENABLED": "False"},
        )
        if cancelled[0] or time.monotonic() >= deadline:
            return {"error_code": "cancelled" if cancelled[0] else "timed_out"}
        if restore is not None:
            if precondition_failed[0] and not write_started[0]:
                return {"error_code": "precondition_failed"}
            if failed[0] or runner.rc != 0 or runner.status != "successful":
                return {"error_code": "runner_failed"}
            # This is execution acknowledgement only. Backend must perform a
            # separate read-only gather and B11.1 comparison before success.
            return {"facts": {}}
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
