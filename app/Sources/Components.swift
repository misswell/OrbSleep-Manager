import SwiftUI

// MARK: - 配色与图标

extension Severity {
    var color: Color {
        switch self {
        case .info: return .secondary
        case .warning: return .orange
        case .blocker: return .red
        case .unknown: return .secondary
        }
    }

    var systemImage: String {
        switch self {
        case .info: return "info.circle"
        case .warning: return "exclamationmark.triangle.fill"
        case .blocker: return "xmark.octagon.fill"
        case .unknown: return "questionmark.circle"
        }
    }
}

extension Verdict {
    var color: Color {
        switch self {
        case .eligible: return .green
        case .needsReview: return .orange
        case .excluded: return .secondary
        case .unknown: return .secondary
        }
    }

    var systemImage: String {
        switch self {
        case .eligible: return "checkmark.circle.fill"
        case .needsReview: return "questionmark.circle.fill"
        case .excluded: return "minus.circle.fill"
        case .unknown: return "questionmark.circle"
        }
    }
}

extension ContainerState {
    var color: Color {
        switch self {
        case .running: return .green
        case .exited, .dead: return .secondary
        case .paused: return .orange
        case .restarting, .removing: return .blue
        case .created, .unknown: return .secondary
        }
    }

    var systemImage: String {
        switch self {
        case .running: return "play.circle.fill"
        case .exited: return "stop.circle"
        case .paused: return "pause.circle.fill"
        case .restarting: return "arrow.triangle.2.circlepath"
        case .created: return "circle.dashed"
        case .dead: return "xmark.circle"
        case .removing: return "trash.circle"
        case .unknown: return "questionmark.circle"
        }
    }
}

extension ServiceStatus {
    var color: Color {
        switch self {
        case .managedRunning: return .green
        case .managedIdle: return .blue
        case .managedStopped: return .secondary
        case .missing: return .orange
        case .warning: return .orange
        case .failed: return .red
        case .unknown: return .secondary
        }
    }

    var systemImage: String {
        switch self {
        case .managedRunning: return "play.circle.fill"
        case .managedIdle: return "moon.zzz.fill"
        case .managedStopped: return "pause.circle.fill"
        case .missing: return "questionmark.circle.fill"
        case .warning: return "exclamationmark.triangle.fill"
        case .failed: return "xmark.octagon.fill"
        case .unknown: return "questionmark.circle"
        }
    }
}

extension OperationStatus {
    var color: Color {
        switch self {
        case .running: return .blue
        case .succeeded: return .green
        case .failed: return .red
        case .rolledBack: return .orange
        case .rollbackFailed: return .red
        case .unknown: return .secondary
        }
    }

    var systemImage: String {
        switch self {
        case .running: return "arrow.triangle.2.circlepath"
        case .succeeded: return "checkmark.circle.fill"
        case .failed: return "xmark.octagon.fill"
        case .rolledBack: return "arrow.uturn.backward.circle.fill"
        case .rollbackFailed: return "exclamationmark.octagon.fill"
        case .unknown: return "questionmark.circle"
        }
    }
}

extension DoctorStatus {
    var color: Color {
        switch self {
        case .ok: return .green
        case .warn: return .orange
        case .fail: return .red
        case .unknown: return .secondary
        }
    }

    var systemImage: String {
        switch self {
        case .ok: return "checkmark.circle.fill"
        case .warn: return "exclamationmark.triangle.fill"
        case .fail: return "xmark.octagon.fill"
        case .unknown: return "questionmark.circle"
        }
    }

    var title: String {
        switch self {
        case .ok: return "正常"
        case .warn: return "警告"
        case .fail: return "失败"
        case .unknown: return "未知"
        }
    }
}

extension PlanStatus {
    var title: String {
        switch self {
        case .ready: return "可执行"
        case .blocked: return "已阻止"
        case .unknown: return "未知"
        }
    }

    var color: Color {
        switch self {
        case .ready: return .green
        case .blocked: return .red
        case .unknown: return .secondary
        }
    }
}

// MARK: - 通用徽标

struct StatusBadge: View {
    let text: String
    var color: Color = .secondary
    var systemImage: String?

    var body: some View {
        HStack(spacing: 4) {
            if let systemImage {
                Image(systemName: systemImage)
                    .font(.caption2)
            }
            Text(text)
                .font(.caption.weight(.medium))
        }
        .foregroundStyle(color)
        .padding(.horizontal, 8)
        .padding(.vertical, 3)
        .background(color.opacity(0.14), in: Capsule())
        .fixedSize()
    }
}

extension StatusBadge {
    init(serviceStatus: ServiceStatus) {
        self.init(
            text: serviceStatus.title,
            color: serviceStatus.color,
            systemImage: serviceStatus.systemImage
        )
    }

