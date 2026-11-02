#!/usr/bin/env python3
"""OrbSleep 阶段 2 / 非 Compose 容器（docker run）端到端验收。

在真实 OrbStack 环境里验证 docrun 策略的完整生命周期：
  docker run 创建富配置容器 -> 扫描（strategy=docrun）-> 计划 -> 接入
  -> 配置无损校验（环境变量/标签/挂载/匿名卷数据/网络/资源限制/健康检查）
  -> 原地址可用 -> 停止后唤醒 -> 空闲自动停止 -> 故障注入回滚（重命名后崩溃/
  重建后崩溃） -> 恢复原始容器（含匿名卷数据） -> 记录清理

安全说明：
  * 使用独立数据目录（临时目录），不触碰正式 OrbSleep 数据。
  * 只创建并清理独立的前缀容器/卷/网络，不执行 prune，不删除其他资源。
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


def log(msg: str) -> None:
    print(f"  {msg}", flush=True)


def step(msg: str) -> None:
    print(f"\n▶ {msg}", flush=True)


def run(cmd, env=None, timeout=300, check=True):
    proc = subprocess.run(cmd, capture_output=True, text=True, env=env, timeout=timeout)
    if check and proc.returncode != 0:
        raise Failure(
            f"命令失败 ({proc.returncode}): {' '.join(str(c) for c in cmd)}\n"
            f"stdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"
        )
    return proc


def ctl(data_dir: Path, *args, timeout=900):
    return run([str(CTL), "--data-dir", str(data_dir), *args], timeout=timeout).stdout


def ctl_json(data_dir: Path, *args, timeout=900):
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
            value = fn()
            if value:
                return value
        except Exception as exc:  # noqa: BLE001
            last = exc
        time.sleep(interval)
    raise Failure(f"等待超时（{timeout:.0f}s）：{desc}" + (f"；最后错误: {last}" if last else ""))


def find_docker() -> str:
    import shutil

    candidates = [shutil.which("docker"), "/Applications/OrbStack.app/Contents/MacOS/bin/docker"]
    for candidate in candidates:
        if candidate and Path(candidate).exists():
            return candidate
    raise Failure("找不到 docker CLI，请先安装并启动 OrbStack")


def free_port(host="127.0.0.1") -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind((host, 0))
        return sock.getsockname()[1]


def http_ok(port, timeout=5.0) -> bool:
    try:
        with HTTP_OPENER.open(f"http://127.0.0.1:{port}/", timeout=timeout) as resp:
            return resp.status == 200
    except Exception:  # noqa: BLE001
        return False


def wait_http(port, timeout, desc):
    wait_for(desc, lambda: http_ok(port), timeout)


def docker_inspect(docker, env, name, fmt) -> str:
    return run([docker, "inspect", "--format", fmt, name], env=env).stdout.strip()


def service_items(data_dir: Path) -> list[dict]:
    data = ctl_json(data_dir, "services", "--json")
    if isinstance(data, list):
        return data
    return data.get("services", []) or []


def start_daemon(data_dir: Path, failpoint: str | None = None) -> subprocess.Popen:
    data_dir.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ)
    if failpoint:
        env["ORBSLEEP_FAILPOINT"] = failpoint
    proc = subprocess.Popen(
        [str(DAEMON), "--data-dir", str(data_dir)],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=env,
    )
    wait_for("daemon ping", lambda: ctl(data_dir, "ping", timeout=5) or True, 20)
    return proc


def stop_daemon(proc: subprocess.Popen) -> None:
    proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()


def config_fingerprint(docker, env, name) -> dict:
    """提取重建等价性所需的全部关键字段。"""
    raw = run([docker, "inspect", name], env=env).stdout
    ins = json.loads(raw)[0]
    cfg = ins.get("Config", {})
    hc = ins.get("HostConfig", {})
    return {
        "image": cfg.get("Image"),
        "env": sorted(e for e in (cfg.get("Env") or []) if not e.startswith("PATH=")),
        "labels": cfg.get("Labels") or {},
        "entrypoint": cfg.get("Entrypoint") or [],
        "cmd": cfg.get("Cmd") or [],
        "workingDir": cfg.get("WorkingDir") or "",
        "portBindings": hc.get("PortBindings") or {},
        "restartPolicy": (hc.get("RestartPolicy") or {}).get("Name"),
        "memory": hc.get("Memory"),
        "binds": sorted(hc.get("Binds") or []),
        "mounts": sorted((m.get("Type"), m.get("Name") or m.get("Source"), m.get("Destination"))
                         for m in (ins.get("Mounts") or [])),
        "networks": sorted((ins.get("NetworkSettings") or {}).get("Networks", {}).keys()),
        "healthcheck": ((cfg.get("Healthcheck") or {}).get("Test") or []),
        "stopSignal": cfg.get("StopSignal") or "",
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--data-dir", default=None, help="默认使用临时目录（隔离）")
    parser.add_argument("--keep", action="store_true", help="保留测试容器与数据目录")
    parser.add_argument("--skip-crash", action="store_true", help="跳过故障注入用例")
    args = parser.parse_args()
    data_dir = Path(args.data_dir) if args.data_dir else Path(tempfile.mkdtemp(prefix="orbsleep-phase2-"))

    docker = find_docker()
    env = dict(os.environ)
    daemon: subprocess.Popen | None = None
    stamp = int(time.time())
    project = f"orbsleep-phase2-{stamp}"
    workdir = Path(tempfile.mkdtemp(prefix="orbsleep-phase2-"))
    html_dir = workdir / "html"
    html_dir.mkdir(parents=True)
    (html_dir / "index.html").write_text("<h1>phase2-docrun</h1>", encoding="utf-8")
    # 第二端口要有真实服务：nginx 额外监听 8080（校验多端口接管链路）。
    nginx_extra = workdir / "zz-phase2.conf"
    nginx_extra.write_text(
        "server {\n  listen 8080;\n  root /usr/share/nginx/html;\n}\n",
        encoding="utf-8",
    )
    name = f"{project}-web"
    net = f"{project}-net"
    named_vol = f"{project}-data"
    port_a, port_b = free_port(), free_port()
    container_id: str | None = None
    service_id: str | None = None
    failures: list[str] = []
    created_volume = False
    created_network = False

    print("OrbSleep 阶段 2 验证（docker run / 非 Compose 容器）")
    print(f"  测试容器: {name}")
    print(f"  测试端口: 127.0.0.1:{port_a}, 127.0.0.1:{port_b}")
    print(f"  数据目录: {data_dir}")

    try:
        step("检查 Docker / OrbStack")
        info = run([docker, "info", "--format", "{{.ServerVersion}}"], env=env)
        log(f"Docker {info.stdout.strip()}")

        step("启动隔离后台助手")
        daemon = start_daemon(data_dir)
        status = ctl_json(data_dir, "status", "--json")
        endpoint = (status.get("context") or {}).get("endpoint") or ""
        if endpoint.startswith("unix://"):
            env["DOCKER_HOST"] = endpoint
        envlist = json.loads(run([str(CTL), "--data-dir", str(data_dir), "environments", "--json"]).stdout)
        env_rows = envlist.get("environments", [])
        log(f"发现 {len(env_rows)} 个容器环境，主环境 primary={envlist.get('primaryId')}")

        step("确保 Lazy TCP Proxy 运行")
        proxy = (status.get("proxy") or {})
        if not proxy.get("installed") or not proxy.get("adminReachable"):
            out = ctl(data_dir, "proxy-setup", timeout=900)
            if "succeeded" not in out:
                raise Failure("proxy-setup 未成功：\n" + out)
        prange = (ctl_json(data_dir, "status", "--json").get("proxy") or {}).get("publishedRange") or {}
        log(f"代理发布范围 {prange.get('start')}-{prange.get('end')}")

        step("创建测试资源（网络 / 具名卷 / 富配置 docker run 容器）")
        run([docker, "network", "create", net], env=env, check=False)
        created_network = True
        run([docker, "volume", "create", named_vol], env=env)
        created_volume = True
        run([docker, "run", "-d", "--name", name,
             "--network", net,
             "--network-alias", "web-alias",
             "--restart", "unless-stopped",
             "--label", f"phase2={project}",
             "--env", f"PHASE2_STAMP={stamp}",
             "--env", "PHASE2_MODE=docrun",
             "--workdir", "/srv",
             "--memory", "256m",
             "--cpus", "0.5",
             "--stop-signal", "SIGQUIT",
             "--health-cmd", "wget -q -O /dev/null http://127.0.0.1:80/ || exit 1",
             "--health-interval", "5s",
             "-v", f"{html_dir}:/usr/share/nginx/html:ro",
             "-v", f"{nginx_extra}:/etc/nginx/conf.d/zz-phase2.conf:ro",
             "-v", f"{named_vol}:/data",
             "-v", "/var/cache/nginx",  # 匿名卷：重建必须保数据
             "-p", f"127.0.0.1:{port_a}:80",
             "-p", f"127.0.0.1:{port_b}:8080",
             "nginx:alpine"], env=env, timeout=300)
        container_id = run([docker, "inspect", "--format", "{{.Id}}", name], env=env).stdout.strip()
        wait_http(port_a, 60, "测试容器直接访问")
        wait_http(port_b, 30, "测试容器第二端口直接访问")
        # 在匿名卷与具名卷中写入标记文件，接入重建后必须仍然存在。
        run([docker, "exec", name, "sh", "-c",
             "echo anon-marker > /var/cache/nginx/marker.txt && echo named-marker > /data/marker.txt"],
            env=env)
        log(f"容器已启动 {container_id[:12]}，标记文件已写入")

        before = config_fingerprint(docker, env, name)

        step("扫描：非 Compose 容器必须可接入（strategy=docrun）")
        scan = ctl_json(data_dir, "scan", "--json", timeout=120)
        target = None
        for container in scan.get("containers", []):
            if container.get("name") == name:
                target = container
                break
        if target is None:
            raise Failure("扫描结果中找不到测试容器（no_compose 排除回归？）")
        if target.get("strategy") != "docrun":
            raise Failure(f"strategy = {target.get('strategy')!r}，期望 docrun")
        if target.get("source") != "docker-run":
            raise Failure(f"source = {target.get('source')!r}，期望 docker-run")
        verdict = target["classification"]["verdict"]
        log(f"容器 {name} strategy={target['strategy']} source={target['source']} 判断={verdict}")
        if verdict != "eligible":
            reasons = target["classification"].get("reasons", []) + target["classification"].get("warnings", [])
            raise Failure("测试容器不是 eligible：" + json.dumps(reasons, ensure_ascii=False))

        step("生成接入计划")
        plan = ctl_json(data_dir, "plan", "--json", target["id"], timeout=120)
        if plan.get("status") != "ready":
            raise Failure("计划被阻塞：" + json.dumps(plan.get("blockers"), ensure_ascii=False))
        item = plan["items"][0]
        if item.get("strategy") != "docrun":
            raise Failure(f"计划 strategy = {item.get('strategy')!r}，期望 docrun")
        host_ports = sorted(r["hostPort"] for r in item["removals"])
        if host_ports != sorted([port_a, port_b]):
            raise Failure(f"计划端口不匹配: {host_ports}")
        log("计划就绪（快照重建路径）")

        step("执行接入（快照 -> 停止 -> 重命名锚点 -> 重建 -> 网关 -> 连通性）")
        out = ctl(data_dir, "apply", plan["id"], timeout=900)
        if "succeeded" not in out:
            raise Failure("接入未成功：\n" + out)
        svc = next((s for s in service_items(data_dir) if s["containerName"] == name), None)
        if not svc:
            raise Failure("接入后没有托管服务记录")
        service_id = svc["id"]
        if svc.get("strategy") != "docrun":
            raise Failure(f"托管记录 strategy = {svc.get('strategy')!r}，期望 docrun")

        step("校验重建等价性（配置 + 数据 + 网络 + 端口）")
        new_id = docker_inspect(docker, env, name, "{{.Id}}")
        if new_id == container_id:
            raise Failure("容器 ID 未变化：没有真正重建")
        after = config_fingerprint(docker, env, name)
        # binds 允许"匿名卷显式化"新增项：-v /dest 产生的匿名卷会被按原卷名
        # 显式挂载（数据保留的关键）。数据等价性由 mounts 全等 + 卷内标记文件校验。
        import re as _re
        extra_binds = set(after["binds"]) - set(before["binds"])
        lost_binds = set(before["binds"]) - set(after["binds"])
        unexpected = [b for b in extra_binds if not _re.match(r"^[0-9a-fA-F]{64}:", b)]
        if lost_binds or unexpected:
            raise Failure(f"重建后 binds 不一致:\n  前: {before['binds']}\n  后: {after['binds']}")
        for key in ("image", "env", "cmd", "workingDir", "restartPolicy", "memory",
                    "mounts", "networks", "healthcheck", "stopSignal"):
            if before[key] != after[key]:
                raise Failure(f"重建后 {key} 不一致:\n  前: {before[key]}\n  后: {after[key]}")
        lost = set(before["labels"].items()) - set(after["labels"].items())
        if lost:
            raise Failure(f"重建后用户标签丢失: {lost}")
        if after["labels"].get("orbsleep.managed") != "true":
            raise Failure("重建后缺少 orbsleep.managed 标签")
        # 被接管的 TCP 端口必须已从容器移除；UDP/其他端口保留（本容器没有 UDP）。
        for host_port in (port_a, port_b):
            raw = docker_inspect(docker, env, name, "{{json .HostConfig.PortBindings}}")
            bindings = json.loads(raw)
            for _key, lst in bindings.items():
                for b in lst or []:
                    if b.get("HostPort") == str(host_port):
                        raise Failure(f"容器仍发布被接管端口 {host_port}: {bindings}")
        # 匿名卷与具名卷数据必须保留。
        anon = run([docker, "exec", name, "cat", "/var/cache/nginx/marker.txt"], env=env,
                   check=False).stdout.strip()
        named = run([docker, "exec", name, "cat", "/data/marker.txt"], env=env, check=False).stdout.strip()
        if anon != "anon-marker":
            raise Failure(f"匿名卷数据丢失: {anon!r}")
        if named != "named-marker":
            raise Failure(f"具名卷数据丢失: {named!r}")
        wait_http(port_a, 60, "接入后通过原地址访问")
        wait_http(port_b, 60, "接入后通过第二端口访问")
        log("OK：配置/数据/网络无损，两个原端口均通过网关链路可用")

        step("手动停止容器，验证按需唤醒")
        run([docker, "stop", name], env=env, timeout=120)
        wait_http(port_a, 120, "停止后自动唤醒")
        log("OK：请求自动唤醒容器")

        step("设置空闲超时 30 秒，验证自动停止")
        ctl(data_dir, "policy", service_id, "--idle", "30", timeout=60)

        def container_stopped() -> bool:
            return docker_inspect(docker, env, name, "{{.State.Running}}") == "false"

        wait_for("容器因空闲停止", container_stopped, 180, interval=3)
        log("OK：空闲后容器自动停止")

        if not args.skip_crash:
            step("故障注入 1：重命名锚点后进程崩溃 → 恢复流程必须还原原容器")
            # 先恢复服务（回到原始容器），再带 failpoint 重新接入。
            out = ctl(data_dir, "restore", service_id, timeout=900)
            if "succeeded" not in out:
                raise Failure("注入前恢复未成功：\n" + out)
            service_id = None
            stop_daemon(daemon)
            daemon = start_daemon(data_dir, failpoint="docrun_original_renamed")
            scan = ctl_json(data_dir, "scan", "--json", timeout=120)
            target = next((c for c in scan.get("containers", []) if c.get("name") == name), None)
            if target is None:
                raise Failure("故障注入扫描不到测试容器")
            plan = ctl_json(data_dir, "plan", "--json", target["id"], timeout=120)
            if plan.get("status") != "ready":
                raise Failure("故障注入计划被阻塞：" + json.dumps(plan.get("blockers"), ensure_ascii=False))
            # failpoint 触发时守护进程被 SIGKILL：CLI 非零退出是预期结果。
            proc = run([str(CTL), "--data-dir", str(data_dir), "apply", plan["id"]],
                       timeout=60, check=False)
            if "succeeded" in proc.stdout:
                raise Failure("failpoint 未生效（接入不应成功）")
            wait_for("守护进程退出", lambda: daemon.poll() is not None, 30)
            stop_daemon(daemon)
            daemon = start_daemon(data_dir)
            # 恢复流程必须把原容器还原：原名字、原端口发布、可访问。
            def original_back() -> bool:
                ins_raw = run([docker, "inspect", name], env=env, check=False)
                if ins_raw.returncode != 0:
                    return False
                ins = json.loads(ins_raw.stdout)[0]
                if ins["Config"]["Labels"].get("orbsleep.managed") == "true":
                    return False
                binds = ins["HostConfig"]["PortBindings"]
                ports = {b.get("HostPort") for lst in binds.values() for b in (lst or [])}
                return str(port_a) in ports and str(port_b) in ports
            wait_for("原容器被还原且端口重新发布", original_back, 240, interval=3)
            wait_http(port_a, 60, "回滚后原端口直接访问")
            # 事务必须以 rolled_back 终态收场。
            rec = ctl_json(data_dir, "recoveries", "--json", timeout=60)
            unresolved = [t for t in rec.get("transactions", []) if t.get("status") in ("running", "rollback_failed", "needs_repair")]
            if unresolved:
                raise Failure("故障注入后仍有未完成事务：" + json.dumps(unresolved, ensure_ascii=False))
            log("OK：重命名后崩溃 → 原容器自动还原，事务收口")

            step("故障注入 2：替换容器创建后崩溃 → 恢复流程撤销替换容器")
            stop_daemon(daemon)
            daemon = start_daemon(data_dir, failpoint="docrun_replacement_created")
            scan = ctl_json(data_dir, "scan", "--json", timeout=120)
            target = next((c for c in scan.get("containers", []) if c.get("name") == name), None)
            if target is None:
                raise Failure("第二次注入扫描不到测试容器")
            plan = ctl_json(data_dir, "plan", "--json", target["id"], timeout=120)
            if plan.get("status") != "ready":
                raise Failure("第二次注入计划被阻塞")
            proc = run([str(CTL), "--data-dir", str(data_dir), "apply", plan["id"]],
                       timeout=60, check=False)
            if "succeeded" in proc.stdout:
                raise Failure("failpoint 未生效")
            wait_for("守护进程退出", lambda: daemon.poll() is not None, 30)
            stop_daemon(daemon)
            daemon = start_daemon(data_dir)
            wait_for("原容器被还原且端口重新发布", original_back, 240, interval=3)
            wait_http(port_a, 60, "第二次回滚后原端口直接访问")
            anon = run([docker, "exec", name, "cat", "/var/cache/nginx/marker.txt"], env=env,
                       check=False).stdout.strip()
            if anon != "anon-marker":
                raise Failure(f"故障注入回滚后匿名卷数据丢失: {anon!r}")
            rec = ctl_json(data_dir, "recoveries", "--json", timeout=60)
            unresolved = [t for t in rec.get("transactions", []) if t.get("status") in ("running", "rollback_failed", "needs_repair")]
            if unresolved:
                raise Failure("第二次注入后仍有未完成事务：" + json.dumps(unresolved, ensure_ascii=False))
            log("OK：重建后崩溃 → 替换容器被撤销，数据无损，事务收口")

        step("恢复原始容器（快照重建路径）")
        # 确保当前是托管状态（若刚经历故障注入则已回滚，需要重新接入一次）。
        svc = next((s for s in service_items(data_dir) if s["containerName"] == name), None)
        if svc is None:
            scan = ctl_json(data_dir, "scan", "--json", timeout=120)
            target = next((c for c in scan.get("containers", []) if c.get("name") == name), None)
            if target is None:
                raise Failure("最终接入前扫描不到测试容器")
            plan = ctl_json(data_dir, "plan", "--json", target["id"], timeout=120)
            if plan.get("status") != "ready":
                raise Failure("最终接入计划被阻塞")
            out = ctl(data_dir, "apply", plan["id"], timeout=900)
            if "succeeded" not in out:
                raise Failure("最终接入未成功：\n" + out)
            svc = next((s for s in service_items(data_dir) if s["containerName"] == name), None)
            if svc is None:
                raise Failure("最终接入后没有托管记录")
            service_id = svc["id"]
        out = ctl(data_dir, "restore", service_id, timeout=900)
        if "succeeded" not in out:
            raise Failure("恢复未成功：\n" + out)
        service_id = None

        step("校验恢复后的原始容器")
        restored = config_fingerprint(docker, env, name)
        import re as _re2
        extra_restored = [b for b in set(restored["binds"]) - set(before["binds"])
                          if not _re2.match(r"^[0-9a-fA-F]{64}:", b)]
        if set(before["binds"]) - set(restored["binds"]) or extra_restored:
            raise Failure(f"恢复后 binds 不一致:\n  前: {before['binds']}\n  后: {restored['binds']}")
        for key in ("image", "env", "cmd", "workingDir", "restartPolicy", "memory",
                    "mounts", "networks", "healthcheck", "stopSignal"):
            if before[key] != restored[key]:
                raise Failure(f"恢复后 {key} 不一致:\n  前: {before[key]}\n  后: {restored[key]}")
        if restored["labels"].get("orbsleep.managed"):
            raise Failure("恢复后的容器不应再带托管标签")
        if restored["labels"].get("phase2") != project:
            raise Failure("恢复后用户标签丢失")
        wait_http(port_a, 120, "恢复后直接访问容器")
        wait_http(port_b, 120, "恢复后第二端口直接访问")
        if any(s["containerName"] == name for s in service_items(data_dir)):
            raise Failure("恢复后仍存在托管服务记录")
        anon = run([docker, "exec", name, "cat", "/var/cache/nginx/marker.txt"], env=env,
                   check=False).stdout.strip()
        if anon != "anon-marker":
            raise Failure(f"恢复后匿名卷数据丢失: {anon!r}")
        # 回滚锚点容器必须已被清理。
        leftovers = run([docker, "ps", "-a", "--format", "{{.Names}}", "--filter",
                         f"label=orbsleep.service-id"], env=env, check=False).stdout
        if leftovers.strip():
            raise Failure("存在残留的 OrbSleep 管理容器: " + leftovers)
        log("OK：原始容器逐字段还原，数据完整，托管记录清空")

    except Exception as exc:  # noqa: BLE001
        failures.append(str(exc))
        print("\n✘ 失败:", exc, file=sys.stderr)
        try:
            for svc in service_items(data_dir):
                if svc["containerName"] == name:
                    print("尝试恢复测试服务…", file=sys.stderr)
                    ctl(data_dir, "restore", svc["id"], timeout=900)
        except Exception as cleanup_exc:  # noqa: BLE001
            print("清理托管服务失败:", cleanup_exc, file=sys.stderr)
        try:
            logs = run([docker, "logs", "--tail", "80", "orbsleep-lazy-tcp-proxy"], check=False)
            if logs.stdout or logs.stderr:
                print("\n代理容器日志:\n" + logs.stdout + logs.stderr, file=sys.stderr)
        except Exception:  # noqa: BLE001
            pass
    finally:
        if not args.keep:
            try:
                if container_id:
                    run([docker, "rm", "-f", name], env=env, timeout=120, check=False)
                    run([docker, "rm", "-f", f"{name}.orbsleep-bak-{container_id[:12]}"], env=env,
                        timeout=60, check=False)
                    run([docker, "rm", "-f", f"{name}.orbsleep-old"], env=env, timeout=60, check=False)
            except Exception:  # noqa: BLE001
                pass
            if created_network:
                run([docker, "network", "rm", net], env=env, timeout=60, check=False)
            if created_volume and not failures:
                # 只有全部通过才删除具名卷；失败时保留现场。
                run([docker, "volume", "rm", named_vol], env=env, timeout=60, check=False)
            shutil.rmtree(workdir, ignore_errors=True)
            if not args.data_dir:
                shutil.rmtree(data_dir, ignore_errors=True)
        if daemon:
            stop_daemon(daemon)

    if failures:
        print("\n阶段 2 验证失败。日志: %s/logs/daemon.log" % data_dir, file=sys.stderr)
        return 1
    print("\n✅ 阶段 2 全部验证通过（docker run 容器接入/休眠/唤醒/恢复 + 故障注入）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
