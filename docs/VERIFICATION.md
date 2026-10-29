# 验证记录

环境：macOS 27.2 (arm64)、OrbStack 2.2.5、Docker Engine 29.8.2、Go 1.27.3、Xcode 27.1。

## 自动化测试

```sh
cd core && go vet ./... && go test ./...
cd app && xcodegen generate && xcodebuild -project OrbSleepManager.xcodeproj \
  -scheme OrbSleepManager -configuration Debug -destination 'platform=macOS' CODE_SIGNING_ALLOWED=NO build
```

覆盖内容：

| 包 | 测试 |
|---|---|
| `internal/compose` | 单/多端口定点删除、空列表移除 `ports:`、长语法、流式写法拒绝、多服务保留、有效配置校验、端口重新添加（含无 `ports:` 键、长语法、幂等） |
| `internal/scan` | 可接入、host 网络、无 Compose、低端口、全网卡、有状态镜像、UDP-only、`--rm` |
| `internal/gateway` | 真实 TCP 转发、并发 20 连接、目标热更新、连接计数、半关闭、端口冲突 |
| `internal/state` | SQLite 服务/操作/设置/元数据读写 |
| `internal/settings` | 默认值、布尔/列表持久化、端口范围校验 |
| `internal/api` | 启动守护进程 + Unix Socket 往返：ping/status/settings/services/operations/logs/doctor，Docker 缺失时 scan 返回协议错误 |

结果：全部 PASS。

## 真实 OrbStack 端到端

### 阶段 0 脚本（`scripts/phase0.py`）

```
▶ 代理监听端口可从 macOS 访问           OK
▶ 扫描 / 计划 / 接入                     succeeded
▶ Compose 中端口已移除，原地址 HTTP 200  OK
▶ docker stop 后请求自动唤醒             OK
▶ 空闲 30 秒后自动停止                   OK
▶ restore 后 Compose 逐字节恢复、原端口直连 OK
✅ 阶段 0 全部验证通过
```

### 额外场景（手工，均通过）

| 场景 | 结果 |
|---|---|
| 同一个 Compose 文件中的两个服务一次性接入 | 两个容器顺序接入，各自独立代理端口，均 200 |
| 从共享 Compose 文件恢复其中一个服务 | 只把该服务的端口加回，另一个服务仍由网关/代理管理，两个地址均 200 |
| 单容器多个 TCP 端口（18184、18185 → 80） | 分配两个代理端口，两个原地址均 200 |
| 接入一个已停止（exited）的 Compose 容器 | 计划可用；执行时重建并启动，原地址 200 |
| 接入后删除容器，再由 Compose 重建 | 状态 `missing` → 自动更新容器 ID → `managed_idle`，原地址 200 |
| 停止守护进程再启动 | 监听在 API 就绪前恢复，两个地址立即 200 |
| 重启代理容器 | 端口监听保持，两个地址 200（代理从磁盘配置恢复） |
| 计划生成后手工修改 Compose 再执行 | 拒绝执行，不改任何文件，原服务保持可用 |
| 端口范围缩小到不包含已托管端口 | 设置更新被拒绝，范围不变 |
| LaunchAgent 安装 / 卸载 | 安装后守护进程由 launchd 拉起并可 ping；卸载后 plist 与进程消失 |
| GUI 连接真实守护进程 | 应用启动、建立 Socket、解码 status/services/scan 正常 |

## 关键发现

### 1. 容器 IP 直连不可靠 → 默认使用「代理端口发布到 127.0.0.1」

在 OrbStack 2.2.5 中曾观测到 macOS 直连代理容器 IP 的监听端口返回
`connect: no route to host`，另一个时间点则只能连到容器内 8080。结论是容器 IP 路由不可依赖：

- 默认 `proxyDialMode=published`：代理容器发布 `127.0.0.1:20000-20099`，网关转发到发布端口。
- 切换到 `container_ip` 前会逐个探测所有已托管服务的代理端口（无服务时探测 8080），探测失败则拒绝保存设置。
- 该设计同时消除了代理容器重启/IP 变化对网关上游地址的影响。

### 2. 代理状态接口是 `/status`，不是 `/metrics`

