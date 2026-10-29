# 阶段 0：技术可行性验证

阶段 0 是项目的技术门槛。在 OrbStack 环境就绪后，按本清单验证；全部通过再继续做 GUI 深度联调。

开发环境说明：开发初期机器未安装 OrbStack；安装后已在 OrbStack 2.2.5 / Docker 29.8.2 / macOS 27.2 上完成全部验证。结果摘要见 [VERIFICATION.md](VERIFICATION.md)。

## 验证项

| # | 验证内容 | 方式 | 状态 |
|---|---|---|---|
| 1 | OrbStack 中运行 Lazy TCP Proxy | `orbsleepctl proxy-setup` | ✅ |
| 2 | macOS 可访问代理监听端口 | `orbsleepctl doctor` 的「代理监听端口可访问」 | ✅ |
| 3 | 集中式 YAML 管理无标签容器 | 代理配置由 `PUT /config/update` 写入并回读 | ✅ |
| 4 | 代理能启动 / 停止业务容器 | phase0 脚本的「停止后唤醒」「空闲停止」 | ✅ |
| 5 | macOS 原生 TCP 网关可转发到代理 | `internal/gateway` 单测（本机真实 TCP） | ✅ |
| 6 | 修改 Compose 后可安全释放/接管端口 | Compose 定点修改单测 + phase0 端到端 | ✅ |
| 7 | macOS 直连代理容器 IP | `proxyDialMode=container_ip` | ⚠️ 不稳定（曾出现 `no route to host`）；默认用发布端口，切换前逐端口探测 |

## 运行

```sh
# 需要 OrbStack 已启动，docker CLI 可用（OrbStack 自带）
make build-core
python3 scripts/phase0.py
```

脚本行为：

1. 复用正在运行的 `orbsleepd`；如果没在运行，用**正式数据目录**启动一个临时守护进程（结束后退出，不删除数据）。这样代理容器只有一份，不会被测试重建。
2. 若代理未安装，执行 `proxy-setup`（首次会拉取 `mountainpass/lazy-tcp-proxy`）。
3. 检查 `127.0.0.1:<代理端口范围起点>` 可连接。
4. 创建临时 Compose 项目：`nginx:alpine`，`127.0.0.1:<随机端口>:80`。
5. 扫描 → 生成计划 → 接入；校验 Compose 中端口已移除、原地址 HTTP 200。
6. `docker stop` 目标容器，再次访问原地址，验证代理自动唤醒。
7. 将空闲超时改为 30 秒，等待容器自动停止。
8. `restore` 恢复原始 Compose 与端口，校验原地址直连容器可用。
9. `docker compose down` 清理测试容器；失败时保留测试目录并打印守护进程日志位置。

## 预期结论与已知风险

- **容器 IP 访问不是必需**：实现默认让代理把监听端口范围发布到 `127.0.0.1`，网关转发到发布端口。若想验证「直连容器 IP」，把设置改为 `proxyDialMode=container_ip` 后重复 phase0。
- **端口范围发布**：代理容器一次性发布 `20000-20099`。OrbStack 需要支持大范围端口映射；若失败，代理容器会启动失败，`proxy-setup` 日志会显示原因，可在设置中缩小范围或换一段空闲范围。
- **Docker Socket 挂载**：代理容器通过 `/var/run/docker.sock` 操作容器。若 OrbStack 不允许容器内访问该 socket，代理无法启动/停止容器；此时 phase0 的唤醒步骤会失败，需要改用 OrbStack 支持的 socket 路径或代理部署方式。
- **首次拉镜像**：`proxy-setup` 可能耗时数分钟，属于正常现象。
- **中断**：重建容器会有 1–5 秒不可用；脚本会明确记录。

## 失败时保留的证据

- 临时数据目录：SQLite、`logs/daemon.log`、`backups/<opID>/`、代理配置。
- `docker logs orbsleep-lazy-tcp-proxy`
- 代理容器 Inspect（端口映射、IP、挂载）
