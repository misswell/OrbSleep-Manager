import SwiftUI

struct ServicesView: View {
    @Environment(AppState.self) private var app

    @State private var removeTarget: Service?
    @State private var restoreTarget: Service?
    @State private var timeoutTarget: Service?
    @State private var timeoutText = ""

    var body: some View {
        VStack(spacing: 0) {
            summaryBar
            Divider()
            if app.services.isEmpty {
                EmptyStateView(
                    systemImage: "shippingbox",
                    title: "还没有托管服务",
                    message: "在「扫描容器」中选择可接入的容器并生成接入计划，执行后服务会出现在这里。",
                    actionTitle: "去扫描容器",
                    action: { app.section = .scan }
                )
            } else {
                table
            }
        }
        .navigationTitle("托管服务")
        .toolbar {
            ToolbarItemGroup(placement: .primaryAction) {
                if app.isRefreshing {
                    ProgressView()
                        .controlSize(.small)
                }
                Button {
                    Task { await app.refreshServices() }
                } label: {
                    Label("刷新", systemImage: "arrow.clockwise")
                }
            }
        }
        .sheet(item: $timeoutTarget) { service in
            timeoutEditor(service)
        }
        .confirmationDialog(
            "移除托管",
            isPresented: Binding(
                get: { removeTarget != nil },
                set: { if !$0 { removeTarget = nil } }
            ),
            presenting: removeTarget
        ) { service in
            Button("移除托管并重建容器", role: .destructive) {
                let target = service
                removeTarget = nil
                Task { await app.performServiceAction(target, action: .remove) }
            }
            Button("取消", role: .cancel) {
                removeTarget = nil
            }
        } message: { service in
            Text("将移除「\(service.displayName)」的托管，并重建容器以恢复原始端口映射。操作会中断服务约 1–5 秒，且不会自动撤销。")
        }
        .confirmationDialog(
            "恢复原始端口映射",
            isPresented: Binding(
                get: { restoreTarget != nil },
                set: { if !$0 { restoreTarget = nil } }
            ),
            presenting: restoreTarget
        ) { service in
            Button("恢复映射并重建容器", role: .destructive) {
                let target = service
                restoreTarget = nil
                Task { await app.performServiceAction(target, action: .restore) }
            }
            Button("取消", role: .cancel) {
                restoreTarget = nil
            }
        } message: { service in
            Text("将把「\(service.displayName)」恢复为原始端口映射，并重建容器。操作会中断服务约 1–5 秒，代理中的对应映射会被删除。")
        }
    }

    // MARK: - 汇总

    private var summaryBar: some View {
        HStack(spacing: 16) {
            Text("共 \(app.services.count) 个托管服务")
                .font(.callout)
                .foregroundStyle(.secondary)
            if let counts = app.status?.counts {
                if let running = counts.running {
                    statusCount("运行", running, .green)
                }
                if let idle = counts.idle {
                    statusCount("空闲", idle, .blue)
                }
                if let stopped = counts.stopped {
                    statusCount("已停止", stopped, .secondary)
                }
                if let missing = counts.missing, missing > 0 {
                    statusCount("缺失", missing, .orange)
                }
            }
            Spacer()
            Text("状态每 3 秒自动刷新")
                .font(.caption)
                .foregroundStyle(.tertiary)
        }
        .padding(12)
    }

    private func statusCount(_ title: String, _ count: Int, _ color: Color) -> some View {
        HStack(spacing: 4) {
            Circle()
                .fill(color)
                .frame(width: 6, height: 6)
            Text("\(title) \(count)")
                .font(.caption)
                .foregroundStyle(.secondary)
        }
    }

    // MARK: - 表格

