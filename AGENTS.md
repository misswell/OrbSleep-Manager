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
- 默认 `proxyDialMode=published`：代理端口范围发布到 `127.0.0.1`。实测 macOS 直连 OrbStack 容器 IP 不可靠（曾出现 `no route to host`）；`container_ip` 仅在逐端口探测通过后才允许启用。
- 接入前必须读取并校验代理配置基线；每个撤销动作必须在写入前登记；回滚只有在容器重建并验证原端口发布成功后才能删除托管记录。
- 恢复时文件存在则只用 `AddBindings` 定点加回端口，禁止用完整备份覆盖；连通性失败要回退托管并报失败。
- 代理安装后端口设置锁定；重复安装必须幂等；失败要恢复旧设置与旧代理。不要加 `--remove-orphans`。
- 共享 Compose 文件：接入时用 `fileStates` 串联修改；恢复时用 `compose.AddBindings` 只加回本服务端口，禁止整体还原备份（会破坏其他托管服务）。
- 代理配置快照必须用 `Config.Clone()`；`Remove`/`Upsert` 不得原地复用切片。回滚后要逐服务比对基线。
- 接入必须在修改前持久化 `transactions`（基线 + 每项备份 + 阶段）；改 Compose 前必须写入 `compose_write_pending` 和预期新内容。启动恢复先于 API。备份清理必须保留 `failed`/`rollback_failed`/`running` 操作的证据，并先清备份后清历史。
- `restore/remove` 必须持久化 `detach` 事务（含真实 `WorkingDir`）；恢复前释放对应网关监听，失败回退托管；不得覆盖外部修改的 Compose；缺少预期恢复快照时不得覆盖。
- 共享 Compose 文件缺失时，必须按其他托管服务的当前状态重建并移除其旧端口，禁止直接用单个服务备份整文件覆盖。
- 事务完成判断必须按 ComposeFile 分组，只校验每个文件的最终内容。
- 回滚不得复用已取消的业务上下文；依赖不可用时事务保持 `running`，不得标记终态。关闭超时不得关闭仍可能被事务使用的资源。
- 接入回滚前必须三态检查 Compose：存在且等于本次写入内容才允许撤销；缺失/不可读/内容不同一律保留现状并标记 `rollback_failed`。
- detach 起始时若 Compose 缺失，必须先持久化重建后的托管快照与预期恢复快照，再写文件。
- 共享 Compose 重建只清其他托管服务的 TCP 端口，禁止删除 UDP/其他协议。`container_ip` 路由失败必须立即移除旧路由。
- Compose 原子写必须保留符号链接；卸载时后台助手不可用或仍有托管服务时默认中止，除非 `--force`。
- 长事务必须通过 `beginOperation` 注册；关闭时先取消并等待回滚，禁止先关数据库。
- 守护进程首次 reconcile 必须在 API 就绪前同步完成（保证重启后端口立刻恢复）。

## 数据位置

`~/Library/Application Support/OrbSleep/`：`state.sqlite`、`proxy/`（compose + config）、`backups/`、`logs/`、`daemon.sock`。LaunchAgent：`com.misswell.orbsleep.daemon`。

## 环境

- 本机 OrbStack 2.2.5、Docker 29.8.2、Go 1.27、Xcode 27.1、macOS 27.2。
- Apple Team `U8U443D7ZL`，bundle id `com.misswell.orbsleepmanager`。
