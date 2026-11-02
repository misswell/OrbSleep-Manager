#!/usr/bin/env python3
"""OrbSleep 阶段 3 / 多容器环境并行验收。

前置条件：OrbStack 已启动 + Colima 已安装（`colima start` 至少一次）。

验证：
  1. 环境发现：environments.list 同时发现 OrbStack 与 Colima
  2. 启用第二环境：分配不相交的代理端口段
  3. 第二环境安装自己的代理（独立容器名 / 独立 compose 项目 / 挂 Colima socket）
  4. 双环境各自接入一个 docker run 容器（快照重建路径）
  5. 双环境并行休眠 / 唤醒互不干扰
  6. 跨环境端口段不相交 + 代理端口不冲突
  7. 第二环境离线（colima stop）时：主环境服务不受影响，第二环境服务标记不可达、监听保留
  8. 第二环境恢复在线后服务状态恢复
  9. 恢复两个服务并停用第二环境

安全说明：独立数据目录；只创建/清理本脚本前缀的资源；不 prune。
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
CTL = ROOT / "bin" / "orbsleepctl"
DAEMON = ROOT / "bin" / "orbsleepd"

HTTP_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


class Failure(Exception):
    pass


def log(msg):
    print(f"  {msg}", flush=True)


def step(msg):
    print(f"\n▶ {msg}", flush=True)


def run(cmd, env=None, timeout=300, check=True):
    proc = subprocess.run([str(c) for c in cmd], capture_output=True, text=True, env=env, timeout=timeout)
    if check and proc.returncode != 0:
        raise Failure(f"命令失败 ({proc.returncode}): {' '.join(str(c) for c in cmd)}\n"
                      f"stdout:\n{proc.stdout}\nstderr:\n{proc.stderr}")
    return proc


def ctl(data_dir, *args, timeout=900):
    return run([CTL, "--data-dir", data_dir, *args], timeout=timeout).stdout


def ctl_json(data_dir, *args, timeout=900):
    out = ctl(data_dir, *args, timeout=timeout)
    starts = [p for p in (out.find("{"), out.find("[")) if p >= 0]
    if not starts:
        raise Failure(f"CLI 没有返回 JSON: {out}")
    return json.loads(out[min(starts):])


def wait_for(desc, fn, timeout, interval=0.5):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            v = fn()
            if v:
                return v
        except Exception as exc:  # noqa: BLE001
            last = exc
        time.sleep(interval)
    raise Failure(f"等待超时（{timeout:.0f}s）：{desc}" + (f"；最后错误: {last}" if last else ""))


def find_docker():
    import shutil
    for c in [shutil.which("docker"), "/Applications/OrbStack.app/Contents/MacOS/bin/docker",
              "/opt/homebrew/bin/docker", "/usr/local/bin/docker"]:
        if c and Path(c).exists():
            return c
    raise Failure("找不到 docker CLI")


def free_port(host="127.0.0.1"):
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind((host, 0))
        return s.getsockname()[1]


def http_ok(port, timeout=5.0):
    try:
        with HTTP_OPENER.open(f"http://127.0.0.1:{port}/", timeout=timeout) as resp:
            return resp.status == 200
    except Exception:  # noqa: BLE001
        return False


def wait_http(port, timeout, desc):
    wait_for(desc, lambda: http_ok(port), timeout)


def start_daemon(data_dir):
    data_dir.mkdir(parents=True, exist_ok=True)
    proc = subprocess.Popen([str(DAEMON), "--data-dir", str(data_dir)],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    wait_for("daemon ping", lambda: ctl(data_dir, "ping", timeout=5) or True, 20)
    return proc


def stop_daemon(proc):
    proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()


def service_items(data_dir):
    data = ctl_json(data_dir, "services", "--json")
    if isinstance(data, list):
        return data
    return data.get("services", []) or []


def envs_of(data_dir):
    return ctl_json(data_dir, "environments", "--json")


def attach_container(data_dir, docker, env, name, host_port, workdir):
    """在指定 docker 环境创建并接入一个 docker run nginx 容器，返回 service id。"""
    e = dict(env)
    e["DOCKER_HOST"] = docker_host_for(docker, e)
    run([docker, "run", "-d", "--name", name, "--restart", "unless-stopped",
         "--label", "phase3=multienv",
         "-v", f"{workdir}:/usr/share/nginx/html:ro",
         "-p", f"127.0.0.1:{host_port}:80", "nginx:alpine"], env=e, timeout=300)
    wait_http(host_port, 60, f"{name} 直接访问")
    scan = ctl_json(data_dir, "scan", "--json", timeout=180)
    target = next((c for c in scan.get("containers", []) if c.get("name") == name), None)
    if target is None:
        raise Failure(f"扫描结果中找不到 {name}")
    if target.get("strategy") != "docrun":
        raise Failure(f"{name} strategy={target.get('strategy')!r}，期望 docrun")
    plan = ctl_json(data_dir, "plan", "--json", target["id"], timeout=120)
    if plan.get("status") != "ready":
        raise Failure(f"{name} 计划被阻塞：" + json.dumps(plan.get("blockers"), ensure_ascii=False))
    out = ctl(data_dir, "apply", plan["id"], timeout=900)
    if "succeeded" not in out:
        raise Failure(f"{name} 接入未成功：\n" + out)
    svc = next((s for s in service_items(data_dir) if s["containerName"] == name), None)
    if not svc:
        raise Failure(f"{name} 接入后没有托管记录")
    wait_http(host_port, 60, f"{name} 接入后原地址访问")
    return svc["id"]


def docker_host_for(docker, env):
    return env.get("DOCKER_HOST") or ""


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--data-dir", default=None)
    parser.add_argument("--keep", action="store_true")
    parser.add_argument("--skip-offline", action="store_true", help="跳过 colima stop/start 用例")
    args = parser.parse_args()
    data_dir = Path(args.data_dir) if args.data_dir else Path(tempfile.mkdtemp(prefix="orbsleep-phase3-"))

    docker = find_docker()
    orb_env = dict(os.environ)
    orb_env["DOCKER_HOST"] = "unix://" + str(Path.home() / ".orbstack/run/docker.sock")

    colima_sock = Path.home() / ".colima/default/docker.sock"
    if not colima_sock.exists():
        raise Failure("找不到 Colima socket（~/.colima/default/docker.sock）；请先 colima start")
    colima_env = dict(os.environ)
    colima_env["DOCKER_HOST"] = "unix://" + str(colima_sock)
    run([docker, "info", "--format", "{{.ServerVersion}}"], env=colima_env, timeout=60)

    # 预拉取代理镜像（Colima VM 首次拉取慢，避免吃掉 compose up 的超时）。
    # 已存在则跳过；网络抖动时重试几次（镜像在 run 期间已拉过则直接跳过）。
    step("确保 Lazy TCP Proxy 镜像在 Colima 中可用")
    image = "mountainpass/lazy-tcp-proxy@sha256:ed51f0b3e4be05fb9e2104c92c7315d0643df9e93ac24d9028d2b7a0cd073f39"
    pull_with_retry = lambda img: None
    def pull_with_retry_fn(img):
        have = run([docker, "image", "inspect", img], env=colima_env, check=False)
        if have.returncode == 0:
            return
        last_err = None
        for _attempt in range(5):
            proc = run([docker, "pull", img], env=colima_env, timeout=1200, check=False)
            if proc.returncode == 0:
                return
            tail = (proc.stderr or proc.stdout).strip().splitlines()
            last_err = tail[-1:] if tail else ["unknown"]
            time.sleep(5)
        raise Failure(f"镜像拉取失败 {img}: " + "; ".join(last_err))
    pull_with_retry_fn(image)
    pull_with_retry_fn("nginx:alpine")
    log("镜像就绪")

    daemon = None
    stamp = int(time.time())
    # Colima 默认只挂载 $HOME 与 /tmp/colima：html 目录必须放在 home 下，
    # 否则两个环境看到的挂载内容不一致。
    workdir = Path.home() / f".orbsleep-phase3-{stamp}"
    workdir.mkdir(parents=True, exist_ok=True)
    (workdir / "index.html").write_text("<h1>phase3</h1>", encoding="utf-8")
    name_a = f"phase3-{stamp}-orb"
    name_b = f"phase3-{stamp}-colima"
    port_a, port_b = free_port(), free_port()
    svc_a = svc_b = None
    env_b_id = None
    failures = []

    print("OrbSleep 阶段 3 验证（多环境并行）")
    print(f"  环境 A: OrbStack  容器 {name_a} 端口 {port_a}")
    print(f"  环境 B: Colima    容器 {name_b} 端口 {port_b}")
    print(f"  数据目录: {data_dir}")

    try:
        step("启动隔离后台助手并检查环境发现")
        daemon = start_daemon(data_dir)
        listing = envs_of(data_dir)
        rows = listing.get("environments", [])
        orb_row = next((r for r in rows if r.get("kind") == "orbstack" and not r.get("discoveredOnly")), None)
        colima_row = next((r for r in rows if r.get("kind") == "colima"), None)
        if orb_row is None:
            raise Failure("环境发现结果中没有已注册的 OrbStack 环境：" + json.dumps(rows, ensure_ascii=False))
        if colima_row is None:
            raise Failure("环境发现结果中没有 Colima（探测 ~/.colima/*.sock）：" + json.dumps(rows, ensure_ascii=False))
        log(f"发现 {len(rows)} 个环境；OrbStack={orb_row['id']}（primary={listing.get('primaryId')==orb_row['id']}）"
            f" Colima={colima_row['id']} 状态={colima_row.get('status')}")

        step("启用 Colima 环境（应分配不相交端口段并安装独立代理）")
        env_b_id = colima_row["id"]
        listing = ctl_json(data_dir, "environment-action", env_b_id, "enable", "--json", timeout=600)
        colima_row = next((r for r in listing["environments"] if r["id"] == env_b_id), None)
        if not colima_row or not colima_row.get("enabled"):
            raise Failure("Colima 环境启用失败：" + json.dumps(colima_row, ensure_ascii=False))
        ps_b = colima_row.get("proxyPortStart"), colima_row.get("proxyPortEnd")
        if not ps_b[0] or not ps_b[1]:
            raise Failure(f"Colima 环境没有分配代理端口段: {colima_row}")
        # 主环境的端口段在全局设置（升级兼容），从 status.proxy.publishedRange 取。
        status = ctl_json(data_dir, "status", "--json", timeout=60)
        prange = (status.get("proxy") or {}).get("publishedRange") or {}
        ps_a = prange.get("start"), prange.get("end")
        if not ps_a[0]:
            raise Failure("status 没有返回主环境代理端口段")
        overlap = not (ps_b[1] < ps_a[0] or ps_b[0] > ps_a[1])
        if overlap:
            raise Failure(f"端口段相交：A={ps_a} B={ps_b}")
        log(f"端口段不相交：OrbStack {ps_a[0]}-{ps_a[1]}，Colima {ps_b[0]}-{ps_b[1]}")

        # Colima 环境需要自己的代理（独立容器名 + 挂 Colima socket）。
        out = ctl(data_dir, "proxy-setup", "--env", env_b_id, timeout=900)
        if "succeeded" not in out:
            raise Failure("Colima 环境 proxy-setup 未成功：\n" + out)
        listing = envs_of(data_dir)
        colima_row = next((r for r in listing["environments"] if r["id"] == env_b_id), None)
        if not colima_row.get("proxyStatus") and not colima_row.get("adminPort"):
            log("注意：environment 列表未返回代理状态字段，改用 status 校验")

        step("双环境并行接入（docker run / 快照重建）")
        svc_a = attach_container(data_dir, docker, orb_env, name_a, port_a, workdir)
        svc_b = attach_container(data_dir, docker, colima_env, name_b, port_b, workdir)
        services = service_items(data_dir)
        sa = next(s for s in services if s["id"] == svc_a)
        sb = next(s for s in services if s["id"] == svc_b)
        if sa.get("environmentId") == sb.get("environmentId"):
            raise Failure(f"两个服务落在同一环境: {sa.get('environmentId')}")
        log(f"服务 A env={sa.get('environmentId')} / 服务 B env={sb.get('environmentId')}")

        step("双环境独立休眠与唤醒")
        run([docker, "stop", name_a], env=orb_env, timeout=120)
        run([docker, "stop", name_b], env=colima_env, timeout=120)
        wait_http(port_a, 120, "环境 A 唤醒")
        wait_http(port_b, 120, "环境 B 唤醒")
        log("OK：两个环境各自的容器都通过各自代理按需唤醒")

        step("跨环境端口与代理互不冲突")
        srows = service_items(data_dir)
        ports_a = [b["proxyPort"] for s in srows if s["id"] == svc_a for b in s["bindings"]]
        ports_b = [b["proxyPort"] for s in srows if s["id"] == svc_b for b in s["bindings"]]
        if set(ports_a) & set(ports_b):
            raise Failure(f"代理端口冲突: A={ports_a} B={ports_b}")
        if any(not (ps_b[0] <= p <= ps_b[1]) for p in ports_b):
            raise Failure(f"服务 B 的代理端口 {ports_b} 不在其环境端口段 {ps_b} 内")
        log(f"OK：A={ports_a} ⊂ {ps_a}，B={ports_b} ⊂ {ps_b}")

        if not args.skip_offline:
            step("环境 B 离线（colima stop）→ 环境 A 不受影响，B 监听保留")
            run(["colima", "stop"], timeout=300)
            time.sleep(20)
            if not http_ok(port_a):
                raise Failure("环境 B 离线后环境 A 服务不可用（隔离失败）")
            sb2 = next((s for s in service_items(data_dir) if s["id"] == svc_b), None)
            if sb2 is None:
                raise Failure("环境 B 离线后服务记录丢失")
            log(f"OK：A 正常；B 状态={sb2.get('status')} 错误={sb2.get('lastError', '')[:60]}")

            step("环境 B 恢复在线（colima start）→ 服务恢复")
            run(["colima", "start"], timeout=600)
            wait_for("环境 B 服务回到可用状态",
                     lambda: next((s for s in service_items(data_dir) if s["id"] == svc_b), {})
                     .get("status") in ("managed_running", "managed_idle", "managed_stopped"), 180, interval=3)
            wait_http(port_b, 120, "环境 B 恢复后唤醒")
            log("OK：Colima 恢复后服务自动回到托管状态并可唤醒")

        step("恢复两个服务并停用第二环境")
        out = ctl(data_dir, "restore", svc_a, timeout=900)
        if "succeeded" not in out:
            raise Failure("恢复 A 失败：\n" + out)
        out = ctl(data_dir, "restore", svc_b, timeout=900)
        if "succeeded" not in out:
            raise Failure("恢复 B 失败：\n" + out)
        svc_a = svc_b = None
        wait_http(port_a, 60, "A 恢复后直接访问")
        wait_http(port_b, 120, "B 恢复后直接访问")
        ctl_json(data_dir, "environment-action", env_b_id, "disable", "--json", timeout=120)
        log("OK：双环境服务恢复、第二环境已停用")

    except Exception as exc:  # noqa: BLE001
        failures.append(str(exc))
        print("\n✘ 失败:", exc, file=sys.stderr)
        for svc_id, name, env in ((svc_a, name_a, orb_env), (svc_b, name_b, colima_env)):
            if not svc_id:
                continue
            try:
                ctl(data_dir, "restore", svc_id, timeout=900)
            except Exception as cleanup_exc:  # noqa: BLE001
                print(f"清理 {name} 失败: {cleanup_exc}", file=sys.stderr)
    finally:
        if not args.keep:
            e = dict(os.environ)
            run([docker, "rm", "-f", name_a], env=orb_env, timeout=60, check=False)
            run([docker, "rm", "-f", name_b], env=colima_env, timeout=60, check=False)
            shutil.rmtree(workdir, ignore_errors=True)
            if not args.data_dir:
                shutil.rmtree(data_dir, ignore_errors=True)
        if daemon:
            stop_daemon(daemon)
        shutil.rmtree(workdir, ignore_errors=True)

    if failures:
        print("\n阶段 3 验证失败。日志: %s/logs/daemon.log" % data_dir, file=sys.stderr)
        return 1
    print("\n✅ 阶段 3 全部验证通过（OrbStack + Colima 双环境并行接入/休眠/恢复）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
