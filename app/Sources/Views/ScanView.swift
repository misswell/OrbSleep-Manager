import SwiftUI

struct ScanView: View {
    @Environment(AppState.self) private var app

    @State private var selectedID: String?
    @State private var selection: Set<String> = []
    @State private var detail: ContainerInspectResult?
    @State private var detailError: String?
    @State private var isLoadingDetail = false

    var body: some View {
        VStack(spacing: 0) {
            filterBar
            Divider()
            if app.scanResult == nil {
                EmptyStateView(
                    systemImage: "magnifyingglass",
                    title: "还没有扫描结果",
                    message: "扫描会读取当前 Docker 上下文中的容器与 Compose 信息，判断哪些容器可以安全接入按需唤醒代理。",
                    actionTitle: app.isScanning ? "扫描中…" : "扫描容器",
                    action: {
                        Task { await app.scan() }
                    }
                )
            } else {
                content
            }
        }
        .navigationTitle("扫描容器")
        .toolbar {
            ToolbarItemGroup(placement: .primaryAction) {
                if app.isCreatingPlan {
                    ProgressView()
                        .controlSize(.small)
                }
                Button {
                    Task { await app.scan() }
                } label: {
                    Label("扫描", systemImage: "arrow.clockwise")
                }
                .disabled(app.isScanning)
            }
        }
        .onChange(of: selectedID) { _, newValue in
            loadDetail(newValue)
        }
        .onChange(of: app.scanResult) { _, newValue in
            let containers = newValue?.containers ?? []
            let selectable = Set(containers.filter { $0.verdict.isSelectable }.map(\.id))
            selection.formIntersection(selectable)
            if let current = selectedID, !containers.contains(where: { $0.id == current }) {
                selectedID = nil
            }
        }
    }

    // MARK: - 筛选栏

