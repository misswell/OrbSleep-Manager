#!/usr/bin/env python3
"""Real OrbStack reliability scenarios for OrbSleep.

Covers UDP preservation, safe merge after external edits, deleted Compose
recovery, permission refusal and container recreation. Uses a disposable
Compose project and the real OrbSleep daemon/proxy.
"""
from __future__ import annotations

import argparse
import json
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
    DAEMON, Failure, ctl, ctl_json, find_docker, free_port, http_ok,
    run, service_items, wait_for, wait_http,
)

DEFAULT_DATA_DIR = Path.home() / "Library" / "Application Support" / "OrbSleep"


def step(msg: str) -> None:
    print(f"\n▶ {msg}", flush=True)


def docker_env(data_dir: Path) -> dict[str, str]:
    env = dict(os.environ)
    status = ctl_json(data_dir, "status", "--json")
    endpoint = status.get("context", {}).get("endpoint") or ""
    if endpoint.startswith("unix://"):
        env["DOCKER_HOST"] = endpoint
    return env


def ensure_daemon_and_proxy(data_dir: Path):
    proc = None
    try:
        ctl(data_dir, "ping", timeout=10)
        return proc
    except Failure:
        if not DAEMON.exists():
            raise Failure("找不到 bin/orbsleepd，请先 make build-core")
        data_dir.mkdir(parents=True, exist_ok=True)
        proc = subprocess.Popen([str(DAEMON), "--data-dir", str(data_dir)],
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        wait_for("daemon ping", lambda: ctl(data_dir, "ping", timeout=5) or True, 20)
    status = ctl_json(data_dir, "status", "--json")
    proxy = status.get("proxy", {})
    if not proxy.get("installed") or not proxy.get("adminReachable"):
        ctl(data_dir, "proxy-setup", timeout=900)
        status = ctl_json(data_dir, "status", "--json")
        proxy = status.get("proxy", {})
    if not proxy.get("adminReachable"):
        raise Failure("代理管理接口不可达")
    return proc


def find_service(data_dir: Path, container_name: str):
    return next((s for s in service_items(data_dir) if s["containerName"] == container_name), None)


def attach_web(data_dir: Path, docker: str, env: dict[str, str], compose: Path, project: str):
    scan = ctl_json(data_dir, "scan", "--json", timeout=120)
    target = None
    for c in scan.get("containers", []):
        if c.get("composeProject") == project and c.get("composeService") == "web":
            target = c
            break
    if not target:
        raise Failure("scan did not find test web container")
    plan = ctl_json(data_dir, "plan", "--json", target["id"], timeout=120)
    if plan.get("status") != "ready":
        raise Failure("plan blocked: " + json.dumps(plan.get("blockers"), ensure_ascii=False))
    out = ctl(data_dir, "apply", plan["id"], timeout=900)
    if "succeeded" not in out:
        raise Failure("attach failed:\n" + out)
    svc = find_service(data_dir, target["name"])
    if not svc:
        raise Failure("attach did not create managed record")
    return svc, target["name"]


def assert_contains(name: str, text: str, needles: list[str]):
    missing = [n for n in needles if n not in text]
    if missing:
        raise Failure(f"{name}: missing {missing}\n{text}")


def restore(data_dir: Path, svc: dict):
    out = ctl(data_dir, "restore", svc["id"], timeout=900)
    if "succeeded" not in out:
        raise Failure("restore failed:\n" + out)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--data-dir", default=str(DEFAULT_DATA_DIR))
    ap.add_argument("--keep", action="store_true")
    args = ap.parse_args()
    data_dir = Path(args.data_dir)
    docker = find_docker()
    daemon_proc = None
    project = f"orbsleep-rel-{int(time.time())}"
    workdir = Path(tempfile.mkdtemp(prefix="orbsleep-rel-"))
    compose = workdir / "docker-compose.yml"
    original = ""
    svc = None
    failures: list[str] = []
    try:
        daemon_proc = ensure_daemon_and_proxy(data_dir)
        env = docker_env(data_dir)
        step("OrbStack / proxy ready")

        tcp_port, udp_port = free_port(), free_port()
        original = f'''services:
  web:
    image: nginx:alpine
    ports:
      - "127.0.0.1:{tcp_port}:80"
      - "127.0.0.1:{udp_port}:5353/udp"
  api:
    image: alpine
    command: sleep 3600
    environment:
      KEEP: original
'''
        compose.write_text(original, encoding="utf-8")
        run([docker, "compose", "-p", project, "-f", str(compose), "up", "-d"], env=env, timeout=300)
        wait_http(tcp_port, 60, "test web direct access")

        print("\n▶ 1. UDP preservation + safe merge after external edit")
        svc, cname = attach_web(data_dir, docker, env, compose, project)
        body = compose.read_text(encoding="utf-8")
        assert_contains("attach", body, [f"127.0.0.1:{udp_port}:5353/udp", "api:", "KEEP: original"])
        if f"127.0.0.1:{tcp_port}:80" in body:
            raise Failure("TCP mapping was not removed")
        inspect = run([docker, "inspect", "--format", '{{json .NetworkSettings.Ports}}', cname], env=env).stdout
        if "5353/udp" not in inspect or str(udp_port) not in inspect:
            raise Failure("container no longer publishes UDP: " + inspect)
        wait_http(tcp_port, 60, "proxied access after attach")
        # External edit to another service must survive detach.
        body = body.replace("KEEP: original", "KEEP: edited")
        compose.write_text(body, encoding="utf-8")
        restore(data_dir, svc)
        body = compose.read_text(encoding="utf-8")
        assert_contains("restore", body, [f"127.0.0.1:{tcp_port}:80", f"127.0.0.1:{udp_port}:5353/udp", "KEEP: edited"])
        if find_service(data_dir, cname):
            raise Failure("service record survived restore")
        wait_http(tcp_port, 60, "direct access after restore")
        svc = None
        print("  PASS: UDP kept, external edit kept, TCP restored")

        print("\n▶ 2. Deleted Compose rebuild")
        svc, cname = attach_web(data_dir, docker, env, compose, project)
        compose.unlink()
        restore(data_dir, svc)
        body = compose.read_text(encoding="utf-8")
        assert_contains("rebuild", body, [f"127.0.0.1:{tcp_port}:80", f"127.0.0.1:{udp_port}:5353/udp", "KEEP: edited"])
        wait_http(tcp_port, 60, "direct access after rebuild")
        if find_service(data_dir, cname):
            raise Failure("service record survived rebuild restore")
        svc = None
        print("  PASS: deleted Compose rebuilt from recovery material")

        print("\n▶ 3. Unreadable Compose refusal")
        svc, cname = attach_web(data_dir, docker, env, compose, project)
        os.chmod(compose, 0)
        try:
            try:
                ctl(data_dir, "restore", svc["id"], timeout=900)
                raise Failure("restore unexpectedly succeeded with mode 000")
            except Failure:
                pass
            if not find_service(data_dir, cname):
                raise Failure("failed restore removed the managed record")
            if not compose.exists():
                raise Failure("failed restore replaced the unreadable file")
        finally:
            os.chmod(compose, 0o644)
        restore(data_dir, svc)
        svc = None
        print("  PASS: unreadable file was preserved and managed state retained")

        print("\n▶ 4. Container deleted then restore")
        svc, cname = attach_web(data_dir, docker, env, compose, project)
        run([docker, "rm", "-f", cname], env=env, timeout=120)
        out = ctl(data_dir, "restore", svc["id"], timeout=900)
        if "succeeded" not in out:
            raise Failure("restore after container deletion failed:\n" + out)
        body = compose.read_text(encoding="utf-8")
        assert_contains("container-recreate", body, [f"127.0.0.1:{tcp_port}:80", f"127.0.0.1:{udp_port}:5353/udp"])
        wait_http(tcp_port, 60, "direct access after container recreation")
        svc = None
        print("  PASS: missing container was recreated and ports restored")

        print("\n▶ 5. Daemon SIGKILL recovery")
        svc, cname = attach_web(data_dir, docker, env, compose, project)
        pid_out = run(["pgrep", "-f", f"orbsleepd --data-dir {data_dir}"], check=False).stdout.strip()
        if not pid_out:
            raise Failure("could not find daemon pid")
        pid = int(pid_out.splitlines()[0])
        os.kill(pid, 9)
        recovered = False
        for _ in range(40):
            try:
                ctl(data_dir, "ping", timeout=5)
                recovered = True
                break
            except Exception:  # noqa: BLE001
                time.sleep(0.5)
        if not recovered:
            # LaunchAgent may not be installed; start the daemon manually.
            subprocess.Popen([str(DAEMON), "--data-dir", str(data_dir)],
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            wait_for("daemon restart", lambda: ctl(data_dir, "ping", timeout=5) or True, 30)
        wait_http(tcp_port, 90, "access after daemon SIGKILL")
        if not find_service(data_dir, cname):
            raise Failure("managed record lost after daemon SIGKILL")
        restore(data_dir, svc)
        svc = None
        print("  PASS: daemon recovered the managed service after SIGKILL")
    except (Failure, Exception) as exc:  # noqa: BLE001
        failures.append(str(exc))
        print("\n✘ FAIL:", exc, file=sys.stderr)
        try:
            if svc:
                print("cleaning managed test service...", file=sys.stderr)
                ctl(data_dir, "restore", svc["id"], timeout=900)
        except Exception as cleanup_exc:  # noqa: BLE001
            print("service cleanup failed:", cleanup_exc, file=sys.stderr)
    finally:
        if not args.keep:
            try:
                run([docker, "compose", "-p", project, "-f", str(compose), "down", "--remove-orphans"],
                    env=env, timeout=180, check=False)
            except Exception:
                pass
            shutil.rmtree(workdir, ignore_errors=True)
        if daemon_proc:
            daemon_proc.terminate()
            try:
                daemon_proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                daemon_proc.kill()
    if failures:
        return 1
    print("\n✅ reliability E2E passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
