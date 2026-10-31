# OrbSleep Manager

macOS 上的 OrbStack / Docker 容器「按需休眠」管理器。扫描现有 Compose 容器，确认后自动把宿主机端口接管到 macOS 端口网关，再由 [Lazy TCP Proxy](https://github.com/mountain-pass/lazy-tcp-proxy) 负责冷启动与空闲停止。访问地址保持原样，GUI 关闭后后台仍然工作。

```text
客户端 → 127.0.0.1:8080（macOS 端口网关）→ 代理发布端口 → Lazy TCP Proxy → 业务容器:80
```

## 安装（最终用户）

1. 下载 DMG。
2. 把 `OrbSleepManager.app` 拖到「应用程序」。
3. 打开应用。
4. 点击「一键配置并启动」。
5. 如 macOS 提示，在「系统设置 → 通用 → 登录项」中允许后台服务运行。

完成。后台服务安装、开机启动注册、Docker 检测、Lazy TCP Proxy 初始化、数据库创建和环境检查都由应用自动完成；不需要打开终端，不需要复制任何二进制，也不需要执行 `sudo`。需要管理员权限时使用 macOS 原生授权界面。

「设置」里两个开关相互独立：登录后自动启动后台服务、登录后自动打开 OrbSleep Manager。

应用包内已经包含运行所需的一切：

```text
OrbSleepManager.app/Contents/
├── MacOS/OrbSleepManager
├── Resources/
│   ├── orbsleepd            # 后台服务
│   └── orbsleepctl          # 可选 CLI
└── Library/LaunchAgents/
    └── com.misswell.orbsleep.daemon.app.plist
```

后台服务通过 macOS 的 `SMAppService` 注册为当前用户的 LaunchAgent，由 launchd 在登录后自动启动。默认只使用当前用户权限；没有需要 root 的功能，因此不引入 LaunchDaemon。

### 从 0.1.x 手动安装迁移

应用启动时会检测旧版安装（`/usr/local/bin/orbsleepd`、`/usr/local/bin/orbsleepctl`、`~/Library/LaunchAgents/com.misswell.orbsleep.daemon.plist`）。安装向导和「诊断与修复」提供「迁移到应用内后台服务」：先创建备份，确认没有未完成事务后停止旧服务、移除旧启动项、注册包内后台服务并验证，最后清理旧二进制（root 所有的文件通过一次性 macOS 管理员授权删除）。数据目录、托管记录、备份和操作历史全部保留。

### 更新

「更新与维护」页负责应用与后台服务升级：

- 应用更新从 `OSUpdateFeedURL`（Info.plist）或环境变量 `ORBSLEEP_UPDATE_FEED` 读取 JSON：`{"version":"0.2.1","downloadURL":"https://.../OrbSleepManager.zip","sha256":"...","notes":"...","sizeBytes":123456}`。
- 下载后校验 SHA256、解包并验证新应用签名，再替换 App Bundle；旧版本保留在 `~/Library/Application Support/OrbSleep/app-backup/`，新版本 12 秒内未运行会自动回滚。
- 后台服务二进制随应用包更新；执行「升级后台服务」会重新注册并重启服务，应用与后台服务版本保持一致。
- 备份中心支持创建数据库快照并在停止态恢复；恢复时旧数据库保留为 `state.sqlite.pre-restore-<时间>`。

## 组成

| 目录 | 内容 |
|---|---|
| `core/` | Go 后台服务 `orbsleepd`、CLI `orbsleepctl`、扫描 / Compose 修改 / 代理管理 / 端口网关 / SQLite |
| `app/` | SwiftUI GUI（XcodeGen 工程），含安装向导、后台服务管理、诊断、日志与卸载 |
| `app/Support/` | 随包分发的 LaunchAgent plist |
| `docs/PROTOCOL.md` | GUI 与后台服务的 Unix Socket NDJSON 协议 |
| `docs/ARCHITECTURE.md` | 组件、后台服务、事务、回滚与设计取舍 |
| `docs/PHASE0.md` | 阶段 0 可行性验证清单与结论 |
| `scripts/embed-service.sh` | 构建时把 `orbsleepd`/`orbsleepctl` 与 plist 放进应用包 |
| `scripts/phase0.py` | 端到端验收脚本（需要 OrbStack） |
| `scripts/phase1_lifecycle.py` | 生命周期故障注入验收（停止保护 / 备份一致性 / 卸载 fail-closed；隔离数据目录） |

## 开发

```sh
make build-core     # bin/orbsleepd bin/orbsleepctl
make test           # go test ./...
make app            # xcodegen + xcodebuild，产物 app/build/Debug/OrbSleepManager.app
make phase0         # 真实 OrbStack 端到端验收
make phase1         # 生命周期故障注入验收（隔离数据目录）
```

`make app` 的 post-build 阶段会调用 `scripts/embed-service.sh`，自动把后台服务与 LaunchAgent 放进 Debug 应用包，因此打开 Debug 构建也能直接使用「一键配置并启动」。

命令行工具仍可用于调试，但不是用户安装流程的一部分：

```sh
./bin/orbsleepctl doctor
./bin/orbsleepctl scan
./bin/orbsleepctl plan <容器ID或名称> --apply
```

在「设置 → 命令行工具」中可以把应用包内的 `orbsleepctl` 安装为 `~/.local/bin/orbsleepctl`（用户级，无需 sudo）。

## 打包发行

```sh
make release   # 或 ./scripts/package-release.sh
```

产物在 `dist/`：

- `OrbSleepManager.app`：通用（arm64 + x86_64），已把 `orbsleepd`、`orbsleepctl` 和 LaunchAgent plist 嵌入包内。
- `OrbSleepManager-<version>-macos-universal.dmg`：只包含应用、`README.txt` 和 Applications 快捷方式，不再分发单独的 `bin/`。
- `OrbSleepManager-<version>-macos-universal.zip` 与 `SHA256SUMS`。

签名与公证：

- 检测到 `Developer ID Application` 证书时，先用带安全时间戳和 Hardened Runtime 的签名签内嵌的 `orbsleepd`/`orbsleepctl`，再签应用包。
- 公证支持两种凭据：`APP_STORE_CONNECT_KEY_ID` / `_ISSUER_ID` / `_PRIVATE_KEY_PATH`，或本机 `notarytool` keychain profile（默认尝试 `agentspace-notary`，也可用 `NOTARY_PROFILE` 指定）。公证通过后自动 staple app 与 DMG，并用 `spctl` 验证。
- 当前 0.2.0 构建已完成 Apple 公证与 staple：`spctl` 对 app 返回 `accepted / Notarized Developer ID`。
- 没有 Developer ID 证书时只做 ad-hoc 签名，`README.txt` 会明确标记 **UNSIGNED** 并说明如何在系统设置中允许。正式发布不使用这种构建。
- 配置 `UPDATE_BASE_URL` 后，打包结束会调用 `scripts/make-appcast.sh` 生成 `dist/appcast.json`，可直接作为 `OSUpdateFeedURL` 的更新源。

## 后台服务与权限模型

- GUI 负责注册、启动、停止、重启、修复、升级和卸载后台服务。
- 后台服务只以当前用户权限运行，数据在 `~/Library/Application Support/OrbSleep/`。
- GUI 通过本地 Unix Socket（`daemon.sock`，权限 0600）与后台服务通信。
- 打开应用后自动执行环境检查，只在发现问题时提示；用户不需要运行 `doctor`。
- 升级随应用一起完成：新版本的后台服务就在新的应用包内，重启服务即加载新版本。
- 卸载会先恢复所有托管服务的原始端口映射，再移除后台服务、开机启动项和可选的 CLI，最后把应用移到废纸篓；数据目录保留。

## 安全边界

- 只读取当前 Docker Context，**不会**切换你的全局 Context。
- 扫描阶段只读；接入前必须生成计划并确认。
- 修改 Compose 前自动备份到 `~/Library/Application Support/OrbSleep/backups/`。
- 只移除被接管的 TCP 端口映射，保留注释、其它端口、UDP、其它服务。
- 修改后再次用 `docker compose config` 对比有效配置，只允许端口发生变化。
- 失败时自动回滚 Compose、代理配置和端口监听；回滚不完整时保留全部备份并输出人工恢复步骤。
- 不执行 `docker system prune`，不删除数据卷，不删除业务镜像。
- 低端口（<1024）默认排除；`host` 网络、`--rm`、非 Compose 容器默认排除。

## 验证状态

已在 OrbStack 2.2.5 / Docker 29.8.2 / macOS 27.2 上完成真实端到端验证：

- 扫描 → 计划 → 接入 → Compose 端口移除 → 容器重建 → 原地址 HTTP 200
- 停止容器后请求自动唤醒；空闲 30 秒后自动停止
- 共享 Compose 文件中的多个服务可同时接入，单独恢复互不影响
- 单容器多端口、已停止容器接入、容器删除后重建恢复
- 守护进程重启后立即恢复监听；代理容器重启后服务不中断
- 进程被强制终止时，未完成的接入/恢复事务会在下次启动自动回滚或补记（Compose、容器、代理目标、原端口、操作记录）
- 应用包内嵌后台服务、LaunchAgent 注册与卸载（SMAppService / 开发构建回退）
- 强制 `kill -9` 后台进程后 launchd 自动重启；`system.shutdown` 优雅停止且不重启
- 备份创建 → 修改设置 → 恢复 → 重启后数据回滚
- 0.2.0 复跑 `scripts/phase0.py`：代理启动 → 接入 → 端口移除后仍 200 → 唤醒 → 空闲停止 → Compose 逐字节恢复
- 公证与 staple：app / DMG 均 Accepted，`spctl` 返回 `accepted / Notarized Developer ID`
- Go 单元测试、真实 Linux TCP 网关测试、GUI 编译与真实守护进程协议直连

详细记录见 [docs/VERIFICATION.md](docs/VERIFICATION.md)。复跑：

```sh
python3 scripts/phase0.py
```

> 实测发现 macOS 无法直连 OrbStack 容器 IP（`no route to host`），因此默认采用「代理端口发布到 127.0.0.1」方案；`container_ip` 模式保留为实验选项。

## 状态

阶段 0–4 的主体实现已落地并验证；阶段 5 的直接分发签名与公证已完成（Developer ID），0.2.0 起安装、后台服务管理、诊断、日志、升级与卸载全部在 GUI 中完成；特权端口助手与阶段 6（UDP/IPv6、多文件 Compose）尚未实现。
