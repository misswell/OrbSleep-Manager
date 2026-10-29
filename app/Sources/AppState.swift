import AppKit
import Foundation
import Observation

// MARK: - 导航

enum AppSection: String, CaseIterable, Identifiable, Hashable {
    case overview
    case scan
    case services
    case operations
    case settings

    var id: String { rawValue }

    var title: String {
        switch self {
        case .overview: return "概览"
        case .scan: return "扫描容器"
        case .services: return "托管服务"
        case .operations: return "操作记录"
        case .settings: return "设置"
        }
    }

    var systemImage: String {
        switch self {
        case .overview: return "rectangle.grid.2x2"
        case .scan: return "magnifyingglass"
        case .services: return "shippingbox"
        case .operations: return "list.bullet.rectangle"
        case .settings: return "gearshape"
        }
    }
}

// MARK: - 服务操作

enum ServiceAction: String, Sendable {
    case start
    case stop
    case restore
    case remove
    case updatePolicy

    var title: String {
        switch self {
        case .start: return "启动服务"
        case .stop: return "停止服务"
        case .restore: return "恢复原始端口映射"
        case .remove: return "移除托管"
        case .updatePolicy: return "更新托管策略"
        }
    }
}

// MARK: - 设置草稿

/// 设置页的本地编辑副本，保存时与原始 `Settings` 比较生成补丁。
struct SettingsDraft: Equatable, Sendable {
    var idleTimeoutSecs: Int = 1800
    var startTimeoutSecs: Int = 60
    var proxyPortStart: Int = 20_000
    var proxyPortEnd: Int = 20_099
    var adminPort: Int = 47_811
    var webPort: Int = 47_810
    var autoScanOnLaunch: Bool = false
    var enableLowPortHelper: Bool = false
    var logRetentionDays: Int = 14
    var proxyDialMode: String = "published"
    var excludeImagePatterns: [String] = []

    init() {}

    init(settings: Settings?) {
        guard let settings else { return }
        idleTimeoutSecs = settings.idleTimeoutSecs ?? idleTimeoutSecs
        startTimeoutSecs = settings.startTimeoutSecs ?? startTimeoutSecs
        proxyPortStart = settings.proxyPortStart ?? proxyPortStart
        proxyPortEnd = settings.proxyPortEnd ?? proxyPortEnd
        adminPort = settings.adminPort ?? adminPort
        webPort = settings.webPort ?? webPort
        autoScanOnLaunch = settings.autoScanOnLaunch ?? autoScanOnLaunch
        enableLowPortHelper = settings.enableLowPortHelper ?? enableLowPortHelper
        logRetentionDays = settings.logRetentionDays ?? logRetentionDays
        proxyDialMode = settings.proxyDialMode ?? proxyDialMode
        excludeImagePatterns = settings.excludeImagePatterns ?? []
    }

    var cleanedPatterns: [String] {
        excludeImagePatterns
            .map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }
            .filter { !$0.isEmpty }
    }

    func patch(comparedTo original: Settings?) -> SettingsPatch {
        var patch = SettingsPatch()
        if original?.idleTimeoutSecs != idleTimeoutSecs {
            patch.idleTimeoutSecs = idleTimeoutSecs
        }
        if original?.startTimeoutSecs != startTimeoutSecs {
            patch.startTimeoutSecs = startTimeoutSecs
        }
        if original?.proxyPortStart != proxyPortStart {
            patch.proxyPortStart = proxyPortStart
        }
        if original?.proxyPortEnd != proxyPortEnd {
            patch.proxyPortEnd = proxyPortEnd
        }
        if original?.adminPort != adminPort {
            patch.adminPort = adminPort
        }
        if original?.webPort != webPort {
            patch.webPort = webPort
        }
        if original?.autoScanOnLaunch != autoScanOnLaunch {
            patch.autoScanOnLaunch = autoScanOnLaunch
        }
        if original?.enableLowPortHelper != enableLowPortHelper {
            patch.enableLowPortHelper = enableLowPortHelper
        }
        if original?.logRetentionDays != logRetentionDays {
            patch.logRetentionDays = logRetentionDays
        }
        if (original?.proxyDialMode ?? "published") != proxyDialMode {
            patch.proxyDialMode = proxyDialMode
        }
        if (original?.excludeImagePatterns ?? []) != cleanedPatterns {
            patch.excludeImagePatterns = cleanedPatterns
        }
        return patch
    }
}

// MARK: - 应用状态

