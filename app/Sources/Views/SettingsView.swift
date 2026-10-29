import SwiftUI

struct SettingsView: View {
    @Environment(AppState.self) private var app

    @State private var draft = SettingsDraft()
    @State private var savedMessage: String?
    @State private var showUninstallAgentConfirmation = false

    var body: some View {
        Form {
            if let savedMessage {
                Section {
                    Label(savedMessage, systemImage: "checkmark.circle.fill")
                        .foregroundStyle(.green)
                        .font(.callout)
                }
            }

            sleepSection
            proxySection
            agentSection
            excludeSection
            pathsSection
            doctorSection
        }
        .formStyle(.grouped)
        .navigationTitle("设置")
        .confirmationDialog("卸载 LaunchAgent", isPresented: $showUninstallAgentConfirmation) {
            Button("卸载并停止后台助手", role: .destructive) {
                Task { await app.uninstallLaunchAgent() }
            }
            Button("取消", role: .cancel) {}
        } message: {
            Text("卸载后 orbsleepd 不再随登录启动；如果后台助手退出，GUI 会断开连接。需要时可以重新安装，或执行 orbsleepctl install-agent。")
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

            Toggle("启用低端口辅助程序（enableLowPortHelper）", isOn: $draft.enableLowPortHelper)
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
            }
            LabeledContent("代理端口结束") {
                TextField("端口", value: $draft.proxyPortEnd, format: .number.grouping(.never))
                    .textFieldStyle(.roundedBorder)
                    .multilineTextAlignment(.trailing)
                    .frame(width: 110)
            }
            if portRangeChanged {
                Label("端口范围已修改，保存后需要重启代理容器才会生效。", systemImage: "exclamationmark.triangle.fill")
                    .font(.caption)
                    .foregroundStyle(.orange)
            }

            LabeledContent("管理端口") {
                TextField("端口", value: $draft.adminPort, format: .number.grouping(.never))
                    .textFieldStyle(.roundedBorder)
                    .multilineTextAlignment(.trailing)
                    .frame(width: 110)
            }
            LabeledContent("Web 端口") {
                TextField("端口", value: $draft.webPort, format: .number.grouping(.never))
                    .textFieldStyle(.roundedBorder)
                    .multilineTextAlignment(.trailing)
                    .frame(width: 110)
            }

            LabeledContent("拨号模式") {
                TextField("published", text: $draft.proxyDialMode)
                    .textFieldStyle(.roundedBorder)
                    .frame(width: 160)
            }
            Text("默认 published：通过代理容器已发布的端口拨号。修改前请确认后台支持该模式。")
                .font(.caption)
                .foregroundStyle(.secondary)
        } header: {
            Text("端口代理")
        }
    }

    // MARK: - LaunchAgent

    private var agentSection: some View {
        Section {
            HStack(spacing: 8) {
                if app.launchAgentInstalled {
                    StatusBadge(text: "已安装", color: .green, systemImage: "checkmark.circle.fill")
                } else {
                    StatusBadge(text: "未安装", color: .orange, systemImage: "exclamationmark.triangle.fill")
                }
                Text("LaunchAgent 负责在登录后自动拉起 orbsleepd。")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Spacer(minLength: 8)
                if app.isTogglingAgent {
                    ProgressView()
                        .controlSize(.small)
                }
                Button(app.launchAgentInstalled ? "卸载 LaunchAgent…" : "安装 LaunchAgent") {
                    if app.launchAgentInstalled {
                        showUninstallAgentConfirmation = true
                    } else {
                        Task { await app.installLaunchAgent() }
                    }
                }
                .disabled(app.isTogglingAgent)
            }
            Text("也可以在终端执行 orbsleepctl install-agent。GUI 不会自行启动或停止后台助手。")
                .font(.caption)
                .foregroundStyle(.secondary)
        } header: {
            Text("开机启动（agent.install / agent.uninstall）")
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
                Text("匹配镜像名的容器会被判定为排除，不会出现在接入建议中。")
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
            LabeledValue(label: "LaunchAgent", value: app.settings?.launchAgentInstalled == true ? "已安装" : "未安装")
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
