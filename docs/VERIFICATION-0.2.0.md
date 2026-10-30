# OrbSleep Manager 0.2.0 验收记录

环境：macOS 27.2 / Xcode 26.6 / Go 1.27.3 / OrbStack 2.2.5 / Docker 29.8.2。应用与后台版本 `0.2.0`。

## 自动化与构建

| 项目 | 结果 |
|---|---|
| `go vet ./...` | 通过 |
| `go test ./...` | 通过 |
| `xcodegen generate` + Debug `xcodebuild` | 通过 |
| Release universal 构建 + 内嵌二进制/plist | 通过 |
| `codesign --verify --deep --strict` | 通过（Developer ID，Hardened Runtime） |
| 主程序与 `orbsleepd`/`orbsleepctl` 架构 | `x86_64 arm64` |
| DMG 内容 | `Applications`、`OrbSleepManager.app`、`README.txt` |
| 应用启动/退出 | 通过 |
| 真实 OrbStack 端到端（`scripts/phase0.py`） | 通过：代理启动 → 扫描/计划/接入 → 端口移除后仍 200 → 停止后自动唤醒 → 空闲自动停止 → Compose 逐字节恢复 |
| 公证 + staple（`agentspace-notary` profile） | app 与 DMG 均 Accepted，`stapler validate` 通过 |
| Gatekeeper `spctl` | app = `accepted / Notarized Developer ID` |

## 后台服务与恢复

| 场景 | 结果 |
|---|---|
| 包内 LaunchAgent（`com.misswell.orbsleep.daemon.app`）注册/加载 | 通过（`orbsleepctl install-agent` 路径） |
| 强制 `kill -9` 后台进程 | launchd 在 4 秒内重启，托管恢复依赖下次启动 reconcile |
| `system.shutdown` 优雅停止 | 进程退出码 0，launchd 不重启 |
| `launchctl kickstart` 启动 | 通过 |
| 备份创建 → 修改设置 → 恢复 → 重启 | 设置从 999 恢复为 1234；旧库保留为 `state.sqlite.pre-restore-*` |
| 旧版 LaunchAgent / `/usr/local/bin` 检测 | `system.health.legacyLaunchAgent` 正确；二进制检测通过 |
| 新 API（health / prepareShutdown / prepareUpdate / recovery / docker.contexts / backup / logs.export） | 实测通过 |

## 34 项验收状态

### 1. 首次安装

| 项 | 状态 |
|---|---|
| DMG 拖入 Applications | ✅ |
| 自动进入初始化向导 | ✅ |
| 检测 macOS 与签名 | 🟡（安装位置/绑定签名已检查，系统版本与签名细节未单独展示） |
| 检测 OrbStack / Docker Desktop | ✅ |
| 检测 Docker Socket 与 CLI | ✅（GUI 可选择 Context，CLI 自动查找） |
| 自动创建数据目录与数据库 | ✅ |
| GUI 注册并启动后台服务 | ✅（SMAppService 代码路径；真实点击验收待做） |
| 首次授权经系统设置 | 🟡（会打开登录项设置；拒绝授权场景待实测） |
| 自动初始化 Lazy TCP Proxy | ✅（初始化流程已加入 `proxy.setup`） |
| 自动 Doctor | ✅ |

### 2. 日常管理

| 项 | 状态 |
|---|---|
| GUI 启动/停止/重启 | ✅ |
| GUI 控制开机自动启动 | ✅ |
| 关闭 GUI 不影响后台 | ✅（launchd agent） |
| 后台异常退出自动恢复 | ✅（kill -9 实测） |
| Docker 未启动时引导 | ✅（打开 OrbStack/Docker Desktop 按钮） |
| 扫描/接入/恢复 GUI 化 | ✅（原有流程保留） |
| 操作进度与错误可视化 | ✅ |
| 日志/诊断/备份 GUI | 🟡（后台日志与备份已接入；Proxy/Gateway 日志未分源） |

### 3. 迁移与升级

| 项 | 状态 |
|---|---|
| 兼容旧版 `/usr/local/bin` | 🟡（检测与清理已实现，真实 0.1.6 迁移待实测） |
| 兼容旧版 LaunchAgent | ✅（检测、bootout、移除 plist） |
| 保留数据库与托管服务 | ✅（同一数据目录；恢复事务检查） |
| 迁移失败可恢复旧版本 | 🟡（新服务验证前不清理旧二进制；失败时旧应用仍在） |
| 应用与后台统一升级 | ✅ |
| 升级前检查未完成事务 | ✅（`system.prepareUpdate`） |
| 升级后健康检查 | ✅ |
| 更新失败回滚 | 🟡（relaunch 脚本 12 秒回滚已实现；真实更新验收待做） |

### 4. 卸载与安全

| 项 | 状态 |
|---|---|
| 卸载前恢复所有托管服务 | ✅ |
| 恢复失败禁止继续卸载 | ✅（仍有服务时中止） |
| 只清理 OrbSleep 资源 | ✅ |
| 不删除业务数据卷/镜像 | ✅ |
| 保留数据 / 完全卸载 | ✅ |
| 无终端命令 | ✅ |
| 无不必要的 root 常驻服务 | ✅ |
| 签名、公证、Staple | ✅ 本机 `agentspace-notary` profile 已公证 app 与 DMG、staple 成功；`spctl` app = `accepted / Notarized Developer ID` |

## 尚需真实环境验收

下面场景需要一台可重启/可拒绝授权的真实 Mac，普通自动化无法覆盖；其余自动化验证结果见上。

- 全新 Mac 首次安装（SMAppService 授权点击路径）。
- 真实 0.1.6 手动安装 → 0.2.0 迁移。
- 拒绝后台服务授权、之后重新允许。
- OrbStack 关闭后重启、macOS 重启。
- 更新过程中异常退出 / 新版本启动失败回滚。
- Docker 不可用时安装。
- 卸载时存在未恢复服务；卸载过程中强制结束应用。

## 0.2.0 增量（本轮）

| 项目 | 结果 |
|---|---|
| 日志中心按来源切换（后台服务 / 代理容器） | ✅ `logs.sources` + `logs.tail.source`，代理源直接读取容器日志 |
| 诊断中心按类别分组（Docker / 代理 / 系统） | ✅ |
| 设置页分组：运行环境 / 休眠 / 端口 / 后台服务 / CLI / 扫描 / 数据 / 路径 / 自检 / 更新 / 关于 | ✅ |
| 设置页「关于」展示签名与公证状态（codesign + spctl） | ✅ |
| 应用图标 AppIcon.icns 集成 | ✅ |
| 应用更新中心（检查/下载/校验/替换/12 秒回滚） | ✅（真实更新源与公证待 CI） |
| 备份恢复（停止态安全替换 + pre-restore 备份） | ✅（实测设置回滚 1234） |
| 强制 kill -9 后台进程 | ✅ launchd 4 秒内重启 |
| 优雅停止 / kickstart 启动 | ✅ |

## 真实 OrbStack 端到端（0.2.0 复跑）

`python3 scripts/phase0.py --data-dir <临时目录>` 输出：

- `proxy-setup` 启动 Lazy TCP Proxy，管理接口可达。
- 测试项目扫描为 `eligible`，生成计划并执行接入。
- Compose 的 `ports` 整体移除后，`127.0.0.1:63252` 仍返回 HTTP 200。
- 手动停止容器后，下一次请求自动唤醒。
- 空闲 30 秒后容器自动停止。
- 恢复后 Compose 逐字节还原，原端口重新由容器发布。
- 结果：`✅ 阶段 0 全部验证通过`；测试项目、代理容器、临时数据目录均已清理。