/// 全局 UI 状态。所有属性都在 MainActor 上，后台请求通过 `CoreClient` 完成。
@MainActor
@Observable
final class AppState {
    private let client: CoreClient

    var section: AppSection = .overview

    // 数据
    var status: DaemonStatus?
    var services: [Service] = []
    var settings: Settings?
    var scanResult: ScanResult?
    var doctorChecks: [DoctorCheck] = []
    var operations: [Operation] = []

    // 连接
    var daemonReachable = true
    var connectionError: String?
    var errorBanner: String?

    // 加载状态
    var isRefreshing = false
    var isScanning = false
    var isCreatingPlan = false
    var isSavingSettings = false
    var isLoadingDoctor = false
    var isLoadingOperations = false
    var isTogglingAgent = false

    // 计划 / 操作
    var plan: Plan?
    var operation: Operation?
    var operationTitle = ""
    var isOperationSheetPresented = false

    // 扫描页筛选条件（跨页面共享，便于「查看待接入」直接跳转）
    var scanFilter: VerdictFilter = .all
    var scanSearch = ""
    var scanProject: String?

    @ObservationIgnored private var didBootstrap = false
    @ObservationIgnored private var pollTask: Task<Void, Never>?
    @ObservationIgnored private var operationTask: Task<Void, Never>?
    @ObservationIgnored private var errorDismissTask: Task<Void, Never>?

    init(client: CoreClient = .shared) {
        self.client = client
    }

    // MARK: 派生属性

    var socketPath: String { client.socketPath }

    var dataDir: String {
        if let dataDir = status?.dataDir, !dataDir.isEmpty { return dataDir }
        if let dataDir = settings?.dataDir, !dataDir.isEmpty { return dataDir }
        return CoreClient.defaultDataDirectory
    }

    var pendingReviewCount: Int {
        status?.counts?.pendingCount ?? 0
    }

    var proxyInstalled: Bool { status?.proxy?.installed == true }
    var proxyRunning: Bool { status?.proxy?.running == true }

    var launchAgentInstalled: Bool {
        settings?.launchAgentInstalled ?? status?.launchAgentInstalled ?? false
    }

    var lastScanAt: String? {
        status?.lastScanAt ?? scanResult?.scannedAt
    }

    var hasOperationInProgress: Bool {
        operation?.statusValue == .running
    }

    // MARK: 生命周期

    /// 首次启动：读取状态、服务、设置，并按设置决定是否自动扫描。
    func bootstrap() async {
        guard !didBootstrap else { return }
        didBootstrap = true
        await refreshStatus()
        await refreshServices()
        await loadSettings()
        if settings?.autoScanOnLaunch == true {
            await scan()
        }
        startPolling()
    }

