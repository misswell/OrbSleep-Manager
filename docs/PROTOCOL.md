# OrbSleep 本地协议 v1

GUI 与后台助手（`orbsleepd`）通过 **Unix Domain Socket** + **换行分隔 JSON（NDJSON）** 通信。

- Socket 路径：`~/Library/Application Support/OrbSleep/daemon.sock`（权限 0600）
- 每个连接：客户端发送一行 JSON 请求，服务端返回一行 JSON 响应。一行必须是一个完整 JSON 对象，UTF-8，不含裸换行。
- 请求/响应通过 `id` 关联；服务端可以乱序（并发）响应，GUI 必须按 `id` 匹配。
- 所有时间均为 RFC 3339（UTC，带 `Z`）。

## 请求

```json
{"v":1,"id":"6C2F...","method":"scan","params":{}}
```

## 成功响应

```json
{"v":1,"id":"6C2F...","ok":true,"result":{...}}
```

## 失败响应

```json
{"v":1,"id":"6C2F...","ok":false,"error":{"code":"docker_unavailable","message":"无法连接 Docker","details":{}}}
```

`code` 取值：`invalid_request`、`not_found`、`conflict`、`unsupported`、`docker_unavailable`、`proxy_unavailable`、`internal`。

## 方法一览

| method | params | result |
|---|---|---|
| `ping` | `{}` | `{"pong":true,"version":"0.1.0"}` |
| `system.status` | `{}` | `Status` |
| `system.doctor` | `{}` | `{"checks":[DoctorCheck]}` |
| `scan` | `{}` | `ScanResult` |
| `container.inspect` | `{"id":"..."}` | `{"container":Container,"inspect":object}` |
| `plan.create` | `{"containerIds":["..."],"allowLanExposure":false}` | `Plan` |
| `plan.apply` | `{"planId":"..."}` | `{"operationId":"..."}` |
| `operation.get` | `{"id":"..."}` | `Operation` |
| `operations.list` | `{"limit":50}` | `{"operations":[Operation]}` |
| `services.list` | `{}` | `{"services":[Service]}` |
| `service.action` | `{"id":"...","action":"start|stop|restore|remove|updatePolicy","params":{...}}` | `{"operationId":"..."}` 或 `{"ok":true}` |
| `settings.get` | `{}` | `Settings` |
| `settings.update` | `SettingsPatch` | `Settings` |
| `proxy.setup` | `{}` | `{"operationId":"..."}` |
| `proxy.stop` | `{}` | `{"operationId":"..."}` |
| `agent.install` | `{}` | `{"installed":true}` |
| `agent.uninstall` | `{}` | `{"installed":false}` |
| `logs.tail` | `{"limit":200}` | `{"lines":[{"at":"...","level":"info","message":"..."}]}` |

GUI 轮询建议：`system.status` + `services.list` 每 3 秒；`scan` 只在用户点击或设置开启时调用；`operation.get` 在操作进行中每 500ms。

## 数据结构

### Reason

```json
{"code":"network_mode_host","message":"容器使用 host 网络，第一版不支持","severity":"blocker"}
```

`severity`: `info` | `warning` | `blocker`。

### PortBinding

```json
{"hostIP":"127.0.0.1","hostPort":8080,"containerPort":80,"protocol":"tcp"}
```

`hostIP` 为空字符串表示原始绑定为所有网卡。

### Container

```json
{
  "id": "706c8f...",
  "name": "blog-nginx-1",
  "image": "nginx:alpine",
  "state": "running",
  "statusText": "Up 2 hours",
  "createdAt": "2026-10-29T12:00:00Z",
  "startedAt": "2026-10-29T12:00:00Z",
  "composeProject": "blog",
  "composeService": "nginx",
  "composeWorkingDir": "/Users/me/blog",
  "composeConfigFiles": ["/Users/me/blog/docker-compose.yml"],
  "composeContainerNumber": 1,
  "oneoff": false,
  "networkMode": "blog_default",
  "networks": [{"name":"blog_default","ip":"172.20.0.2","gateway":"172.20.0.1"}],
  "restartPolicy": "unless-stopped",
  "autoRemove": false,
  "privileged": false,
  "readonlyRootfs": false,
  "healthcheck": {"configured": true, "status": "healthy", "test": "CMD-SHELL curl -f http://localhost/ || exit 1"},
  "mounts": [{"type":"volume","name":"blog_data","source":"blog_data","destination":"/data","readOnly":false}],
  "ports": [{"hostIP":"127.0.0.1","hostPort":8080,"containerPort":80,"protocol":"tcp"}],
  "managedServiceId": "svc-...",
  "classification": {
    "verdict": "eligible",
    "reasons": [{"code":"static_tcp","message":"可安全迁移的静态 TCP 端口映射","severity":"info"}],
    "warnings": [{"code":"lan_exposure","message":"原始端口绑定所有网卡，接入后仍将保持","severity":"warning"}],
    "proxyCandidates": [{"hostIP":"127.0.0.1","hostPort":8080,"containerPort":80,"protocol":"tcp"}]
  }
}
```

`state` 为 Docker 原始状态：`running`、`exited`、`created`、`paused`、`restarting`、`dead`、`removing`。
`verdict`: `eligible`（可自动接入）、`needs_review`（需人工确认）、`excluded`（排除）。
`managedServiceId` 为空表示未托管。

### ScanResult

```json
{
  "scannedAt": "2026-10-29T12:00:00Z",
  "context": {"name":"orbstack","endpoint":"unix:///Users/me/.orbstack/run/docker.sock","kind":"orbstack"},
  "containers": [Container],
  "counts": {"eligible":2,"needsReview":1,"excluded":10,"managed":3}
}
```

### Plan