    init(verdict: Verdict) {
        self.init(text: verdict.title, color: verdict.color, systemImage: verdict.systemImage)
    }

    init(containerState: ContainerState) {
        self.init(
            text: containerState.title,
            color: containerState.color,
            systemImage: containerState.systemImage
        )
    }

    init(operationStatus: OperationStatus) {
        self.init(
            text: operationStatus.title,
            color: operationStatus.color,
            systemImage: operationStatus.systemImage
        )
    }

    init(doctorStatus: DoctorStatus) {
        self.init(
            text: doctorStatus.title,
            color: doctorStatus.color,
            systemImage: doctorStatus.systemImage
        )
    }
}

// MARK: - 原因列表

struct ReasonList: View {
    let reasons: [Reason]
    var emptyText: String?

    var body: some View {
        if reasons.isEmpty {
            if let emptyText {
                Text(emptyText)
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }
        } else {
            VStack(alignment: .leading, spacing: 6) {
                ForEach(Array(reasons.enumerated()), id: \.offset) { _, reason in
                    HStack(alignment: .firstTextBaseline, spacing: 6) {
                        Image(systemName: reason.severityValue.systemImage)
                            .font(.caption)
                            .foregroundStyle(reason.severityValue.color)
                        Text(reason.text)
                            .font(.callout)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
            }
        }
    }
}

/// 表格里的紧凑原因摘要。
struct ReasonSummary: View {
    let reasons: [Reason]
    var limit: Int = 2

    var body: some View {
        if reasons.isEmpty {
            Text("—")
                .font(.caption)
                .foregroundStyle(.tertiary)
        } else {
            VStack(alignment: .leading, spacing: 2) {
                ForEach(Array(reasons.prefix(limit).enumerated()), id: \.offset) { _, reason in
                    Text(reason.text)
                        .font(.caption)
                        .lineLimit(1)
                        .truncationMode(.tail)
                }
                if reasons.count > limit {
                    Text("+\(reasons.count - limit) 条原因")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                }
            }
        }
    }
}

// MARK: - 端口

struct PortsText: View {
    let ports: [PortBinding]
    var showsProxy: Bool = false
    /// 表格里限制显示行数，完整列表在详情面板中展示。
    var maxLines: Int?

    var body: some View {
        if ports.isEmpty {
            Text("无端口")
                .font(.caption)
                .foregroundStyle(.tertiary)
        } else {
            VStack(alignment: .leading, spacing: 2) {
                ForEach(Array(visiblePorts.enumerated()), id: \.offset) { _, port in
                    Text(line(for: port))
                        .font(.caption.monospaced())
                        .foregroundStyle(.primary)
                        .lineLimit(1)
                }
                if let maxLines, ports.count > maxLines {
                    Text("+\(ports.count - maxLines) 个端口")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                }
            }
        }
    }

    private var visiblePorts: [PortBinding] {
        guard let maxLines, ports.count > maxLines else { return ports }
        return Array(ports.prefix(maxLines))
    }

    private func line(for port: PortBinding) -> String {
        let prefix = "\(PortsFormatter.host(port.hostIP)):\(port.hostPort ?? 0)"
        let container = "容器 \(port.containerPort ?? 0)"
        if showsProxy, let proxyPort = port.proxyPort {
            return "\(prefix) ⇢ 代理 \(proxyPort) ⇢ \(container)"
        }
        return "\(prefix) → \(container)"
    }
}

// MARK: - 空状态与横幅

struct EmptyStateView: View {
    let systemImage: String
    let title: String
    let message: String
    var actionTitle: String?
    var action: (() -> Void)?

    var body: some View {
        VStack(spacing: 12) {
            Image(systemName: systemImage)
                .font(.system(size: 40, weight: .light))
                .foregroundStyle(.secondary)
            Text(title)
                .font(.title3.weight(.semibold))
            Text(message)
                .font(.callout)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
                .frame(maxWidth: 420)
            if let actionTitle, let action {
                Button(actionTitle, action: action)
                    .buttonStyle(.borderedProminent)
                    .controlSize(.large)
                    .padding(.top, 4)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .padding(40)
    }
}

struct ErrorBanner: View {
    let message: String
    var onDismiss: (() -> Void)?

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: "exclamationmark.triangle.fill")
                .foregroundStyle(.orange)
            Text(message)
                .font(.callout)
                .foregroundStyle(.primary)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 8)
            if let onDismiss {
                Button {
                    onDismiss()
                } label: {
                    Image(systemName: "xmark")
                        .font(.caption.bold())
                }
                .buttonStyle(.borderless)
                .foregroundStyle(.secondary)
            }
        }
        .padding(10)
        .background(Color.orange.opacity(0.14), in: RoundedRectangle(cornerRadius: 8))
        .overlay(
            RoundedRectangle(cornerRadius: 8)
                .stroke(Color.orange.opacity(0.35), lineWidth: 1)
        )
    }
}