    private var filterBar: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 12) {
                searchField

                Picker("项目", selection: projectBinding) {
                    Text("全部项目").tag(String?.none)
                    ForEach(projects, id: \.self) { project in
                        Text(project).tag(String?.some(project))
                    }
                }
                .frame(width: 170)

                Picker("判断", selection: verdictBinding) {
                    ForEach(VerdictFilter.allCases) { filter in
                        Text(filter.title).tag(filter)
                    }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                .frame(width: 290)

                Spacer(minLength: 8)

                Button {
                    Task { await app.scan() }
                } label: {
                    Label(app.isScanning ? "扫描中…" : "扫描", systemImage: "magnifyingglass")
                }
                .disabled(app.isScanning)

                Button {
                    Task {
                        // 计划层先允许 LAN 暴露，真正的执行确认由 PlanView 中逐项的
                                // 「我确认允许该服务继续暴露在局域网」复选框门控。
                                await app.createPlan(containerIDs: Array(selection), allowLanExposure: true)
                    }
                } label: {
                    Label("生成接入计划", systemImage: "list.clipboard")
                }
                .buttonStyle(.borderedProminent)
                .disabled(selection.isEmpty || app.isCreatingPlan || app.isScanning)
            }

            HStack(spacing: 12) {
                Text("共 \(filtered.count) / \(allContainers.count) 个容器")
                if !selection.isEmpty {
                    Text("已选 \(selection.count) 个")
                }
                if needsReviewSelectionCount > 0 {
                    Label(
                        "\(needsReviewSelectionCount) 个「需确认」容器，请先核对原因再接入",
                        systemImage: "exclamationmark.triangle.fill"
                    )
                    .foregroundStyle(.orange)
                }
                Spacer()
                if let scannedAt = app.scanResult?.scannedAt {
                    Text("扫描于 \(RelativeTime.text(scannedAt))")
                }
            }
            .font(.caption)
            .foregroundStyle(.secondary)
        }
        .padding(12)
    }

    private var searchField: some View {
        HStack(spacing: 6) {
            Image(systemName: "magnifyingglass")
                .foregroundStyle(.secondary)
            TextField(
                "搜索名称 / 镜像 / 端口",
                text: Binding(
                    get: { app.scanSearch },
                    set: { app.scanSearch = $0 }
                )
            )
            .textFieldStyle(.plain)
            if !app.scanSearch.isEmpty {
                Button {
                    app.scanSearch = ""
                } label: {
                    Image(systemName: "xmark.circle.fill")
                }
                .buttonStyle(.borderless)
                .foregroundStyle(.secondary)
            }
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 6)
        .frame(minWidth: 220)
        .background(Color(nsColor: .textBackgroundColor), in: RoundedRectangle(cornerRadius: 6))
        .overlay(
            RoundedRectangle(cornerRadius: 6)
                .stroke(Color.secondary.opacity(0.22), lineWidth: 1)
        )
    }

    private var projectBinding: Binding<String?> {
        Binding(
            get: { app.scanProject },
            set: { app.scanProject = $0 }
        )
    }

    private var verdictBinding: Binding<VerdictFilter> {
        Binding(
            get: { app.scanFilter },
            set: { app.scanFilter = $0 }
        )
    }

    // MARK: - 主体

    private var content: some View {
        HStack(spacing: 0) {
            if filtered.isEmpty {
                EmptyStateView(
                    systemImage: "line.3.horizontal.decrease.circle",
                    title: "没有匹配的容器",
                    message: "试试调整搜索关键词、项目或判断筛选。"
                )
            } else {
                table
            }

            if let selected = selectedContainer {
                Divider()
                detailPanel(selected)
                    .frame(width: 340)
                    .background(Color(nsColor: .windowBackgroundColor))
            }
        }
    }

    private var table: some View {
        Table(filtered, selection: $selectedID) {
            TableColumn("") { container in
                selectionToggle(container)
            }
            .width(28)

            TableColumn("容器") { container in
                VStack(alignment: .leading, spacing: 2) {
                    Text(container.displayName)
                        .font(.callout.weight(.medium))
                        .lineLimit(1)
                    Text(container.image ?? "—")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
            }
            .width(min: 110, ideal: 210)

            TableColumn("项目 / 服务") { container in
                Text(container.projectServiceText)
                    .font(.callout)
                    .lineLimit(1)
                    .truncationMode(.tail)
            }
            .width(min: 80, ideal: 140)

            TableColumn("端口") { container in
                PortsText(ports: container.ports ?? [], maxLines: 3)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
            .width(min: 100, ideal: 180)

            TableColumn("状态") { container in
                StatusBadge(containerState: container.state ?? .unknown)
            }
            .width(72)

            TableColumn("判断") { container in
                StatusBadge(verdict: container.verdict)
            }
            .width(72)

            TableColumn("原因") { container in
                ReasonSummary(reasons: container.allReasons)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
            .width(min: 90, ideal: 190)

            TableColumn("操作") { container in
                rowActions(container)
            }
            .width(76)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    private func selectionToggle(_ container: Container) -> some View {
        Toggle(
            "",
            isOn: Binding(
                get: { selection.contains(container.id) },
                set: { isOn in
                    if isOn {
                        selection.insert(container.id)
                    } else {
                        selection.remove(container.id)
                    }
                }
            )
        )
        .toggleStyle(.checkbox)
        .labelsHidden()
        .disabled(!container.verdict.isSelectable)
        .help(container.verdict.isSelectable ? "勾选以加入接入计划" : "该容器被排除，无法接入")
    }

    @ViewBuilder
    private func rowActions(_ container: Container) -> some View {
        if container.isManaged {
            StatusBadge(text: "已托管", color: .blue, systemImage: "lock.fill")
        } else if container.verdict.isSelectable {
            Button(selection.contains(container.id) ? "取消" : "选择") {
                if selection.contains(container.id) {
                    selection.remove(container.id)
                } else {
                    selection.insert(container.id)
                }
            }
            .buttonStyle(.link)
            .font(.caption)
        } else {
            Text("—")
                .font(.caption)
                .foregroundStyle(.tertiary)
        }
    }

    // MARK: - 详情面板

    private func detailPanel(_ container: Container) -> some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                HStack(alignment: .top, spacing: 8) {
                    VStack(alignment: .leading, spacing: 3) {
                        Text(container.displayName)
                            .font(.headline)
                        Text(container.image ?? "—")
                            .font(.caption.monospaced())
                            .foregroundStyle(.secondary)
                            .textSelection(.enabled)
                            .lineLimit(2)
                    }
                    Spacer(minLength: 0)
                    Button {
                        selectedID = nil
                    } label: {
                        Image(systemName: "xmark.circle.fill")
                    }
                    .buttonStyle(.borderless)
                    .foregroundStyle(.secondary)
                    .help("关闭详情")
                }

                HStack(spacing: 6) {
                    StatusBadge(containerState: container.state ?? .unknown)
                    StatusBadge(verdict: container.verdict)
                    if container.isManaged {
                        StatusBadge(text: "已托管", color: .blue, systemImage: "lock.fill")
                    }
                }

                if isLoadingDetail {
                    HStack(spacing: 6) {
                        ProgressView()
                            .controlSize(.small)
                        Text("正在读取容器详情…")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
                if let detailError {
                    Text(detailError)
                        .font(.caption)
                        .foregroundStyle(.red)
                }

                Divider()

                DetailSectionHeader(title: "基本信息")
                LabeledValue(label: "容器 ID", value: String(container.id.prefix(12)), monospaced: true)
                LabeledValue(label: "Docker 状态", value: container.statusText ?? container.state?.title ?? "—")
                LabeledValue(label: "创建于", value: RelativeTime.text(container.createdAt, placeholder: "未知"))
                LabeledValue(label: "启动于", value: RelativeTime.text(container.startedAt, placeholder: "—"))
                LabeledValue(label: "重启策略", value: container.restartPolicy ?? "—")
                LabeledValue(label: "网络模式", value: container.networkMode ?? "—")
                LabeledValue(label: "Compose", value: container.projectServiceText)
                if let workingDir = container.composeWorkingDir, !workingDir.isEmpty {
                    LabeledValue(label: "工作目录", value: workingDir, monospaced: true)
                }
                if let files = container.composeConfigFiles, !files.isEmpty {
                    LabeledValue(label: "Compose 文件", value: files.joined(separator: "\n"), monospaced: true)
                }
                LabeledValue(label: "特权模式", value: container.privileged == true ? "是" : "否")
                LabeledValue(label: "只读根目录", value: container.readonlyRootfs == true ? "是" : "否")
                LabeledValue(label: "自动删除", value: container.autoRemove == true ? "是" : "否")
                LabeledValue(label: "一次性容器", value: container.oneoff == true ? "是" : "否")
                if let health = container.healthcheck, health.configured == true {
                    LabeledValue(label: "健康检查", value: health.status ?? "已配置")
                    if let test = health.test, !test.isEmpty {
                        LabeledValue(label: "健康命令", value: test, monospaced: true)
                    }
                }

                Divider()
                DetailSectionHeader(title: "端口")
                if (container.ports ?? []).isEmpty {
                    Text("没有端口映射")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                } else {
                    VStack(alignment: .leading, spacing: 4) {
                        ForEach(Array((container.ports ?? []).enumerated()), id: \.offset) { _, port in
                            Text("\(port.hostAddress):\(port.hostPort ?? 0) → 容器 \(port.containerPort ?? 0)/\(port.protoText)")
                                .font(.caption.monospaced())
                                .textSelection(.enabled)
                        }
                    }
                }
                if !container.proxyCandidates.isEmpty {
                    Text("代理候选端口")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .padding(.top, 2)
                    VStack(alignment: .leading, spacing: 4) {
                        ForEach(Array(container.proxyCandidates.enumerated()), id: \.offset) { _, port in
                            Text("\(port.hostAddress):\(port.hostPort ?? 0) → \(port.containerPort ?? 0)/\(port.protoText)")
                                .font(.caption.monospaced())
                                .foregroundStyle(.secondary)
                        }
                    }
                }

                Divider()
                DetailSectionHeader(title: "挂载")
                if (container.mounts ?? []).isEmpty {
                    Text("没有挂载")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                } else {
                    VStack(alignment: .leading, spacing: 6) {
                        ForEach(Array((container.mounts ?? []).enumerated()), id: \.offset) { _, mount in
                            VStack(alignment: .leading, spacing: 2) {
                                Text("\(mount.type ?? "volume") · \(mount.destination ?? "—")\(mount.readOnly == true ? "（只读）" : "")")
                                    .font(.caption.weight(.medium))
                                if let source = mount.source, !source.isEmpty {
                                    Text(source)
                                        .font(.caption2.monospaced())
                                        .foregroundStyle(.secondary)
                                        .textSelection(.enabled)
                                }
                            }
                        }
                    }
                }

                Divider()
                DetailSectionHeader(title: "网络")
                if (container.networks ?? []).isEmpty {
                    Text("没有网络信息")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                } else {
                    VStack(alignment: .leading, spacing: 4) {
                        ForEach(Array((container.networks ?? []).enumerated()), id: \.offset) { _, network in
                            Text("\(network.name ?? "—") · \(network.ip ?? "—") · 网关 \(network.gateway ?? "—")")
                                .font(.caption.monospaced())
                        }
                    }
                }

                Divider()
                DetailSectionHeader(title: "分类原因")
                ReasonList(reasons: container.reasons, emptyText: "没有分类原因")
                if !container.warnings.isEmpty {
                    Text("注意事项")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .padding(.top, 4)
                    ReasonList(reasons: container.warnings)
                }

                Divider()
                DisclosureGroup {
                    if isLoadingDetail {
                        ProgressView()
                            .controlSize(.small)
                            .padding(8)
                    } else if let inspect = detail?.inspect {
                        ScrollView([.horizontal, .vertical]) {
                            Text(inspect.prettyPrinted())
                                .font(.system(.caption2, design: .monospaced))
                                .textSelection(.enabled)
                                .frame(maxWidth: .infinity, alignment: .leading)
                                .padding(8)
                        }
                        .frame(maxHeight: 280)
                        .background(Color(nsColor: .textBackgroundColor))
                    } else {
                        Text("暂无数据")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .padding(8)
                    }
                } label: {
                    Text("原始 inspect JSON")
                        .font(.caption.weight(.semibold))
                }
            }
            .padding(14)
        }
    }

    // MARK: - 计算属性

    private var allContainers: [Container] {
        app.scanResult?.containers ?? []
    }

    private var projects: [String] {
        Array(
            Set(
                allContainers
                    .compactMap { $0.composeProject }
                    .filter { !$0.isEmpty }
            )
        ).sorted()
    }

    private var filtered: [Container] {
        let keyword = app.scanSearch.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        return allContainers.filter { container in
            guard app.scanFilter.matches(container.verdict) else { return false }
            if let project = app.scanProject, !project.isEmpty {
                guard (container.composeProject ?? "") == project else { return false }
            }
            guard !keyword.isEmpty else { return true }
            let haystack = [
                container.name,
                container.image ?? "",
                container.composeProject ?? "",
                container.composeService ?? "",
                PortsFormatter.oneLine(container.ports ?? [])
            ]
            .joined(separator: " ")
            .lowercased()
            return haystack.contains(keyword)
        }
    }

    private var selectedContainer: Container? {
        guard let selectedID else { return nil }
        return allContainers.first { $0.id == selectedID }
    }

    private var needsReviewSelectionCount: Int {
        allContainers.filter { selection.contains($0.id) && $0.verdict == .needsReview }.count
    }

    // MARK: - 详情加载

    private func loadDetail(_ id: String?) {
        guard let id else {
            detail = nil
            detailError = nil
            isLoadingDetail = false
            return
        }
        if detail?.container?.id == id { return }
        detail = nil
        detailError = nil
        isLoadingDetail = true
        Task {
            let result = await app.inspectContainer(id: id)
            guard selectedID == id else { return }
            isLoadingDetail = false
            if let result {
                detail = result
            } else {
                detailError = "无法获取容器详情"
            }
        }
    }
}
