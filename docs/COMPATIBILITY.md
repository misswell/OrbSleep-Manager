# OrbSleep 兼容性矩阵（0.3.0）

> 诚实边界：只有下表标注 **已实测** 的组合才宣称支持。其余组合要么"未实测"（代码路径一致但缺少真实验证环境），要么"不支持"（结构上无法工作，不会假装可用）。
> 三种兼容性独立评估：**容器运行时兼容**（引擎）、**容器创建方式兼容**（Compose / docker run / GUI / API）、**管理工具兼容**（Portainer / Dockge 等面板只是普通容器，无需单独适配）。

## 一、运行时支持矩阵

| 运行时 | 识别/发现 | 扫描 | 接入（休眠） | 端口接管 | 状态 |
|---|---|---|---|---|---|
| OrbStack 2.2.5 | ✅ 自动 | ✅ | ✅ Compose + docker run | ✅ | **已实测**（phase0/1/2/3 全流程） |
| Colima 0.10.x | ✅ 自动（`~/.colima/*/docker.sock`） | ✅ | ✅ Compose + docker run | ✅ | **已实测**（phase3 双环境并行 + 离线韧性）。注意：Colima 只为"逐端口"发布建立宿主机转发（范围表达式不转发），该环境的代理以逐端口方式发布；容器端口移除后其转发器异步释放，网关绑定带重试窗口 |
| Docker Desktop | ✅ 自动（`~/.docker/run/docker.sock`） | ✅* | ✅*（与 OrbStack 同为 Moby，路径一致） | ✅* | 未实测（无验证环境；与本机 OrbStack 共享同一 Docker API 代码路径） |
| Docker Engine（Linux 自装） | ✅ 自动（`/var/run/docker.sock`） | ✅* | ✅* | ✅* | 未实测（同上） |
| Rancher Desktop（dockerd 模式） | ✅ 自动（`~/.rd/docker.sock`） | ✅* | ✅* | ✅* | 未实测 |
| Finch / Lima | ✅ 自动 | ✅* | ✅* | ✅* | 未实测 |
| Docker Context（多上下文） | ✅ 自动枚举 `~/.docker/contexts` | ✅ | ✅（unix 端点） | ✅ | 已实测（OrbStack/Colima 即经 socket 接入；上下文枚举在环境页展示） |
| Podman（兼容 socket） | ✅ 自动识别（engine 标识 + socket 路径） | ✅（识别+展示） | ❌ 有意禁用 | ❌ | **不支持接入**——兼容 API 未实测，禁止假装可用；环境页明确标注 |
| Podman 原生 API / containerd / CRI-O / nerdctl | ❌ | ❌ | ❌ | ❌ | **不支持**。不把它们伪装成 Docker；需要独立适配器（后续版本） |
| Kubernetes / K3s / MicroK8s | ❌ | ❌ | ❌ | ❌ | **不支持**。工作负载模型（Pod/Deployment）与单容器休眠语义不匹配（后续版本） |
| 远程 Docker（SSH Context / tcp://） | ✅ 自动识别为 `remote` | ✅（识别+展示） | ❌ 有意禁用 | ❌ 结构性不支持 | 远程机器发布的端口本机网关无法接管。需要远程 Host Agent（后续版本），当前环境页诚实标注 |

\* = 与已实测运行时共享同一 Docker Engine API 代码路径，但未在该运行时上跑过完整验收，按"未实测"对待。

## 二、容器创建方式支持矩阵

| 创建方式 | 识别 | 接入策略 | 恢复保证 | 状态 |
|---|---|---|---|---|
| Docker Compose | ✅ Compose 标签 | `compose`：Compose 文件定点改写 + `up --force-recreate` | 逐字节还原原文件；共享文件只加回本服务端口 | **已实测**（phase0/1） |
| `docker run` CLI | ✅ 无 Compose 标签 | `docrun`：完整 inspect 快照 → Engine API 重建 | 原容器保留为重命名锚点，验证通过后才删除；按快照原样还原 | **已实测**（phase2） |
| Docker Desktop / OrbStack GUI 创建 | ✅ 同 docker run | `docrun` | 同上 | 与 docker run 同路径（GUI 创建即 API 创建），未单独实测 |
| Portainer / Dockge / Komodo / Coolify / CasaOS / Yacht 创建 | ✅ 按底层容器识别（有 Compose 标签走 compose，否则 docrun） | 同上两条 | 同上 | 无需单独适配（原则：识别底层运行时，不为面板重复实现）；未逐一实测 |
| Docker Engine API 直接创建 | ✅ | `docrun` | 同上 | **已实测**（phase2 即 API 创建路径） |

