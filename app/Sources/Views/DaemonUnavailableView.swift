import SwiftUI

/// 后台服务未运行时显示的界面。所有恢复动作都在应用内完成，不出现终端命令。
struct DaemonUnavailableView: View {
    @Environment(AppState.self) private var app

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 20) {
                VStack(alignment: .leading, spacing: 8) {
                    Image(systemName: "bolt.horizontal.circle")
                        .font(.system(size: 44, weight: .light))
                        .foregroundStyle(.orange)
                    Text("后台服务未运行")
                        .font(.largeTitle.weight(.semibold))
                    Text("OrbSleep Manager 通过本地 Unix Socket 与后台服务通信。可以在这里直接启动它，无需打开终端。")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }

                if let error = app.connectionError, !error.isEmpty {
                    ErrorBanner(message: error)
                }

                Card("恢复连接", systemImage: "wrench.and.screwdriver") {
                    VStack(alignment: .leading, spacing: 14) {
                        step(
                            index: 1,
                            title: "启动后台服务",
                            detail: "应用会通过 launchd 启动已注册的后台服务；如果尚未注册，会先完成注册。"
                        )
                        step(
                            index: 2,
                            title: "确认数据目录与 Socket 路径",
                            detail: "后台服务会在这里创建 daemon.sock（权限 0600）。"
                        )
                        LabeledValue(label: "数据目录", value: app.dataDir, monospaced: true)
                        LabeledValue(label: "Socket", value: app.socketPath, monospaced: true)
                        step(
                            index: 3,
                            title: "如仍失败，查看诊断",
                            detail: "「诊断与修复」会自动检查 Docker、代理与端口，并提供一键修复。"
                        )
                    }
                }

                HStack(spacing: 10) {
                    Button {
                        Task { await app.startService() }
                    } label: {
                        Label("启动后台服务", systemImage: "play.circle.fill")
                    }
                    .buttonStyle(.borderedProminent)
                    .controlSize(.large)
                    .disabled(app.isBusyService)

                    Button {
                        Task { await app.restartService() }
                    } label: {
                        Label("重新启动", systemImage: "arrow.clockwise")
                    }
                    .controlSize(.large)
                    .disabled(app.isBusyService)

                    Button {
                        app.section = .diagnostics
                        app.setupCompleted = true
                    } label: {
                        Label("打开诊断与修复", systemImage: "stethoscope")
                    }
                    .controlSize(.large)

                    Button {
                        app.openDataDirectory()
                    } label: {
                        Label("打开数据目录", systemImage: "folder")
                    }
                    .controlSize(.large)

                    if app.isBusyService {
                        ProgressView().controlSize(.small)
                    }
                }

                Text("提示：关闭应用不会影响后台服务，也不会停止已经托管的服务。")
                    .font(.caption)
                    .foregroundStyle(.tertiary)
            }
            .frame(maxWidth: 720, alignment: .leading)
            .padding(32)
            .frame(maxWidth: .infinity, alignment: .center)
        }
        .background(Color(nsColor: .windowBackgroundColor))
    }

    private func step(index: Int, title: String, detail: String) -> some View {
        HStack(alignment: .top, spacing: 10) {
            Text("\(index)")
                .font(.caption.bold())
                .foregroundStyle(.white)
                .frame(width: 20, height: 20)
                .background(Circle().fill(Color.accentColor))
            VStack(alignment: .leading, spacing: 3) {
                Text(title)
                    .font(.callout.weight(.semibold))
                Text(detail)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
    }
}
