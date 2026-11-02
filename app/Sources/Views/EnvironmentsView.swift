import SwiftUI

/// 容器环境页：自动发现 + 手动添加 + 多环境并行管理。
/// 每个环境显示运行时、连接状态、能力与独立代理状态；远程环境诚实标注
/// 端口接管不支持。
struct EnvironmentsView: View {
    @Environment(AppState.self) private var app
    @State private var showAddSheet = false
    @State private var busyEnvId: String?

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                header
                envCards
                capabilityNote
            }
            .padding(20)
        }
        .navigationTitle("容器环境")
        .task {
            await app.loadEnvironments()
        }
        .refreshable {
            await app.loadEnvironments()
        }
        .sheet(isPresented: $showAddSheet) {
            AddEnvironmentSheet { endpoint, name in
                showAddSheet = false
                Task { await app.addEnvironment(endpoint: endpoint, name: name) }
            }
        }
    }

    private var header: some View {
        HStack(alignment: .firstTextBaseline) {
            VStack(alignment: .leading, spacing: 4) {
                Text("容器环境")
                    .font(.title2.bold())
                Text("OrbSleep 自动发现本机的容器运行时；多个环境可以同时启用并分别接入。")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }
            Spacer()
            Button {
                showAddSheet = true
            } label: {
                Label("添加 Socket", systemImage: "plus")
            }
            Button {
                Task { await app.loadEnvironments() }
            } label: {
                Label("重新检测", systemImage: "arrow.clockwise")
            }
        }
    }

    @ViewBuilder
    private var envCards: some View {
        if app.environments.isEmpty {
            Text("未发现任何容器环境；请确认 OrbStack / Docker Desktop / Colima 已启动，或手动添加 Socket。")
                .font(.callout)
                .foregroundStyle(.secondary)
                .padding(.vertical, 24)
        }
        ForEach(app.environments) { env in
            EnvironmentCard(
                env: env,
                isPrimary: env.id == app.primaryEnvironmentId,
                busy: busyEnvId == env.id,
                onEnable: {
                    busyEnvId = env.id
                    Task {
                        await app.environmentAction(env.id, "enable")
                        busyEnvId = nil
                    }
                },
                onDisable: {
                    busyEnvId = env.id
                    Task {
                        await app.environmentAction(env.id, "disable")
                        busyEnvId = nil
                    }
                },
                onRemove: {
                    busyEnvId = env.id
                    Task {
                        await app.environmentAction(env.id, "remove")
                        busyEnvId = nil
                    }
                }
            )
        }
    }

    private var capabilityNote: some View {
        VStack(alignment: .leading, spacing: 6) {
            Label("兼容性说明", systemImage: "info.circle")
                .font(.headline)
            Text("已实测：OrbStack（Compose + docker run 全流程）。Docker Desktop / Colima / Rancher Desktop / Lima / Finch 与 Docker API 兼容，接入路径一致。Podman 与远程 Docker（SSH/TCP）仅识别展示：Podman 重建路径尚未实测，远程环境的端口发布在远程机器上、本机网关无法接管——两者都不会假装可接入。")
                .font(.callout)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color.blue.opacity(0.06), in: RoundedRectangle(cornerRadius: 8))
    }
}

