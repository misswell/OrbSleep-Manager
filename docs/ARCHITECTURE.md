# 架构说明

## 1. 组件

| 进程 | 语言 | 职责 |
|---|---|---|
| `orbsleepd` | Go | Docker 扫描、Compose 定位与修改、代理配置、端口网关、SQLite、Unix Socket API |
| `orbsleepctl` | Go | 同一套核心的 CLI（诊断、扫描、计划、执行、恢复；可选高级入口） |
| OrbSleep Manager | SwiftUI | 安装向导、后台服务管理、概览、扫描、计划预览、托管服务、诊断与修复、日志与维护、设置、操作日志 |
| Lazy TCP Proxy | 官方镜像 | 按连接启动容器、空闲停止、TCP 转发 |
| LaunchAgent | launchd | 用户登录后启动 `orbsleepd`，GUI 关闭后保持端口监听 |

GUI 与后台服务使用 Unix Socket（`~/Library/Application Support/OrbSleep/daemon.sock`，0600）通信，协议见 [PROTOCOL.md](PROTOCOL.md)。GUI 不直接操作 Docker，也不执行用户输入的 Shell。

## 1.1 打包与后台服务

`orbsleepd`、`orbsleepctl` 和 LaunchAgent plist 全部位于应用包内，由构建阶段 `scripts/embed-service.sh` 放入：

```text
OrbSleepManager.app/Contents/
├── MacOS/OrbSleepManager
├── Resources/{orbsleepd,orbsleepctl}
└── Library/LaunchAgents/com.misswell.orbsleep.daemon.app.plist
```

GUI 通过 `SMAppService.agent(plistName:)` 注册应用包内的 LaunchAgent；plist 使用 `BundleProgram = Contents/Resources/orbsleepd`，因此应用被移动后仍能正确启动。注册、启动、停止、重启、修复、升级和卸载都由 GUI 发起：

- 启动：`launchctl kickstart -k gui/<uid>/com.misswell.orbsleep.daemon.app`
- 停止：调用 `system.shutdown`，后台服务完成事务后以退出码 0 退出（`KeepAlive.SuccessfulExit=false` 不会重启）
- 升级：后台服务二进制随应用包更新，重启服务即加载新版本
- 卸载：先恢复所有托管服务，再 `SMAppService.unregister` 并清理可选的 `~/.local/bin/orbsleepctl`

开发构建若没有 Developer ID 签名，`SMAppService.register` 可能被拒绝，此时 GUI 回退到由包内 `orbsleepctl install-agent` 写入用户级 LaunchAgent；两条路径都不需要用户打开终端或输入 `sudo`。后台服务只以当前用户权限运行，没有 LaunchDaemon。

### 1.2 旧版迁移与运行时契约

0.2.0 使用新的 LaunchAgent label `com.misswell.orbsleep.daemon.app`，与 0.1.x 手动安装的 `com.misswell.orbsleep.daemon` 并存而不冲突。GUI 启动时检测旧版 plist、`/usr/local/bin/orbsleepd` / `orbsleepctl` 与旧 launchd 任务，并按「备份 → 检查未完成事务 → 停止旧服务 → 移除旧启动项 → 注册包内服务 → 验证 → 清理旧二进制」的顺序迁移；root 所有的旧二进制通过一次性 macOS 管理员授权删除，不作为常驻 root 服务。

后台协议在 0.2.0 增加 `system.health`、`system.prepareShutdown`、`system.prepareUpdate`、`system.recovery.list/resolve`、`docker.contexts/select`、`backup.create/list/restore`、`logs.export`。`agent.install/uninstall` 从正常 API 移除，LaunchAgent 生命周期统一由 Swift 的 `BackgroundService` 管理，避免 GUI 与 daemon 同时管理同一任务。Docker Context 的选择只保存为 OrbSleep 自己的配置（`settings.dockerSocket`），不修改用户全局 Docker Context。

