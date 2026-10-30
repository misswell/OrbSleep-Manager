import SwiftUI

/// 更新中心：应用内更新、后台服务升级与更新状态。
struct UpdateView: View {
    @Environment(AppState.self) private var app

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                header
                versionsCard
                updateCard
                serviceCard
            }
            .padding(22)
        }
        .navigationTitle("更新与维护")
        .task {
            if app.availableUpdate == nil, case .idle = app.updateStatus {
                // Do not auto-download; only show current state.
            }
        }
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("更新与维护")
                .font(.largeTitle.weight(.semibold))
            Text("应用与后台服务使用同一版本号；后台服务二进制随应用更新，重启服务即加载新版本。")
                .font(.callout)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
        }
    }

    private var versionsCard: some View {
        Card("版本", systemImage: "number") {
            VStack(alignment: .leading, spacing: 8) {
                LabeledValue(label: "应用版本", value: app.appVersion.isEmpty ? "未知" : app.appVersion)
                LabeledValue(label: "后台版本", value: app.status?.daemonVersion ?? "未连接")
                LabeledValue(label: "安装位置", value: Bundle.main.bundleURL.path, monospaced: true)
                LabeledValue(label: "更新源", value: app.updateManager.feedDescription, monospaced: true)
                if app.currentServiceState == .versionMismatch {
                    Label("应用与后台版本不一致，升级后台服务即可对齐。", systemImage: "exclamationmark.triangle.fill")
                        .font(.caption)
                        .foregroundStyle(.orange)
                }
            }
        }
    }

    private var updateCard: some View {
        Card("应用更新", systemImage: "arrow.triangle.2.circlepath.circle") {
            VStack(alignment: .leading, spacing: 10) {
                switch app.updateStatus {
                case .idle:
                    Text("检查更新源以获取新版本。未配置更新源时，这里会说明需要设置的 Info.plist 键。")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                    Button {
                        Task { await app.checkForUpdates() }
                    } label: {
                        Label("检查更新", systemImage: "arrow.clockwise")
                    }
                    .buttonStyle(.borderedProminent)

                case .checking:
                    HStack(spacing: 8) {
                        ProgressView().controlSize(.small)
                        Text("正在检查更新…").font(.callout)
                    }

                case .available(let update):
                    HStack(spacing: 8) {
                        StatusBadge(text: "发现新版本 \(update.version)", color: .blue, systemImage: "arrow.down.circle.fill")
                        Spacer()
                        if let size = update.sizeBytes {
                            Text(ByteCountFormatter.string(fromByteCount: size, countStyle: .file))
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                    }
                    if let published = update.publishedAt, !published.isEmpty {
                        Text("发布时间：\(published)")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                    if let notes = update.notes, !notes.isEmpty {
                        Text(notes)
                            .font(.callout)
                            .textSelection(.enabled)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    HStack(spacing: 10) {
                        Button {
                            Task { await app.downloadAndInstallUpdate() }
                        } label: {
                            Label("下载并安装…", systemImage: "arrow.down.circle")
                        }
                        .buttonStyle(.borderedProminent)
                        Button {
                            Task { await app.checkForUpdates() }
                        } label: {
                            Label("重新检查", systemImage: "arrow.clockwise")
                        }
                    }
                    Text("安装会保留当前版本；如果新版本在 12 秒内没有运行，会自动回滚到旧版本。更新过程中应用会退出并重新打开。")
                        .font(.caption)
                        .foregroundStyle(.secondary)

                case .downloading(let progress):
                    VStack(alignment: .leading, spacing: 6) {
                        HStack {
                            Text("正在下载更新…").font(.callout)
                            Spacer()
                            Text("\(Int(progress * 100))%")
                                .font(.caption.monospaced())
                                .foregroundStyle(.secondary)
                        }
                        ProgressView(value: progress)
                    }

                case .installing:
                    HStack(spacing: 8) {
                        ProgressView().controlSize(.small)
                        Text("正在校验并替换应用，随后会重新启动…").font(.callout)
                    }

                case .failed(let message):
                    ErrorBanner(message: message)
                    Button {
                        Task { await app.checkForUpdates() }
                    } label: {
                        Label("重新检查", systemImage: "arrow.clockwise")
                    }
                }
            }
        }
    }

    private var serviceCard: some View {
        Card("后台服务", systemImage: "gearshape.2") {
            VStack(alignment: .leading, spacing: 10) {
                HStack(spacing: 8) {
                    StatusBadge(
                        text: app.currentServiceState.title,
                        color: app.daemonReachable ? .green : .orange,
                        systemImage: "circle.fill"
                    )
                    Text("后台服务随应用包升级；执行升级会优雅停止并重新注册/启动服务。")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    Spacer()
                }
                HStack(spacing: 10) {
                    Button {
                        Task { await app.upgradeService() }
                    } label: {
                        Label("升级后台服务", systemImage: "arrow.up.circle")
                    }
                    .disabled(app.isBusyService)
                    Button {
                        Task { await app.restartService() }
                    } label: {
                        Label("重启后台服务", systemImage: "arrow.triangle.2.circlepath")
                    }
                    .disabled(app.isBusyService)
                    if app.isBusyService {
                        ProgressView().controlSize(.small)
                    }
                    Spacer()
                }
                if app.serviceVersionMismatch {
                    Text("当前后台版本 \(app.status?.daemonVersion ?? "?") 与应用 \(app.appVersion) 不一致。")
                        .font(.caption)
                        .foregroundStyle(.orange)
                }
            }
        }
    }
}