private struct EnvironmentCard: View {
    let env: OrbEnvironment
    let isPrimary: Bool
    let busy: Bool
    let onEnable: () -> Void
    let onDisable: () -> Void
    let onRemove: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 8) {
                Image(systemName: icon)
                    .foregroundStyle(env.isEnabled ? Color.green : Color.secondary)
                Text(env.displayName)
                    .font(.headline)
                Text(env.kindText)
                    .font(.caption)
                    .padding(.horizontal, 6)
                    .padding(.vertical, 2)
                    .background(Color.secondary.opacity(0.12), in: Capsule())
                if isPrimary {
                    Text("默认")
                        .font(.caption2.bold())
                        .padding(.horizontal, 6)
                        .padding(.vertical, 2)
                        .background(Color.accentColor.opacity(0.15), in: Capsule())
                }
                if env.isDiscoveredOnly {
                    Text("未注册")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                }
                if env.isRemote {
                    Text("远程")
                        .font(.caption2)
                        .padding(.horizontal, 6)
                        .padding(.vertical, 2)
                        .background(Color.orange.opacity(0.15), in: Capsule())
                }
                Spacer()
                statusDot
            }

            Grid(alignment: .leading, horizontalSpacing: 16, verticalSpacing: 4) {
                GridRow {
                    Text("端点").foregroundStyle(.secondary)
                    Text(env.endpoint ?? "—")
                        .font(.caption.monospaced())
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
                GridRow {
                    Text("状态").foregroundStyle(.secondary)
                    HStack(spacing: 6) {
                        Text(env.statusText)
                        if let v = env.engineVersion, !v.isEmpty {
                            Text("引擎 \(v)").foregroundStyle(.secondary)
                        }
                        if let v = env.apiVersion, !v.isEmpty {
                            Text("API \(v)").foregroundStyle(.secondary)
                        }
                    }
                }
                if env.isEnabled, let start = env.proxyPortStart, let end = env.proxyPortEnd, start > 0 {
                    GridRow {
                        Text("代理端口段").foregroundStyle(.secondary)
                        Text("\(start)–\(end)")
                    }
                    GridRow {
                        Text("管理端口").foregroundStyle(.secondary)
                        Text("\(env.adminPort ?? 0) / \(env.webPort ?? 0)")
                    }
                }
                GridRow {
                    Text("支持能力").foregroundStyle(.secondary)
                    Text(env.capabilityText)
                }
                if let err = env.lastError, !err.isEmpty {
                    GridRow {
                        Text("错误").foregroundStyle(.secondary)
                        Text(err).foregroundStyle(.red).font(.caption)
                    }
                }
            }
            .font(.callout)

            HStack {
                Spacer()
                if env.isDiscoveredOnly {
                    Button("启用此环境") { onEnable() }
                        .disabled(busy)
                } else if env.isEnabled {
                    Button("停用") { onDisable() }
                        .disabled(busy)
                } else {
                    Button("启用") { onEnable() }
                        .disabled(busy)
                    Button("删除", role: .destructive) { onRemove() }
                        .disabled(busy)
                }
            }
        }
        .padding(14)
        .background(Color.secondary.opacity(0.05), in: RoundedRectangle(cornerRadius: 10))
    }

    private var icon: String {
        switch env.kind ?? "" {
        case "orbstack": return "cube.transparent"
        case "docker-desktop": return "display"
        case "colima", "lima": return "cpu"
        case "rancher-desktop": return "desktopcomputer"
        case "podman": return "podcasts"
        case "remote": return "network"
        default: return "shippingbox"
        }
    }

    private var statusDot: some View {
        HStack(spacing: 5) {
            Circle()
                .fill(color)
                .frame(width: 8, height: 8)
            Text(env.isEnabled ? "已启用" : "未启用")
                .font(.caption)
                .foregroundStyle(.secondary)
        }
    }

    private var color: Color {
        switch env.status ?? "" {
        case "online": return env.isEnabled ? .green : .yellow
        case "offline": return .red
        default: return .gray
        }
    }
}

private struct AddEnvironmentSheet: View {
    let onAdd: (String, String) -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var endpoint = "unix://"
    @State private var name = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("添加容器环境")
                .font(.headline)
            Text("输入容器引擎的 Unix Socket 路径（例如 ~/.colima/default/docker.sock）。远程 TCP/SSH 端点的端口无法由本机接管，暂不支持。")
                .font(.callout)
                .foregroundStyle(.secondary)
            TextField("unix:///path/to/docker.sock", text: $endpoint)
                .textFieldStyle(.roundedBorder)
            TextField("显示名称（可选）", text: $name)
                .textFieldStyle(.roundedBorder)
            HStack {
                Spacer()
                Button("取消") { dismiss() }
                Button("测试并添加") { onAdd(endpoint, name) }
                    .keyboardShortcut(.defaultAction)
            }
        }
        .padding(20)
        .frame(width: 460)
    }
}