`backup.restore` 不直接覆盖运行中的 SQLite：daemon 先写入 `restore-request.json` 并退出，`main` 在 `engine.New` 之前调用 `ApplyPendingRestore` 替换数据库，旧库保留为 `state.sqlite.pre-restore-<ts>`，因此恢复过程即使中断也不会留下半写状态。

应用更新中心从 `OSUpdateFeedURL` / `ORBSLEEP_UPDATE_FEED` 读取 JSON，下载后校验 SHA256、解包并 `codesign --verify --deep --strict`，再把当前 App 移到 `app-backup/` 后替换；由独立 relaunch 脚本启动新版本，12 秒内未运行则自动还原旧版本。后台服务二进制随应用包更新，升级时重新注册 LaunchAgent 并重启服务。

## 2. 端口链路的关键取舍

```text
客户端 → 127.0.0.1:8080（gateway，Go net.Listen）
       → 127.0.0.1:20000（代理容器发布到 loopback 的端口）
       → Lazy TCP Proxy 内部监听 20000
       → 业务容器:80
```

计划原本写的是「转发到代理容器 IP:20000」。实现中默认使用**代理容器把监听端口范围发布到 127.0.0.1**（`20000-20099:20000-20099`），原因：

1. 实测在 OrbStack 2.2.5 上，macOS 直连代理容器 IP 返回 `no route to host`，容器 IP 路径不可依赖。
2. 代理容器重启 / IP 变化时不需要更新上游地址。
3. 端口范围在代理容器创建时固定发布，分配结果与范围一一对应，冲突面可控。
4. 保留 `settings.proxyDialMode = "container_ip"` 供实验，但该模式在当前环境不可用，默认必须为 `published`。

`published` 模式下每接入一个端口就占用范围中的一个端口；范围耗尽时计划会阻塞并提示扩大范围。修改范围需要重建代理容器（`proxy-setup`）。

## 3. 扫描与分类

- `dockerx.Detect` 依次尝试：`ORBSLEEP_DOCKER_SOCKET`、`DOCKER_HOST`、`orbstack` context 的 endpoint、`~/.orbstack/run/docker.sock`、`~/.docker/run/docker.sock`、`/var/run/docker.sock`。只读取，不修改 `currentContext`。
- 扫描会 `inspect` 所有容器（含已停止），并发上限 8。
- 分类输出 `eligible` / `needs_review` / `excluded`，并附带可读原因与 `proxyCandidates`。
- 排除项：`host` 网络、`--rm`、非 Compose、`compose run` 一次性容器、纯 UDP、动态宿主机端口、低于 1024 的端口（未启用特权助手）。
- 需确认项：绑定所有网卡、IPv6、多 Compose 文件、多实例、特权模式、数据库/消息队列镜像、混合 UDP。
- 分类只依据实际 Inspect 数据，不靠镜像名「猜测安全」，镜像名只用于提示有状态服务。

## 4. Compose 定点修改

`internal/compose` 不把解析后的 YAML 重新序列化，而是：

1. 用 `yaml.v3` 的 Node 定位 `services.<name>.ports` 的列表项及其**行号**。
2. 只支持块状列表、短语法/长语法、静态端口；拒绝流式写法、锚点/别名、`extends`、`<<` 合并键、端口范围。
3. 删除目标项所在行（以及列表为空时的 `ports:` 键行），保留注释、缩进、引号与其它服务。
4. 用 `yaml.Unmarshal` 重建 map，删除预期端口后与修改后的内容做深度比较，确保没有其它变化。
5. 写入采用临时文件 + fsync + rename，保留原文件权限。

执行接入时还会调用 `docker compose config --format json`，对修改前后的**有效配置**做同样的「只允许端口差异」校验，覆盖 `.env` 插值等解析差异。

## 5. 共享 Compose 文件

一个 Compose 文件里可能有多个服务同时被接入。实现要点：