当前镜像的 `/metrics` 只提供 PostgreSQL 活动统计（未配置时返回 503）；
服务运行状态在 `WEB_PORT` 的 `/status`。实现已改用 `/status`。

### 3. 共享 Compose 文件的恢复必须「加回端口」而非「还原备份」

一个 Compose 文件中的多个服务先后接入时，后者的备份包含前者已移除端口的文件。
如果用备份整体恢复，会把其他仍托管服务的端口一起恢复并造成冲突。
因此 detach 使用定点 `AddBindings`（保留注释与其他服务），只有无法定点编辑时才回退到完整备份并给出警告。

### 4. 守护进程启动必须先恢复监听再开放 API

初版在后台 goroutine 中做首次 reconcile，实测重启后存在最长 15 秒的端口不可用窗口。
现改为 `Start()` 内同步执行首次 reconcile（带 25s 超时），API 就绪时监听已恢复。

## 代码审查后的加固（0.1.1）

针对 `464ce3c` 的静态审查发现的事务问题，已修复并补齐故障注入测试：

| 问题 | 修复 |
|---|---|
| 代理基线读取失败后仍继续，回滚可能清空配置 | 基线必须完整读取并通过校验才允许开始；`rollbackApplied` 只移除本次回滚成功的服务，并恢复基线中同名旧条目 |
| 更新成功后确认读取失败，撤销未注册 | 撤销步骤在首次写入前注册；确认失败也会执行恢复并校验 |
| 恢复时删除代理配置失败仍继续 | 删除代理目标是恢复的前置步骤，失败立即中止（网关与 Compose 均未改动） |
| 多端口网关部分监听残留 | 监听前先登记整体清理撤销；任一端口失败会移除本服务全部监听 |
| 恢复失败时用完整备份覆盖用户修改 | 文件存在时只用 `AddBindings` 定点恢复；失败则保持现状并回退到托管状态，不再整体覆盖 |
| 批量回滚先删记录、CLI 失败被忽略 | 先恢复文件、重建、验证原端口发布成功，最后才删除记录；CLI/代理不可用时直接报告回滚未执行 |
| 恢复连通性失败仍报成功 | 连通性失败视为恢复失败，回退托管并报告 `failed` / `rollback_failed` |
| 代理安装不可重复执行 | 运行时端口视为自有；管理/Web 端口联合分配；端口未变化且管理接口可达时直接复用；`--remove-orphans` 已移除 |
| 设置与运行中代理不一致 | 代理安装后锁定端口设置；新设置在代理启动、管理接口可达、配置同步全部成功后才提交，失败自动恢复旧设置与旧代理 |
| container_ip / 低端口 / IPv6 | container_ip 先逐端口探测；低端口始终排除；IPv6 绑定直接排除 |
| 生命周期竞争 | start/stop/updatePolicy 使用同一写锁的 `TryLock`，事务期间返回冲突 |
| 日志与权限 | 数据目录 0700、DB/日志 0600、launchd Umask 077；日志与操作历史按保留天数定期清理；备份目录独立清理，保留服务恢复备份 |

新增的故障注入测试（`core/internal/engine/attach_test.go`）：

- 代理配置写入成功但确认读取超时 → 配置恢复、Compose 未改、无托管记录、无遗留监听
- 多端口第二个监听被占用 → 第一个监听被清理、原容器恢复、无托管记录
- 回滚时容器无法重建 → 托管记录保留
- 代理基线不可读取 → 操作立即失败且“未做任何修改”
- 恢复时代理删除失败 → 全部保持原样
- 恢复后连通性失败 → 回退托管状态，记录与代理目标保留
- 代理自有端口识别、管理端口避让、基线配置校验

## 第二轮复审加固（0.1.2）

