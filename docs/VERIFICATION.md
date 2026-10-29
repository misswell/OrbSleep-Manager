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

### 1. 容器 IP 直连不可用 → 默认使用「代理端口发布到 127.0.0.1」

在 OrbStack 2.2.5 中，从 macOS 直接连接代理容器 IP `192.168.148.4:20000` 返回
`connect: no route to host`。因此：

- 默认 `proxyDialMode=published`：代理容器发布 `127.0.0.1:20000-20099`，网关转发到发布端口。
- `container_ip` 模式保留但标记为实验性；实测在该环境下不可用。
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

## 未验证 / 未实现

- 特权端口助手（<1024）、UDP、IPv6、多 Compose 文件自动修改、`extends`/锚点。
- 独立 `docker run` 容器的自动迁移。
- 应用签名、公证、自动更新、菜单栏常驻。
- GUI 未做屏幕截图验证（当前会话无屏幕录制权限）；已通过编译、真实协议往返与应用运行/连接验证。
