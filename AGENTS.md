# OrbSleep Manager — 项目约定

## 结构

- `core/`：Go 模块 `orbsleep`。`cmd/orbsleepd`（守护进程）、`cmd/orbsleepctl`（CLI）、`internal/engine`（业务核心）、`internal/compose`（Compose 定点修改）、`internal/gateway`（macOS TCP 网关）、`internal/proxy`（Lazy TCP Proxy 管理与配置）、`internal/dockerx`（Docker API over Unix Socket）、`internal/state`（SQLite）。
- `app/`：SwiftUI 客户端，XcodeGen 工程（`app/project.yml`），产物 `app/build/Debug/OrbSleepManager.app`。
- `docs/PROTOCOL.md`：GUI ↔ daemon 的 NDJSON 协议，**唯一契约**。改协议必须同时改 `core/internal/model`、`core/internal/api`、`app/Sources/Models.swift` 与文档。
- `scripts/phase0.py`：真实 OrbStack 端到端验收。

## 构建与测试

```sh
make build-core     # bin/orbsleepd bin/orbsleepctl
make test           # go test ./...
make app            # xcodegen + xcodebuild（产物 app/build/Debug）
make phase0         # 需要 OrbStack；会创建/清理测试 Compose 项目
```

## 不可破坏的规则

- 不切换用户的全局 Docker Context；所有 docker 命令显式使用检测到的 socket（`DOCKER_HOST`）。
- 不执行 `docker system prune`，不删除数据卷、镜像、业务文件。
- 修改 Compose 必须：备份 → 定点编辑（保留注释/顺序/其它服务）→ YAML 重解析 → `docker compose config --format json` 只允许端口差异 → 失败回滚。
- 默认 `proxyDialMode=published`：代理端口范围发布到 `127.0.0.1`。实测 macOS 无法直连 OrbStack 容器 IP（`no route to host`），不要改回默认 `container_ip`。
- 共享 Compose 文件：接入时用 `fileStates` 串联修改；恢复时用 `compose.AddBindings` 只加回本服务端口，禁止整体还原备份（会破坏其他托管服务）。
- 守护进程首次 reconcile 必须在 API 就绪前同步完成（保证重启后端口立刻恢复）。

## 数据位置

`~/Library/Application Support/OrbSleep/`：`state.sqlite`、`proxy/`（compose + config）、`backups/`、`logs/`、`daemon.sock`。LaunchAgent：`com.misswell.orbsleep.daemon`。

## 环境

- 本机 OrbStack 2.2.5、Docker 29.8.2、Go 1.27、Xcode 27.1、macOS 27.2。
- Apple Team `U8U443D7ZL`，bundle id `com.misswell.orbsleepmanager`。