```json
{
  "id": "plan-3f9a...",
  "createdAt": "2026-10-29T12:00:00Z",
  "status": "ready",
  "blockers": [],
  "warnings": [{"code":"interruption","message":"重建容器会导致约 1-5 秒中断","severity":"warning"}],
  "items": [{
    "containerId": "706c...",
    "containerName": "blog-nginx-1",
    "composeProject": "blog",
    "composeService": "nginx",
    "composeFile": "/Users/me/blog/docker-compose.yml",
    "removals": [{"hostIP":"127.0.0.1","hostPort":8080,"containerPort":80,"protocol":"tcp"}],
    "proxyMappings": [{"listenPort":20000,"targetPort":80}],
    "bindings": [{"hostIP":"127.0.0.1","hostPort":8080,"containerPort":80,"proxyPort":20000,"protocol":"tcp"}],
    "requiresLanConfirmation": false,
    "risks": [{"code":"recreate","message":"将执行 docker compose up -d --no-deps nginx","severity":"info"}],
    "diff": "--- a/docker-compose.yml\n+++ b/docker-compose.yml\n@@ ...\n",
    "steps": ["备份 Compose 文件", "写入代理配置", "移除端口映射", "校验 Compose", "仅重建 nginx", "启动端口网关", "连通性测试"]
  }],
  "confirmation": {"allowLanExposure": false}
}
```

`status`: `ready` | `blocked`。`blockers` 非空时 GUI 必须禁用执行按钮。

### Operation

```json
{
  "id": "op-8a2b...",
  "kind": "attach|detach|restore|proxy_setup|start|stop",
  "status": "running|succeeded|failed|rolled_back|rollback_failed",
  "startedAt": "2026-10-29T12:00:00Z",
  "finishedAt": "2026-10-29T12:00:05Z",
  "error": "",
  "backupDir": "/Users/me/Library/Application Support/OrbSleep/backups/op-8a2b",
  "affectedServiceIds": ["svc-..."],
  "log": [{"at":"2026-10-29T12:00:01Z","level":"info","message":"已备份 /Users/me/blog/docker-compose.yml"}]
}
```

### Service

```json
{
  "id": "svc-...",
  "containerId": "706c...",
  "containerName": "blog-nginx-1",
  "composeProject": "blog",
  "composeService": "nginx",
  "composeFile": "/Users/me/blog/docker-compose.yml",
  "status": "managed_running",
  "bindings": [{"hostIP":"127.0.0.1","hostPort":8080,"containerPort":80,"proxyPort":20000,"protocol":"tcp"}],
  "idleTimeoutSecs": 1800,
  "startTimeoutSecs": 60,
  "activeConnections": 0,
  "lastActiveAt": "2026-10-29T12:00:00Z",
  "createdAt": "2026-10-29T12:00:00Z",
  "lastError": "",
  "excludedFromSleep": false
}
```

`status`: `managed_running`（运行且有活动连接）、`managed_idle`（运行但空闲）、`managed_stopped`（已被代理停止）、`missing`（容器不存在）、`warning`、`failed`。

### Status

```json
{
  "daemonVersion": "0.1.8",
  "dataDir": "/Users/me/Library/Application Support/OrbSleep",
  "context": {"name":"orbstack","endpoint":"unix:///Users/me/.orbstack/run/docker.sock","kind":"orbstack"},
  "dockerReachable": true,
  "proxy": {
    "installed": true,
    "running": true,
    "adminReachable": true,
    "metricsReachable": true,
    "containerName": "orbsleep-lazy-tcp-proxy",
    "containerIP": "192.168.139.2",
    "publishedRange": {"start":20000,"end":20099},
    "error": ""
  },
  "counts": {"managed":3,"running":1,"idle":1,"stopped":1,"missing":0,"eligible":2,"needsReview":1,"excluded":10},
  "lastScanAt": "2026-10-29T12:00:00Z",
  "lastError": {"at":"2026-10-29T11:00:00Z","message":"proxy config update failed"},
  "launchAgentInstalled": true
}
```

`proxy.installed` 表示代理容器当前存在（`docker compose down` 后为 false）；`running` 表示容器在运行；`adminReachable` 表示管理 API 可从 macOS 访问。

### Settings

```json
{
  "idleTimeoutSecs": 1800,
  "startTimeoutSecs": 60,
  "proxyPortStart": 20000,
  "proxyPortEnd": 20099,
  "adminPort": 47811,
  "webPort": 47810,
  "autoScanOnLaunch": false,
  "enableLowPortHelper": false,
  "logRetentionDays": 14,
  "backupDir": "/Users/me/Library/Application Support/OrbSleep/backups",
  "dataDir": "/Users/me/Library/Application Support/OrbSleep",
  "socketPath": "/Users/me/Library/Application Support/OrbSleep/daemon.sock",
  "proxyDialMode": "published",
  "excludeImagePatterns": ["mysql","mariadb","postgres","redis","mongo","rabbitmq","kafka","elasticsearch"],
  "launchAgentInstalled": true
}
```

`settings.update` 接受上述字段的任意子集（`SettingsPatch`），返回更新后的完整 `Settings`。修改端口范围后需要重启代理容器才会生效（GUI 应提示）。

### DoctorCheck

```json
{"id":"docker","title":"Docker / OrbStack 连接","status":"ok","detail":"orbstack · Docker 28.0.4","remediation":""}
```

`status`: `ok` | `warn` | `fail`。

## 错误与并发

- `scan` 可能耗时数秒，GUI 超时设为 60 秒。
- `plan.create` 会重新读取容器与 Compose 文件，超时 30 秒。
- 所有写操作在后台助手中串行化，返回 `conflict` 表示已有操作在进行。
- `plan` 有效期 10 分钟，过期后 `plan.apply` 返回 `not_found`，需要重新生成。
