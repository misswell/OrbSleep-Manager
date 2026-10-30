import SwiftUI

/// 首次启动的安装向导。用户只需要点击「一键配置并启动」，
/// 应用会注册后台服务、启动进程并自动执行环境检查。
struct SetupView: View {
    @Environment(AppState.self) private var app

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 22) {
                header
                statusGrid
                if app.status?.dockerReachable != true {
                    HStack(spacing: 10) {
                        Button {
                            app.openDockerApp()
                        } label: {
                            Label("打开 OrbStack / Docker Desktop", systemImage: "arrow.up.forward.app")
                        }
                        Text("启动容器运行时后，再点击「一键配置并启动」。")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                        Spacer()
                    }
                }
                if app.legacyInstallation.isPresent {
                    legacyCard
                }
                stepsCard
                if let error = app.lastSetupError, !error.isEmpty {
                    ErrorBanner(message: error)
                }
                actionBar
                footer
            }
            .frame(maxWidth: 780, alignment: .leading)
            .padding(32)
            .frame(maxWidth: .infinity, alignment: .center)
        }
        .background(Color(nsColor: .windowBackgroundColor))
        .onAppear {
            if app.setupSteps.isEmpty {
                app.prepareSetupSteps()
            }
        }
    }

    // MARK: - 头部

    private var header: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 12) {
                Image(systemName: "bolt.horizontal.circle.fill")
                    .font(.system(size: 46, weight: .regular))
                    .foregroundStyle(.tint)
                    .symbolEffect(.pulse, options: .repeating, isActive: app.isSettingUp)
                VStack(alignment: .leading, spacing: 4) {
                    Text("欢迎使用 OrbSleep Manager")
                        .font(.largeTitle.weight(.semibold))
                    Text("自动配置后台服务，连接 OrbStack 或 Docker，让容器按需休眠。")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                }
            }
            Text("整个安装过程在应用内完成：无需打开终端，无需复制文件，无需手动执行 sudo。")
                .font(.callout)
                .foregroundStyle(.secondary)
        }
    }

    // MARK: - 状态卡

    private var statusGrid: some View {
        LazyVGrid(columns: [GridItem(.adaptive(minimum: 220), spacing: 14)], spacing: 14) {
            statusCard(
                title: "应用程序",
                value: installedLocation,
                systemImage: "app.badge.checkmark",
                ok: installedInApplications
            )
            statusCard(
                title: "Docker / OrbStack",
                value: app.dockerSummary,
                systemImage: "shippingbox",
                ok: app.status?.dockerReachable == true
            )
            statusCard(
                title: "后台服务",
                value: app.backgroundService.statusText,
                systemImage: "gearshape.2",
                ok: app.backgroundService.installed
            )
        }
    }

    private var installedInApplications: Bool {
        Bundle.main.bundleURL.path.hasPrefix("/Applications") && !app.backgroundService.isTranslocated
    }

    private var installedLocation: String {
        if app.backgroundService.isTranslocated {
            return "正在从隔离位置运行，请拖到 Applications"
        }
        return installedInApplications ? "已安装到 Applications" : "开发构建（未在 /Applications）"
    }

    private func statusCard(title: String, value: String, systemImage: String, ok: Bool) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 8) {
                Image(systemName: systemImage)
                    .foregroundStyle(.secondary)
                Text(title)
                    .font(.headline)
                Spacer()
                Circle()
                    .fill(ok ? Color.green : Color.orange)
                    .frame(width: 9, height: 9)
            }
            Text(value)
                .font(.callout)
                .foregroundStyle(.secondary)
                .lineLimit(2)
        }
        .padding(14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color(nsColor: .controlBackgroundColor), in: RoundedRectangle(cornerRadius: 10))
        .overlay(
            RoundedRectangle(cornerRadius: 10)
                .stroke(Color.secondary.opacity(0.16), lineWidth: 1)
        )
    }

    // MARK: - 旧版迁移

    private var legacyCard: some View {
        Card("检测到旧版安装", systemImage: "arrow.triangle.2.circlepath") {
            VStack(alignment: .leading, spacing: 8) {
                Text("发现 0.1.x 手动安装：\(app.legacyInstallation.summary)")
                    .font(.callout)
                Text("迁移会保留数据目录、托管记录、备份与操作历史；先创建备份，再停止旧启动项并切换到应用内后台服务。")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                HStack(spacing: 10) {
                    Button {
                        Task { await app.migrateLegacyInstall() }
                    } label: {
                        Label("迁移到应用内后台服务", systemImage: "arrow.right.circle")
                    }
                    .buttonStyle(.borderedProminent)
                    .disabled(app.isMigrating)

                    Button {
                        Task { await app.cleanupLegacyComponents() }
                    } label: {
                        Label("仅清理旧组件", systemImage: "trash")
                    }
                    .disabled(app.isMigrating || app.isBusyMaintenance)

                    if app.isMigrating || app.isBusyMaintenance {
                        ProgressView().controlSize(.small)
                    }
                    Spacer()
                }
            }
        }
    }

    // MARK: - 步骤

    private var stepsCard: some View {
        Card("安装步骤", systemImage: "list.number") {
            VStack(alignment: .leading, spacing: 12) {
                ForEach(app.setupSteps) { step in
                    stepRow(step)
                }
            }
        }
    }

    private func stepRow(_ step: SetupStep) -> some View {
        HStack(alignment: .top, spacing: 12) {
            stepIcon(step.state)
                .frame(width: 22, height: 22)
            VStack(alignment: .leading, spacing: 3) {
                Text(step.title)
                    .font(.callout.weight(.semibold))
                if let message = step.message, !message.isEmpty {
                    Text(message)
                        .font(.caption)
                        .foregroundStyle(step.state == .failed ? .red : .secondary)
                        .fixedSize(horizontal: false, vertical: true)
                } else {
                    Text(step.detail)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
            Spacer(minLength: 0)
            Text(stateLabel(step.state))
                .font(.caption2.weight(.medium))
                .foregroundStyle(stateColor(step.state))
        }
    }

    @ViewBuilder
    private func stepIcon(_ state: SetupStep.State) -> some View {
        switch state {
        case .pending:
            Image(systemName: "circle")
                .foregroundStyle(.secondary)
        case .running:
            ProgressView()
                .controlSize(.small)
        case .done:
            Image(systemName: "checkmark.circle.fill")
                .foregroundStyle(.green)
        case .failed:
            Image(systemName: "xmark.circle.fill")
                .foregroundStyle(.red)
        case .skipped:
            Image(systemName: "minus.circle")
                .foregroundStyle(.secondary)
        }
    }

    private func stateLabel(_ state: SetupStep.State) -> String {
        switch state {
        case .pending: return "等待"
        case .running: return "进行中"
        case .done: return "完成"
        case .failed: return "需要处理"
        case .skipped: return "跳过"
        }
    }

    private func stateColor(_ state: SetupStep.State) -> Color {
        switch state {
        case .pending: return .secondary
        case .running: return .blue
        case .done: return .green
        case .failed: return .red
        case .skipped: return .secondary
        }
    }

    // MARK: - 操作

    private var actionBar: some View {
        HStack(spacing: 12) {
            Button {
                Task { await app.performSetup() }
            } label: {
                HStack(spacing: 8) {
                    if app.isSettingUp {
                        ProgressView().controlSize(.small)
                    } else {
                        Image(systemName: "play.circle.fill")
                    }
                    Text(app.isSettingUp ? "正在配置…" : "一键配置并启动")
                        .fontWeight(.semibold)
                }
                .frame(minWidth: 190)
                .padding(.vertical, 4)
            }
            .buttonStyle(.borderedProminent)
            .controlSize(.large)
            .disabled(app.isSettingUp)

            if app.setupCompleted, app.daemonReachable {
                Button {
                    app.section = .overview
                    app.setupCompleted = true
                } label: {
                    Label("进入主界面", systemImage: "arrow.right.circle")
                }
                .controlSize(.large)
            }

            if app.backgroundService.needsApproval {
                Button {
                    app.backgroundService.openLoginItemsSettings()
                } label: {
                    Label("打开登录项设置", systemImage: "switch.2")
                }
                .controlSize(.large)
            }

            Spacer()
        }
    }

    private var footer: some View {
        Text("需要管理员权限时，macOS 会使用系统自带的授权窗口，应用不会要求你在终端输入任何命令。")
            .font(.caption)
            .foregroundStyle(.tertiary)
    }
}