- 计划中的多个项目按顺序执行；执行器维护「文件 → 当前内容」状态，后一项以前一项修改后的内容为基线，并继续校验用户没有在计划生成后改动文件。
- 每个接入项各自备份修改前的内容，回滚按逆序恢复到接入前。
- 恢复（detach）不整体还原备份，而是用定点 `AddBindings` 只把该服务的端口加回，避免把同一文件中其他仍托管服务的端口一起恢复。只有无法定点编辑时才回退到完整备份，并在操作日志中告警。

## 6. 接入事务

```text
创建操作记录
  → 备份 Compose + 代理配置 + 元数据（backups/<opID>/item-N/）
  → 在代理中注册目标（PUT /config/update，GET 校验）
  → 定点删除 Compose 端口并原子写入
  → docker compose config 有效配置校验
  → docker compose up -d --no-deps --force-recreate <service>
  → 轮询容器就绪、确认原端口不再由容器发布
  → 启动 macOS 端口监听（同一原绑定地址/端口）
  → 原端口 TCP 连通性测试
  → 写入 SQLite，复制一份稳定的原始配置备份（backups/services/<svcID>/）
```

任一环节失败，按逆序撤销已完成的步骤（删除监听、删除记录、恢复 Compose 并重建、恢复代理配置）。回滚失败时操作状态为 `rollback_failed`，日志给出备份路径与人工恢复步骤；任何情况下都不删除数据卷。

多容器计划按顺序执行；第 N 个失败时，回滚前 N-1 个已成功项。

## 7. 状态与恢复

- SQLite 表：`services`、`operations`、`settings`、`meta`。
- 守护进程启动时 `reconcile`：读取托管服务，按名称重新绑定容器 ID，按原绑定地址恢复端口监听，并同步代理配置。
- 每 15 秒对**已托管容器**做一次轻量 reconcile；未托管容器不会被持续扫描或唤醒。
- 订阅 Docker 容器事件，只对已托管容器和代理容器的 start/stop/die/destroy/create 触发立即 reconcile；删除容器不会立刻清空管理记录。
- 服务状态：`managed_running`（有活动连接）、`managed_idle`（运行但空闲）、`managed_stopped`（已被代理停止）、`missing`、`warning`、`failed`。
- 若发现容器又重新发布了被接管的端口（有人手动恢复 Compose），标记 `warning`，避免静默冲突。

## 8. 代理配置

- 代理容器：`orbsleep-lazy-tcp-proxy`，由 `~/Library/Application Support/OrbSleep/proxy/docker-compose.yml` 管理。
- 管理 API 与状态页只发布到 `127.0.0.1`；密钥随机生成，优先存入 Keychain，失败时写 0600 文件。
- 配置由 SQLite 中的托管服务生成，使用 `PUT /config/update` 写入，`GET /config` 回读比对。
- 代理没有标签依赖；Compose 重建后容器名不变，代理按名称重新解析新容器。
- 代理状态读取使用状态页的 `/status`（`/metrics` 仅用于 PostgreSQL 活动统计，未配置时返回 503）。
- 「临时排除休眠」通过把该服务的 `idle_timeout_secs` 提高到 7 天实现（代理没有无限空闲模式），界面中会说明。

## 9. 事务与并发保证

