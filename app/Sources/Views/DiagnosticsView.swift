import SwiftUI

/// 诊断与修复：自动自检、一键修复、导出报告、事务恢复说明与 GUI 卸载。
struct DiagnosticsView: View {
    @Environment(AppState.self) private var app
    @State private var showUninstallConfirmation = false
    @State private var restoreTarget: BackupEntry?

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                header
                serviceCard
                environmentCard
                migrationCard
                doctorCard
                recoveryCard
                backupCard
                uninstallCard
            }
            .padding(22)
        }
        .navigationTitle("诊断与修复")
        .toolbar {
            ToolbarItemGroup(placement: .primaryAction) {
                Button {
                    Task { await app.loadDoctor() }
                } label: {
                    Label("重新检查", systemImage: "arrow.clockwise")
                }
                .disabled(app.isLoadingDoctor)
                Button {
                    Task { await app.repairService() }
                } label: {
                    Label("一键修复", systemImage: "wrench.and.screwdriver")
                }
                .disabled(app.isBusyService)
            }
        }
        .confirmationDialog("从备份恢复", isPresented: Binding(
            get: { restoreTarget != nil },
            set: { if !$0 { restoreTarget = nil } }
        )) {
            Button("恢复并重启后台服务", role: .destructive) {
                if let target = restoreTarget {
                    restoreTarget = nil
                    Task { await app.restoreBackup(target) }
                }
            }
            Button("取消", role: .cancel) { restoreTarget = nil }
        } message: {
            Text("会停止后台服务，并在下次启动时用备份中的数据库替换当前数据库；当前数据库会保留为 state.sqlite.pre-restore-<时间>。托管服务恢复后请重新执行诊断。")
        }
        .confirmationDialog("卸载 OrbSleep Manager", isPresented: $showUninstallConfirmation) {
            Button("卸载应用，保留数据", role: .destructive) {
                Task { await app.uninstallApplication(mode: .keepData) }
            }
            Button("完全卸载（删除数据与自有代理）", role: .destructive) {
                Task { await app.uninstallApplication(mode: .full) }
            }
            Button("取消", role: .cancel) {}
        } message: {
            Text("会先恢复所有托管服务的原始端口映射；只要仍有服务未恢复就会中止。保留数据模式只移除后台服务、开机启动项与 CLI；完全卸载还会停止 OrbSleep 自建代理并删除数据目录 \(app.dataDir)。")
        }
        .task {
            guard app.daemonReachable else { return }
            if app.doctorChecks.isEmpty {
                await app.loadDoctor()
            }
            await app.refreshHealth()
            await app.loadDockerContexts()
            await app.loadRecovery()
            await app.loadBackups()
        }
    }

    // MARK: - 运行环境

    private var environmentCard: some View {
        Card("运行环境 / Docker 上下文", systemImage: "shippingbox") {
            VStack(alignment: .leading, spacing: 10) {
                LabeledValue(
                    label: "当前上下文",
                    value: app.health?.dockerContext?.name ?? app.status?.context?.name ?? "—"
                )
                LabeledValue(
                    label: "Socket",
                    value: app.status?.context?.endpoint ?? app.health?.socketPath ?? "—",
                    monospaced: true
                )
                if app.dockerContexts.isEmpty {
                    Text("尚未检测到 Docker 环境；启动 OrbStack 或 Docker Desktop 后点击“重新检测”。")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                } else {
                    ForEach(app.dockerContexts) { context in
                        HStack(alignment: .firstTextBaseline, spacing: 8) {
                            VStack(alignment: .leading, spacing: 2) {
                                Text(context.name).font(.callout.weight(.medium))
                                Text(context.endpoint ?? "")
                                    .font(.caption.monospaced())
                                    .foregroundStyle(.secondary)
                                    .lineLimit(1)
                                    .truncationMode(.middle)
                            }
                            Spacer()
                            if context.current == true {
                                StatusBadge(text: "当前", color: .green, systemImage: "checkmark.circle.fill")
                            } else if context.reachable == true {
                                Button("使用") {
                                    Task { await app.selectDockerContext(context.name) }
                                }
                                .controlSize(.small)
                            } else {
                                StatusBadge(text: "不可达", color: .secondary, systemImage: "xmark.circle")
                            }
                        }
                    }
                }
                HStack {
                    Button {
                        Task { await app.loadDockerContexts() }
                    } label: {
                        Label("重新检测", systemImage: "arrow.clockwise")
                    }
                    Button {
                        app.openDockerApp()
                    } label: {
                        Label("打开 Docker", systemImage: "arrow.up.forward.app")
                    }
                    Spacer()
                    Text("不会修改 Docker 全局 Context，只看 OrbSleep 自己的选择。")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
        }
    }

    // MARK: - 旧版迁移

    private var migrationCard: some View {
        Card("旧版安装迁移", systemImage: "arrow.triangle.2.circlepath") {
            VStack(alignment: .leading, spacing: 8) {
                if app.legacyInstallation.isPresent {
                    Text("检测到：\(app.legacyInstallation.summary)")
                        .font(.callout)
                    Text("迁移保留原数据目录、托管记录、备份和操作历史。")
                        .font(.caption)
                        .foregroundStyle(.secondary)
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
                } else {
                    Text("未检测到旧版 /usr/local/bin 安装或旧 LaunchAgent。")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                }
            }
        }
    }

    // MARK: - 头部

    private var header: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("诊断与修复")
                .font(.largeTitle.weight(.semibold))
            Text("自动检测运行环境与后台服务状态；出现问题时可在这里一键处理。")
                .font(.callout)
                .foregroundStyle(.secondary)
            HStack(spacing: 8) {
                StatusBadge(
                    text: app.daemonReachable ? "后台服务已连接" : "后台服务未连接",
                    color: app.daemonReachable ? .green : .red,
                    systemImage: app.daemonReachable ? "checkmark.circle.fill" : "xmark.circle.fill"
                )
                StatusBadge(
                    text: app.currentServiceState.title,
                    color: serviceStateColor(app.currentServiceState),
                    systemImage: "gearshape.2"
                )
                if app.serviceVersionMismatch {
                    StatusBadge(text: "后台版本 \(app.status?.daemonVersion ?? "?") ≠ 应用 \(app.appVersion)", color: .orange, systemImage: "arrow.triangle.2.circlepath")
                }
                if app.serviceNeedsApproval {
                    StatusBadge(text: "等待系统授权", color: .orange, systemImage: "lock.fill")
                }
            }
        }
    }

    // MARK: - 后台服务

    private func serviceStateColor(_ state: BackgroundServiceState) -> Color {
        switch state {
        case .running: return .green
        case .starting, .upgrading: return .blue
        case .awaitingApproval, .versionMismatch, .recoveryRequired: return .orange
        case .failed: return .red
        case .notInstalled, .stopped: return .secondary
        }
    }

    private var serviceCard: some View {
        Card("后台服务", systemImage: "gearshape.2") {
            VStack(alignment: .leading, spacing: 10) {
                LabeledValue(label: "应用版本", value: app.appVersion.isEmpty ? "未知" : app.appVersion)
                LabeledValue(label: "服务版本", value: app.status?.daemonVersion ?? "未连接")
                LabeledValue(label: "注册状态", value: app.backgroundService.statusText)
                LabeledValue(label: "Socket", value: app.socketPath, monospaced: true)
                Divider()
                HStack(spacing: 10) {
                    Button {
                        Task { await app.startService() }
                    } label: {
                        Label("启动服务", systemImage: "play.circle")
                    }
                    .disabled(app.isBusyService || app.daemonReachable)

                    Button {
                        Task { await app.stopService() }
                    } label: {
                        Label("停止服务", systemImage: "stop.circle")
                    }
                    .disabled(app.isBusyService || !app.daemonReachable)

                    Button {
                        Task { await app.restartService() }
                    } label: {
                        Label("重启服务", systemImage: "arrow.triangle.2.circlepath")
                    }
                    .disabled(app.isBusyService)

                    if app.serviceVersionMismatch {
                        Button {
                            Task { await app.upgradeService() }
                        } label: {
                            Label("升级后台服务", systemImage: "arrow.up.circle")
                        }
                        .disabled(app.isBusyService)
                    }

                    if app.isBusyService {
                        ProgressView().controlSize(.small)
                    }
                    Spacer()
                }
            }
        }
    }

    // MARK: - 自检

    private var doctorCard: some View {
        Card("系统自检", systemImage: "stethoscope") {
            VStack(alignment: .leading, spacing: 10) {
                if app.isLoadingDoctor {
                    HStack(spacing: 8) {
                        ProgressView().controlSize(.small)
                        Text("正在检查…").font(.caption).foregroundStyle(.secondary)
                    }
                } else if app.doctorChecks.isEmpty {
                    Text("暂无自检结果")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                } else {
                    ForEach(doctorGroups, id: \.0) { group in
                        Text(group.0)
                            .font(.caption.weight(.semibold))
                            .foregroundStyle(.secondary)
                            .textCase(.uppercase)
                            .padding(.top, 2)
                        ForEach(Array(group.1.enumerated()), id: \.offset) { _, check in
                            doctorRow(check)
                        }
                    }
                }
                Divider()
                HStack(spacing: 10) {
                    Button {
                        Task { await app.loadDoctor() }
                    } label: {
                        Label("重新检查", systemImage: "stethoscope")
                    }
                    Button {
                        Task { await app.repairService() }
                    } label: {
                        Label("一键修复", systemImage: "wrench.and.screwdriver")
                    }
                    .disabled(app.isBusyService)
                    Button {
                        app.exportDiagnostics()
                    } label: {
                        Label("导出诊断报告", systemImage: "square.and.arrow.up")
                    }
                    Spacer()
                }
            }
        }
    }

    private var doctorGroups: [(String, [DoctorCheck])] {
        var order: [String] = []
        var map: [String: [DoctorCheck]] = [:]
        func category(_ id: String?) -> String {
            let value = id ?? ""
            if ["docker", "context", "compose"].contains(value) { return "Docker / 环境" }
            if value.hasPrefix("proxy") { return "代理" }
            if ["data_dir", "backup_dir", "launch_agent", "low_ports", "foreign_proxy"].contains(value) {
                return "系统"
            }
            return "其他"
        }
        for check in app.doctorChecks {
            let key = category(check.id)
            if map[key] == nil {
                order.append(key)
                map[key] = []
            }
            map[key]?.append(check)
        }
        return order.map { ($0, map[$0] ?? []) }
    }

    private func doctorRow(_ check: DoctorCheck) -> some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: check.statusValue.systemImage)
                .foregroundStyle(check.statusValue.color)
                .frame(width: 18)
            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: 8) {
                    Text(check.displayTitle)
                        .font(.callout.weight(.medium))
                    StatusBadge(doctorStatus: check.statusValue)
                }
                if let detail = check.detail, !detail.isEmpty {
                    Text(detail)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }
                if let remediation = check.remediationText {
                    Text("修复建议：\(remediation)")
                        .font(.caption)
                        .foregroundStyle(.orange)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
            Spacer(minLength: 0)
        }
    }

    // MARK: - 事务恢复

    private var recoveryCard: some View {
        Card("事务恢复", systemImage: "arrow.uturn.backward.circle") {
            VStack(alignment: .leading, spacing: 10) {
                Text("接入 / 恢复事务在写入前会持久化到 SQLite。后台服务在下次启动时会先回滚未完成的事务，再恢复端口监听。")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                if app.recoveryTransactions.isEmpty {
                    Label("当前没有未完成事务", systemImage: "checkmark.circle.fill")
                        .font(.callout)
                        .foregroundStyle(.green)
                } else {
                    ForEach(app.recoveryTransactions) { txn in
                        HStack(alignment: .top, spacing: 8) {
                            Image(systemName: "exclamationmark.triangle.fill")
                                .foregroundStyle(.orange)
                            VStack(alignment: .leading, spacing: 2) {
                                Text("\(txn.kind ?? "事务") · \(txn.id)")
                                    .font(.callout.weight(.medium))
                                Text("阶段 \(txn.phase ?? "-") · 状态 \(txn.status ?? "-")")
                                    .font(.caption)
                                    .foregroundStyle(.secondary)
                                if let error = txn.error, !error.isEmpty {
                                    Text(error).font(.caption).foregroundStyle(.red)
                                }
                            }
                            Spacer()
                        }
                    }
                }
                HStack(spacing: 10) {
                    Button {
                        Task { await app.resolveRecovery() }
                    } label: {
                        Label("执行恢复", systemImage: "play.circle")
                    }
                    .disabled(app.isBusyMaintenance || app.recoveryTransactions.isEmpty)
                    Button {
                        Task { await app.restartService() }
                    } label: {
                        Label("重启后台服务", systemImage: "arrow.triangle.2.circlepath")
                    }
                    .disabled(app.isBusyService)
                    Button {
                        Task { await app.loadRecovery() }
                    } label: {
                        Label("刷新", systemImage: "arrow.clockwise")
                    }
                    Spacer()
                }
            }
        }
    }

    // MARK: - 备份

    private var backupCard: some View {
        Card("备份", systemImage: "archivebox") {
            VStack(alignment: .leading, spacing: 10) {
                HStack(spacing: 10) {
                    Button {
                        Task { await app.createBackup() }
                    } label: {
                        Label("立即备份", systemImage: "square.and.arrow.down")
                    }
                    .disabled(app.isBusyMaintenance || !app.daemonReachable)
                    Button {
                        Task { await app.loadBackups() }
                    } label: {
                        Label("刷新", systemImage: "arrow.clockwise")
                    }
                    if app.isBusyMaintenance {
                        ProgressView().controlSize(.small)
                    }
                    Spacer()
                    Text("备份包含数据库快照与设置，保存在备份目录/export。")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                if app.backups.isEmpty {
                    Text("暂无手动备份")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                } else {
                    ForEach(app.backups) { entry in
                        HStack(spacing: 8) {
                            Text(entry.id)
                                .font(.caption.monospaced())
                                .lineLimit(1)
                            Spacer()
                            Text(formatBytes(entry.sizeBytes ?? 0))
                                .font(.caption)
                                .foregroundStyle(.secondary)
                            Button("显示") {
                                if let path = entry.path { app.openPath(path) }
                            }
                            .buttonStyle(.link)
                            .font(.caption)
                            Button("恢复…") {
                                restoreTarget = entry
                            }
                            .buttonStyle(.link)
                            .font(.caption)
                            .disabled(app.isBusyMaintenance)
                        }
                    }
                }
            }
        }
    }

    private func formatBytes(_ bytes: Int64) -> String {
        let formatter = ByteCountFormatter()
        formatter.countStyle = .file
        return formatter.string(fromByteCount: bytes)
    }

    // MARK: - 卸载

    private var uninstallCard: some View {
        Card("卸载", systemImage: "trash") {
            VStack(alignment: .leading, spacing: 8) {
                LabeledValue(label: "托管服务", value: "\(app.services.count)")
                LabeledValue(label: "未完成事务", value: "\(app.recoveryTransactions.count)")
                LabeledValue(label: "代理", value: app.proxyRunning ? "运行中" : (app.proxyInstalled ? "未运行" : "未安装"))
                Text("卸载前会先恢复所有托管服务；只要仍有服务未恢复就会中止，不会删除业务数据卷或镜像。")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                HStack(spacing: 10) {
                    Button(role: .destructive) {
                        showUninstallConfirmation = true
                    } label: {
                        Label("卸载 OrbSleep Manager…", systemImage: "trash")
                    }
                    .disabled(app.isUninstalling)
                    if app.isUninstalling {
                        ProgressView().controlSize(.small)
                    }
                }
            }
        }
    }
}
