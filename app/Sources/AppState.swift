import AppKit
import Foundation
import Observation

// MARK: - 导航

enum AppSection: String, CaseIterable, Identifiable, Hashable {
    case overview
    case scan
    case services
    case operations
    case diagnostics
    case logs
    case update
    case settings

    var id: String { rawValue }

    var title: String {
        switch self {
        case .overview: return "概览"
        case .scan: return "扫描容器"
        case .services: return "托管服务"
        case .operations: return "操作记录"
        case .diagnostics: return "诊断与修复"
        case .logs: return "日志与维护"
        case .update: return "更新与维护"
        case .settings: return "设置"
        }
    }

    var systemImage: String {
        switch self {
        case .overview: return "rectangle.grid.2x2"
        case .scan: return "magnifyingglass"
        case .services: return "shippingbox"
        case .operations: return "list.bullet.rectangle"
        case .diagnostics: return "stethoscope"
        case .logs: return "doc.text.magnifyingglass"
        case .update: return "arrow.triangle.2.circlepath.circle"
        case .settings: return "gearshape"
        }
    }
}

// MARK: - 安装向导

/// 安装向导中的一步，状态直接驱动 SetupView。
struct SetupStep: Identifiable, Equatable {
    enum State: Equatable {
        case pending
        case running
        case done
        case failed
        case skipped
    }

    let id: String
    let title: String
    let detail: String
    var state: State = .pending
    var message: String?

