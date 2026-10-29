#!/usr/bin/env python3
"""OrbSleep 阶段 0 / 验收端到端测试。

在真实 OrbStack 环境里验证：
  代理启动 -> 发布端口可从 macOS 访问 -> 扫描 -> 计划 -> 接入 ->
  原地址可用 -> 停止后唤醒 -> 空闲自动停止 -> 恢复原始端口

安全说明：
  * 使用真实的 OrbSleep 数据目录（与 GUI / LaunchAgent 相同），因此代理容器
    只有一份，不会被测试脚本重建或覆盖。
  * 只创建并清理一个独立的测试 Compose 项目（Nginx + 127.0.0.1 随机端口）。
  * 不会执行 docker system prune，不会删除任何卷。
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
DEFAULT_DATA_DIR = Path.home() / "Library" / "Application Support" / "OrbSleep"

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


def ctl(data_dir: Path, *args: str, timeout: int = 600) -> str:
    proc = run([str(CTL), "--data-dir", str(data_dir), *args], timeout=timeout)
    return proc.stdout


def ctl_json(data_dir: Path, *args: str, timeout: int = 600) -> dict:
    out = ctl(data_dir, *args, timeout=timeout)
    starts = [p for p in (out.find("{"), out.find("[")) if p >= 0]
    if not starts:
        raise Failure(f"CLI 没有返回 JSON: {out}")
    return json.loads(out[min(starts):])


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


def tcp_reachable(host: str, port: int, timeout: float = 3.0) -> bool:
    try:
        with socket.create_connection((host, port), timeout=timeout):
            return True
    except OSError:
        return False


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--data-dir", default=str(DEFAULT_DATA_DIR))
    parser.add_argument("--keep", action="store_true", help="结束后保留测试 Compose 项目")
    args = parser.parse_args()
    data_dir = Path(args.data_dir)

    docker = find_docker()
    env = dict(os.environ)
    daemon_proc: subprocess.Popen | None = None
    project = f"orbsleep-phase0-{int(time.time())}"
    workdir = Path(tempfile.mkdtemp(prefix="orbsleep-phase0-"))
    compose_file = workdir / "docker-compose.yml"
    port = free_port()
    container_name = f"{project}-web-1"
    service_id: str | None = None
    failures: list[str] = []
    started_daemon = False

    print("OrbSleep 阶段 0 验证")
    print(f"  测试项目: {project}")
    print(f"  测试端口: 127.0.0.1:{port}")
    print(f"  数据目录: {data_dir}")

    try:
        step("检查 Docker / OrbStack")
        info = run([docker, "info", "--format", "{{.ServerVersion}}"], env=env)
        log(f"Docker {info.stdout.strip()}")
        endpoint = None

        step("确保后台助手运行")
        try:
            ctl(data_dir, "ping", timeout=10)
            log("复用已在运行的 orbsleepd")
        except Failure:
            if not DAEMON.exists():
                raise Failure("找不到 bin/orbsleepd，请先 make build-core")
            data_dir.mkdir(parents=True, exist_ok=True)
            daemon_proc = subprocess.Popen(
                [str(DAEMON), "--data-dir", str(data_dir)],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            )
            started_daemon = True
            wait_for("daemon ping", lambda: ctl(data_dir, "ping", timeout=5) or True, 20)
            log(f"已启动临时守护进程 pid={daemon_proc.pid}")

        status = ctl_json(data_dir, "status", "--json")
        endpoint = status.get("context", {}).get("endpoint") or ""
        if endpoint.startswith("unix://"):
            env["DOCKER_HOST"] = endpoint
        log(f"context={status.get('context', {}).get('name')} endpoint={endpoint}")

        step("确保 Lazy TCP Proxy 运行")
        proxy = status.get("proxy", {})
        if not proxy.get("installed") or not proxy.get("adminReachable"):
            log("代理未就绪，执行 proxy-setup（首次会拉取镜像）")
            out = ctl(data_dir, "proxy-setup", timeout=900)
            if "succeeded" not in out:
                raise Failure("proxy-setup 未成功：\n" + out)
        status = ctl_json(data_dir, "status", "--json")
        proxy = status.get("proxy", {})
        if not proxy.get("adminReachable"):
            raise Failure(f"代理管理接口不可达: {proxy.get('error')}")
        prange = proxy.get("publishedRange", {})
        log(f"代理 IP={proxy.get('containerIP')} 发布范围={prange.get('start')}-{prange.get('end')}")

        step("验证 macOS 可访问代理发布端口（阶段 0 关键项）")
        if not tcp_reachable("127.0.0.1", int(prange["start"])):
            raise Failure(
                f"无法连接 127.0.0.1:{prange['start']}；代理端口范围没有正确发布，"
                "请检查 docker port orbsleep-lazy-tcp-proxy 与代理容器日志"
            )
        log("OK：代理监听端口可从 macOS 访问")

        step("创建测试 Compose 项目")
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
        log("Nginx 已启动并可通过原端口直接访问")

        step("扫描容器")
        scan = ctl_json(data_dir, "scan", "--json", timeout=120)
        target = None
        for container in scan.get("containers", []):
            if container.get("composeProject") == project:
                target = container
                break
        if target is None:
            raise Failure("扫描结果中找不到测试容器")
        log(f"容器 {target['name']} 判断={target['classification']['verdict']}")
        if target["classification"]["verdict"] != "eligible":
            reasons = target["classification"].get("reasons", []) + target["classification"].get("warnings", [])
            raise Failure("测试容器不是 eligible：" + json.dumps(reasons, ensure_ascii=False))
        container_name = target["name"]

        step("生成接入计划")
        plan = ctl_json(data_dir, "plan", "--json", target["id"], timeout=120)
        if plan.get("status") != "ready":
            raise Failure("计划被阻塞：" + json.dumps(plan.get("blockers"), ensure_ascii=False))
        item = plan["items"][0]
        if item["removals"][0]["hostPort"] != port:
            raise Failure(f"计划端口不匹配: {item['removals']}")
        log("计划就绪，diff：")
        for line in item.get("diff", "").splitlines():
            print("    " + line)

        step("执行接入（备份、改 Compose、重建、起网关、连通性测试）")
        out = ctl(data_dir, "apply", plan["id"], timeout=900)
        if "succeeded" not in out:
            raise Failure("接入未成功：\n" + out)
        svc = next((s for s in service_items(data_dir) if s["containerName"] == container_name), None)
        if not svc:
            raise Failure("接入后没有托管服务记录")
        service_id = svc["id"]

        step("验证 Compose 已移除端口，原地址仍可访问")
        content = compose_file.read_text(encoding="utf-8")
        if f"127.0.0.1:{port}:80" in content:
            raise Failure("Compose 文件中仍存在测试端口")
        if "ports:" not in content:
            log("Compose 中 ports 键已整体移除（列表中只有这一个端口）")
        wait_http(port, 60, "接入后通过原地址访问")
        log("OK：原地址 127.0.0.1:%d 仍返回 200" % port)

        step("手动停止容器，验证按需唤醒")
        run([docker, "stop", container_name], env=env, timeout=120)
        log("容器已停止，发起请求…")
        wait_http(port, 120, "停止后自动唤醒")
        log("OK：请求自动唤醒容器")

        step("设置空闲超时 30 秒，验证自动停止")
        ctl(data_dir, "policy", service_id, "--idle", "30", timeout=60)
        def container_stopped() -> bool:
            state = run([docker, "inspect", "--format", "{{.State.Running}}", container_name], env=env).stdout.strip()
            return state == "false"
        wait_for("容器因空闲停止", container_stopped, 180, interval=3)
        log("OK：空闲后容器自动停止")

        step("恢复原始端口映射")
        out = ctl(data_dir, "restore", service_id, timeout=900)
        if "succeeded" not in out:
            raise Failure("恢复未成功：\n" + out)
        service_id = None
        restored = compose_file.read_text(encoding="utf-8")
        if restored != original_content:
            raise Failure("恢复后的 Compose 文件与原始内容不一致")
        wait_http(port, 120, "恢复后直接访问容器")
        if any(s["containerName"] == container_name for s in service_items(data_dir)):
            raise Failure("恢复后仍存在托管服务记录")
        log("OK：Compose 已逐字节恢复，原端口重新由容器发布")

    except (Failure, Exception) as exc:  # noqa: BLE001
        failures.append(str(exc))
        print("\n✘ 失败:", exc, file=sys.stderr)
        try:
            for svc in service_items(data_dir):
                if svc["containerName"] == container_name:
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
                run([docker, "compose", "-p", project, "-f", str(compose_file), "down", "--remove-orphans"],
                    env=env, timeout=180, check=False)
            except Exception:  # noqa: BLE001
                pass
            shutil.rmtree(workdir, ignore_errors=True)
        if started_daemon and daemon_proc:
            daemon_proc.terminate()
            try:
                daemon_proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                daemon_proc.kill()
        try:
            if not started_daemon:
                ctl(data_dir, "scan", timeout=60)  # refresh dashboard state
        except Exception:  # noqa: BLE001
            pass

    if failures:
        print("\n阶段 0 验证失败。日志: %s/logs/daemon.log" % data_dir, file=sys.stderr)
        return 1
    print("\n✅ 阶段 0 全部验证通过")
    return 0


if __name__ == "__main__":
    sys.exit(main())
