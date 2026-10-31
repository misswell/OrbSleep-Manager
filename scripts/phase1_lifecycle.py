#!/usr/bin/env python3
"""生命周期故障注入验收（真实 OrbStack、隔离数据目录）。

针对 v0.2.1 复审的 P1-04/05/06，在真实环境验证：

  A. 停止保护：有托管服务时 system.shutdown 无 force 必须被拒绝（fail-closed），
     force 后退出；重启后网关监听在 API 之前恢复，原端口立刻可达。
  B. 备份一致性：备份 → Compose 被外部修改 → 恢复备份 → Compose 回到备份时
     的托管内容、漂移以 .pre-restore 证据保留、服务仍托管、原端口可达；
     随后 CLI restore 逐字节还原原始 Compose。
  C. 卸载 fail-closed：注入 rollback_failed 事务后 system.prepareShutdown
     必须 safe=false、system.shutdown 无 force 拒绝、backup.restore 拒绝。

安全说明：
  * 使用独立 --data-dir，绝不触碰真实数据目录与 LaunchAgent。
  * 只创建并清理一个独立测试 Compose 项目（Nginx + 127.0.0.1 随机端口）。
  * 复用已在运行的 Lazy TCP Proxy 容器；不会执行 docker system prune。
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import socket
import sqlite3
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


def run(cmd: list[str], env: dict[str, str] | None = None, timeout: int = 300,
        check: bool = True) -> subprocess.CompletedProcess:
    proc = subprocess.run(cmd, capture_output=True, text=True, env=env, timeout=timeout)
    if check and proc.returncode != 0:
        raise Failure(
            f"命令失败 ({proc.returncode}): {' '.join(cmd)}\n"
            f"stdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"
        )
    return proc


def ctl(data_dir: Path, *args: str, timeout: int = 900) -> str:
    proc = run([str(CTL), "--data-dir", str(data_dir), *args], timeout=timeout)
    return proc.stdout


def ctl_json(data_dir: Path, *args: str, timeout: int = 900) -> dict:
    out = ctl(data_dir, *args, timeout=timeout)
    starts = [p for p in (out.find("{"), out.find("[")) if p >= 0]
    if not starts:
        raise Failure(f"CLI 没有返回 JSON: {out}")
    return json.loads(out[min(starts):])


def rpc(sock_path: Path, method: str, params: dict | None = None, timeout: float = 60) -> dict:
    """一次 NDJSON RPC；返回完整响应（调用方自行断言 ok/error）。"""
    req = {"v": 1, "id": "phase1", "method": method, "params": params or {}}
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as sock:
        sock.settimeout(timeout)
        sock.connect(str(sock_path))
        sock.sendall((json.dumps(req) + "\n").encode())
        buf = b""
        while not buf.endswith(b"\n"):
            chunk = sock.recv(65536)
            if not chunk:
                break
            buf += chunk
    if not buf.strip():
        raise Failure(f"{method}: 空响应")
    return json.loads(buf.splitlines()[0])


def expect_ok(resp: dict, method: str) -> dict:
    if not resp.get("ok"):
        raise Failure(f"{method} 应成功: {resp.get('error')}")
    return resp.get("result") or {}


def expect_error(resp: dict, method: str, needle: str) -> str:
    if resp.get("ok"):
        raise Failure(f"{method} 应被拒绝，却成功了: {resp.get('result')}")
    message = (resp.get("error") or {}).get("message", "")
    if needle not in message:
        raise Failure(f"{method} 的拒绝原因不对：{message}")
    return message


def wait_for(desc: str, fn, timeout: float, interval: float = 0.5):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            value = fn()
            if value:
                return value
        except Exception as exc:  # noqa: BLE001 - retry loop
            last = exc
        time.sleep(interval)
    raise Failure(f"等待超时（{timeout:.0f}s）：{desc}" + (f"；最后错误: {last}" if last else ""))


def find_docker() -> str:
    candidates = [shutil.which("docker"), "/Applications/OrbStack.app/Contents/MacOS/bin/docker"]
    for candidate in candidates:
        if candidate and Path(candidate).exists():
            return candidate
    raise Failure("找不到 docker CLI，请先安装并启动 OrbStack")


def free_port(host: str = "127.0.0.1") -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind((host, 0))
        return sock.getsockname()[1]


def http_ok(port: int, timeout: float = 5.0) -> bool:
    try:
        with HTTP_OPENER.open(f"http://127.0.0.1:{port}/", timeout=timeout) as resp:
            return resp.status == 200
    except Exception:  # noqa: BLE001
        return False


def wait_http(port: int, timeout: float, desc: str) -> None:
    wait_for(desc, lambda: http_ok(port), timeout)


def service_items(data_dir: Path) -> list[dict]:
    data = ctl_json(data_dir, "services", "--json")
    if isinstance(data, list):
        return data
    return data.get("services", []) or []


def wait_daemon_exit(proc: subprocess.Popen, seconds: float, desc: str) -> None:
    deadline = time.time() + seconds
    while time.time() < deadline:
        if proc.poll() is not None:
            log(f"{desc}（退出码 {proc.returncode}）")
            return
        time.sleep(0.3)
    raise Failure(f"{desc}：后台进程在 {seconds:.0f}s 内未退出")


def start_daemon(data_dir: Path) -> subprocess.Popen:
    data_dir.mkdir(parents=True, exist_ok=True)
    proc = subprocess.Popen(
        [str(DAEMON), "--data-dir", str(data_dir)],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    wait_for("daemon ping", lambda: ctl(data_dir, "ping", timeout=5) or True, 20)
    return proc


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--data-dir", default=None, help="默认使用临时目录")
    parser.add_argument("--keep", action="store_true", help="结束后保留测试 Compose 项目")
    args = parser.parse_args()
    data_dir = Path(args.data_dir) if args.data_dir else Path(tempfile.mkdtemp(prefix="orbsleep-phase1-"))

    docker = find_docker()
    env = dict(os.environ)
    daemon: subprocess.Popen | None = None
    project = f"orbsleep-phase1-{int(time.time())}"
    workdir = Path(tempfile.mkdtemp(prefix="orbsleep-phase1-"))
    compose_file = workdir / "docker-compose.yml"
    port = free_port()
    container_name = f"{project}-web-1"
    original_content = ""
    failures: list[str] = []
    sock = data_dir / "daemon.sock"

    print("OrbSleep 生命周期故障注入验收")
    print(f"  测试项目: {project}")
    print(f"  测试端口: 127.0.0.1:{port}")
    print(f"  数据目录: {data_dir}")

    try:
        step("启动隔离后台助手（0.2.2）")
        version = run([str(CTL), "version"]).stdout.strip()
        log(f"CLI {version}")
        daemon = start_daemon(data_dir)

        status = ctl_json(data_dir, "status", "--json")
        endpoint = (status.get("context") or {}).get("endpoint") or ""
        if endpoint.startswith("unix://"):
            env["DOCKER_HOST"] = endpoint
        run([docker, "info", "--format", "{{.ServerVersion}}"], env=env)

        step("确保 Lazy TCP Proxy 可用（复用现有容器）")
        proxy = (status.get("proxy") or {})
        if not proxy.get("installed") or not proxy.get("adminReachable"):
            out = ctl(data_dir, "proxy-setup", timeout=900)
            if "succeeded" not in out:
                raise Failure("proxy-setup 未成功：\n" + out)
        prange = (ctl_json(data_dir, "status", "--json").get("proxy") or {}).get("publishedRange") or {}
        log(f"代理发布范围 {prange.get('start')}-{prange.get('end')}")

        step("创建并接入测试服务")
        compose_file.write_text(
            "services:\n"
            "  web:\n"
            "    image: nginx:alpine\n"
            "    ports:\n"
            f"      - \"127.0.0.1:{port}:80\"\n",
            encoding="utf-8",
        )
        original_content = compose_file.read_text(encoding="utf-8")
        run([docker, "compose", "-p", project, "-f", str(compose_file), "up", "-d"], env=env, timeout=300)
        wait_http(port, 60, "测试容器直接访问")
        scan = ctl_json(data_dir, "scan", "--json", timeout=120)
        target = next((c for c in scan.get("containers", []) if c.get("composeProject") == project), None)
        if target is None:
            raise Failure("扫描结果中找不到测试容器")
        plan = ctl_json(data_dir, "plan", "--json", target["id"], timeout=120)
        if plan.get("status") != "ready":
            raise Failure("计划被阻塞：" + json.dumps(plan.get("blockers"), ensure_ascii=False))
        out = ctl(data_dir, "apply", plan["id"], timeout=900)
        if "succeeded" not in out:
            raise Failure("接入未成功：\n" + out)
        svc = next((s for s in service_items(data_dir) if s["containerName"] == target["name"]), None)
        if not svc:
            raise Failure("接入后没有托管服务记录")
        service_id = svc["id"]
        container_name = target["name"]
        managed_content = compose_file.read_text(encoding="utf-8")
        log(f"服务已接入 {service_id}，原端口经网关可达")

        # ---------- 场景 A：停止保护 ----------
        step("A1. 有托管服务时，system.shutdown（无 force）必须被拒绝")
        resp = rpc(sock, "system.shutdown")
        expect_error(resp, "system.shutdown", "托管服务")
        if daemon.poll() is not None:
            raise Failure("被拒绝的停止请求竟然终止了后台进程")
        if not http_ok(port):
            raise Failure("拒绝停止后原端口应仍然可达")
        log(f"OK：拒绝原因「{(resp.get('error') or {}).get('message', '')[:40]}…」，进程与监听均正常")

        step("A2. force 停止 → 进程退出")
        expect_ok(rpc(sock, "system.shutdown", {"force": True}), "system.shutdown(force)")
        wait_daemon_exit(daemon, 30, "force 停止已退出")
        daemon = None

        step("A3. 重启后监听在 API 之前恢复，原端口立刻可达")
        daemon = start_daemon(data_dir)
        if not http_ok(port):
            wait_http(port, 15, "重启后原端口经网关可达")
        log("OK：重启后原端口立即恢复可达")

        # ---------- 场景 B：备份一致性 ----------
        step("B1. 创建备份（含 Compose 快照）")
        result = expect_ok(rpc(sock, "backup.create"), "backup.create")
        backup_id = (result.get("backup") or {}).get("id")
        if not backup_id:
            raise Failure(f"backup.create 未返回 id: {result}")
        log(f"备份 {backup_id}")

        step("B2. 外部修改 Compose（模拟用户漂移）")
        with open(compose_file, "a", encoding="utf-8") as fh:
            fh.write("    # user edit after backup\n")
        drifted = compose_file.read_text(encoding="utf-8")
        if drifted == original_content:
            raise Failure("漂移写入失败")

        step("B3. 恢复备份 → 数据库与文件一致")
        expect_ok(rpc(sock, "backup.restore", {"id": backup_id}), "backup.restore")
        wait_daemon_exit(daemon, 30, "恢复请求后后台已退出")
        daemon = start_daemon(data_dir)
        restored = compose_file.read_text(encoding="utf-8")
        if restored != managed_content:
            raise Failure(f"恢复后 Compose 应为备份时托管内容:\n got={restored!r}\nwant={managed_content!r}")
        pre = list(workdir.glob("docker-compose.yml.pre-restore-*"))
        if len(pre) != 1:
            raise Failure(f"漂移证据应保留一份，实际 {len(pre)}")
        if not service_items(data_dir):
            raise Failure("恢复备份后服务应仍处于托管状态（数据库与文件一致）")
        if not http_ok(port):
            wait_http(port, 30, "恢复备份后原端口经网关可达")
        log("OK：Compose 已还原、漂移证据保留、服务仍托管、原端口可达")

        step("B4. CLI restore 逐字节还原原始 Compose")
        out = ctl(data_dir, "restore", service_id, timeout=900)
        if "succeeded" not in out:
            raise Failure("恢复服务未成功：\n" + out)
        if compose_file.read_text(encoding="utf-8") != original_content:
            raise Failure("CLI restore 后 Compose 与原始内容不一致")
        wait_http(port, 60, "还原后直接访问容器")
        if any(s["containerName"] == container_name for s in service_items(data_dir)):
            raise Failure("还原后仍存在托管服务记录")
        log("OK：Compose 逐字节还原，原端口由容器直接发布")

        # ---------- 场景 C：卸载 fail-closed ----------
        step("C1. 注入 rollback_failed 事务（模拟接入中断且回滚失败、无服务记录）")
        now = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        db = data_dir / "state.sqlite"
        conn = sqlite3.connect(str(db))
        with conn:
            conn.execute(
                "INSERT INTO transactions (id, kind, operation_id, status, phase, started_at, updated_at, error, data) "
                "VALUES (?, 'attach', '', 'rollback_failed', 'rollback_failed', ?, ?, '注入的失败恢复', '{}')",
                (f"txn-inject-{int(time.time())}", now, now),
            )
        conn.close()
        log("已注入 rollback_failed 事务")

        step("C2. prepareShutdown 必须 safe=false，停止与恢复备份必须被拒绝")
        prep = rpc(sock, "system.prepareShutdown")
        prep_result = expect_ok(prep, "system.prepareShutdown")
        if prep_result.get("safe"):
            raise Failure(f"存在 rollback_failed 事务时 prepareShutdown 不应安全: {prep_result}")
        expect_error(rpc(sock, "system.shutdown"), "system.shutdown", "事务")
        expect_error(rpc(sock, "backup.restore", {"id": backup_id}), "backup.restore", "事务")
        log("OK：停止与恢复备份均 fail-closed")

        step("C3. 清除注入事务后恢复放行")
        conn = sqlite3.connect(str(db))
        with conn:
            conn.execute("DELETE FROM transactions WHERE id LIKE 'txn-inject-%'")
        conn.close()
        prep_result = expect_ok(rpc(sock, "system.prepareShutdown"), "system.prepareShutdown")
        if not prep_result.get("safe"):
            raise Failure(f"清洁状态应 safe: {prep_result}")
        log("OK：清洁状态 prepareShutdown.safe=true")

    except Exception as exc:  # noqa: BLE001
        failures.append(str(exc))
        print("\n✘ 失败:", exc, file=sys.stderr)
        try:
            for svc in service_items(data_dir):
                if svc["containerName"] == container_name:
                    print("尝试恢复测试服务…", file=sys.stderr)
                    ctl(data_dir, "restore", svc["id"], timeout=900)
        except Exception as cleanup_exc:  # noqa: BLE001
            print("清理托管服务失败:", cleanup_exc, file=sys.stderr)
    finally:
        if not args.keep:
            try:
                run([docker, "compose", "-p", project, "-f", str(compose_file), "down", "--remove-orphans"],
                    env=env, timeout=180, check=False)
            except Exception:  # noqa: BLE001
                pass
            shutil.rmtree(workdir, ignore_errors=True)
        if daemon and daemon.poll() is None:
            try:
                rpc(sock, "system.shutdown", {"force": True}, timeout=10)
                wait_daemon_exit(daemon, 20, "清理：后台已退出")
            except Exception:  # noqa: BLE001
                daemon.terminate()
                try:
                    daemon.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    daemon.kill()
        if not args.keep and not failures:
            shutil.rmtree(data_dir, ignore_errors=True)

    if failures:
        print("\n生命周期故障注入验收失败。", file=sys.stderr)
        return 1
    print("\n✅ 生命周期故障注入验收全部通过（A 停止保护 / B 备份一致性 / C 卸载 fail-closed）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
