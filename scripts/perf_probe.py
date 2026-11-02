#!/usr/bin/env python3
"""网关链路时延对比：直连容器 vs 接入后（网关 → 代理 → 容器）。

方法：隔离数据目录 + 独立 nginx 容器；接入前后各打 N 个 HTTP 请求，
报告 p50/p95/均值。输出用于 0.3.0 的性能对比说明。
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import socket
import statistics
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
CTL = ROOT / "bin" / "orbsleepctl"
DAEMON = ROOT / "bin" / "orbsleepd"
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def probe(port, n=200):
    lat = []
    for _ in range(n):
        t0 = time.perf_counter()
        try:
            with OPENER.open(f"http://127.0.0.1:{port}/", timeout=3) as r:
                r.read()
        except Exception:
            continue
        lat.append((time.perf_counter() - t0) * 1000)
    return lat


def stats(lat):
    if not lat:
        return {"n": 0}
    s = sorted(lat)
    return {
        "n": len(s),
        "p50_ms": round(statistics.median(s), 2),
        "p95_ms": round(s[int(len(s) * 0.95)], 2),
        "mean_ms": round(statistics.fmean(s), 2),
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--requests", type=int, default=200)
    args = parser.parse_args()

    docker = "/Applications/OrbStack.app/Contents/MacOS/bin/docker"
    if not Path(docker).exists():
        docker = "docker"
    data_dir = Path(tempfile.mkdtemp(prefix="orbsleep-perf-"))
    workdir = Path(tempfile.mkdtemp(prefix="orbsleep-perf-html-"))
    (workdir / "index.html").write_text("perf", encoding="utf-8")
    name = "orbsleep-perf-web"
    port = free_port()
    env = dict(os.environ)
    env["DOCKER_HOST"] = "unix://" + str(Path.home() / ".orbstack/run/docker.sock")
    daemon = None
    svc_id = None
    try:
        daemon = subprocess.Popen([str(DAEMON), "--data-dir", str(data_dir)],
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for _ in range(40):
            if subprocess.run([str(CTL), "--data-dir", str(data_dir), "ping"],
                              capture_output=True).returncode == 0:
                break
            time.sleep(0.5)
        out = subprocess.run([str(CTL), "--data-dir", str(data_dir), "proxy-setup"],
                             capture_output=True, text=True, timeout=900)
        if "succeeded" not in out.stdout:
            raise SystemExit("proxy-setup 失败:\n" + out.stdout + out.stderr)

        subprocess.run([docker, "rm", "-f", name], env=env, capture_output=True)
        subprocess.run([docker, "run", "-d", "--name", name, "-v", f"{workdir}:/usr/share/nginx/html:ro",
                        "-p", f"127.0.0.1:{port}:80", "nginx:alpine"], env=env, check=True,
                       capture_output=True, timeout=300)
        for _ in range(60):
            try:
                with OPENER.open(f"http://127.0.0.1:{port}/", timeout=2) as r:
                    r.read()
                break
            except Exception:
                time.sleep(0.5)

        direct = probe(port, args.requests)

        scan = json.loads(subprocess.run([str(CTL), "--data-dir", str(data_dir), "scan", "--json"],
                                         capture_output=True, text=True, timeout=120).stdout)
        target = next(c for c in scan["containers"] if c["name"] == name)
        plan = json.loads(subprocess.run([str(CTL), "--data-dir", str(data_dir), "plan", "--json", target["id"]],
                                         capture_output=True, text=True, timeout=120).stdout)
        if plan["status"] != "ready":
            raise SystemExit("计划被阻塞: " + json.dumps(plan["blockers"], ensure_ascii=False))
        out = subprocess.run([str(CTL), "--data-dir", str(data_dir), "apply", plan["id"]],
                             capture_output=True, text=True, timeout=900)
        if "succeeded" not in out.stdout:
            raise SystemExit("接入失败:\n" + out.stdout)
        services = json.loads(subprocess.run([str(CTL), "--data-dir", str(data_dir), "services", "--json"],
                                             capture_output=True, text=True).stdout)
        if isinstance(services, list):
            svc_id = services[0]["id"]
        else:
            svc_id = services["services"][0]["id"]
        for _ in range(60):
            try:
                with OPENER.open(f"http://127.0.0.1:{port}/", timeout=2) as r:
                    r.read()
                break
            except Exception:
                time.sleep(0.5)
        chained = probe(port, args.requests)

        result = {"direct": stats(direct), "gateway_chain": stats(chained),
                  "requests": args.requests}
        print(json.dumps(result, ensure_ascii=False, indent=2))
        return 0
    finally:
        if svc_id:
            subprocess.run([str(CTL), "--data-dir", str(data_dir), "restore", svc_id],
                           capture_output=True, timeout=900)
        subprocess.run([docker, "rm", "-f", name], env=env, capture_output=True)
        if daemon:
            daemon.terminate()
            try:
                daemon.wait(timeout=10)
            except subprocess.TimeoutExpired:
                daemon.kill()
        shutil.rmtree(data_dir, ignore_errors=True)
        shutil.rmtree(workdir, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
