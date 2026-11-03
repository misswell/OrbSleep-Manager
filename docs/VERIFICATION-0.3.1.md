# OrbSleep Manager 0.3.1 发版验证记录

- 提交：`625493e`（main，v0.3.1 tag 指向该提交）
- 性质：0.3.0 之后的**安全加固补丁**（Code Review P0/P1/P2 修复），无新功能
- 工具链：Go 1.27、Xcode 27.1、macOS 27.2 (arm64)、OrbStack 2.2.5、Colima 0.10.3

## 变更内容（相对 0.3.0，`840209f..625493e`）

1. `65f76be` fail-closed 状态检查与按环境限定扫描身份
   - `validateContainerIPDialMode`：托管服务读取失败即拒绝切换 `container_ip`
   - `UpdateSettings` 端口变更：代理安装状态未知即拒绝（不再当作未安装）
   - 扫描：按名关联限定本环境；`scanAll` 全局 16 并发上限
2. `8b36810` docrun 恢复/回滚安全加固
   - `RestoreOriginal`：先核对 parked 容器 ID + 服务标签；无标签外部同名容器 → 中止而非接管；非 404 Inspect 错误 → 中止
   - 回滚一律按记录 ID 删除容器（名称冲突不可能误删外部容器）
   - 回滚锚点移到 `committed_item` 持久化之后删除
   - recovery 全部 Inspect 严格区分 404 与故障；不可读状态保留事务等待重试
   - `VerifyManaged` 的已验证 ID 贯穿到 `RestoreOriginal`（消除 verify/act 竞态）
   - `enableEnvironment`：DB 读取 fail-closed；enable/disable/remove 串行化到写锁
   - 扫描 `managed` 计数按环境；`ListEnvironments` 并发探测（上限 8）

## 门禁结果（最终 SHA 上重跑，全部通过）

| 检查 | 入口 | 结果 |
|---|---|---|
| Go 全量单测（-count=1，12 包） | `cd core && go test ./...` | ✅ |
| 阶段 0：OrbStack Compose 端到端 | `make phase0` | ✅ |
| 阶段 1：生命周期故障注入 | `make phase1` | ✅ |
| 阶段 2：docker run 快照重建 + 两处崩溃注入（覆盖本次修改的恢复/回滚链路） | `make phase2` | ✅ |
| 阶段 3：OrbStack + Colima 双环境（覆盖环境启停/端口分配/扫描） | `make phase3` | ✅ |
| App 构建（Release 配置） | xcodebuild | ✅ BUILD SUCCEEDED，0.3.1 |

## 新增回归测试

- `TestRestoreOriginalProtectsExternalContainer`：同名外部容器在恢复期间出现时，恢复必须中止且外部容器不被 stop/rename/remove
- `TestRestoreOriginalRollbackRemovesByID`：半成品恢复容器按记录 ID 回滚删除，托管容器按 ID 还原并重启

## 升级与兼容

- 数据库 schema 与 0.3.0 完全一致，无迁移；0.3.0 → 0.3.1 就地替换二进制即可
- 协议无变化（纯内部行为加固）
