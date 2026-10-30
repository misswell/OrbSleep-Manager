# OrbSleep Manager 0.1.7 可靠性改造报告

版本：0.1.7（0.1.6 已发布，本版作为 0.1.6 全量可靠性方案的后续交付，不覆盖已发布的 0.1.6 资产）

## 1. 已完成的架构改造

- Compose 文件级快照：`internal/compose/snapshot.go` 保存 Original / LastKnownGood、SHA256、权限、UID/GID、文件类型与符号链接目标；attach/detach 成功或重建后更新最后可信快照。
- 文件指纹：`internal/compose/fingerprint.go` 把内容、权限、所有者、类型、链接目标纳入一致性判断。
- 安全合并：`RestoreManagedBindings` 使用 AST 定点添加端口，保留其他服务、UDP、环境变量和注释；宿主机端口冲突会拒绝恢复。
- 原子写：同目录临时文件、Sync、Rename、父目录 fsync、保留权限/所有者、拒绝非普通文件；符号链接写入解析后的目标并保留链接。
- 跨进程文件锁：`internal/transaction/lock.go` 进程内队列 + flock，按规范化 Compose 路径串行化 attach/detach。
- 事务阶段：`internal/transaction/phase.go` 定义 PREPARED/APPLYING/VERIFYING/COMMITTED/ROLLBACK_* /NEEDS_REPAIR 及合法转换。
- SQLite：WAL + `synchronous=FULL`，状态与操作终态原子提交；新增 PRAGMA 测试。
- Reconcile：单次容器列表、单次代理读取、期望路由计算；只在运行时字段变化时 Update；`Gateway.Matches` 校验目标 IP/端口/归属，失败路由立即移除。
- Gateway：Route 增加 ServiceID/Protocol/CreatedAt；`Snapshot()` 用于诊断。
- 恢复与诊断：`orbsleepctl recover [--transaction ID]`、`doctor --verify`、`--force` 卸载前导出 tar.gz 恢复包。
- 故障注入：`internal/failpoint` + 真实 daemon SIGKILL；覆盖 attach/detach 关键阶段。
- 发布 CI：gofmt、vet、race、CLI/ZIP/DMG 签名、staple、SHA、tag/HEAD 一致性校验。

## 2. 测试证据

### 本地单元与竞态

```sh
cd core
test -z "$(gofmt -l .)"
go vet ./...
go test -race -count=1 ./...
```

结果：11 个包全部通过（2026-10-30）。

### OrbStack 阶段 0

```sh
make phase0
```

结果：通过。覆盖真实代理、真实 Compose、接入、原地址访问、停止唤醒、空闲停止、恢复逐字节一致。

### 真实可靠性场景

```sh
make reliability-e2e
```

结果：5/5 通过：

1. TCP + UDP 混合端口接入；UDP 映射和容器发布保持。
2. 托管期间外部修改另一服务环境变量；恢复保留外部修改并加回 TCP。
3. 删除 Compose 文件后恢复；从备份/最后可信快照重建，UDP 保留。
4. Compose chmod 000；恢复被拒绝，文件不被替换，托管记录保留。
5. 容器被删除；恢复重建容器并重新发布原始端口。
6. daemon SIGKILL 后重启；托管服务仍可通过原地址访问。

### 真实 SIGKILL 故障注入

```sh
make fault-injection-e2e
```

结果：9/9 通过：

- attach：`proxy_updated`、`compose_written`、`container_ready`、`gateway_ready`、`before_database_commit`
- detach：`detach_gateway_removed`、`detach_compose_written`、`detach_container_ready`、`detach_before_record_delete`

每个场景均由独立 daemon 进程在指定 hook 触发 SIGKILL，重启后执行恢复，并验证 Compose/端口/托管状态一致。

### 诊断

```sh
orbsleepctl doctor --verify
orbsleepctl recover
```

真实数据目录检查通过；无未完成事务、Compose 快照、网关路由不一致。

## 3. 0.1.6 验收清单状态

