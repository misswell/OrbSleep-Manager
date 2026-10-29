import SwiftUI

struct DaemonUnavailableView: View {
    @Environment(AppState.self) private var app
    @State private var copied = false

    private let installCommand = "orbsleepctl install-agent"

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 20) {
                VStack(alignment: .leading, spacing: 8) {
                    Image(systemName: "bolt.horizontal.circle")
                        .font(.system(size: 44, weight: .light))
                        .foregroundStyle(.orange)
                    Text("未连接到 orbsleepd")
                        .font(.largeTitle.weight(.semibold))
                    Text("OrbSleep Manager 通过本地 Unix Socket 与后台助手通信。GUI 不会自行启动或停止 orbsleepd，请确认它已安装并在运行。")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }

                if let error = app.connectionError, !error.isEmpty {
                    ErrorBanner(message: error)
                }

                Card("排查步骤", systemImage: "wrench.and.screwdriver") {
                    VStack(alignment: .leading, spacing: 14) {
                        step(
                            index: 1,
                            title: "安装并启动 LaunchAgent",
                            detail: "在终端执行下面的命令；它会安装 LaunchAgent 并拉起 orbsleepd。"
                        )
                        codeBlock(installCommand)
                        step(
                            index: 2,
                            title: "确认数据目录与 Socket 路径",
                            detail: "后台助手会在这里创建 daemon.sock（权限 0600）。"
                        )
                        LabeledValue(label: "数据目录", value: app.dataDir, monospaced: true)
                        LabeledValue(label: "Socket", value: app.socketPath, monospaced: true)
                        Text("Socket 路径可通过环境变量 ORBSLEEP_SOCKET 覆盖，需要与 LaunchAgent 的配置保持一致。")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                        step(
                            index: 3,
                            title: "重试连接",
                            detail: "安装后等待几秒，再点击「重试连接」。也可以通过「打开数据目录」确认日志与备份是否生成。"
                        )
                    }
                }

                HStack(spacing: 10) {
                    Button {
                        Task { await app.retryConnection() }
                    } label: {
                        Label("重试连接", systemImage: "arrow.clockwise")
                    }
                    .buttonStyle(.borderedProminent)
                    .controlSize(.large)

                    Button {
                        app.copyInstallCommand()
                        withAnimation {
                            copied = true
                        }
                        Task {
                            try? await Task.sleep(nanoseconds: 2_000_000_000)
                            withAnimation {
                                copied = false
                            }
                        }
                    } label: {
                        Label(copied ? "已复制" : "复制安装命令", systemImage: copied ? "checkmark" : "doc.on.doc")
                    }
                    .controlSize(.large)

                    Button {
                        app.openDataDirectory()
                    } label: {
                        Label("打开数据目录", systemImage: "folder")
                    }
                    .controlSize(.large)
                }

                Text("提示：GUI 关闭不会影响后台助手，也不会停止已经托管的服务。")
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

    private func codeBlock(_ text: String) -> some View {
        HStack(spacing: 8) {
            Text(text)
                .font(.system(.callout, design: .monospaced))
                .textSelection(.enabled)
            Spacer(minLength: 0)
            Button {
                app.copyInstallCommand()
            } label: {
                Image(systemName: "doc.on.doc")
            }
            .buttonStyle(.borderless)
            .help("复制命令")
        }
        .padding(10)
        .background(Color(nsColor: .textBackgroundColor), in: RoundedRectangle(cornerRadius: 6))
        .overlay(
            RoundedRectangle(cornerRadius: 6)
                .stroke(Color.secondary.opacity(0.2), lineWidth: 1)
        )
    }
}
