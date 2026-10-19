#!/usr/bin/env python3
"""Opt-in real user-systemd acceptance test. Never touches portmap.service.
Usage: python3 tests/upgrade_systemd.py /absolute/path/to/new/portmap
"""
import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
import uuid

source = Path(sys.argv[1]).resolve()
root = Path(tempfile.mkdtemp(prefix="portmap-update-test-", dir=os.environ.get("TMPDIR", "/tmp")))
unit = "portmap-update-test-" + uuid.uuid4().hex[:10] + ".service"
unit_path = Path.home() / ".config/systemd/user" / unit
binary = root / "portmap"
data = root / "data"
data.mkdir(mode=0o700)
shutil.copy2(source, binary)
# Produce a distinct valid candidate without changing executable behavior.
candidate = root / "candidate"
candidate.write_bytes(source.read_bytes() + b"portmap-acceptance-candidate\n")
candidate.chmod(0o700)
with socket.socket() as sock:
    sock.bind(("127.0.0.1", 0))
    address = "127.0.0.1:" + str(sock.getsockname()[1])
config = {"remotes": [{"name": "test", "host": "127.0.0.1", "port": 22, "user": "test", "auth": {"method": "password", "password": "test-only"}}],
          "groups": [{"name": "test", "mappings": [
              {"name": "stopped", "type": "ssh", "remote": "test", "listen_port": 19991, "local_port": 22, "description": "保持停止的管理入口", "expires_at": "2099-01-01T00:00:00Z"},
              {"name": "vm", "type": "multipass", "instance": "test-vm", "instance_port": 22, "listen_port": 19992, "description": "虚拟机入口", "expires_at": "2000-01-01T00:00:00Z"}]}]}
config_path = data / "config.json"
config_path.write_text(json.dumps(config, ensure_ascii=False))
config_path.chmod(0o600)
state_path = data / "state.json"
state_path.write_text(json.dumps({"mappings": {"test/stopped": {"running": False}, "test/vm": {"running": False}}}))
state_path.chmod(0o600)


def ctl(*args, check=True):
    return subprocess.run(["systemctl", "--user", *args], check=check, capture_output=True, text=True)


def health():
    with urllib.request.urlopen("http://" + address + "/api/health", timeout=2) as response:
        return json.load(response)


def ready():
    for _ in range(75):
        try:
            return health()
        except OSError:
            time.sleep(0.2)
    raise RuntimeError("isolated test service did not start")


def run_update(path, *extra, success=True):
    result = subprocess.run([str(source), "--data-dir", str(data), "--api-addr", address,
                             "upgrade", "--binary", str(path), "--install-path", str(binary),
                             "--service", unit, "--timeout", "6s", *extra], capture_output=True, text=True)
    print(result.stdout + result.stderr, flush=True)
    assert (result.returncode == 0) == success, result
    return result


try:
    unit_path.parent.mkdir(parents=True, exist_ok=True)
    unit_path.write_text("[Unit]\nStartLimitIntervalSec=0\n[Service]\nType=simple\n"
                         f"ExecStart={binary} --data-dir {data} --api-addr {address} serve --web-addr= --docker-host unix://{root}/absent.sock\n"
                         "Restart=always\nRestartSec=0.2\n")
    ctl("daemon-reload")
    ctl("start", unit)
    before = ready()
    config_bytes = config_path.read_bytes()
    run_update(candidate)
    assert ready()["pid"] != before["pid"]
    assert config_path.read_bytes() == config_bytes
    assert all(not value["running"] for value in json.loads(state_path.read_text())["mappings"].values())
    with urllib.request.urlopen("http://" + address + "/api/mappings", timeout=2) as response:
        rows = {row["name"]: row for row in json.load(response)}
    assert rows["stopped"]["expires_at"] == "2099-01-01T00:00:00Z"
    assert rows["vm"]["invalid_reason"] == "expired" and rows["vm"]["status"] == "invalid"
    print("PASS: successful replacement preserves expiry, invalidation, descriptions and stopped state", flush=True)
    before = health()
    run_update(candidate)
    assert health()["pid"] == before["pid"]
    print("PASS: same-binary update does not restart service", flush=True)
    run_update(source, "--sha256", "0" * 64, success=False)
    assert health()["pid"] == before["pid"]
    print("PASS: invalid checksum leaves running service untouched", flush=True)
    bad = root / "bad-candidate"
    bad.write_text("#!/bin/sh\nif [ \"$1\" = version ]; then printf '%s\\n' '{\"config_version\":1}'; exit 0; fi\nexit 42\n")
    bad.chmod(0o700)
    prior_digest = hashlib.sha256(binary.read_bytes()).hexdigest()
    result = run_update(bad, success=False)
    assert "rolled back successfully" in result.stderr
    assert hashlib.sha256(binary.read_bytes()).hexdigest() == prior_digest
    assert config_path.read_bytes() == config_bytes
    ready()
    print("PASS: failed startup automatically restores previous executable, config and state", flush=True)
finally:
    ctl("stop", unit, check=False)
    ctl("reset-failed", unit, check=False)
    unit_path.unlink(missing_ok=True)
    ctl("daemon-reload", check=False)
    shutil.rmtree(root)