## 三、docrun 快照保留字段清单（无损重建契约）

镜像/Entrypoint/Cmd/Env/WorkingDir/User、Labels（OrbSleep 管理标签除外）、
Healthcheck、ExposedPorts、StopSignal/StopTimeout、Tty/OpenStdin/StdinOnce、
Binds/HostConfig.Mounts/Tmpfs/匿名卷（按原卷名显式重挂载）、NetworkMode、
NetworkingConfig（主网络别名 + 静态 IP，附属网络重建后重连）、PortBindings、
RestartPolicy、Privileged/CapAdd/CapDrop/SecurityOpt/Devices/DeviceCgroupRules、
DNS/ExtraHosts/GroupAdd/Links、LogConfig、Ulimits/Sysctls、
Memory/CPU/Blkio/PidsLimit 等资源限制、Init/ShmSize/OomScoreAdj/CgroupnsMode。

**重建序列**（任何一步失败都会回退）：持久化快照 → 停止原容器 → 重命名为
`<name>.orbsleep-bak-<id12>`（回滚锚点）→ 按快照创建替换容器（移除被接管的
TCP 端口）→ 启动 → 重连附属网络 → 验证 → 网关/代理 → 通过原端口连通测试 →
删除锚点。恢复时反向执行；外部修改过托管容器（指纹不符）时拒绝自动恢复。

## 四、已验证的故障注入

| 场景 | 期望 | 验证 |
|---|---|---|
| Compose 接入/恢复（phase0/1 全套：停止保护、备份一致性、卸载 fail-closed） | 事务收口、原文件逐字节还原 | ✅ |
| docrun 接入后"重命名锚点"阶段崩溃 | 启动恢复：删除替换容器、还原原容器、事务 rolled_back | ✅ phase2 |
| docrun 接入后"替换容器已创建"阶段崩溃 | 同上（按管理标签定位替换容器） | ✅ phase2 |
| Colima 离线 | 环境 A 服务不受影响；B 服务标记不可达、监听保留；恢复在线后自动回归 | ✅ phase3 |
| 双环境并行休眠/唤醒 | 各走各的代理，互不干扰 | ✅ phase3 |
| 双环境代理端口段 | 每环境独立端口段（20000-20099 / 20100-20199）、代理端口无交集 | ✅ phase3 |

## 四·一、性能对比（scripts/perf_probe.py，OrbStack，300 请求）

| 路径 | p50 | p95 | 均值 |
|---|---|---|---|
| 直连容器 | 0.83 ms | 2.96 ms | 1.07 ms |
| 接入后（网关 → 代理 → 容器） | 2.31 ms | 6.0 ms | 2.91 ms |

接入链路增加约 1.5 ms（本机两跳 userspace 转发），p95 约 +3 ms；对按需休眠场景可忽略。

## 五、不支持清单（诚实声明）

1. **UDP 端口转发**：UDP 映射原样保留在容器上（不由代理管理），仅 TCP 接管。
2. **<1024 特权端口**：需要特权助手（未实现），分类器直接排除。
3. **IPv6 绑定**：网关仅支持 IPv4。
4. **host / container:/none 网络、--rm 容器、Compose oneoff**：结构性排除。
5. **Podman 接入、远程 Docker 接管、containerd/CRI-O/Kubernetes**：见运行时矩阵。
6. **`docker system prune` / 删除数据卷**：OrbSleep 永不执行。

## 六、升级兼容（0.2.x → 0.3.0）

- SQLite 自动迁移：新增 `environments` 表、`services.environment_id/strategy` 列（幂等）。
- 既有托管服务回填为 `strategy=compose`、钉在原 socket 对应的默认环境；代理容器名/项目/端口范围不变，GUI/CLI 协议纯增量（帧版本仍为 1）。
- 老版本 GUI 与新 daemon 并存可用（新字段可选）。