| 复审问题 | 修复与验证 |
|---|---|
| `Config` 浅拷贝污染基线 | 新增 `Config.Clone()`、非原地 `Remove()`、拷贝式 `Upsert()`/`Service()`/`Marshal()`；新增 3 服务回滚测试，验证恢复失败后 A/B/C 配置逐项与操作前一致 |
| 崩溃后未完成接入无恢复 | 新增 SQLite `transactions` 持久化事务（阶段、基线、每项备份、editing 后内容）；启动时在 API 前自动回滚。真实模拟：手工写入 `compose_updated` 事务 + 已删除端口映射，SIGKILL 守护进程后重启 → Compose 恢复、原端口 HTTP 200、事务标记 `rolled_back` |
| 代理安装回滚不可靠 | 旧 Compose 先校验读取（非 `IsNotExist` 即中止）、快照写入备份目录、原子写入、失败统一走恢复；恢复失败标记 `rollback_failed`。测试覆盖首次安装失败与旧代理恢复失败 |
| 关闭打断事务 | 新增事务注册表：关闭时拒绝新操作、取消运行中事务、等待回滚（10 分钟），再关闭数据库/日志/网关；测试验证取消与等待 |
| 停止代理后无法改端口 | `proxy.installed` 改为“容器存在”；新增 `proxy.stop`（保留设置）与 GUI 停止按钮；停止后可改端口并重新安装（实测） |
| 旧低端口配置卡死设置 | 启动时 `MigrateLegacy()` 自动将旧 `enableLowPortHelper=true` 迁移为 false，并加单测 |
| 失败备份被清理 | 备份清理关联操作状态：`failed` / `rollback_failed` / `running` 一律保留，未知目录保留；单测覆盖 |
| 删除托管记录失败未回退 | 删除失败现在走 `revertManaged`，失败则 `rollback_failed`，保留托管记录；故障注入测试模拟 SQLite 删除失败并验证 Compose/代理/网关全部回到托管状态 |

新增/扩展测试：`proxy_test.go`（深拷贝）、`recovery_test.go`（崩溃恢复、外部修改保护、关闭取消）、`attach_test.go`（代理安装失败回滚、多服务配置保持、备份保留策略）。`go test -race ./...` 全部通过（并修复了网关目标热更新的数据竞争）。

## 第三轮复审加固（0.1.3）

| 复审问题 | 修复与验证 |
|---|---|
| Compose 写入与事务阶段之间的崩溃窗口 | 写入前持久化 `compose_write_pending` + 完整 `PendingContent`；恢复按原始/预期新/实际三态判断。新增“原子替换已完成但 ManagedContent 未写”故障注入测试 |
| 关闭取消导致回滚不执行 | 回滚使用独立 20 分钟上下文；依赖不可用返回 `rollbackDeferredError`，事务保持 `running` 由启动恢复重试；关闭超时不再关闭数据库/日志/网关，避免使用已关闭资源 |
| 延迟恢复与 reconcile 网关冲突 | 恢复每个服务前先 `gw.RemoveService`；恢复失败回退托管并重建监听。新增“首次恢复失败 → reconcile 留下监听 → 第二次恢复成功”测试 |
| restore/remove 无持久化事务 | 新增 `detach` 事务（服务、托管内容、预期恢复内容）；启动恢复先释放网关，再完成/回退，外部修改不覆盖。新增中途崩溃回退与已完成补记测试 |
| 批量回滚删除托管记录失败 | `rollbackApplied` 删除失败时调用 `revertToManaged`，恢复 Compose/容器/网关，代理目标保留；新增故障注入测试 |
| 备份清理顺序与嵌套目录 | 先按操作状态清理备份，再清理操作/事务历史；递归处理 `backups/proxy/<opID>`；成功删除、失败保留均有测试 |
| 代理安装备份写入错误被忽略 | 快照全部改为检查错误的原子写入，任何备份失败在修改 Compose 前中止；新增不可写备份目录测试 |
| 事务终态与操作终态窗口 | 新增 `FinishTransactionWithOperation` 单 SQLite 事务；启动时 `ReconcileOrphanOperations` 修正孤立 running 操作；新增原子提交与孤儿修正测试 |

真实 OrbStack 验收（2026-10-29）：`phase0.py` 全流程通过；手工模拟 `compose_write_pending` 崩溃（文件已替换、`ManagedContent` 未写、`SIGKILL`）后重启，Compose/原端口/事务/操作记录全部恢复；手工模拟 `detach` 崩溃（Compose 已恢复、记录未删）后重启，自动回退托管、网关与原端口 HTTP 200。`go test -race ./...` 通过。

## 第四轮复审加固（0.1.4）

