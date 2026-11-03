# OrbSleep Manager 0.3.2 发版验证记录

- 提交：见 tag `v0.3.2`（main）
- 性质：0.3.1 之后的安全加固补丁（两轮 Code Review 的 P0/P1/P2 修复 + Runtime 接口接入），无新功能
- 工具链：Go 1.27、Xcode 27.1、macOS 27.2 (arm64)、OrbStack 2.2.5、Colima 0.10.3

## 变更内容（相对 0.3.1，`407794c..HEAD`）

1. `227d992` strict identity and error triage across docrun cleanup/recovery
   - `RemoveManagedContainer` 严格身份（ServiceID 非空 + service-id/managed 标签精确匹配）
   - `RestoreOriginal` 重命名失败时组合上报重启错误
   - `isNotFound`/`isContainerNotFound` 只认结构化 404（错误文本嗅探删除）
   - `VerifyManaged`/`LoadSnapshot` 强制服务身份
   - `CreateAndStart` 清理失败携带孤儿容器 ID
   - 扫描 fail-closed（托管服务读取失败拒绝扫描）、managed 统计按环境、
     环境状态持久化失败告警
2. `8dcbd1e` the unified Runtime interface now drives the engine
   - `runtimeForEnv()` 统一入口；docrun 全链路/扫描/reconcile/事件/启停/
     代理检查/计划期检查迁移到 `runtime.Runtime`；Docker 专用垫片删除；
     剩余 raw client 调用者仅为 compose CLI 等 Docker 固有路径
3. 版本号 0.3.2

## 门禁结果（最终 SHA 上重跑）

| 检查 | 入口 | 结果 |
|---|---|---|
| Go 全量单测（-count=1，12 包，含 16 个 docrun 安全/映射回归测试） | `cd core && go test ./...` | ✅ |
| go vet | `go vet ./...` | ✅ |
| App 构建（Release 配置，0.3.2） | `make app` | ✅ BUILD SUCCEEDED |
| 阶段 0：OrbStack Compose 端到端 | `make phase0` | ✅ |
| 阶段 1：生命周期故障注入 | `make phase1` | ✅ |
| 阶段 2：docker run 快照重建 + 两处崩溃注入（覆盖 Runtime 迁移后的全部路径） | `make phase2` | ✅ |
| 阶段 3：OrbStack + Colima 双环境（覆盖环境启停/端口分配/扫描并发） | `make phase3` | ✅ |

## 新增回归测试（相对 0.3.1）

- `TestRemoveManagedContainerIdentityMatrix`：清理入口身份矩阵 6 分支
- `TestRestoreOriginalRenameAndStartFail`：重命名+启动双失败组合上报
- `TestHTTP500WithNoSuchBodyIsNotNotFound`：错误正文嗅探不再放行破坏性分支
- `TestVerifyManagedRequiresLabel` / `TestLoadSnapshotRequiresServiceID`：强制身份
- `TestRestoreOriginalSocketDownKeepsState`：socket 断开零变更
- `TestCreateAndStartSurfacesCleanupFailure`：孤儿容器 ID 上报
- `TestRestoreTwiceIsSafe`：恢复幂等（第二次拒绝无标签容器且零变更）
- `TestRestoreOriginalProtectsExternalContainer` / `TestRestoreOriginalRollbackRemovesByID`（0.3.1 已有，继续通过）

## 升级与兼容

- 数据库 schema 与 0.3.0/0.3.1 完全一致，无迁移；0.3.0/0.3.1 → 0.3.2 就地替换二进制即可
- 协议无变化（纯内部加固）
- 兼容性矩阵不变：OrbStack/Colima 已实测；Docker Desktop/Rancher/Lima/Finch 与 Docker API 兼容（未实测）；Podman 接入、远程接管、containerd/K8s 不支持（如实标注）
