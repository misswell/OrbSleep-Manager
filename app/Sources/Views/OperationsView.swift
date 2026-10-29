import SwiftUI

struct OperationsView: View {
    @Environment(AppState.self) private var app

    var body: some View {
        VStack(spacing: 0) {
            header
            Divider()
            if app.operations.isEmpty {
                EmptyStateView(
                    systemImage: "list.bullet.rectangle",
                    title: app.isLoadingOperations ? "正在加载操作记录…" : "还没有操作记录",
                    message: "接入、移除托管、恢复端口映射以及代理安装都会在这里留下可展开的日志。"
                )
            } else {
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 10) {
                        ForEach(app.operations) { operation in
                            operationCard(operation)
                        }
                    }
                    .padding(16)
                }
            }
        }
        .navigationTitle("操作记录")
        .toolbar {
            ToolbarItemGroup(placement: .primaryAction) {
                if app.isLoadingOperations {
                    ProgressView()
                        .controlSize(.small)
                }
                Button {
                    Task { await app.loadOperations() }
                } label: {
                    Label("刷新", systemImage: "arrow.clockwise")
                }
            }
        }
        .task {
            if app.operations.isEmpty {
                await app.loadOperations()
            }
        }
    }

    private var header: some View {
        HStack(spacing: 12) {
            Text("最近 \(app.operations.count) 条操作")
                .font(.callout)
                .foregroundStyle(.secondary)
            Spacer()
            Text("日志由后台助手记录，本地只读")
                .font(.caption)
                .foregroundStyle(.tertiary)
        }
        .padding(12)
    }

    private func operationCard(_ operation: Operation) -> some View {
        DisclosureGroup {
            VStack(alignment: .leading, spacing: 10) {
                Grid(alignment: .leadingFirstTextBaseline, horizontalSpacing: 12, verticalSpacing: 6) {
                    GridRow {
                        metaLabel("开始")
                        Text("\(RelativeTime.text(operation.startedAt, placeholder: "—")) · \(RelativeTime.absolute(RelativeTime.date(from: operation.startedAt) ?? Date()))")
                            .font(.caption)
                    }
                    GridRow {
                        metaLabel("结束")
                        Text(RelativeTime.text(operation.finishedAt, placeholder: "进行中"))
                            .font(.caption)
                    }
                    GridRow {
                        metaLabel("耗时")
                        Text(DurationText.between(start: operation.startedAt, end: operation.finishedAt) ?? "—")
                            .font(.caption)
                    }
                    if let serviceIds = operation.affectedServiceIds, !serviceIds.isEmpty {
                        GridRow {
                            metaLabel("影响服务")
                            Text(serviceIds.joined(separator: "、"))
                                .font(.caption.monospaced())
                        }
                    }
                    if let backupDir = operation.backupDir, !backupDir.isEmpty {
                        GridRow {
                            metaLabel("备份目录")
                            HStack(spacing: 6) {
                                Text(backupDir)
                                    .font(.caption.monospaced())
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
                    }
                }

                if let error = operation.errorText {
                    Text(error)
                        .font(.callout)
                        .foregroundStyle(.red)
                        .fixedSize(horizontal: false, vertical: true)
                }

                let lines = operation.logLines
                if lines.isEmpty {
                    Text("没有日志")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                } else {
                    ScrollView {
                        VStack(alignment: .leading, spacing: 4) {
                            ForEach(Array(lines.enumerated()), id: \.offset) { _, line in
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
                            }
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(10)
                    }
                    .frame(maxHeight: 240)
                    .background(Color(nsColor: .textBackgroundColor), in: RoundedRectangle(cornerRadius: 6))
                    .overlay(
                        RoundedRectangle(cornerRadius: 6)
                            .stroke(Color.secondary.opacity(0.18), lineWidth: 1)
                    )
                }
            }
            .padding(.top, 8)
        } label: {
            HStack(alignment: .center, spacing: 10) {
                StatusBadge(operationStatus: operation.statusValue)
                VStack(alignment: .leading, spacing: 2) {
                    Text(operation.kindValue.title)
                        .font(.callout.weight(.medium))
                    Text(operation.id)
                        .font(.caption.monospaced())
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
                Spacer(minLength: 8)
                if let error = operation.errorText {
                    Text(error)
                        .font(.caption)
                        .foregroundStyle(.red)
                        .lineLimit(1)
                        .truncationMode(.tail)
                        .frame(maxWidth: 260, alignment: .trailing)
                }
                Text(RelativeTime.text(operation.startedAt))
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            .padding(.vertical, 4)
        }
        .padding(12)
        .background(Color(nsColor: .controlBackgroundColor), in: RoundedRectangle(cornerRadius: 10))
        .overlay(
            RoundedRectangle(cornerRadius: 10)
                .stroke(Color.secondary.opacity(0.16), lineWidth: 1)
        )
    }

    private func metaLabel(_ text: String) -> some View {
        Text(text)
            .font(.caption)
            .foregroundStyle(.secondary)
            .frame(width: 64, alignment: .leading)
    }
}
