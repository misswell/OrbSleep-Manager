import SwiftUI

/// 操作进度：每 500ms 由 `AppState` 轮询 `operation.get`，
/// 这里只负责展示日志流与最终结果。
struct OperationProgressView: View {
    @Environment(AppState.self) private var app
    let operation: Operation?
    let title: String
    var onClose: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            header
            Divider()

            if let operation {
                resultBanner(operation)
                logStream(operation)
                footer(operation)
            } else {
                VStack(spacing: 10) {
                    ProgressView()
                    Text("正在准备操作…")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    // MARK: - 头部

    private var header: some View {
        HStack(alignment: .center, spacing: 12) {
            VStack(alignment: .leading, spacing: 4) {
                Text(title.isEmpty ? "操作进度" : title)
                    .font(.title3.weight(.semibold))
                if let operation {
                    HStack(spacing: 8) {
                        Text(operation.id)
                            .font(.caption.monospaced())
                            .foregroundStyle(.secondary)
                            .textSelection(.enabled)
                        Text(operation.kindValue.title)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
            }
            Spacer()
            if let operation {
                StatusBadge(operationStatus: operation.statusValue)
            }
        }
        .padding(16)
    }

    // MARK: - 结果横幅

    @ViewBuilder
    private func resultBanner(_ operation: Operation) -> some View {
        let status = operation.statusValue
        if status == .running {
            VStack(alignment: .leading, spacing: 4) {
                HStack(spacing: 10) {
                    ProgressView()
                        .controlSize(.small)
                    Text(app.daemonReachable ? "操作进行中，请稍候…" : "与后台助手断开，无法继续获取进度")
                        .font(.callout)
                    Spacer()
                    Text("开始于 \(RelativeTime.text(operation.startedAt, placeholder: "刚刚"))")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                if let error = operation.errorText {
                    Text(error)
                        .font(.caption)
                        .foregroundStyle(.red)
                }
            }
            .padding(.horizontal, 16)
            .padding(.vertical, 10)
            .background(Color.blue.opacity(0.08))
        } else {
            VStack(alignment: .leading, spacing: 6) {
                HStack(spacing: 8) {
                    Image(systemName: status.systemImage)
                        .foregroundStyle(status.color)
                    Text(resultTitle(status))
                        .font(.callout.weight(.semibold))
                        .foregroundStyle(status.color)
                    Spacer()
                    if let duration = DurationText.between(start: operation.startedAt, end: operation.finishedAt) {
                        Text("耗时 \(duration)")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
                if let error = operation.errorText {
                    Text(error)
                        .font(.caption)
                        .foregroundStyle(status == .succeeded ? Color.secondary : Color.red)
                        .textSelection(.enabled)
                        .fixedSize(horizontal: false, vertical: true)
                }
                if let backupDir = operation.backupDir, !backupDir.isEmpty {
                    HStack(spacing: 6) {
                        Text("备份目录：")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                        Text(backupDir)
                            .font(.caption.monospaced())
                            .foregroundStyle(.secondary)
                            .lineLimit(1)
                            .truncationMode(.middle)
                            .textSelection(.enabled)
                        Button("打开") {
                            app.openPath(backupDir)
                        }
                        .buttonStyle(.link)
                        .font(.caption)
                    }
                }
                if let serviceIds = operation.affectedServiceIds, !serviceIds.isEmpty {
                    Text("影响服务：\(serviceIds.joined(separator: "、"))")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
            .padding(.horizontal, 16)
            .padding(.vertical, 10)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(status.color.opacity(0.08))
        }
    }

    private func resultTitle(_ status: OperationStatus) -> String {
        switch status {
        case .succeeded: return "执行成功"
        case .failed: return "执行失败"
        case .rolledBack: return "执行失败，已回滚"
        case .rollbackFailed: return "执行失败，回滚也失败"
        case .running: return "操作进行中"
        case .unknown: return "未知状态"
        }
    }

    // MARK: - 日志

    private func logStream(_ operation: Operation) -> some View {
        let lines = operation.logLines
        return ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 4) {
                    if lines.isEmpty {
                        Text("暂无日志")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .padding(12)
                    } else {
                        ForEach(Array(lines.enumerated()), id: \.offset) { index, line in
                            HStack(alignment: .firstTextBaseline, spacing: 8) {
                                Text(RelativeTime.clock(line.at))
                                    .font(.caption2.monospaced())
                                    .foregroundStyle(.secondary)
                                LogLevelBadge(level: line.level)
                                Text(line.message ?? "")
                                    .font(.system(.caption, design: .monospaced))
                                    .textSelection(.enabled)
                                    .fixedSize(horizontal: false, vertical: true)
                                Spacer(minLength: 0)
                            }
                            .id(index)
                        }
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(12)
            }
            .background(Color(nsColor: .textBackgroundColor))
            .onChange(of: lines.count) { _, newCount in
                guard newCount > 0 else { return }
                withAnimation(.easeOut(duration: 0.15)) {
                    proxy.scrollTo(newCount - 1, anchor: .bottom)
                }
            }
        }
    }

    // MARK: - 底部

    private func footer(_ operation: Operation) -> some View {
        HStack(spacing: 10) {
            if operation.statusValue == .running {
                Text("操作进行中，完成后可以关闭窗口。")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            } else {
                Text("关闭窗口不会影响后台助手；可以回到列表重新扫描以刷新状态。")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Spacer()
            Button("关闭", action: onClose)
                .keyboardShortcut(.cancelAction)
                .disabled(operation.statusValue == .running && app.daemonReachable)
        }
        .padding(16)
    }
}

/// 服务操作 / 代理安装使用的进度窗口。
struct OperationSheetView: View {
    @Environment(AppState.self) private var app

    var body: some View {
        OperationProgressView(operation: app.operation, title: app.operationTitle) {
            app.closeOperationSheet()
        }
        .frame(minWidth: 720, minHeight: 520)
    }
}