    private var table: some View {
        Table(app.services) {
            TableColumn("名称") { service in
                VStack(alignment: .leading, spacing: 2) {
                    Text(service.displayName)
                        .font(.callout.weight(.medium))
                        .lineLimit(1)
                    Text(service.projectServiceText)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                }
            }
            .width(min: 150, ideal: 200)

            TableColumn("状态") { service in
                HStack(spacing: 4) {
                    StatusBadge(serviceStatus: service.statusValue)
                    if service.excludedFromSleep == true {
                        StatusBadge(text: "已排除休眠", color: .purple, systemImage: "moon.zzz.fill")
                    }
                }
            }
            .width(150)

            TableColumn("连接数") { service in
                Text("\(service.activeConnections ?? 0)")
                    .font(.callout.monospacedDigit())
            }
            .width(64)

            TableColumn("最后活动") { service in
                Text(RelativeTime.text(service.lastActiveAt, placeholder: "从未"))
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            .width(96)

            TableColumn("空闲超时") { service in
                Text(DurationText.short(service.idleTimeoutSecs))
                    .font(.caption)
            }
            .width(84)

            TableColumn("端口（原始 ⇢ 代理）") { service in
                PortsText(ports: service.bindings ?? [], showsProxy: true, maxLines: 3)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
            .width(min: 160, ideal: 220)

            TableColumn("错误") { service in
                if let error = service.errorText {
                    Text(error)
                        .font(.caption)
                        .foregroundStyle(.red)
                        .lineLimit(2)
                } else {
                    Text("—")
                        .font(.caption)
                        .foregroundStyle(.tertiary)
                }
            }
            .width(min: 100, ideal: 140)

            TableColumn("操作") { service in
                actionMenu(service)
            }
            .width(60)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    private func actionMenu(_ service: Service) -> some View {
        Menu {
            if service.statusValue == .managedStopped || service.statusValue == .missing {
                Button {
                    Task { await app.performServiceAction(service, action: .start) }
                } label: {
                    Label("立即启动", systemImage: "play.fill")
                }
            } else {
                Button {
                    Task { await app.performServiceAction(service, action: .stop) }
                } label: {
                    Label("立即停止", systemImage: "pause.fill")
                }
            }

            Button {
                beginTimeoutEdit(service)
            } label: {
                Label("修改休眠时间…", systemImage: "timer")
            }

            if service.excludedFromSleep == true {
                Button {
                    Task { await app.setExcludedFromSleep(service, excluded: false) }
                } label: {
                    Label("取消临时排除休眠", systemImage: "moon.zzz")
                }
            } else {
                Button {
                    Task { await app.setExcludedFromSleep(service, excluded: true) }
                } label: {
                    Label("临时排除休眠", systemImage: "moon.zzz.fill")
                }
            }

            Divider()

            Button(role: .destructive) {
                restoreTarget = service
            } label: {
                Label("恢复原始端口映射…", systemImage: "arrow.uturn.backward")
            }

            Button(role: .destructive) {
                removeTarget = service
            } label: {
                Label("移除托管…", systemImage: "trash")
            }
        } label: {
            Image(systemName: "ellipsis.circle")
        }
        .menuStyle(.borderlessButton)
        .fixedSize()
        .help("服务操作")
    }

    // MARK: - 休眠时间编辑

    private var parsedTimeout: Int? {
        let trimmed = timeoutText.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let value = Int(trimmed), value > 0 else { return nil }
        return value
    }

    private func beginTimeoutEdit(_ service: Service) {
        timeoutText = String(service.idleTimeoutSecs ?? app.settings?.idleTimeoutSecs ?? 1800)
        timeoutTarget = service
    }

    private func timeoutEditor(_ service: Service) -> some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("修改休眠时间")
                .font(.headline)
            Text("容器空闲超过该时长后会被代理自动停止，下次有连接时再拉起。")
                .font(.callout)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)

            HStack(spacing: 8) {
                TextField("秒", text: $timeoutText)
                    .textFieldStyle(.roundedBorder)
                    .frame(width: 120)
                Text("秒")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                Spacer()
                if let parsedTimeout {
                    Text("约 \(DurationText.short(parsedTimeout))")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }

            if parsedTimeout == nil {
                Text("请输入大于 0 的整数秒数。")
                    .font(.caption)
                    .foregroundStyle(.red)
            }

            HStack {
                Text("当前值：\(DurationText.short(service.idleTimeoutSecs))")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Spacer()
                Button("取消") {
                    timeoutTarget = nil
                }
                .keyboardShortcut(.cancelAction)
                Button("保存") {
                    saveTimeout(service)
                }
                .keyboardShortcut(.defaultAction)
                .disabled(parsedTimeout == nil)
            }
        }
        .padding(20)
        .frame(width: 420)
    }

    private func saveTimeout(_ service: Service) {
        guard let seconds = parsedTimeout else { return }
        timeoutTarget = nil
        Task {
            await app.updateIdleTimeout(service, seconds: seconds)
        }
    }
}
