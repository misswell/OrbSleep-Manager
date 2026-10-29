import SwiftUI

struct OverviewView: View {
    @Environment(AppState.self) private var app

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                header

                if let message = app.status?.lastError?.message, !message.isEmpty {
                    ErrorBanner(message: "最近错误：\(message)")
                }

                LazyVGrid(
                    columns: [GridItem(.adaptive(minimum: 270), spacing: 14)],
                    alignment: .leading,
                    spacing: 14
                ) {
                    contextCard
                    proxyCard
                    managedCard
                    pendingCard
                    scanCard
                    errorCard
                }

                actions
            }
            .padding(22)
        }
        .navigationTitle("概览")
        .toolbar {
            ToolbarItem(placement: .primaryAction) {
                Button {
                    Task { await app.refreshAll() }
                } label: {
                    Label("刷新", systemImage: "arrow.clockwise")
                }
                .disabled(app.isRefreshing)
            }
        }
    }

    // MARK: - 头部

    private var header: some View {
        HStack(alignment: .center, spacing: 12) {
            VStack(alignment: .leading, spacing: 4) {
                Text("OrbSleep Manager")
                    .font(.largeTitle.weight(.semibold))
                HStack(spacing: 8) {
                    Text("按需唤醒的 OrbStack / Docker 端口代理")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                    if let version = app.status?.daemonVersion, !version.isEmpty {
                        StatusBadge(text: "orbsleepd \(version)", color: .secondary, systemImage: "bolt.fill")
                    }
                }
            }
            Spacer()
            if app.isRefreshing {
                ProgressView()
                    .controlSize(.small)
            }
        }
    }

    // MARK: - 卡片

    private var contextCard: some View {
        Card("Docker 连接", systemImage: "shippingbox") {
            HStack(alignment: .firstTextBaseline) {
                Text(app.status?.context?.name ?? "—")
                    .font(.title3.weight(.semibold))
                Spacer()
                if app.status?.dockerReachable == true {
                    StatusBadge(text: "已连接", color: .green, systemImage: "checkmark.circle.fill")
                } else {
                    StatusBadge(text: "不可达", color: .red, systemImage: "xmark.octagon.fill")
                }
            }
            if let endpoint = app.status?.context?.endpoint, !endpoint.isEmpty {
                Text(endpoint)
                    .font(.caption.monospaced())
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
                    .lineLimit(2)
                    .truncationMode(.middle)
            }
            if let kind = app.status?.context?.kind, !kind.isEmpty {
                Text("类型：\(kind)")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
    }

    private var proxyCard: some View {
        Card("代理状态", systemImage: "network") {
            HStack(spacing: 8) {
                if app.proxyRunning {
                    StatusBadge(text: "运行中", color: .green, systemImage: "play.circle.fill")
                } else if app.proxyInstalled {
                    StatusBadge(text: "未运行", color: .orange, systemImage: "pause.circle.fill")
                } else {
                    StatusBadge(text: "未安装", color: .red, systemImage: "xmark.circle")
                }
                if let proxy = app.status?.proxy {
                    if proxy.adminReachable == true {
                        StatusBadge(text: "管理 API", color: .green, systemImage: "checkmark.circle")
                    } else {
                        StatusBadge(text: "管理 API 不可达", color: .orange, systemImage: "exclamationmark.triangle")
                    }
                }
            }
            if let range = app.status?.proxy?.publishedRange,
               let start = range.start, let end = range.end {
                Text("端口范围：\(start)–\(end)")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            if let name = app.status?.proxy?.containerName, !name.isEmpty {
                Text("容器：\(name)")
                    .font(.caption.monospaced())
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
            }
            if let message = app.status?.proxy?.errorText {
                Text(message)
                    .font(.caption)
                    .foregroundStyle(.red)
                    .lineLimit(2)
            }
        }
    }

    private var managedCard: some View {
        Card("托管服务", systemImage: "square.stack.3d.up") {
            HStack(alignment: .firstTextBaseline, spacing: 18) {
                stat(title: "运行", value: app.status?.counts?.running, color: .green)
                stat(title: "空闲", value: app.status?.counts?.idle, color: .blue)
                stat(title: "已停止", value: app.status?.counts?.stopped, color: .secondary)
            }
            Divider()
            HStack {
                Text("托管总数")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                Spacer()
                Text("\(app.status?.counts?.managed ?? app.services.count)")
                    .font(.title3.weight(.semibold))
            }
            if let missing = app.status?.counts?.missing, missing > 0 {
                Text("有 \(missing) 个托管容器缺失")
                    .font(.caption)
                    .foregroundStyle(.orange)
            }
        }
    }

    private var pendingCard: some View {
        Card("待接入容器", systemImage: "tray.and.arrow.down") {
            HStack(alignment: .firstTextBaseline, spacing: 18) {
                stat(title: "可接入", value: app.status?.counts?.eligible, color: .green)
                stat(title: "需确认", value: app.status?.counts?.needsReview, color: .orange)
                stat(title: "排除", value: app.status?.counts?.excluded, color: .secondary)
            }
            Divider()
            Button {
                app.scanFilter = .needsReview
                app.section = .scan
            } label: {
                Label("查看待确认容器", systemImage: "arrow.right.circle")
                    .font(.callout)
            }
            .buttonStyle(.link)
        }
    }

    private var scanCard: some View {
        Card("扫描", systemImage: "magnifyingglass") {
            LabeledValue(label: "最近扫描", value: RelativeTime.text(app.lastScanAt, placeholder: "尚未扫描"))
            if let scanned = app.scanResult?.scannedAt, !scanned.isEmpty {
                LabeledValue(label: "扫描时间", value: RelativeTime.absolute(RelativeTime.date(from: scanned) ?? Date()))
            }
            HStack(spacing: 8) {
                Button {
                    Task {
                        if await app.scan() {
                            app.section = .scan
                        }
                    }
                } label: {
                    Label(app.isScanning ? "扫描中…" : "立即扫描", systemImage: "magnifyingglass")
                }
                .disabled(app.isScanning)
                if app.isScanning {
                    ProgressView()
                        .controlSize(.small)
                }
            }
        }
    }

    private var errorCard: some View {
        Card("最近错误", systemImage: "exclamationmark.bubble") {
            if let error = app.status?.lastError, let message = error.message, !message.isEmpty {
                Text(message)
                    .font(.callout)
                    .foregroundStyle(.red)
                    .fixedSize(horizontal: false, vertical: true)
                Text(RelativeTime.text(error.at, placeholder: "时间未知"))
                    .font(.caption)
                    .foregroundStyle(.secondary)
            } else {
                Text("没有记录到错误")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }
        }
    }

    // MARK: - 大按钮

    private var actions: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("快捷操作")
                .font(.headline)
            HStack(spacing: 12) {
                BigActionButton(
                    title: "扫描容器",
                    subtitle: "读取 Docker 容器并分类",
                    systemImage: "magnifyingglass",
                    isBusy: app.isScanning,
                    isEnabled: !app.isScanning
                ) {
                    Task {
                        if await app.scan() {
                            app.section = .scan
                        }
                    }
                }
                BigActionButton(
                    title: "查看待接入",
                    subtitle: "可接入与需确认的容器",
                    systemImage: "tray.and.arrow.down"
                ) {
                    app.scanFilter = .needsReview
                    app.section = .scan
                }
                BigActionButton(
                    title: "托管服务",
                    subtitle: "启停、休眠与端口映射",
                    systemImage: "shippingbox"
                ) {
                    app.section = .services
                }
                BigActionButton(
                    title: "设置",
                    subtitle: "超时、端口与自检",
                    systemImage: "gearshape"
                ) {
                    app.section = .settings
                }
            }
        }
        .padding(.top, 4)
    }

    // MARK: - 小组件

    private func stat(title: String, value: Int?, color: Color) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text("\(value ?? 0)")
                .font(.title2.weight(.semibold))
                .foregroundStyle(color)
            Text(title)
                .font(.caption)
                .foregroundStyle(.secondary)
        }
    }
}
