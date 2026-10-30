#!/usr/bin/env python3
"""Real SIGKILL fault injection for attach/detach transaction phases.

The daemon is started with ORBSLEEP_FAILPOINT=<phase>; when the hook is hit the
process SIGKILLs itself. The harness then starts a normal daemon, runs recovery
and verifies the original Compose/ports are restored (attach) or the managed
state is retained (detach).
"""
from __future__ import annotations

import argparse
import os
import shutil
import subprocess
import sys
import tempfile
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "scripts"))
from phase0 import (  # noqa: E402
    DAEMON, Failure, ctl, ctl_json, find_docker, free_port, http_ok, run,
    service_items, wait_for, wait_http,
)

DATA_DIR = Path.home() / "Library" / "Application Support" / "OrbSleep"


def daemon_pid(data_dir: Path) -> int | None:
    out = run(["pgrep", "-f", f"orbsleepd --data-dir {data_dir}"], check=False).stdout
    for line in out.splitlines():
        try:
            return int(line.strip())
        except ValueError:
            pass
    return None


def kill_daemon(data_dir: Path):
    pid = daemon_pid(data_dir)
    if pid:
        os.kill(pid, 9)
    deadline = time.time() + 20
    while time.time() < deadline:
        try:
            ctl(data_dir, "ping", timeout=2)
        except Exception:  # noqa: BLE001
            return
        time.sleep(0.2)
    raise Failure("daemon did not exit after SIGKILL")


def start_daemon(data_dir: Path, failpoint: str | None = None) -> subprocess.Popen:
    env = dict(os.environ)
    if failpoint:
        env["ORBSLEEP_FAILPOINT"] = failpoint
    proc = subprocess.Popen([str(DAEMON), "--data-dir", str(data_dir)], env=env,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    wait_for("daemon ping", lambda: ctl(data_dir, "ping", timeout=5) or True, 30)
    return proc


def ensure_proxy(data_dir: Path):
    status = ctl_json(data_dir, "status", "--json")
    if not status.get("proxy", {}).get("adminReachable"):
        start_daemon(data_dir)
        ctl(data_dir, "proxy-setup", timeout=900)


def docker_env(data_dir: Path):
    env = dict(os.environ)
    status = ctl_json(data_dir, "status", "--json")
    endpoint = status.get("context", {}).get("endpoint") or ""
    if endpoint.startswith("unix://"):
        env["DOCKER_HOST"] = endpoint
    return env


def service_for(data_dir: Path, name: str):
    return next((s for s in service_items(data_dir) if s["containerName"] == name), None)


def attach(data_dir: Path, project: str):
    scan = ctl_json(data_dir, "scan", "--json", timeout=120)
    target = next((c for c in scan.get("containers", []) if c.get("composeProject") == project
                   and c.get("composeService") == "web"), None)
    if not target:
        raise Failure("scan did not find test web")
    plan = ctl_json(data_dir, "plan", "--json", target["id"], timeout=120)
    if plan.get("status") != "ready":
        raise Failure("plan blocked")
    ctl(data_dir, "apply", plan["id"], timeout=900)
    svc = service_for(data_dir, target["name"])
    if not svc:
        raise Failure("attach did not create record")
    return svc, target["name"]


def wait_no_service(data_dir: Path, name: str):
    wait_for("service restored", lambda: service_for(data_dir, name) is None, 120, interval=1)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--data-dir", default=str(DATA_DIR))
    ap.add_argument("--keep", action="store_true")
    args = ap.parse_args()
    data_dir = Path(args.data_dir)
    docker = find_docker()
    project = f"orbsleep-fault-{int(time.time())}"
    workdir = Path(tempfile.mkdtemp(prefix="orbsleep-fault-"))
    compose = workdir / "docker-compose.yml"
    svc = None
    failures: list[str] = []
    original = ""
    daemon: subprocess.Popen | None = None
    try:
        ensure_proxy(data_dir)
        env = docker_env(data_dir)
        port = free_port()
        original = f'''services:
  web:
    image: nginx:alpine
    ports:
      - "127.0.0.1:{port}:80"
'''
        compose.write_text(original, encoding="utf-8")
        run([docker, "compose", "-p", project, "-f", str(compose), "up", "-d"], env=env, timeout=300)
        wait_http(port, 60, "test web direct")

        for phase in ["proxy_updated", "compose_written", "container_ready", "gateway_ready", "before_database_commit"]:
            step = f"attach SIGKILL at {phase}"
            print(f"\n▶ {step}")
            kill_daemon(data_dir)
            daemon = start_daemon(data_dir, phase)
            try:
                attach(data_dir, project)
                raise Failure("attach unexpectedly completed before failpoint")
            except Failure:
                pass
            wait_for("daemon killed by failpoint", lambda: daemon_pid(data_dir) is None, 20, interval=0.5)
            daemon = start_daemon(data_dir)
            ctl(data_dir, "recover", timeout=900)
            wait_no_service(data_dir, f"{project}-web-1")
            body = compose.read_text(encoding="utf-8")
            if body != original:
                raise Failure(f"recovery did not restore original after {phase}:\n{body}")
            wait_http(port, 90, "direct access after recovery")
            print(f"  PASS: {phase}")

        for phase in ["detach_gateway_removed", "detach_compose_written", "detach_container_ready", "detach_before_record_delete"]:
            print(f"\n▶ detach SIGKILL at {phase}")
            svc, cname = attach(data_dir, project)
            kill_daemon(data_dir)
            daemon = start_daemon(data_dir, phase)
            try:
                ctl(data_dir, "restore", svc["id"], timeout=900)
                raise Failure("restore unexpectedly completed before failpoint")
            except Failure:
                pass
            wait_for("daemon killed by failpoint", lambda: daemon_pid(data_dir) is None, 20, interval=0.5)
            daemon = start_daemon(data_dir)
            ctl(data_dir, "recover", timeout=900)
            if not service_for(data_dir, cname):
                raise Failure(f"detach recovery removed the managed record after {phase}")
            body = compose.read_text(encoding="utf-8")
            if f"127.0.0.1:{port}:80" in body:
                raise Failure(f"detach recovery left restored state after {phase}")
            wait_http(port, 90, "managed service reachable after detach recovery")
            ctl(data_dir, "restore", svc["id"], timeout=900)
            svc = None
            print(f"  PASS: {phase}")
    except (Failure, Exception) as exc:  # noqa: BLE001
        failures.append(str(exc))
        print("\n✘ FAIL:", exc, file=sys.stderr)
        try:
            if svc:
                ctl(data_dir, "restore", svc["id"], timeout=900)
        except Exception:
            pass
    finally:
        if not args.keep:
            try:
                run([docker, "compose", "-p", project, "-f", str(compose), "down", "--remove-orphans"],
                    env=env, timeout=180, check=False)
            except Exception:
                pass
            shutil.rmtree(workdir, ignore_errors=True)
        # Always leave a normal daemon running for the user.
        try:
            start_daemon(data_dir)
        except Exception:
            pass
    if failures:
        return 1
    print("\n✅ fault injection E2E passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