    /// 每 3 秒轮询 `system.status` + `services.list`（绝不触发 scan）。
    func startPolling() {
        guard pollTask == nil else { return }
        pollTask = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: 3_000_000_000)
                guard !Task.isCancelled, let self else { return }
                await self.pollOnce()
            }
        }
    }

    func stopPolling() {
        pollTask?.cancel()
        pollTask = nil
    }

    private func pollOnce() async {
        await refreshStatus(silent: true)
        if daemonReachable {
            await refreshServices(silent: true)
            if settings == nil {
                await loadSettings()
            }
        }
    }

    // MARK: 状态与服务

    func refreshStatus(silent: Bool = false) async {
        if !silent { isRefreshing = true }
        defer {
            if !silent { isRefreshing = false }
        }
        do {
            let value = try await client.call("system.status", as: DaemonStatus.self)
            status = value
            markReachable()
        } catch {
            handle(error)
        }
    }

    func refreshServices(silent: Bool = false) async {
        do {
            let value = try await client.call("services.list", as: ServicesResult.self)
            services = value.services ?? []
            markReachable()
        } catch {
            if !silent { handle(error) }
        }
    }

    /// 手动刷新（工具栏按钮）。
    func refreshAll() async {
        await refreshStatus()
        guard daemonReachable else { return }
        await refreshServices()
        await loadSettings()
    }

    func retryConnection() async {
        client.resetConnection()
        await refreshStatus()
        guard daemonReachable else { return }
        await refreshServices()
        await loadSettings()
    }

    // MARK: 扫描

    @discardableResult
    func scan() async -> Bool {
        guard !isScanning else { return false }
        isScanning = true
        defer { isScanning = false }
        do {
            let result = try await client.call("scan", as: ScanResult.self)
            scanResult = result
            markReachable()
            return true
        } catch {
            handle(error)
            return false
        }
    }

    // MARK: 计划与操作

    func createPlan(containerIDs: [String], allowLanExposure: Bool) async {
        guard !containerIDs.isEmpty, !isCreatingPlan else { return }
        isCreatingPlan = true
        defer { isCreatingPlan = false }
        do {
            let params: [String: JSONValue] = [
                "containerIds": .array(containerIDs.map { .string($0) }),
                "allowLanExposure": .bool(allowLanExposure)
            ]
            let value = try await client.call("plan.create", params: params, as: Plan.self)
            plan = value
            operation = nil
            operationTitle = "接入计划"
            markReachable()
        } catch {
            handle(error)
        }
    }

    func applyPlan(_ plan: Plan) async {
        guard !plan.isBlocked else {
            showError("计划存在阻塞项，无法执行")
            return
        }
        do {
            let value = try await client.call(
                "plan.apply",
                params: ["planId": .string(plan.id)],
                as: PlanApplyResult.self
            )
            markReachable()
            guard let operationId = value.operationId else {
                showError("plan.apply 没有返回 operationId")
                return
            }
            beginOperation(operationId: operationId, title: "执行接入计划")
        } catch {
            handle(error)
        }
    }

    func closePlanSheet() {
        operationTask?.cancel()
        operationTask = nil
        operation = nil
        plan = nil
    }

    /// 开始跟踪一个操作（每 500ms 轮询 `operation.get`）。
    func beginOperation(operationId: String, title: String) {
        operationTitle = title
        operation = Operation(id: operationId)
        operationTask?.cancel()
        operationTask = Task { [weak self] in
            await self?.pollOperation(operationId: operationId)
        }
    }

    private func pollOperation(operationId: String) async {
        while !Task.isCancelled {
            do {
                let latest = try await client.call(
                    "operation.get",
                    params: ["id": .string(operationId)],
                    as: Operation.self
                )
                operation = latest
                markReachable()
                if latest.statusValue.isTerminal {
                    await refreshAfterOperation()
                    return
                }
            } catch {
                handle(error)
                if var current = operation, current.statusValue == .running {
                    current.error = "无法获取操作状态：\(error.localizedDescription)"
                    operation = current
                }
                return
            }
            try? await Task.sleep(nanoseconds: 500_000_000)
        }
    }

    private func refreshAfterOperation() async {
        await refreshStatus(silent: true)
        await refreshServices(silent: true)
    }

    func closeOperationSheet() {
        operationTask?.cancel()
        operationTask = nil
        operation = nil
        isOperationSheetPresented = false
    }

    // MARK: 服务操作

    func performServiceAction(
        _ service: Service,
        action: ServiceAction,
        params: [String: JSONValue] = [:]
    ) async {
        do {
            let requestParams: [String: JSONValue] = [
                "id": .string(service.id),
                "action": .string(action.rawValue),
                "params": .object(params)
            ]
            let value = try await client.call(
                "service.action",
                params: requestParams,
                as: ServiceActionResult.self
            )
            markReachable()
            if let operationId = value.operationId {
                isOperationSheetPresented = true
                beginOperation(operationId: operationId, title: "\(action.title) · \(service.displayName)")
            } else {
                await refreshStatus(silent: true)
                await refreshServices(silent: true)
            }
        } catch {
            handle(error)
        }
    }

    func updateIdleTimeout(_ service: Service, seconds: Int) async {
        var params: [String: JSONValue] = ["idleTimeoutSecs": .number(Double(seconds))]
        if let excluded = service.excludedFromSleep {
            params["excludedFromSleep"] = .bool(excluded)
        }
        await performServiceAction(service, action: .updatePolicy, params: params)
    }

    func setExcludedFromSleep(_ service: Service, excluded: Bool) async {
        var params: [String: JSONValue] = ["excludedFromSleep": .bool(excluded)]
        if let idle = service.idleTimeoutSecs {
            params["idleTimeoutSecs"] = .number(Double(idle))
        }
        await performServiceAction(service, action: .updatePolicy, params: params)
    }

    func setupProxy() async {
        do {
            let value = try await client.call("proxy.setup", as: PlanApplyResult.self)
            markReachable()
            guard let operationId = value.operationId else {
                showError("proxy.setup 没有返回 operationId")
                return
            }
            isOperationSheetPresented = true
            beginOperation(operationId: operationId, title: "安装 / 启动代理")
        } catch {
            handle(error)
        }
    }

    // MARK: LaunchAgent

    func installLaunchAgent() async {
        await updateLaunchAgent(install: true)
    }

    func uninstallLaunchAgent() async {
        await updateLaunchAgent(install: false)
    }

    private func updateLaunchAgent(install: Bool) async {
        guard !isTogglingAgent else { return }
        isTogglingAgent = true
        defer { isTogglingAgent = false }
        do {
            let value = try await client.call(
                install ? "agent.install" : "agent.uninstall",
                as: AgentStatusResult.self
            )
            markReachable()
            if let installed = value.installed {
                if var copy = settings {
                    copy.launchAgentInstalled = installed
                    settings = copy
                }
                if var copy = status {
                    copy.launchAgentInstalled = installed
                    status = copy
                }
            }
            await refreshStatus(silent: true)
        } catch {
            handle(error)
        }
    }

    // MARK: 设置与自检

    func loadSettings() async {
        do {
            let value = try await client.call("settings.get", as: Settings.self)
            settings = value
            markReachable()
        } catch {
            handle(error)
        }
    }

    func updateSettings(_ patch: SettingsPatch) async -> Bool {
        guard !patch.isEmpty else { return true }
        isSavingSettings = true
        defer { isSavingSettings = false }
        do {
            let encoded = try JSONEncoder().encode(patch)
            let json = try JSONDecoder().decode(JSONValue.self, from: encoded)
            let value = try await client.call(
                "settings.update",
                params: json.objectValue ?? [:],
                as: Settings.self
            )
            settings = value
            markReachable()
            return true
        } catch {
            handle(error)
            return false
        }
    }

    func loadDoctor() async {
        isLoadingDoctor = true
        defer { isLoadingDoctor = false }
        do {
            let value = try await client.call("system.doctor", as: DoctorResult.self)
            doctorChecks = value.checks ?? []
            markReachable()
        } catch {
            handle(error)
        }
    }

    // MARK: 操作记录

    func loadOperations() async {
        isLoadingOperations = true
        defer { isLoadingOperations = false }
        do {
            let value = try await client.call(
                "operations.list",
                params: ["limit": .number(50)],
                as: OperationsResult.self
            )
            operations = value.operations ?? []
            markReachable()
        } catch {
            handle(error)
        }
    }

    // MARK: 容器详情

    func inspectContainer(id: String) async -> ContainerInspectResult? {
        do {
            let value = try await client.call(
                "container.inspect",
                params: ["id": .string(id)],
                as: ContainerInspectResult.self
            )
            markReachable()
            return value
        } catch {
            handle(error)
            return nil
        }
    }

    // MARK: 系统动作（仅打开文件 / 目录，或复制文本）

    func copyInstallCommand() {
        let pasteboard = NSPasteboard.general
        pasteboard.clearContents()
        pasteboard.setString("orbsleepctl install-agent", forType: .string)
    }

    func openDataDirectory() {
        let path = dataDir
        var isDirectory: ObjCBool = false
        if FileManager.default.fileExists(atPath: path, isDirectory: &isDirectory), isDirectory.boolValue {
            NSWorkspace.shared.open(URL(fileURLWithPath: path))
        } else {
            let parent = URL(fileURLWithPath: path).deletingLastPathComponent()
            NSWorkspace.shared.open(parent)
        }
    }

    func openPath(_ path: String) {
        guard !path.isEmpty else { return }
        let url = URL(fileURLWithPath: path)
        var isDirectory: ObjCBool = false
        guard FileManager.default.fileExists(atPath: path, isDirectory: &isDirectory) else { return }
        NSWorkspace.shared.open(url)
    }

    // MARK: 错误处理

    func markReachable() {
        if !daemonReachable {
            daemonReachable = true
        }
        if connectionError != nil {
            connectionError = nil
        }
    }

    func handle(_ error: Error) {
        guard let core = error as? CoreError else {
            showError(error.localizedDescription)
            return
        }
        switch core {
        case .unreachable, .disconnected:
            daemonReachable = false
            connectionError = core.errorDescription
        case .timeout:
            showError(core.errorDescription ?? "请求超时")
        case .remote(_, let message):
            showError(message)
        case .invalidResponse(let message):
            showError(message)
        }
    }

    func showError(_ message: String) {
        let trimmed = message.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return }
        guard errorBanner != trimmed else { return }
        errorBanner = trimmed
        errorDismissTask?.cancel()
        errorDismissTask = Task { [weak self] in
            try? await Task.sleep(nanoseconds: 12_000_000_000)
            guard !Task.isCancelled else { return }
            self?.errorBanner = nil
        }
    }

    func dismissError() {
        errorDismissTask?.cancel()
        errorDismissTask = nil
        errorBanner = nil
    }
}