// MARK: - 卡片与信息行

struct Card<Content: View>: View {
    private let title: String
    private let systemImage: String?
    private let content: Content

    init(_ title: String, systemImage: String? = nil, @ViewBuilder content: () -> Content) {
        self.title = title
        self.systemImage = systemImage
        self.content = content()
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 6) {
                if let systemImage {
                    Image(systemName: systemImage)
                        .foregroundStyle(.secondary)
                }
                Text(title)
                    .font(.headline)
            }
            content
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(14)
        .background(Color(nsColor: .controlBackgroundColor), in: RoundedRectangle(cornerRadius: 10))
        .overlay(
            RoundedRectangle(cornerRadius: 10)
                .stroke(Color.secondary.opacity(0.16), lineWidth: 1)
        )
    }
}

struct LabeledValue: View {
    let label: String
    let value: String
    var monospaced: Bool = false
    var secondaryLabel: Bool = true

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text(label)
                .font(.callout)
                .foregroundStyle(secondaryLabel ? .secondary : .primary)
                .frame(width: 96, alignment: .leading)
            Text(value.isEmpty ? "—" : value)
                .font(monospaced ? .callout.monospaced() : .callout)
                .textSelection(.enabled)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 0)
        }
    }
}

struct DetailSectionHeader: View {
    let title: String

    var body: some View {
        Text(title)
            .font(.caption.weight(.semibold))
            .foregroundStyle(.secondary)
            .textCase(.uppercase)
    }
}

struct BigActionButton: View {
    let title: String
    let subtitle: String
    let systemImage: String
    var isBusy: Bool = false
    var isEnabled: Bool = true
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            HStack(spacing: 12) {
                Image(systemName: systemImage)
                    .font(.title2)
                    .foregroundStyle(.tint)
                    .frame(width: 28)
                VStack(alignment: .leading, spacing: 2) {
                    Text(title)
                        .font(.headline)
                    Text(subtitle)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                }
                Spacer(minLength: 0)
                if isBusy {
                    ProgressView()
                        .controlSize(.small)
                }
            }
            .padding(14)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Color(nsColor: .controlBackgroundColor), in: RoundedRectangle(cornerRadius: 10))
            .overlay(
                RoundedRectangle(cornerRadius: 10)
                    .stroke(Color.secondary.opacity(0.18), lineWidth: 1)
            )
        }
        .buttonStyle(.plain)
        .disabled(!isEnabled || isBusy)
        .opacity(isEnabled ? 1 : 0.55)
    }
}

/// 等宽 diff 文本（`+` 绿色、`-` 红色、`@@` 蓝色）。
struct DiffText: View {
    let diff: String
    var maxHeight: CGFloat = 220

    private var lines: [String] {
        diff.components(separatedBy: "\n")
    }

    var body: some View {
        ScrollView([.horizontal, .vertical]) {
            VStack(alignment: .leading, spacing: 0) {
                ForEach(Array(lines.enumerated()), id: \.offset) { _, line in
                    Text(line.isEmpty ? " " : line)
                        .font(.system(.caption, design: .monospaced))
                        .foregroundStyle(color(for: line))
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
            }
            .padding(8)
        }
        .frame(maxHeight: maxHeight)
        .background(Color(nsColor: .textBackgroundColor), in: RoundedRectangle(cornerRadius: 6))
        .overlay(
            RoundedRectangle(cornerRadius: 6)
                .stroke(Color.secondary.opacity(0.2), lineWidth: 1)
        )
    }

    private func color(for line: String) -> Color {
        if line.hasPrefix("+++") || line.hasPrefix("---") {
            return .secondary
        }
        if line.hasPrefix("+") {
            return .green
        }
        if line.hasPrefix("-") {
            return .red
        }
        if line.hasPrefix("@@") {
            return .blue
        }
        if line.hasPrefix("diff ") || line.hasPrefix("index ") {
            return .secondary
        }
        return .primary
    }
}

/// 日志级别徽标。
struct LogLevelBadge: View {
    let level: String?

    private var normalized: String {
        (level ?? "info").lowercased()
    }

    private var color: Color {
        switch normalized {
        case "error", "fatal": return .red
        case "warn", "warning": return .orange
        case "debug": return .secondary
        default: return .blue
        }
    }

    var body: some View {
        Text(normalized.uppercased())
            .font(.caption2.weight(.bold).monospaced())
            .foregroundStyle(color)
            .padding(.horizontal, 5)
            .padding(.vertical, 1)
            .background(color.opacity(0.12), in: RoundedRectangle(cornerRadius: 4))
    }
}