    var isTerminal: Bool {
        switch state {
        case .done, .failed, .skipped: return true
        case .pending, .running: return false
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

/// 卸载方式：保留数据，或连同 OrbSleep 数据与自有代理一起删除。
enum UninstallMode: String, Sendable {
    case keepData
    case full
}

/// 更新中心状态。
enum UpdateStatus: Equatable {
    case idle
    case checking
    case available(AppUpdate)
    case downloading(Double)
    case installing
    case failed(String)
}

/// 全局 UI 状态。所有属性都在 MainActor 上，后台请求通过 `CoreClient` 完成。
@MainActor
@Observable
final class AppState {
    private let client: CoreClient
    let backgroundService = BackgroundService()

    var section: AppSection = .overview

    // 应用与后台服务
    var appVersion = ""
    var serviceVersion: String?
    var isBootstrapping = true
    var serviceRegistered = false
    var serviceNeedsApproval = false
    var cliInstalled = false
    var launchesGUIAtLogin = false
    var signingSummary = "检查中…"
    var notarizationSummary = "检查中…"
    var isSettingUp = false
    var isBusyService = false
    var isUninstalling = false
    var setupCompleted = false
    var setupSteps: [SetupStep] = []
    var logs: [LogLine] = []
    var isLoadingLogs = false
    var logSource = "daemon"
    var logSources: [String] = ["daemon"]
    var lastSetupError: String?

    // 系统管理 / 维护
    var health: Health?
    var legacyInstallation: LegacyInstallation = .none
    var dockerContexts: [DockerContextEntry] = []
    var selectedDockerSocket = ""
    var backups: [BackupEntry] = []
    var recoveryTransactions: [RecoveryTransaction] = []
    var isMigrating = false
    var isBusyMaintenance = false
    var isLoadingMaintenance = false
    var availableUpdate: AppUpdate?
    let updateManager = UpdateManager()
    var updateStatus: UpdateStatus = .idle

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
    var noticeMessage: String?

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
    @ObservationIgnored private var didAutoDoctorAlert = false
    @ObservationIgnored private var pollTask: Task<Void, Never>?
    @ObservationIgnored private var operationTask: Task<Void, Never>?
    @ObservationIgnored private var errorDismissTask: Task<Void, Never>?
    @ObservationIgnored private var noticeDismissTask: Task<Void, Never>?

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
        serviceRegistered
            || (settings?.launchAgentInstalled ?? false)
            || (status?.launchAgentInstalled ?? false)
    }

    var lastScanAt: String? {
        status?.lastScanAt ?? scanResult?.scannedAt
    }

    var hasOperationInProgress: Bool {
        operation?.statusValue == .running
    }

    // MARK: 生命周期

    /// 首次启动：先探测后台服务；能连上就自动加载并自检，连不上就进入安装向导。
    /// 用户不需要打开终端，也不需要手动执行任何命令。
    func bootstrap() async {
        guard !didBootstrap else { return }
        didBootstrap = true
        defer { isBootstrapping = false }
        refreshLocalState()
        loadSigningInfo()
        if await probeDaemon() {
            setupCompleted = true
            await loadCoreState()
            if serviceVersionMismatch {
                await upgradeService()
            }
            await runAutoDoctor()
        } else {
            prepareSetupSteps()
        }
        startPolling()
    }

    func refreshLocalState() {
        appVersion = Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? ""
        serviceRegistered = backgroundService.installed
        serviceNeedsApproval = backgroundService.needsApproval
        cliInstalled = backgroundService.cliInstalled
        launchesGUIAtLogin = backgroundService.launchesAtLogin
        legacyInstallation = backgroundService.detectLegacy()
    }

    /// 读取本机 codesign / spctl 信息，供设置页「关于」展示。
    func loadSigningInfo() {
        let path = Bundle.main.bundleURL.path
        let code = capture("/usr/bin/codesign", ["-dv", "--verbose=4", path])
        if let line = code.output
            .split(separator: "\n")
            .first(where: { $0.contains("Authority=Developer ID Application") }) {
            signingSummary = String(line)
                .replacingOccurrences(of: "Authority=", with: "")
                .trimmingCharacters(in: .whitespaces)
        } else if code.output.contains("Signature=adhoc") {
            signingSummary = "ad-hoc（开发构建）"
        } else {
            signingSummary = "未签名或未知"
        }
        let assess = capture("/usr/sbin/spctl", ["--assess", "--type", "execute", "--verbose", path])
        notarizationSummary = assess.status == 0 ? "已公证并通过 Gatekeeper" : "未公证或 Gatekeeper 未放行"
    }

    private func capture(_ executable: String, _ arguments: [String]) -> (status: Int32, output: String) {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: executable)
        process.arguments = arguments
        let pipe = Pipe()
        process.standardOutput = pipe
        process.standardError = pipe
        do {
            try process.run()
        } catch {
            return (-1, error.localizedDescription)
        }
        process.waitUntilExit()
        let data = pipe.fileHandleForReading.readDataToEndOfFile()
        return (process.terminationStatus, String(decoding: data, as: UTF8.self))
    }

    /// 统一的后台服务状态，供 GUI 按状态显示不同操作。
    var currentServiceState: BackgroundServiceState {
        if isMigrating || isBusyService {
            return .starting
        }
        if backgroundService.needsApproval {
            return .awaitingApproval
        }
        if daemonReachable {
            if serviceVersionMismatch {
                return .versionMismatch
            }
            if let pending = health?.pendingTransactions, pending > 0 {
                return .recoveryRequired
            }
            return .running
        }
        if !backgroundService.installed && !legacyInstallation.isPresent {
            return .notInstalled
        }
        if backgroundService.installed || legacyInstallation.isPresent {
            return .stopped
        }
        return .failed
    }

    private func loadCoreState() async {
        await refreshStatus()
        await refreshHealth()
        await refreshServices()
        await loadSettings()
        if settings?.autoScanOnLaunch == true {
            await scan()
        }
    }

    /// 打开应用后自动运行，只在发现失败项时提示，不要求用户主动执行 doctor。
    private func runAutoDoctor() async {
        await loadDoctor()
        let failures = doctorChecks.filter { $0.statusValue == .fail }
        guard !failures.isEmpty, !didAutoDoctorAlert else { return }
        didAutoDoctorAlert = true
        showError("自检发现 \(failures.count) 项问题：\(failures[0].displayTitle)。可在「诊断与修复」中一键处理。")
    }

    func probeDaemon(timeout: TimeInterval = 2) async -> Bool {
        do {
            _ = try await client.call("ping", timeout: timeout, as: PingResult.self)
            markReachable()
            return true
        } catch {
            daemonReachable = false
            if let core = error as? CoreError {
                connectionError = core.errorDescription
            }
            return false
        }
    }

    /// 安装向导的初始状态。
    func prepareSetupSteps() {
        setupSteps = [
            SetupStep(
                id: "app",
                title: "应用程序",
                detail: "OrbSleepManager.app",
                state: .done,
                message: nil
            ),
            SetupStep(
                id: "service",
                title: "后台服务",
                detail: backgroundService.installed ? "已注册，等待启动" : "等待初始化",
                state: .pending
            ),
            SetupStep(
                id: "docker",
                title: "Docker / OrbStack",
                detail: "初始化后自动检测 Docker Socket",
                state: .pending
            ),
            SetupStep(
                id: "proxy",
                title: "Lazy TCP Proxy",
                detail: "自动安装并启动代理容器",
                state: .pending
            ),
            SetupStep(
                id: "doctor",
                title: "环境检查",
                detail: "安装完成后自动执行",
                state: .pending
            )
        ]
    }

    private func updateStep(
        _ id: String,
        state: SetupStep.State,
        message: String? = nil
    ) {
        guard let index = setupSteps.firstIndex(where: { $0.id == id }) else { return }
        setupSteps[index].state = state
        if let message {
            setupSteps[index].message = message
        }
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
        await refreshHealth()
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

    func stopProxy() async {
        do {
            let value = try await client.call("proxy.stop", as: PlanApplyResult.self)
            markReachable()
            guard let operationId = value.operationId else {
                showError("proxy.stop 没有返回 operationId")
                return
            }
            isOperationSheetPresented = true
            beginOperation(operationId: operationId, title: "停止代理")
        } catch {
            handle(error)
        }
    }

    // MARK: 后台服务（全部由 GUI 驱动）

    var serviceVersionMismatch: Bool {
        guard let version = status?.daemonVersion, !version.isEmpty, !appVersion.isEmpty else { return false }
        return version != appVersion
    }

    var dockerSummary: String {
        guard let status else { return "未连接" }
        if status.dockerReachable == true {
            let name = status.context?.name ?? "docker"
            return "已连接 \(name)"
        }
        return "不可达"
    }

    /// 安装向导的「一键配置并启动」。
    func performSetup() async {
        guard !isSettingUp else { return }
        isSettingUp = true
        lastSetupError = nil
        defer { isSettingUp = false }
        if setupSteps.isEmpty { prepareSetupSteps() }
        updateStep("app", state: .done, message: "已就绪")

        updateStep("service", state: .running, message: "正在注册并启动后台服务…")
        do {
            try await ensureServiceRunning()
            refreshLocalState()
            updateStep("service", state: .done, message: backgroundService.statusText)
        } catch {
            let message = (error as? LocalizedError)?.errorDescription ?? error.localizedDescription
            lastSetupError = message
            updateStep("service", state: .failed, message: message)
            handleServiceError(error)
            return
        }

        await refreshStatus()
        await refreshHealth()
        await refreshServices()
        await loadSettings()

        let dockerOK = status?.dockerReachable == true
        updateStep("docker", state: dockerOK ? .done : .failed, message: dockerSummary)
        if !dockerOK {
            showError("未检测到可用的 Docker / OrbStack。请先启动 OrbStack 或 Docker Desktop，然后点击「重新检查」。")
        }

        if dockerOK {
            updateStep("proxy", state: .running, message: "正在检查 Lazy TCP Proxy…")
            if status?.proxy?.installed == true, status?.proxy?.running == true {
                updateStep("proxy", state: .done, message: "代理运行中")
            } else {
                do {
                    let value = try await client.call("proxy.setup", as: PlanApplyResult.self)
                    if let operationId = value.operationId {
                        await waitOperationForUninstall(operationId, timeoutSeconds: 1200)
                    }
                    await refreshStatus(silent: true)
                    if status?.proxy?.running == true {
                        updateStep("proxy", state: .done, message: "代理已就绪")
                    } else {
                        updateStep("proxy", state: .failed, message: status?.proxy?.errorText ?? "代理未运行")
                    }
                } catch {
                    let message = (error as? LocalizedError)?.errorDescription ?? error.localizedDescription
                    updateStep("proxy", state: .failed, message: message)
                }
            }
        } else {
            updateStep("proxy", state: .skipped, message: "Docker 不可用，跳过代理初始化")
        }

        updateStep("doctor", state: .running, message: "正在执行环境检查…")
        await loadDoctor()
        let failures = doctorChecks.filter { $0.statusValue == .fail }
        if failures.isEmpty {
            updateStep("doctor", state: .done, message: "全部通过")
            setupCompleted = true
            section = .overview
        } else {
            updateStep("doctor", state: .failed, message: "\(failures.count) 项需要处理")
            setupCompleted = true
            didAutoDoctorAlert = true
            showError("安装完成，但自检有 \(failures.count) 项需要处理。可在「诊断与修复」中查看并一键修复。")
        }
    }

    /// 注册并启动后台服务，必要时回退到用户级 LaunchAgent（开发构建）。
    func ensureServiceRunning() async throws {
        refreshLocalState()
        if await probeDaemon() { return }
        if backgroundService.isTranslocated {
            throw BackgroundServiceError.appIsTranslocated
        }
        if backgroundService.needsApproval {
            backgroundService.openLoginItemsSettings()
            throw BackgroundServiceError.requiresApproval
        }
        if !backgroundService.isRegistered {
            do {
                try backgroundService.register()
                refreshLocalState()
            } catch {
                guard let daemon = backgroundService.bundledDaemonURL else {
                    throw BackgroundServiceError.missingBundledExecutable("orbsleepd")
                }
                try backgroundService.installLegacyAgent(daemon: daemon)
                refreshLocalState()
            }
            if backgroundService.needsApproval {
                backgroundService.openLoginItemsSettings()
                throw BackgroundServiceError.requiresApproval
            }
            // SMAppService starts the agent on register; the legacy CLI waits
            // for the socket itself. Avoid a redundant kill/restart.
            if await waitForDaemon(seconds: 10) { return }
        }
        try? backgroundService.kickstart()
        if await waitForDaemon(seconds: 20) { return }
        if let daemon = backgroundService.bundledDaemonURL {
            try? backgroundService.installLegacyAgent(daemon: daemon)
        }
        guard await waitForDaemon(seconds: 30) else {
            throw BackgroundServiceError.daemonDidNotStart
        }
    }

    func waitForDaemon(seconds: Int) async -> Bool {
        let attempts = max(1, seconds * 4)
        for _ in 0..<attempts {
            if await probeDaemon(timeout: 1) { return true }
            try? await Task.sleep(nanoseconds: 250_000_000)
        }
        return false
    }

    private func waitForDaemonToStop(seconds: Int) async -> Bool {
        let attempts = max(1, seconds * 4)
        for _ in 0..<attempts {
            if !(await probeDaemon(timeout: 1)) { return true }
            try? await Task.sleep(nanoseconds: 250_000_000)
        }
        return false
    }

    /// 单独控制「登录后自动启动后台服务」，与是否运行、是否打开 GUI 分离。
    func setAutoStart(_ enabled: Bool) async {
        guard !isBusyService else { return }
        isBusyService = true
        defer { isBusyService = false }
        if enabled {
            if !backgroundService.isRegistered {
                do {
                    try backgroundService.register()
                } catch {
                    if let daemon = backgroundService.bundledDaemonURL {
                        try? backgroundService.installLegacyAgent(daemon: daemon)
                    }
                }
            }
        } else {
            try? await backgroundService.unregister()
            backgroundService.removeLegacyAgent()
        }
        refreshLocalState()
        await refreshStatus(silent: true)
    }

    /// 登录后自动打开 GUI，与后台服务开机启动分开控制。
    func setLaunchGUIAtLogin(_ enabled: Bool) async {
        do {
            try await backgroundService.setLaunchAtLogin(enabled)
            refreshLocalState()
        } catch {
            showError((error as? LocalizedError)?.errorDescription ?? error.localizedDescription)
        }
    }

    /// 打开本机已安装的 OrbStack / Docker Desktop，供环境检查失败时使用。
    func openDockerApp() {
        let candidates = [
            "/Applications/OrbStack.app",
            "/Applications/Docker.app",
            "/Applications/Docker Desktop.app",
        ]
        for path in candidates where FileManager.default.fileExists(atPath: path) {
            NSWorkspace.shared.open(URL(fileURLWithPath: path))
            return
        }
        if let url = URL(string: "https://orbstack.dev") {
            NSWorkspace.shared.open(url)
        }
    }

    func startService() async {
        guard !isBusyService else { return }
        isBusyService = true
        defer { isBusyService = false }
        do {
            try await ensureServiceRunning()
            refreshLocalState()
            await refreshStatus()
            await refreshServices()
            await loadSettings()
        } catch {
            handleServiceError(error)
        }
    }

    func stopService() async {
        guard !isBusyService else { return }
        isBusyService = true
        defer { isBusyService = false }
        if daemonReachable {
            _ = try? await client.call("system.shutdown", as: ShutdownResult.self)
        } else {
            backgroundService.killJob()
        }
        _ = await waitForDaemonToStop(seconds: 15)
        daemonReachable = false
    }

    func restartService() async {
        guard !isBusyService else { return }
        await stopService()
        await startService()
    }

    /// 一键修复：确保服务在运行，补装缺失的代理，然后重新自检。
    func repairService() async {
        guard !isBusyService else { return }
        isBusyService = true
        defer { isBusyService = false }
        do {
            try await ensureServiceRunning()
            refreshLocalState()
            await refreshStatus()
            await refreshServices()
            await loadSettings()
            if status?.proxy?.installed != true {
                let value = try await client.call("proxy.setup", as: PlanApplyResult.self)
                if let operationId = value.operationId {
                    await waitOperationForUninstall(operationId)
                }
                await refreshStatus(silent: true)
            }
            await loadDoctor()
            let remaining = doctorChecks.filter { $0.statusValue == .fail }
            if remaining.isEmpty {
                showNotice("修复完成，所有检查项均已通过。")
            } else {
                showError("已尝试修复，仍有 \(remaining.count) 项需要处理：\(remaining[0].displayTitle)。")
            }
        } catch {
            handleServiceError(error)
        }
    }

    /// 升级随应用一起进行：后台服务二进制就在应用包内，重启即加载新版本。
    /// SMAppService 建议在可执行文件变化后重新注册，因此升级会先注销再注册。
    func upgradeService() async {
        guard !isBusyService else { return }
        isBusyService = true
        defer { isBusyService = false }
        if daemonReachable {
            _ = try? await client.call("system.shutdown", as: ShutdownResult.self)
        }
        _ = await waitForDaemonToStop(seconds: 10)
        if backgroundService.isRegistered {
            try? await backgroundService.unregister()
        }
        do {
            try await ensureServiceRunning()
            refreshLocalState()
            await refreshStatus()
            await refreshServices()
            await loadSettings()
            if let version = status?.daemonVersion, version == appVersion {
                showNotice("后台服务已升级到 \(appVersion)。")
            } else {
                showError("后台服务已重启，但版本仍为 \(status?.daemonVersion ?? "未知")；请查看诊断。")
            }
        } catch {
            handleServiceError(error)
        }
    }

    /// 只移除后台服务与开机启动，保留应用本身。
    func uninstallBackgroundService() async {
        guard !isBusyService && !isUninstalling else { return }
        isBusyService = true
        defer { isBusyService = false }
        if daemonReachable {
            await restoreAllManagedServices()
            await refreshServices()
            if !services.isEmpty {
                showError("仍有 \(services.count) 个托管服务未恢复，已中止。请先在「托管服务」中逐个恢复。")
                return
            }
            _ = try? await client.call("system.shutdown", as: ShutdownResult.self)
        }
        _ = await waitForDaemonToStop(seconds: 10)
        try? await backgroundService.unregister()
        backgroundService.removeLegacyAgent()
        daemonReachable = false
        refreshLocalState()
        showNotice("已恢复托管服务并移除后台服务与开机启动。再次点击「启动」会重新注册。")
    }

    /// 应用内一键卸载。先恢复所有托管服务；任何服务恢复失败都会中止卸载。
    /// `mode = .full` 时还会停止 OrbSleep 自建代理并删除数据目录。
    func uninstallApplication(mode: UninstallMode = .keepData) async {
        guard !isUninstalling else { return }
        isUninstalling = true
        defer { isUninstalling = false }
        let directory = dataDir

        if daemonReachable {
            await restoreAllManagedServices()
            await refreshServices()
            if !services.isEmpty {
                showError("仍有 \(services.count) 个托管服务未恢复，已中止卸载。请先恢复这些服务后再卸载。")
                return
            }
            if mode == .full {
                if let value = try? await client.call("proxy.stop", as: PlanApplyResult.self),
                   let operationId = value.operationId {
                    await waitOperationForUninstall(operationId)
                }
            }
            _ = try? await client.call("system.shutdown", as: ShutdownResult.self)
        }
        _ = await waitForDaemonToStop(seconds: 10)
        try? await backgroundService.unregister()
        backgroundService.removeLegacyAgent()
        backgroundService.removeCLI()
        daemonReachable = false
        refreshLocalState()

        if mode == .full {
            try? FileManager.default.removeItem(atPath: directory)
        }

        let appURL = Bundle.main.bundleURL
        if appURL.path.hasPrefix("/Applications") {
            do {
                try FileManager.default.trashItem(at: appURL, resultingItemURL: nil)
                NSApp.terminate(nil)
            } catch {
                let kept = mode == .full ? "数据与后台服务已移除" : "数据已保留"
                showError("\(kept)，但移动应用失败：\(error.localizedDescription)。请手动把 OrbSleepManager.app 移到废纸篓。")
            }
        } else {
            let kept = mode == .full ? "数据与后台服务已移除" : "数据已保留"
            showNotice("\(kept)。请手动把 OrbSleepManager.app 移到废纸篓以完成卸载。")
        }
    }

    private func restoreAllManagedServices() async {
        await refreshServices()
        for service in services {
            do {
                let value = try await client.call(
                    "service.action",
                    params: [
                        "id": .string(service.id),
                        "action": .string(ServiceAction.restore.rawValue),
                        "params": .object([:])
                    ],
                    as: ServiceActionResult.self
                )
                if let operationId = value.operationId {
                    await waitOperationForUninstall(operationId)
                }
            } catch {
                // Keep restoring the remaining services.
            }
        }
    }

    private func waitOperationForUninstall(_ id: String, timeoutSeconds: Int = 60) async {
        let attempts = max(1, timeoutSeconds * 2)
        for _ in 0..<attempts {
            guard let op = try? await client.call(
                "operation.get",
                params: ["id": .string(id)],
                as: Operation.self
            ) else { return }
            if op.statusValue.isTerminal { return }
            try? await Task.sleep(nanoseconds: 500_000_000)
        }
    }

    private func handleServiceError(_ error: Error) {
        if let serviceError = error as? BackgroundServiceError, case .requiresApproval = serviceError {
            backgroundService.openLoginItemsSettings()
        }
        showError((error as? LocalizedError)?.errorDescription ?? error.localizedDescription)
    }

    // MARK: 命令行工具（可选）

    func installCommandLineTools() {
        do {
            try backgroundService.installCLI()
            cliInstalled = true
            showNotice("已把 orbsleepctl 安装到 ~/.local/bin（可选的高级入口）。")
        } catch {
            showError((error as? LocalizedError)?.errorDescription ?? error.localizedDescription)
        }
    }

    func removeCommandLineTools() {
        backgroundService.removeCLI()
        cliInstalled = false
    }

    // MARK: 日志与诊断

    func loadLogs() async {
        guard !isLoadingLogs else { return }
        isLoadingLogs = true
        defer { isLoadingLogs = false }
        do {
            let value = try await client.call(
                "logs.tail",
                params: ["limit": .number(300), "source": .string(logSource)],
                as: LogsResult.self
            )
            logs = value.lines ?? []
            markReachable()
        } catch {
            if !daemonReachable {
                logs = []
            } else {
                handle(error)
            }
        }
    }

    func loadLogSources() async {
        guard daemonReachable else { return }
        do {
            let value = try await client.call("logs.sources", as: LogSourcesResult.self)
            let sources = value.sources ?? ["daemon"]
            logSources = sources
            if !sources.contains(logSource) {
                logSource = sources.first ?? "daemon"
            }
        } catch {
            // Keep the daemon source as a safe default.
        }
    }

    func setLogSource(_ source: String) async {
        logSource = source
        await loadLogs()
    }

    @discardableResult
    func exportDiagnostics() -> URL? {
        var lines: [String] = []
        lines.append("OrbSleep Manager 诊断报告")
        lines.append("生成时间: \(ISO8601DateFormatter().string(from: Date()))")
        lines.append("应用版本: \(appVersion.isEmpty ? "未知" : appVersion)")
        lines.append("后台服务版本: \(status?.daemonVersion ?? "未连接")")
        lines.append("服务注册: \(backgroundService.statusText)")
        lines.append("Socket: \(socketPath)")
        lines.append("数据目录: \(dataDir)")
        lines.append("Docker: \(status?.context?.endpoint ?? "未知")")
        lines.append("")
        lines.append("自检结果:")
        if doctorChecks.isEmpty {
            lines.append("  (无)")
        } else {
            for check in doctorChecks {
                lines.append("  [\(check.statusValue.title)] \(check.displayTitle): \(check.detail ?? "")")
                if let fix = check.remediationText {
                    lines.append("      → \(fix)")
                }
            }
        }
        lines.append("")
        lines.append("最近日志:")
        for line in logs.suffix(200) {
            lines.append("  \(line.at ?? "") \(line.level ?? "") \(line.message ?? "")")
        }
        let text = lines.joined(separator: "\n")
        let formatter = DateFormatter()
        formatter.dateFormat = "yyyyMMdd-HHmmss"
        let name = "OrbSleep-Diagnostics-\(formatter.string(from: Date())).txt"
        let directory = FileManager.default.urls(for: .downloadsDirectory, in: .userDomainMask).first
            ?? FileManager.default.temporaryDirectory
        let url = directory.appendingPathComponent(name)
        do {
            try text.write(to: url, atomically: true, encoding: .utf8)
            NSWorkspace.shared.activateFileViewerSelecting([url])
            return url
        } catch {
            showError("导出诊断报告失败：\(error.localizedDescription)")
            return nil
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

    func copyToPasteboard(_ text: String) {
        let pasteboard = NSPasteboard.general
        pasteboard.clearContents()
        pasteboard.setString(text, forType: .string)
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

    // MARK: 系统健康 / 环境 / 恢复 / 备份

    func refreshHealth() async {
        do {
            let value = try await client.call("system.health", as: Health.self)
            health = value
            markReachable()
        } catch {
            if !daemonReachable { health = nil } else { handle(error) }
        }
    }

    func loadDockerContexts() async {
        guard daemonReachable else { return }
        do {
            let value = try await client.call("docker.contexts", as: DockerContextsResult.self)
            dockerContexts = value.contexts ?? []
            selectedDockerSocket = value.selected ?? ""
        } catch {
            handle(error)
        }
    }

    func selectDockerContext(_ name: String) async {
        guard daemonReachable else { return }
        do {
            let value = try await client.call(
                "docker.context.select",
                params: ["name": .string(name)],
                as: DockerContextsResult.self
            )
            dockerContexts = value.contexts ?? []
            selectedDockerSocket = value.selected ?? ""
            await refreshStatus(silent: true)
            showNotice("已切换到 Docker 环境：\(name)")
        } catch {
            handle(error)
        }
    }

    func loadRecovery() async {
        guard daemonReachable else { return }
        do {
            let value = try await client.call("system.recovery.list", as: RecoveryListResult.self)
            recoveryTransactions = value.transactions ?? []
        } catch {
            handle(error)
        }
    }

    func resolveRecovery() async {
        guard daemonReachable, !isBusyMaintenance else { return }
        isBusyMaintenance = true
        defer { isBusyMaintenance = false }
        do {
            let value = try await client.call("system.recovery.resolve", as: RecoveryListResult.self)
            recoveryTransactions = value.transactions ?? []
            await refreshHealth()
            showNotice(value.message ?? "恢复处理已执行")
        } catch {
            handle(error)
        }
    }

    func loadBackups() async {
        guard daemonReachable else { return }
        isLoadingMaintenance = true
        defer { isLoadingMaintenance = false }
        do {
            let value = try await client.call("backup.list", as: BackupListResult.self)
            backups = value.backups ?? []
        } catch {
            handle(error)
        }
    }

    func createBackup() async {
        guard daemonReachable, !isBusyMaintenance else { return }
        isBusyMaintenance = true
        defer { isBusyMaintenance = false }
        do {
            let value = try await client.call("backup.create", as: BackupCreateResult.self)
            await loadBackups()
            if let path = value.backup?.path {
                showNotice("备份已创建：\(path)")
            }
        } catch {
            handle(error)
        }
    }

    /// 从备份恢复：后台服务先写恢复标记并退出，随后 GUI 重新启动服务，
    /// 新进程在打开 SQLite 前应用备份。
    func restoreBackup(_ entry: BackupEntry) async {
        guard daemonReachable, !isBusyMaintenance else { return }
        isBusyMaintenance = true
        defer { isBusyMaintenance = false }
        do {
            let result = try await client.call(
                "backup.restore",
                params: ["id": .string(entry.id)],
                as: RestoreResult.self
            )
            _ = await waitForDaemonToStop(seconds: 15)
            try await ensureServiceRunning()
            refreshLocalState()
            await refreshStatus()
            await refreshHealth()
            await refreshServices()
            await loadSettings()
            await loadBackups()
            showNotice(result.message ?? "已从备份 \(entry.id) 恢复")
        } catch {
            handleServiceError(error)
        }
    }

    func exportLogs() async {
        guard daemonReachable else { return }
        do {
            let value = try await client.call(
                "logs.export",
                params: ["limit": .number(2000)],
                as: LogsExportResult.self
            )
            if let path = value.path {
                NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: path)])
                showNotice("日志已导出：\(path)")
            }
        } catch {
            handle(error)
        }
    }

    // MARK: 更新中心

    func checkForUpdates() async {
        updateStatus = .checking
        do {
            if let update = try await updateManager.checkForUpdates() {
                availableUpdate = update
                updateStatus = .available(update)
            } else {
                availableUpdate = nil
                updateStatus = .idle
                showNotice("当前已是最新版本（\(appVersion)）。")
            }
        } catch {
            updateStatus = .failed(error.localizedDescription)
            showError((error as? LocalizedError)?.errorDescription ?? error.localizedDescription)
        }
    }

    func downloadAndInstallUpdate() async {
        guard case .available(let update) = updateStatus else { return }
        updateStatus = .downloading(0)
        do {
            let package = try await updateManager.download(update) { [weak self] progress in
                Task { @MainActor [weak self] in
                    self?.updateStatus = .downloading(progress)
                }
            }
            updateStatus = .installing
            try await updateManager.install(package: package)
        } catch {
            updateStatus = .failed(error.localizedDescription)
            showError((error as? LocalizedError)?.errorDescription ?? error.localizedDescription)
        }
    }

    // MARK: 旧版安装迁移

    /// 从 0.1.x 手动安装迁移：备份 → 安全停止旧服务 → 移除旧启动项 →
    /// 注册包内后台服务 → 验证 → 清理旧二进制。不更换数据目录。
    func migrateLegacyInstall() async {
        guard !isMigrating else { return }
        isMigrating = true
        defer { isMigrating = false }
        refreshLocalState()
        let legacy = legacyInstallation
        guard legacy.isPresent else {
            showNotice("未检测到旧版安装")
            return
        }
        if daemonReachable {
            _ = try? await client.call("backup.create", as: BackupCreateResult.self)
            if let prep = try? await client.call("system.prepareShutdown", as: ShutdownPreparation.self),
               prep.safe == false {
                showError("存在未完成事务，已暂停迁移：\(prep.message ?? "")。请先在“诊断与修复”中执行恢复。")
                return
            }
            _ = try? await client.call("system.shutdown", as: ShutdownResult.self)
            if !(await waitForDaemonToStop(seconds: 10)) {
                // 0.1.x daemons may predate system.shutdown; terminate the
                // known manual-install binary and stop the old launchd job.
                backgroundService.terminateLegacyDaemon()
                _ = await waitForDaemonToStop(seconds: 8)
            }
        }
        backgroundService.bootoutLegacyJob()
        _ = await waitForDaemonToStop(seconds: 5)
        try? backgroundService.removeOldPlist()
        do {
            try await ensureServiceRunning()
        } catch {
            handleServiceError(error)
            return
        }
        var removed: [String] = []
        do {
            removed = try backgroundService.cleanupLegacyBinaries()
        } catch {
            showError("旧版二进制清理未完成：\(error.localizedDescription)。可在“诊断与修复”中重试。")
        }
        refreshLocalState()
        await refreshStatus(silent: true)
        let suffix = removed.isEmpty ? "" : "，并移除 \(removed.count) 个旧二进制"
        showNotice("迁移完成：已切换到应用内后台服务\(suffix)。")
    }

    /// 只清理旧版组件（/usr/local/bin 与旧 plist），不迁移服务。
    func cleanupLegacyComponents() async {
        guard !isBusyMaintenance else { return }
        isBusyMaintenance = true
        defer { isBusyMaintenance = false }
        backgroundService.bootoutLegacyJob()
        try? backgroundService.removeOldPlist()
        do {
            let removed = try backgroundService.cleanupLegacyBinaries()
            refreshLocalState()
            showNotice(removed.isEmpty ? "没有需要清理的旧组件" : "已清理 \(removed.count) 个旧组件")
        } catch {
            showError("清理旧组件失败：\(error.localizedDescription)")
        }
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

    func showNotice(_ message: String) {
        let trimmed = message.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return }
        noticeMessage = trimmed
        noticeDismissTask?.cancel()
        noticeDismissTask = Task { [weak self] in
            try? await Task.sleep(nanoseconds: 8_000_000_000)
            guard !Task.isCancelled else { return }
            self?.noticeMessage = nil
        }
    }

    func dismissNotice() {
        noticeDismissTask?.cancel()
        noticeDismissTask = nil
        noticeMessage = nil
    }
}