- 接入前必须成功读取并校验完整代理配置基线；失败则不进入任何修改步骤。
- 每个修改步骤在**执行写入之前**登记撤销动作；即使“写入成功但确认失败”，也会执行恢复。
- 网关监听在创建前登记整体清理；多端口部分失败不会残留监听器。
- 批量回滚按逆序进行：停止监听 → 恢复 Compose → 重建 → 验证原端口发布 → 最后删除托管记录。任何一步失败都保留记录并进入 `rollback_failed`。
- 恢复（detach）先删除并验证代理目标，再动网关和 Compose；连通性测试失败会回退到托管状态，而不是删除记录。
- 恢复时只用 `AddBindings` 定点加回本服务端口；文件存在时绝不整体覆盖备份，避免抹掉用户后续修改。
- `start` / `stop` / `updatePolicy` 与接入、恢复、代理安装共用同一把写锁，事务期间返回冲突而不是交错执行。
- 代理端口设置在安装后锁定；重新安装必须成功启动、管理接口可达且配置同步成功后才提交新设置，失败自动回滚到旧设置与旧代理。
- 代理配置快照使用深拷贝（`Config.Clone`），`Remove` 总是分配新切片，避免工作副本污染基线。
- 接入事务在校验基线后、任何修改前写入 SQLite `transactions` 表，并按 `prepared → proxy_updated → compose_write_pending → compose_updated → container_recreated → gateway_ready → verified → committed_item` 持久化阶段；每项备份路径在修改前登记。`compose_write_pending` 会在原子替换 Compose 之前持久化完整的新文件内容（write-ahead），因此“文件已换、事务未更新”的崩溃窗口也能被识别并回滚。
- 守护进程启动时在开放 API 前扫描 `running` 事务：若全部项目已 `committed_item` 则补记完成，否则按备份回滚 Compose、重建容器、验证原端口、删除托管记录并清理代理目标。共享同一 Compose 文件时，按文件只校验最后一项的完成内容，避免把已全部成功的批量接入误判为未完成。恢复每个服务前先释放该服务的网关监听，避免与 reconcile 已重建的监听抢端口；恢复失败时回退到一致的托管状态并重建监听。代理/Docker 不可用时保留事务，后续 reconcile 重试。
- `restore/remove` 同样有持久化事务（`detach`）：记录服务、真实工作目录、托管 Compose、预期恢复后的 Compose 和阶段。启动恢复会先释放网关，再决定继续完成还是回退到托管状态；外部修改的 Compose 不会被覆盖。缺少 `ExpectedRestored` 时采取保守策略，不覆盖现有文件。Compose 文件缺失且与其他托管服务共享时，从备份重建并移除其他托管服务的旧端口，避免重新引入冲突端口。
- 事务终态与操作终态在同一个 SQLite 事务中提交；启动时还会修正孤立的 `running` 操作记录（有已提交事务则补记成功，无事务则标记失败）。
- 长事务通过 `beginOperation` 注册：关闭时先拒绝新操作、取消运行中的事务，并等待其回滚完成（最多 10 分钟）后才关闭日志、数据库和网关；未完成阶段仍由事务日志兜底。
- 代理容器可通过 `proxy.stop` 停止并移除（保留设置和配置）；停止后端口设置可编辑，重新 `proxy.setup` 生效。

## 10. 安全

- Socket 0600；请求带协议版本；所有写操作在守护进程内串行化。
- API 参数白名单校验（Compose 文件路径来自 Docker 标签，不来自 GUI 任意输入）。
- 不把 Docker API 暴露到网络；不执行 `prune`；不删除卷。
- 数据目录 0700、数据库与日志 0600、launchd 任务 Umask 077；日志/操作历史按保留天数定期清理，服务恢复备份不会被清理。
- 低端口助手、IPv6 网关尚未实现：低端口与 IPv6 绑定在分类阶段直接排除，`container_ip` 只有逐端口探测通过后才允许启用。
- 旧版本中 `enableLowPortHelper=true` 会在启动时自动迁移为 `false`；失败或 `rollback_failed` 操作的备份不会被保留期清理，直到用户主动处理。备份清理先于操作/事务历史清理执行，并覆盖 `backups/proxy/<opID>` 嵌套目录。
- 接入前后核对挂载、网络、环境变量、健康检查、重启策略（通过有效配置比较与容器 Inspect 展示）。

## 11. 未实现

- UDP 转发、IPv6、低端口特权助手、多文件 Compose 自动修改、复杂 `extends` / 锚点。
- 独立 `docker run` 容器的自动迁移（只给出「不支持」原因，不猜测创建命令）。
- 应用内自动下载并替换应用包（后台服务升级随应用更新已完成，但应用自身的自动更新尚未实现）。
