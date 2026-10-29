# OrbSleep Manager

macOS 上的 OrbStack / Docker 容器「按需休眠」管理器。扫描现有 Compose 容器，确认后自动把宿主机端口接管到 macOS 端口网关，再由 [Lazy TCP Proxy](https://github.com/mountain-pass/lazy-tcp-proxy) 负责冷启动与空闲停止。访问地址保持原样，GUI 关闭后后台仍然工作。

```text
客户端 → 127.0.0.1:8080（macOS 端口网关）→ 代理发布端口 → Lazy TCP Proxy → 业务容器:80
```

## 组成

| 目录 | 内容 |
|---|---|
| `core/` | Go 后台助手 `orbsleepd`、CLI `orbsleepctl`、扫描 / Compose 修改 / 代理管理 / 端口网关 / SQLite |
| `app/` | SwiftUI GUI（XcodeGen 工程） |
| `docs/PROTOCOL.md` | GUI 与守护进程的 Unix Socket NDJSON 协议 |
| `docs/ARCHITECTURE.md` | 组件、事务、回滚与设计取舍 |
| `docs/PHASE0.md` | 阶段 0 可行性验证清单与结论 |
| `scripts/phase0.py` | 端到端验收脚本（需要 OrbStack） |

## 快速开始

```sh
# 1. 构建后台与 CLI
make build-core

# 2. 安装开机自启（LaunchAgent），并等待守护进程就绪
./bin/orbsleepctl install-agent

# 3. 检查环境（OrbStack / Docker / 代理 / 端口）
./bin/orbsleepctl doctor

# 4. 安装并启动 Lazy TCP Proxy（首次需要拉镜像）
./bin/orbsleepctl proxy-setup

# 5. 扫描容器
./bin/orbsleepctl scan

# 6. 生成计划 → 预览 diff → 执行
./bin/orbsleepctl plan <容器ID或名称> --apply
```

GUI：

```sh
make app          # xcodegen + xcodebuild
open app/build/Debug/OrbSleepManager.app
```

## 打包发行

```sh
make release   # 或 ./scripts/package-release.sh
```

产物在 `dist/`：通用（arm64 + x86_64）的 `OrbSleepManager.app`、`orbsleepd`、`orbsleepctl`，以及 DMG、ZIP 和 `SHA256SUMS`。

- 检测到 `Developer ID Application` 证书时会带安全时间戳签名；配置 `APP_STORE_CONNECT_KEY_ID` / `_ISSUER_ID` / `_PRIVATE_KEY_PATH` 后会自动公证并 staple。
- 没有 Developer ID 证书时只做 ad-hoc 签名，`README.txt` 和产物会明确标记 **UNSIGNED**，首次启动需右键打开。
- 未公证的构建不应作为稳定版公开发布。

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
- LaunchAgent 安装/卸载
- Go 单元测试、真实 Linux TCP 网关测试、GUI 编译与真实守护进程协议直连

详细记录见 [docs/VERIFICATION.md](docs/VERIFICATION.md)。复跑：

```sh
python3 scripts/phase0.py
```

> 实测发现 macOS 无法直连 OrbStack 容器 IP（`no route to host`），因此默认采用「代理端口发布到 127.0.0.1」方案；`container_ip` 模式保留为实验选项。

## 状态

阶段 0–4 的主体实现已落地并验证；阶段 5（特权端口助手、签名公证、自动升级）与阶段 6（UDP/IPv6、多文件 Compose）尚未实现，界面与文档中已明确标注。
