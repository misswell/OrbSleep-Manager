# OrbSleep Manager 0.3.0 发版验证记录

- 提交：`0617e93`（main，v0.3.0 tag 指向该提交）
- 工具链：Go 1.27、Xcode 27.1、macOS 27.2 (arm64)、OrbStack 2.2.5、Colima 0.10.3
- 构建配置：`app/project.yml` MARKETING_VERSION 0.3.0；签名/公证由 CI（release.yml）在 macos runner 完成

## 门禁结果（全部通过）

| 检查 | 入口 | 结果 |
|---|---|---|
| Go 全量单测（12 包，含 docrun 金样/重建回滚/环境代理段） | `cd core && go test ./...` | ✅ |
| gofmt / go vet | CI 与本地 | ✅ |
| 更新回滚故障注入 | `make swift-check` | ✅ |
| App 构建（XcodeGen + xcodebuild） | `make app` | ✅ BUILD SUCCEEDED，包版本 0.3.0 |
| 阶段 0：OrbStack Compose 端到端 | `make phase0`（正式数据目录） | ✅ |
| 阶段 1：生命周期故障注入（停止保护/备份一致性/卸载 fail-closed） | `make phase1`（隔离数据目录） | ✅ |
| 阶段 2：非 Compose（docker run）快照重建端到端 + 两处崩溃注入 | `make phase2`（隔离数据目录） | ✅ |
| 阶段 3：OrbStack + Colima 双环境并行 + 离线韧性 | `make phase3`（隔离数据目录，需 colima start） | ✅ |

四阶段均在版本号定为 0.3.0 的最终源码上重跑（bin/orbsleepd、bin/orbsleepctl、Release app 均为 0.3.0）。

## 升级与用户数据（规则第 1 条）

- SQLite 迁移幂等：`environments` 表 + `services.environment_id/strategy` 列，重复启动不重复迁移（`PRAGMA user_version=1`）。
- 旧记录回填：无 `environment_id` 的服务映射到其钉住 socket 对应的默认环境；`strategy` 缺省视为 compose。
- 代理容器名 / Compose 项目 / 全局端口段对默认环境保持不变；正式数据目录实测：升级后注册 OrbStack 为默认环境、0 条服务、代理无需重建。

## 中断与并发（规则第 2 条）

- docrun 接入在"重命名锚点后"与"替换容器已创建后"两个崩溃点注入 SIGKILL，启动恢复自动还原原容器、事务收口（phase2 实测）。
- compose 路径既有故障注入（phase1）继续通过：停止保护、备份一致性、卸载 fail-closed。
- 管理接口瞬态错误（VM 转发器抖动）自动重试；网关绑定对"地址被滞后占用的转发器"有 20s 重试窗口（phase3 实测）。

## 已知行为与未覆盖项（诚实记录）

- Colima 仅对逐端口发布建立宿主机转发 → 该环境代理以逐端口方式发布（功能正常，见 COMPATIBILITY.md）。
- Docker Desktop / Rancher / Lima / Finch / Podman / 远程 Docker：未在本机实测或结构性不支持，矩阵如实标注；不宣称已验证。
- 真实旧版（0.2.9 GUI）→ 0.3.0 的就地升级未在 GUI 层人工操作过：协议为纯增量、数据迁移有自动化覆盖；缺口记录为"GUI 就地升级人工验收未做"。
- 性能：直连 p50 0.83ms → 接入链路 p50 2.31ms（`scripts/perf_probe.py`，300 请求）。