| # | 项目 | 状态 | 说明 |
|---|---|---|---|
| 1 | Attach/Detach 统一事务状态机 | 部分 | 已有持久化事务 + 阶段模型；尚未把所有编排完全迁入独立 transaction manager |
| 2 | 所有关键阶段 SIGKILL 恢复 | 部分 | 9 个关键阶段真实 SIGKILL 通过；未覆盖每个错误分支 |
| 3 | SQLite 与备份持久化一致 | 通过 | WAL + synchronous=FULL；终态原子提交测试 |
| 4 | 异常状态不覆盖用户文件 | 通过 | 删除/不可读/外部修改测试 |
| 5 | 恢复操作幂等 | 部分 | 端口添加与恢复测试幂等；全事务重放未穷举 |
| 6 | 共享 Compose 多服务互不影响 | 通过 | 单元 + 真实场景 |
| 7 | TCP/UDP 混合正确 | 通过 | 单元 + 真实场景 |
| 8 | 外部修改安全合并 | 通过 | 真实场景保留其他服务环境变量 |
| 9 | 缺失文件从可信快照重建 | 通过 | 真实场景 |
| 10 | 容器实际端口映射验证 | 通过 | attach/detach/reconcile 均检查 |
| 11 | 符号链接安全保护 | 通过 | 单元测试 |
| 12 | 代理配置修改可验证/回滚 | 部分 | 原有验证逻辑保留；新增场景未独立故障注入 |
| 13 | 网关失败不误报 | 通过 | 忙碌端口/失效路由测试 |
| 14 | 代理 IP 改变自动更新 | 部分 | `Matches`/Ensure 更新目标已覆盖；真实 IP 漂移未独立演练 |
| 15 | 代理 IP 失效清理旧路由 | 通过 | 单元测试 |
| 16 | 无孤立监听/错误转发 | 通过 | reconcile Prune + 路由所有权 |
| 17 | 后台助手不可用时禁止危险卸载 | 通过 | CLI 代码路径；未在真实卸载中演练 |
| 18 | 卸载前全部恢复并验证 | 通过 | 服务数量检查后才卸载 |
| 19 | Reconcile 增量更新 | 通过 | 单次 List + 仅变更 Upsert |
| 20 | 无明显 Goroutine/连接泄漏 | 部分 | race 测试通过；未做长时间 soak |
| 21 | 备份/临时文件安全清理 | 通过 | 原有清理规则保留 |
| 22 | Go Vet/Race 通过 | 通过 | 本地 + CI |
| 23 | 故障注入全部通过 | 部分 | 9 个关键阶段；非所有分支 |
| 24 | OrbStack 真实集成测试 | 通过 | phase0 + 5 场景 + 9 故障注入 |
| 25 | Universal Binary | 通过 | CI app/daemon/CLI 均验证 arm64+x86_64 |
| 26 | 签名/公证/Staple | 通过 | Developer ID、Accepted、staple、spctl Notarized Developer ID |
| 27 | Release 资产/SHA | 通过 | ZIP/DMG/SHA256SUMS 下载后逐字节校验 |

## 4. 未解决问题

1. **事务管理器尚未完全独立**：阶段、锁、快照已抽包，但 attach/detach/recovery 仍由 `engine` 直接编排；未达到方案中 `internal/transaction/manager` 完全接管所有操作的程度。
2. **阶段转换未强制校验**：`transaction.CanTransition` 已实现，但现有 `updateTxn` 没有强制走该状态机，历史阶段字符串也未统一。
3. **未执行 macOS 重启 / OrbStack 重启**：当前会话无法安全重启主机；已执行 daemon SIGKILL 与容器重建。
4. **真实卸载流程未演练**：`--force` 恢复包导出有单元测试，但未在真实环境执行完整卸载。
5. **真实代理 IP 漂移未演练**：路由匹配与失效清理有单测，未在真实代理重建场景验证。
6. **CI 无 OrbStack 集成测试**：GitHub Runner 没有 OrbStack，真实 E2E 为本地执行记录；CI 只做单元/race/构建/签名/公证。
7. **UDP 仍不由 OrbSleep 代理**：本版只保证 UDP 映射不被删除，未实现 UDP 网关。
8. **macOS 重启恢复**：daemon 重启恢复已测，系统重启后的 LaunchAgent/GUI 组合未测。

## 5. CI 记录

- Release：https://github.com/misswell/OrbSleep-Manager/releases/tag/v0.1.7
- CI run：https://github.com/misswell/OrbSleep-Manager/actions/runs/40985620473
- Tag source commit：`f4fbf69`
- gofmt / vet / race：通过
- App/`orbsleepd`/`orbsleepctl`：Developer ID Team `U8U443D7ZL`，安全时间戳，arm64+x86_64
- App/DMG：Apple 公证 Accepted，已 staple；`spctl=Notarized Developer ID`
- 资产：
```text
eb17e72aebddf7985a0eeeec202822875e08dc96f1bc4ff11b172d81cffdac37  OrbSleepManager-0.1.7-macos-universal.zip
3e4e9c45f26da2592bca0e6675e383e6f462e6413401b31373ae8111a6caa024  OrbSleepManager-0.1.7-macos-universal.dmg
```
- CI 对 CloudKit `stapler validate` 临时网络抖动采用 3 次重试；staple 成功与 `spctl` 作为最终判定。
- 前一次 CI run 40984938692 因验证步骤在线 validate 网络中断失败，未发布；修复后 40985620473 成功。

## 6. Transaction manager integration (0.1.8)

- `core/internal/transaction/manager.go` centralizes durable transaction persistence with phase validation.
- `startTxn`, `updateTxn`, `finishTxn/finishTxnAtomic` now route through the manager; illegal transitions rejected by `CanTransition`.
- All existing unit tests, fault injection E2E (9 phases), and real OrbStack reliability scenarios continue to pass after migration.
- Additional test fixtures in recovery_test updated to use valid transition paths.
- Phase model includes 17 states with explicit allowed transitions and terminal states; recovery can jump from any non-terminal to recovery branches.

### Verification (local, 0.1.8 build)

- Go vet/race: passing on macOS with 10+ packages.
- Unit: full engine/engine tests (2.9s).
- Real environments: phase0 (✓), reliability E2E (5 scenarios ✓), SIGKILL fault injection (9 hook levels ✓).
- Docker network cleanup done (`docker network prune -f`) to allow subsequent runs.
