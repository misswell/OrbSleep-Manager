import SwiftUI

struct PlanSheetView: View {
    @Environment(AppState.self) private var app
    let plan: Plan

    @State private var lanConfirmed: Set<String> = []
    @State private var isApplying = false

    var body: some View {
        VStack(spacing: 0) {
            if let operation = app.operation {
                OperationProgressView(operation: operation, title: app.operationTitle) {
                    app.closePlanSheet()
                }
            } else {
                header
                Divider()
                ScrollView {
                    content
                }
                Divider()
                footer
            }
        }
        .frame(minWidth: 900, idealWidth: 960, minHeight: 640, idealHeight: 740)
    }

    // MARK: - 头部

    private var header: some View {
        HStack(alignment: .top, spacing: 12) {
            VStack(alignment: .leading, spacing: 5) {
                Text("接入计划")
                    .font(.title2.weight(.semibold))
                HStack(spacing: 8) {
                    Text(plan.id)
                        .font(.caption.monospaced())
                        .foregroundStyle(.secondary)
                        .textSelection(.enabled)
                    StatusBadge(
                        text: plan.statusValue.title,
                        color: plan.statusValue.color,
                        systemImage: plan.isBlocked ? "xmark.octagon.fill" : "checkmark.circle.fill"
                    )
                }
                Text("创建于 \(RelativeTime.text(plan.createdAt))")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Spacer()
            Text("\(plan.itemList.count) 个容器")
                .font(.callout)
                .foregroundStyle(.secondary)
        }
        .padding(18)
    }

    // MARK: - 内容

    private var content: some View {
        VStack(alignment: .leading, spacing: 14) {
            if !plan.blockerList.isEmpty {
                VStack(alignment: .leading, spacing: 8) {
                    Label("阻塞项", systemImage: "xmark.octagon.fill")
                        .font(.headline)
                        .foregroundStyle(.red)
                    ReasonList(reasons: plan.blockerList)
                }
                .padding(12)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(Color.red.opacity(0.08), in: RoundedRectangle(cornerRadius: 8))
                .overlay(
                    RoundedRectangle(cornerRadius: 8)
                        .stroke(Color.red.opacity(0.3), lineWidth: 1)
                )
            }

            if !plan.warningList.isEmpty {
                VStack(alignment: .leading, spacing: 6) {
                    Label("注意事项", systemImage: "exclamationmark.triangle.fill")
                        .font(.callout.weight(.semibold))
                        .foregroundStyle(.orange)
                    ReasonList(reasons: plan.warningList)
                }
                .padding(12)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(Color.orange.opacity(0.08), in: RoundedRectangle(cornerRadius: 8))
                .overlay(
                    RoundedRectangle(cornerRadius: 8)
                        .stroke(Color.orange.opacity(0.3), lineWidth: 1)
                )
            }

            if plan.itemList.isEmpty {
                Text("计划中没有需要变更的容器。")
                    .foregroundStyle(.secondary)
                    .padding(.vertical, 20)
            } else {
                VStack(alignment: .leading, spacing: 10) {
                    Text("逐项对比")
                        .font(.headline)
                    ForEach(plan.itemList) { item in
                        itemCard(item)
                    }
                }
            }

            Text("计划有效期为 10 分钟，过期后需要重新生成。执行过程中会重建相关容器，可能造成约 1–5 秒中断。")
                .font(.caption)
                .foregroundStyle(.secondary)
                .padding(.top, 4)
        }
        .padding(18)
    }

    private func itemCard(_ item: PlanItem) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                Text(item.displayName)
                    .font(.headline)
                if let project = item.composeProject, !project.isEmpty {
                    Text(item.composeService.map { "\(project) / \($0)" } ?? project)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                Spacer(minLength: 8)
                if item.requiresLanConfirmation == true {
                    StatusBadge(
                        text: "需要 LAN 暴露确认",
                        color: .orange,
                        systemImage: "antenna.radiowaves.left.and.right"
                    )
                }
            }

            if let file = item.composeFile, !file.isEmpty {
                HStack(spacing: 6) {
                    Image(systemName: "doc.text")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    Text(file)
                        .font(.caption.monospaced())
                        .foregroundStyle(.secondary)
                        .textSelection(.enabled)
                        .lineLimit(1)
                        .truncationMode(.middle)
                    Button("打开目录") {
                        app.openPath(URL(fileURLWithPath: file).deletingLastPathComponent().path)
                    }
                    .buttonStyle(.link)
                    .font(.caption)
                }
            }

            HStack(alignment: .top, spacing: 16) {
                beforeColumn(item)
                Image(systemName: "arrow.right")
                    .foregroundStyle(.secondary)
                    .padding(.top, 20)
                afterColumn(item)
            }

            if let risks = item.risks, !risks.isEmpty {
                VStack(alignment: .leading, spacing: 4) {
                    DetailSectionHeader(title: "风险")
                    ReasonList(reasons: risks)
                }
            }

            HStack(spacing: 6) {
                Text("需要重建的容器：")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                Text(item.displayName)
                    .font(.callout.weight(.medium))
            }

            if let steps = item.steps, !steps.isEmpty {
                DisclosureGroup {
                    VStack(alignment: .leading, spacing: 4) {
                        ForEach(Array(steps.enumerated()), id: \.offset) { index, step in
                            Text("\(index + 1). \(step)")
                                .font(.caption)
                        }
                    }
                    .padding(.top, 4)
                } label: {
                    Text("执行步骤（\(steps.count)）")
                        .font(.caption.weight(.semibold))
                }
            }

            if let diff = item.diff, !diff.isEmpty {
                DisclosureGroup {
                    DiffText(diff: diff)
                        .padding(.top, 4)
                } label: {
                    Text("Compose diff")
                        .font(.caption.weight(.semibold))
                }
            }

            if item.requiresLanConfirmation == true {
                Toggle(isOn: lanBinding(for: item)) {
                    VStack(alignment: .leading, spacing: 2) {
                        Text("我确认允许该服务继续暴露在局域网")
                            .font(.callout.weight(.medium))
                        Text("原始端口绑定所有网卡，接入代理后仍可从局域网访问；请确认这是预期行为。")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
                .toggleStyle(.checkbox)
                .padding(8)
                .background(Color.orange.opacity(0.08), in: RoundedRectangle(cornerRadius: 6))
            }
        }
        .padding(14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color(nsColor: .controlBackgroundColor), in: RoundedRectangle(cornerRadius: 10))
        .overlay(
            RoundedRectangle(cornerRadius: 10)
                .stroke(Color.secondary.opacity(0.16), lineWidth: 1)
        )
    }

    private func beforeColumn(_ item: PlanItem) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            DetailSectionHeader(title: "修改前")
            let removals = item.removals ?? []
            if removals.isEmpty {
                Text("无端口变化")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            } else {
                ForEach(Array(removals.enumerated()), id: \.offset) { _, port in
                    Text("\(port.hostAddress):\(port.hostPort ?? 0) → 容器 \(port.containerPort ?? 0)/\(port.protoText)")
                        .font(.caption.monospaced())
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func afterColumn(_ item: PlanItem) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            DetailSectionHeader(title: "修改后")
            let bindings = item.bindings ?? []
            if !bindings.isEmpty {
                ForEach(Array(bindings.enumerated()), id: \.offset) { _, binding in
                    Text(
                        "\(binding.hostAddress):\(binding.hostPort ?? 0) ⇢ 代理 \(binding.proxyPort ?? 0) ⇢ 容器 \(binding.containerPort ?? 0)"
                    )
                    .font(.caption.monospaced())
                }
            } else {
                let mappings = item.proxyMappings ?? []
                if mappings.isEmpty {
                    Text("无端口变化")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                } else {
                    ForEach(Array(mappings.enumerated()), id: \.offset) { _, mapping in
                        Text(PortsFormatter.mapping(mapping))
                            .font(.caption.monospaced())
                    }
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    // MARK: - 底部

    private var footer: some View {
        HStack(spacing: 12) {
            if plan.isBlocked {
                Label("计划存在阻塞项，无法执行", systemImage: "xmark.octagon.fill")
                    .font(.callout)
                    .foregroundStyle(.red)
            } else if unconfirmedLanCount > 0 {
                Label("请先勾选 \(unconfirmedLanCount) 个 LAN 暴露确认", systemImage: "exclamationmark.triangle.fill")
                    .font(.callout)
                    .foregroundStyle(.orange)
            } else {
                Label("计划已就绪", systemImage: "checkmark.circle.fill")
                    .font(.callout)
                    .foregroundStyle(.green)
            }

            Spacer()

            Button("取消") {
                app.closePlanSheet()
            }
            .keyboardShortcut(.cancelAction)
            .disabled(isApplying)

            Button {
                apply()
            } label: {
                if isApplying {
                    HStack(spacing: 6) {
                        ProgressView()
                            .controlSize(.small)
                        Text("提交中…")
                    }
                } else {
                    Label("执行计划", systemImage: "play.fill")
                }
            }
            .buttonStyle(.borderedProminent)
            .keyboardShortcut(.defaultAction)
            .disabled(!canExecute)
        }
        .padding(16)
    }

    // MARK: - 状态

    private var unconfirmedLanCount: Int {
        plan.needsLanConfirmation.filter { !lanConfirmed.contains($0.id) }.count
    }

    private var canExecute: Bool {
        guard !plan.isBlocked else { return false }
        guard !plan.itemList.isEmpty else { return false }
        return unconfirmedLanCount == 0 && !isApplying && app.operation == nil
    }

    private func lanBinding(for item: PlanItem) -> Binding<Bool> {
        Binding(
            get: { lanConfirmed.contains(item.id) },
            set: { isOn in
                if isOn {
                    lanConfirmed.insert(item.id)
                } else {
                    lanConfirmed.remove(item.id)
                }
            }
        )
    }

    private func apply() {
        guard canExecute else { return }
        isApplying = true
        Task {
            await app.applyPlan(plan)
            isApplying = false
        }
    }
}
