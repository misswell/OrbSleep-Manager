import SwiftUI

struct SettingsView: View {
    @Environment(AppState.self) private var app

    @State private var draft = SettingsDraft()
    @State private var savedMessage: String?
    @State private var showUninstallServiceConfirmation = false
    @State private var showStopProxyConfirmation = false

    var body: some View {
        Form {
            if let savedMessage {
                Section {
                    Label(savedMessage, systemImage: "checkmark.circle.fill")
                        .foregroundStyle(.green)
                        .font(.callout)
                }
            }

            environmentSection
            sleepSection
            proxySection
            serviceSection
            cliSection
            excludeSection
            dataSection
            pathsSection
            doctorSection
            updateLinkSection
            aboutSection
        }
        .formStyle(.grouped)
        .navigationTitle("设置")
        .confirmationDialog("停止代理容器", isPresented: $showStopProxyConfirmation) {
            Button("停止并移除代理容器", role: .destructive) {
                Task { await app.stopProxy() }
            }
            Button("取消", role: .cancel) {}
        } message: {
            Text("代理容器会被停止并移除，配置、设置和托管记录保留。停止后新连接无法唤醒托管容器；可在修改端口设置后重新安装启动。")
        }
        .confirmationDialog("卸载后台服务", isPresented: $showUninstallServiceConfirmation) {
            Button("停止并移除后台服务", role: .destructive) {
                Task { await app.uninstallBackgroundService() }
            }
            Button("取消", role: .cancel) {}
        } message: {
            Text("会先恢复所有托管服务的原始端口映射，再停止后台服务、移除开机启动项和可选的命令行工具。数据目录会保留。")
        }
        .toolbar {
            ToolbarItemGroup(placement: .primaryAction) {
                if app.isSavingSettings {
                    ProgressView()
                        .controlSize(.small)
                }
                Button {
                    Task {
                        await app.loadSettings()
                        await app.loadDoctor()
                        syncDraft()
                    }
                } label: {
                    Label("重新加载", systemImage: "arrow.clockwise")
                }
                Button {
                    save()
                } label: {
                    Label("保存", systemImage: "square.and.arrow.down")
                }
                .disabled(!isDirty || app.isSavingSettings)
            }
        }
        .task {
            guard app.daemonReachable else { return }
            if app.settings == nil {
                await app.loadSettings()
            }
            if app.doctorChecks.isEmpty {
                await app.loadDoctor()
            }
            if app.settings != nil {
                syncDraft()
            }
        }
        .onChange(of: app.settings) { _, _ in
            if !isDirty {
                syncDraft()
            }
        }
    }

    // MARK: - 运行环境

