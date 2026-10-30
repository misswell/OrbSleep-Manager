import SwiftUI

/// 日志与维护：实时日志、按级别筛选、备份目录与数据目录入口。
struct LogsView: View {
    @Environment(AppState.self) private var app
    @State private var levelFilter = "all"
    @State private var search = ""

    private let levels = ["all", "info", "warn", "error", "debug"]

    var body: some View {
        VStack(spacing: 0) {
            toolbar
            Divider()
            logList
        }
        .navigationTitle("日志与维护")
        .task {
            await app.loadLogSources()
            while !Task.isCancelled {
                await app.loadLogs()
                try? await Task.sleep(nanoseconds: 2_000_000_000)
            }
        }
    }

    private var toolbar: some View {
        HStack(spacing: 12) {
            Picker("来源", selection: Binding(
                get: { app.logSource },
                set: { source in Task { await app.setLogSource(source) } }
            )) {
                ForEach(app.logSources, id: \.self) { source in
                    Text(sourceLabel(source)).tag(source)
                }
            }
            .pickerStyle(.menu)
            .frame(width: 130)

            Picker("级别", selection: $levelFilter) {
                ForEach(levels, id: \.self) { level in
                    Text(level == "all" ? "全部级别" : level.uppercased()).tag(level)
                }
            }
            .pickerStyle(.menu)
            .frame(width: 130)

            TextField("搜索日志", text: $search)
                .textFieldStyle(.roundedBorder)
                .frame(width: 220)

            Button {
                Task { await app.loadLogs() }
            } label: {
                Label("刷新", systemImage: "arrow.clockwise")
            }
            .disabled(app.isLoadingLogs)

            if app.isLoadingLogs {
                ProgressView().controlSize(.small)
            }

            Spacer()

            Menu {
                Button {
                    app.openPath(URL(fileURLWithPath: app.dataDir).appendingPathComponent("logs").path)
                } label: {
                    Label("打开日志目录", systemImage: "folder")
                }
                Button {
                    app.openDataDirectory()
                } label: {
                    Label("打开数据目录", systemImage: "internaldrive")
                }
                if let backup = app.settings?.backupDir, !backup.isEmpty {
                    Button {
                        app.openPath(backup)
                    } label: {
                        Label("打开备份目录", systemImage: "archivebox")
                    }
                }
                Divider()
                Button {
                    Task { await app.exportLogs() }
                } label: {
                    Label("导出日志文件", systemImage: "square.and.arrow.up")
                }
                Button {
                    app.exportDiagnostics()
                } label: {
                    Label("导出诊断报告", systemImage: "square.and.arrow.up")
                }
            } label: {
                Label("维护", systemImage: "wrench.and.screwdriver")
            }
            .menuStyle(.borderlessButton)
            .frame(width: 90)
        }
        .padding(.horizontal, 18)
        .padding(.vertical, 12)
    }

    @ViewBuilder
    private var logList: some View {
        if filteredLogs.isEmpty {
            EmptyStateView(
                systemImage: "doc.text.magnifyingglass",
                title: app.isLoadingLogs ? "正在读取日志" : "暂无日志",
                message: app.daemonReachable
                    ? "后台服务运行后，日志会实时出现在这里。"
                    : "后台服务未连接。可先在「诊断与修复」中启动服务。",
                actionTitle: nil,
                action: nil
            )
        } else {
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 0) {
                    ForEach(Array(filteredLogs.enumerated()), id: \.offset) { _, line in
                        logRow(line)
                        Divider().opacity(0.35)
                    }
                }
                .padding(.horizontal, 18)
                .padding(.vertical, 8)
            }
        }
    }

    private var filteredLogs: [LogLine] {
        app.logs.filter { line in
            let level = (line.level ?? "info").lowercased()
            let matchesLevel = levelFilter == "all" || level.contains(levelFilter)
            let text = "\(line.message ?? "")"
            let matchesSearch = search.isEmpty || text.localizedCaseInsensitiveContains(search)
            return matchesLevel && matchesSearch
        }
    }

    private func sourceLabel(_ source: String) -> String {
        switch source {
        case "daemon": return "后台服务"
        case "proxy": return "代理容器"
        default: return source
        }
    }

    private func logRow(_ line: LogLine) -> some View {
        HStack(alignment: .top, spacing: 10) {
            Text(line.at ?? "")
                .font(.system(.caption2, design: .monospaced))
                .foregroundStyle(.secondary)
                .frame(width: 170, alignment: .leading)
            LogLevelBadge(level: line.level)
                .frame(width: 54, alignment: .leading)
            Text(line.message ?? "")
                .font(.system(.caption, design: .monospaced))
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
        .padding(.vertical, 4)
    }
}