| 复审问题 | 修复与验证 |
|---|---|
| 共享 Compose 文件导致已提交事务被误判 | `txnAllCommitted` 按 ComposeFile 分组，只比较最后一项的完成内容；新增两个服务共享文件、全部 committed 但终态未写的恢复测试，验证补记 `committed` 而不回滚 |
| detach 恢复丢失工作目录 | `detachPayload` 新增 `WorkingDir`，创建事务时从备份元数据读取；恢复时使用该目录。新增不同工作目录的恢复测试 |
| 共享 Compose 缺失时整文件恢复引入其他端口 | 新增 `rebuildMissingCompose`：从备份重建并移除其他仍托管服务的旧端口，再补回本服务端口；新增共享文件缺失恢复测试 |
| `ExpectedRestored` 为空时覆盖外部修改 | 缺少预期快照且当前文件不等于托管内容时拒绝自动覆盖并标记 `rollback_failed`；新增不覆盖测试 |
| detach 回滚仍使用取消上下文 / container_ip 静默回退 | `revertManaged` 的网关恢复改用独立 30 秒上下文；`container_ip` 无法解析代理 IP 时返回错误，不回退 loopback；新增路由错误测试 |
| SQLite `ALTER TABLE` 忽略所有错误 | 仅忽略 duplicate column，其他迁移错误直接返回；新增二次打开与错误识别测试 |

## 第五轮复审加固（0.1.5）

| 复审问题 | 修复与验证 |
|---|---|
| 接入回滚无条件整文件覆盖 | `attachOne` 的撤销栈执行前校验 Compose 是否仍等于本次写入内容；被外部修改时跳过全部撤销、补齐托管记录/网关监听并标记 `rollback_failed`；写入前二次校验缩小竞态窗口。新增“接入中外部修改 + 重建失败”故障注入测试，断言用户编辑、托管记录、代理与监听全部保留 |
| 读取失败被当作文件缺失 | `restoreService` 仅在 `errors.Is(err, os.ErrNotExist)` 时从备份重建，权限/IO 错误直接中止且不覆盖；恢复入口 `runDetach` 同样放宽为仅对缺失文件继续 |
| 正常恢复入口走不到缺失文件重建 | `runDetach` 允许 Compose 缺失时继续（事务记录托管内容为空、标注重建路径）；新增经 `runDetach` 入口的缺失文件重建测试，验证端口加回、其他托管服务端口不回写、记录与代理正确处理 |
| 共享文件中间阶段崩溃误判外部修改 | `recoverAttachTxn` 新增分支：无托管/待写快照的项（崩溃在 `proxy_updated` 等）不按计划时 `Content` 比对，仅撤销其代理注册。新增 A 已提交、B 停在 `proxy_updated` 的崩溃恢复测试，断言文件回到接入前、代理配置等于基线 |
| 重建共享文件可能回写其他服务旧端口 | 新增 `compose.ClearServicePorts` 清除其他托管服务在备份中的全部端口声明（含历史旧端口），无法安全编辑的结构直接中止重建；删除已无调用点的 `RemoveBindingsLenient`。新增“备份含其他服务旧端口”回归测试与编辑器单测 |
| reconcile 自建路由绕过 `container_ip` 检查 | 路由构造抽取为 `gatewayRouteFor`，reconcile 与接入共用；无法解析代理 IP 时标记 `warning`、不创建 `127.0.0.1` 回退监听。新增 reconcile 缺代理 IP 测试 |

`go vet ./...`、`go test -race ./...` 全部通过；守护进程/CLI 以 `make build-core` 构建通过。本轮未重跑真实 OrbStack 故障注入（上轮 2026-10-29 的 phase0 结果仍有效；建议合并后在真机补“接入中外部修改 Compose”与“共享文件 `proxy_updated` 崩溃”两个场景）。

## 未验证 / 未实现

- 特权端口助手（<1024）、UDP、IPv6、多 Compose 文件自动修改、`extends`/锚点。
- 独立 `docker run` 容器的自动迁移。
- 自动更新、菜单栏常驻；直接分发构建已用 Developer ID 签名并完成 Apple 公证（v0.1.5）。
- GUI 未做屏幕截图验证（当前会话无屏幕录制权限）；已通过编译、真实协议往返与应用运行/连接验证。