    private var environmentSection: some View {
        Section {
            LabeledValue(
                label: "当前 Docker",
                value: app.health?.dockerContext?.name ?? app.status?.context?.name ?? "—"
            )
            LabeledValue(
                label: "Socket",
                value: app.status?.context?.endpoint ?? "—",
                monospaced: true
            )
            ForEach(app.dockerContexts) { context in
                HStack(spacing: 8) {
                    VStack(alignment: .leading, spacing: 2) {
                        Text(context.name).font(.callout)
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
            HStack {
                Button("重新检测") { Task { await app.loadDockerContexts() } }
                Button("打开 Docker") { app.openDockerApp() }
                Spacer()
            }
        } header: {
            Text("运行环境")
        }
        .task { await app.loadDockerContexts() }
    }

    // MARK: - 数据管理

    private var dataSection: some View {
        Section {
            LabeledValue(label: "数据目录", value: app.dataDir, monospaced: true)
            LabeledValue(label: "备份目录", value: app.settings?.backupDir ?? "—", monospaced: true)
            LabeledValue(label: "手动备份", value: "\(app.backups.count)")
            HStack(spacing: 10) {
                Button("立即备份") { Task { await app.createBackup() } }
                Button("打开备份目录") {
                    if let path = app.settings?.backupDir { app.openPath(path) }
                }
                Button("打开数据目录") { app.openDataDirectory() }
                Spacer()
            }
        } header: {
            Text("数据管理")
        }
        .task { await app.loadBackups() }
    }

    // MARK: - 更新入口

    private var updateLinkSection: some View {
        Section {
            HStack {
                Text("应用更新、后台服务升级与失败回滚在「更新与维护」中。")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Spacer()
                Button("打开更新与维护") { app.section = .update }
            }
        } header: {
            Text("更新")
        }
    }

    // MARK: - 关于

    private var aboutSection: some View {
        Section {
            LabeledValue(label: "应用版本", value: app.appVersion.isEmpty ? "未知" : app.appVersion)
            LabeledValue(label: "后台版本", value: app.status?.daemonVersion ?? "—")
            LabeledValue(label: "Bundle ID", value: Bundle.main.bundleIdentifier ?? "—", monospaced: true)
            LabeledValue(label: "安装位置", value: Bundle.main.bundleURL.path, monospaced: true)
            LabeledValue(label: "签名", value: app.signingSummary)
            LabeledValue(label: "公证", value: app.notarizationSummary)
        } header: {
            Text("关于")
        }
    }

    // MARK: - 休眠与扫描

    private var sleepSection: some View {
        Section {
            LabeledContent("空闲超时") {
                HStack(spacing: 6) {
                    TextField("秒", value: $draft.idleTimeoutSecs, format: .number.grouping(.never))
                        .textFieldStyle(.roundedBorder)
                        .multilineTextAlignment(.trailing)
                        .frame(width: 110)
                    Text("秒")
                        .foregroundStyle(.secondary)
                }
            }
            Text("容器空闲超过该时长后会被代理停止，下一次连接会自动拉起。当前约为 \(DurationText.short(draft.idleTimeoutSecs))。")
                .font(.caption)
                .foregroundStyle(.secondary)

            LabeledContent("启动超时") {
                HStack(spacing: 6) {
                    TextField("秒", value: $draft.startTimeoutSecs, format: .number.grouping(.never))
                        .textFieldStyle(.roundedBorder)
                        .multilineTextAlignment(.trailing)
                        .frame(width: 110)
                    Text("秒")
                        .foregroundStyle(.secondary)
                }
            }
            Text("等待容器启动并可以接受连接的最长时间。")
                .font(.caption)
                .foregroundStyle(.secondary)

            LabeledContent("日志保留") {
                HStack(spacing: 6) {
                    TextField("天", value: $draft.logRetentionDays, format: .number.grouping(.never))
                        .textFieldStyle(.roundedBorder)
                        .multilineTextAlignment(.trailing)
                        .frame(width: 110)
                    Text("天")
                        .foregroundStyle(.secondary)
                }
            }

            Toggle("启动时自动扫描（autoScanOnLaunch）", isOn: $draft.autoScanOnLaunch)
            Text("开启后每次启动 GUI 会自动扫描一次容器；关闭时只有手动点击「扫描」才会读取 Docker 容器。轮询不会触发扫描。")
                .font(.caption)
                .foregroundStyle(.secondary)

            Toggle("启用低端口辅助程序（尚未实现）", isOn: $draft.enableLowPortHelper)
                .disabled(true)
            Text("特权端口助手尚未实现；低于 1024 的宿主机端口始终被排除。")
                .font(.caption)
                .foregroundStyle(.secondary)
        } header: {
            Text("休眠与扫描")
        }
    }

    // MARK: - 代理

    private var proxySection: some View {
        Section {
            HStack(spacing: 8) {
                if app.proxyRunning {
                    StatusBadge(text: "代理运行中", color: .green, systemImage: "play.circle.fill")
                } else if app.proxyInstalled {
                    StatusBadge(text: "代理未运行", color: .orange, systemImage: "pause.circle.fill")
                } else {
                    StatusBadge(text: "代理未安装", color: .red, systemImage: "xmark.circle")
                }
                if let proxy = app.status?.proxy {
                    if proxy.adminReachable == true {
                        StatusBadge(text: "管理 API 可达", color: .green, systemImage: "checkmark.circle")
                    } else {
                        StatusBadge(text: "管理 API 不可达", color: .orange, systemImage: "exclamationmark.triangle")
                    }
                    if proxy.metricsReachable == true {
                        StatusBadge(text: "指标 API 可达", color: .green, systemImage: "chart.bar")
                    }
                }
                Spacer()
                Button {
                    Task { await app.setupProxy() }
                } label: {
                    Label("安装 / 启动代理", systemImage: "shippingbox.and.arrow.backward")
                }
                .disabled(app.hasOperationInProgress)
                Button(role: .destructive) {
                    showStopProxyConfirmation = true
                } label: {
                    Label("停止代理", systemImage: "stop.circle")
                }
                .disabled(!app.proxyRunning || app.hasOperationInProgress)
            }

            if let name = app.status?.proxy?.containerName, !name.isEmpty {
                LabeledValue(label: "容器名", value: name, monospaced: true)
            }
            if let range = app.status?.proxy?.publishedRange, let start = range.start, let end = range.end {
                LabeledValue(label: "已发布端口", value: "\(start)–\(end)")
            }
            if let message = app.status?.proxy?.errorText {
                Text(message)
                    .font(.callout)
                    .foregroundStyle(.red)
            }

            LabeledContent("代理端口起始") {
                TextField("端口", value: $draft.proxyPortStart, format: .number.grouping(.never))
                    .textFieldStyle(.roundedBorder)
                    .multilineTextAlignment(.trailing)
                    .frame(width: 110)
                    .disabled(app.proxyInstalled)
            }
            LabeledContent("代理端口结束") {
                TextField("端口", value: $draft.proxyPortEnd, format: .number.grouping(.never))
                    .textFieldStyle(.roundedBorder)
                    .multilineTextAlignment(.trailing)
                    .frame(width: 110)
                    .disabled(app.proxyInstalled)
            }
            if portRangeChanged {
                Label("代理安装后端口设置会被锁定；修改范围需要先停止代理容器再重新安装。", systemImage: "exclamationmark.triangle.fill")
                    .font(.caption)
                    .foregroundStyle(.orange)
            }

            LabeledContent("管理端口") {
                TextField("端口", value: $draft.adminPort, format: .number.grouping(.never))
                    .textFieldStyle(.roundedBorder)
                    .multilineTextAlignment(.trailing)
                    .frame(width: 110)
                    .disabled(app.proxyInstalled)
            }
            LabeledContent("Web 端口") {
                TextField("端口", value: $draft.webPort, format: .number.grouping(.never))
                    .textFieldStyle(.roundedBorder)
                    .multilineTextAlignment(.trailing)
                    .frame(width: 110)
                    .disabled(app.proxyInstalled)
            }
            if app.proxyInstalled {
                Text("代理已安装：端口设置已锁定，需先停止代理容器才能修改。")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }

            LabeledContent("拨号模式") {
                TextField("published", text: $draft.proxyDialMode)
                    .textFieldStyle(.roundedBorder)
                    .frame(width: 160)
            }
            Text("当前仅支持 published（代理端口发布到 127.0.0.1）。container_ip 需先探测可达，否则保存会被拒绝。")
                .font(.caption)
                .foregroundStyle(.secondary)
            Text("默认 published：通过代理容器已发布的端口拨号。修改前请确认后台支持该模式。")
                .font(.caption)
                .foregroundStyle(.secondary)
        } header: {
            Text("端口代理")
        }
    }

    // MARK: - 后台服务

    private var serviceSection: some View {
        Section {
            HStack(spacing: 8) {
                if app.daemonReachable {
                    StatusBadge(text: "运行中", color: .green, systemImage: "checkmark.circle.fill")
                } else if app.backgroundService.installed {
                    StatusBadge(text: "已注册但未运行", color: .orange, systemImage: "exclamationmark.triangle.fill")
                } else {
                    StatusBadge(text: "未注册", color: .orange, systemImage: "exclamationmark.triangle.fill")
                }
                Text("后台服务随应用包一起分发，由 launchd 管理，登录后自动启动。")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Spacer(minLength: 8)
                if app.isBusyService {
                    ProgressView().controlSize(.small)
                }
            }

            LabeledValue(label: "应用版本", value: app.appVersion.isEmpty ? "未知" : app.appVersion)
            LabeledValue(label: "服务版本", value: app.status?.daemonVersion ?? "未连接")
            LabeledValue(label: "注册状态", value: app.backgroundService.statusText)
            Toggle("登录后自动启动后台服务", isOn: Binding(
                get: { app.backgroundService.installed },
                set: { enabled in Task { await app.setAutoStart(enabled) } }
            ))
            Text("与是否打开 GUI 分离：关闭 GUI 后后台服务仍可继续运行。")
                .font(.caption)
                .foregroundStyle(.secondary)
            Toggle("登录后自动打开 OrbSleep Manager", isOn: Binding(
                get: { app.launchesGUIAtLogin },
                set: { enabled in Task { await app.setLaunchGUIAtLogin(enabled) } }
            ))
            Text("只控制 GUI 是否随登录打开，不影响后台服务。")
                .font(.caption)
                .foregroundStyle(.secondary)

            HStack(spacing: 10) {
                Button {
                    Task { await app.startService() }
                } label: {
                    Label("启动", systemImage: "play.circle")
                }
                .disabled(app.isBusyService || app.daemonReachable)

                Button {
                    Task { await app.stopService() }
                } label: {
                    Label("停止", systemImage: "stop.circle")
                }
                .disabled(app.isBusyService || !app.daemonReachable)

                Button {
                    Task { await app.restartService() }
                } label: {
                    Label("重启", systemImage: "arrow.triangle.2.circlepath")
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

                Spacer()

                Button(role: .destructive) {
                    showUninstallServiceConfirmation = true
                } label: {
                    Label("卸载后台服务…", systemImage: "trash")
                }
                .disabled(app.isBusyService)
            }

            if app.serviceNeedsApproval {
                HStack(spacing: 8) {
                    Label("后台服务需要在系统设置中允许。", systemImage: "lock.fill")
                        .font(.caption)
                        .foregroundStyle(.orange)
                    Button("打开登录项设置") {
                        app.backgroundService.openLoginItemsSettings()
                    }
                    .buttonStyle(.link)
                    .font(.caption)
                }
            }
        } header: {
            Text("后台服务")
        }
    }

    // MARK: - 命令行工具

    private var cliSection: some View {
        Section {
            HStack(spacing: 8) {
                if app.cliInstalled {
                    StatusBadge(text: "已安装", color: .green, systemImage: "checkmark.circle.fill")
                } else {
                    StatusBadge(text: "未安装", color: .secondary, systemImage: "circle")
                }
                Text("命令行工具是可选的开发者入口，普通使用不需要它。")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Spacer(minLength: 8)
                if app.cliInstalled {
                    Button("卸载命令行工具") {
                        app.removeCommandLineTools()
                    }
                } else {
                    Button("安装命令行工具") {
                        app.installCommandLineTools()
                    }
                }
            }
            Text("安装位置：~/.local/bin/orbsleepctl（用户级，无需 sudo）。")
                .font(.caption)
                .foregroundStyle(.secondary)
        } header: {
            Text("命令行工具（可选）")
        }
    }

    // MARK: - 排除镜像

    private var excludeSection: some View {
        Section {
            ForEach(draft.excludeImagePatterns.indices, id: \.self) { index in
                HStack(spacing: 8) {
                    TextField("镜像关键词", text: $draft.excludeImagePatterns[index])
                        .textFieldStyle(.roundedBorder)
                    Button {
                        guard draft.excludeImagePatterns.indices.contains(index) else { return }
                        draft.excludeImagePatterns.remove(at: index)
                    } label: {
                        Image(systemName: "minus.circle")
                    }
                    .buttonStyle(.borderless)
                    .foregroundStyle(.red)
                    .help("删除该关键词")
                }
            }
            HStack {
                Button {
                    draft.excludeImagePatterns.append("")
                } label: {
                    Label("添加关键词", systemImage: "plus.circle")
                }
                .buttonStyle(.borderless)
                Spacer()
                Text("匹配镜像名的容器会被标记为「需确认」，仍需逐项确认后才能接入。")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        } header: {
            Text("排除镜像（excludeImagePatterns）")
        }
    }

    // MARK: - 路径

    private var pathsSection: some View {
        Section {
            pathRow(
                label: "数据目录",
                value: app.settings?.dataDir ?? app.dataDir,
                actionTitle: "打开"
            ) {
                app.openDataDirectory()
            }
            pathRow(
                label: "备份目录",
                value: app.settings?.backupDir ?? "—",
                actionTitle: "打开"
            ) {
                if let path = app.settings?.backupDir {
                    app.openPath(path)
                }
            }
            pathRow(
                label: "Socket 路径",
                value: app.settings?.socketPath ?? app.socketPath,
                actionTitle: "打开所在目录"
            ) {
                let path = app.settings?.socketPath ?? app.socketPath
                app.openPath(URL(fileURLWithPath: path).deletingLastPathComponent().path)
            }
            LabeledValue(label: "后台服务", value: app.backgroundService.statusText)
        } header: {
            Text("路径")
        }
    }

    private func pathRow(
        label: String,
        value: String,
        actionTitle: String,
        action: @escaping () -> Void
    ) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text(label)
                .foregroundStyle(.secondary)
                .frame(width: 96, alignment: .leading)
            Text(value.isEmpty ? "—" : value)
                .font(.callout.monospaced())
                .textSelection(.enabled)
                .lineLimit(2)
                .truncationMode(.middle)
            Spacer(minLength: 4)
            Button(actionTitle, action: action)
                .buttonStyle(.link)
                .font(.caption)
        }
    }

    // MARK: - 自检

    private var doctorSection: some View {
        Section {
            if app.isLoadingDoctor {
                HStack(spacing: 8) {
                    ProgressView()
                        .controlSize(.small)
                    Text("正在检查…")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
            if app.doctorChecks.isEmpty, !app.isLoadingDoctor {
                Text("暂无自检结果")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }
            ForEach(Array(app.doctorChecks.enumerated()), id: \.offset) { _, check in
                doctorRow(check)
            }
            HStack {
                Button {
                    Task { await app.loadDoctor() }
                } label: {
                    Label("重新检查", systemImage: "stethoscope")
                }
                .buttonStyle(.borderless)
                Spacer()
            }
        } header: {
            Text("系统自检（system.doctor）")
        }
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

    // MARK: - 状态

    private var isDirty: Bool {
        !draft.patch(comparedTo: app.settings).isEmpty
    }

    private var portRangeChanged: Bool {
        guard let settings = app.settings else { return false }
        return draft.proxyPortStart != (settings.proxyPortStart ?? 20_000)
            || draft.proxyPortEnd != (settings.proxyPortEnd ?? 20_099)
    }

    private func syncDraft() {
        draft = SettingsDraft(settings: app.settings)
    }

    private func save() {
        let patch = draft.patch(comparedTo: app.settings)
        guard !patch.isEmpty else { return }
        guard draft.proxyPortStart < draft.proxyPortEnd else {
            app.showError("代理端口起始必须小于结束端口")
            return
        }
        guard draft.idleTimeoutSecs > 0, draft.startTimeoutSecs > 0, draft.logRetentionDays > 0 else {
            app.showError("超时与日志保留天数必须大于 0")
            return
        }
        savedMessage = nil
        Task {
            let succeeded = await app.updateSettings(patch)
            if succeeded {
                syncDraft()
                savedMessage = "设置已保存"
                try? await Task.sleep(nanoseconds: 2_500_000_000)
                if savedMessage == "设置已保存" {
                    savedMessage = nil
                }
            }
        }
    }
}
