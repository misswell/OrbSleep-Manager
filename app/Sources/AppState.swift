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

/// 自检加载状态：只有真正执行成功才算通过，空结果不能解释为「全部通过」。
enum DoctorLoadState: Equatable {
    case idle
    case loading
    case succeeded
    case failed
}

/// 全局 UI 状态。所有属性都在 MainActor 上，后台请求通过 `CoreClient` 完成。
@MainActor
@Observable
final class AppState {
    private let client: CoreClient
    let backgroundService = BackgroundService()
    /// 互斥维护操作（卸载 / 迁移 / 恢复 / 升级 / 服务生命周期）共用的串行门闩。
    @ObservationIgnored private let maintenanceGate = MaintenanceGate()

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
    var doctorLoadState: DoctorLoadState = .idle
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
    @ObservationIgnored private var didReportRestoreFailure = false
    @ObservationIgnored private var didReportNeedsRepair = false
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
            // A 0.1.x installation can still answer on the socket; complete the
            // migration (old agent removal, bundled service registration,
            // version verification) before trusting the session.
            await refreshLegacyDetection()
            if legacyInstallation.isPresent {
                await migrateLegacyInstall()
            }
            setupCompleted = true
            await loadCoreState()
            if serviceVersionMismatch {
                await upgradeService()
            }
            // The update helper only waits 45s for the health marker: finish
            // the handshake right after the version check, before the slow
            // diagnostics can eat the whole window.
            await completeUpdateHandshake()
            await runAutoDoctor()
        } else {
            await refreshLegacyDetection()
            prepareSetupSteps()
        }
        startPolling()
    }

    /// 互斥维护操作的串行入口。
    private func withMaintenanceLock(_ body: () async -> Void) async {
        await maintenanceGate.acquire()
        defer { Task { await maintenanceGate.release() } }
        await body()
    }

    func refreshLocalState() {
        appVersion = Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? ""
        serviceRegistered = backgroundService.installed
        serviceNeedsApproval = backgroundService.needsApproval
        cliInstalled = backgroundService.cliInstalled
        launchesGUIAtLogin = backgroundService.launchesAtLogin
    }

    /// 旧版安装探测要走 `launchctl print`（异步进程调用）。需要旧版信息的
    /// 调用方显式等待结果，而不是在同步刷新里读一个还没写好的值。
    func refreshLegacyDetection() async {
        legacyInstallation = await backgroundService.detectLegacy()
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
        guard doctorLoadState == .succeeded else { return }
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

    /// 刷新托管服务列表。返回是否真正读取成功：读取失败时旧数组保持原样，
    /// 安全流程（关闭自启动、卸载）必须把它当成 fail-closed 信号，而不是
    /// 把「读取失败」当成「没有服务」。
    @discardableResult
    func refreshServices(silent: Bool = false) async -> Bool {
        do {
            let value = try await client.call("services.list", as: ServicesResult.self)
            services = value.services ?? []
            markReachable()
            return true
        } catch {
            if !silent { handle(error) }
            return false
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
                        if !(await waitOperation(operationId, timeoutSeconds: 1200)) {
                            updateStep("proxy", state: .failed, message: "代理安装操作未成功，请查看操作记录")
                        }
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
        if doctorPassed {
            updateStep("doctor", state: .done, message: "全部通过")
            setupCompleted = true
            section = .overview
        } else if doctorLoadState == .succeeded {
            let failures = doctorChecks.filter { $0.statusValue == .fail }
            updateStep("doctor", state: .failed, message: "\(failures.count) 项需要处理")
            setupCompleted = true
            didAutoDoctorAlert = true
            showError("安装完成，但自检有 \(failures.count) 项需要处理。可在「诊断与修复」中查看并一键修复。")
        } else {
            updateStep("doctor", state: .failed, message: "自检执行失败")
            setupCompleted = true
            showError("自检未能完成（\(connectionError ?? "请求失败")）。可在「诊断与修复」中重新执行。")
        }
    }

    /// 注册并启动后台服务。正式版只走 SMAppService；旧式 LaunchAgent 仅作为
    /// 未签名开发构建（DEBUG）的降级方案，避免用同一 Label 掩盖启动失败。
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
                #if DEBUG
                guard let daemon = backgroundService.bundledDaemonURL else {
                    throw BackgroundServiceError.missingBundledExecutable("orbsleepd")
                }
                try await backgroundService.installLegacyAgent(daemon: daemon)
                refreshLocalState()
                #else
                throw error
                #endif
            }
            if backgroundService.needsApproval {
                backgroundService.openLoginItemsSettings()
                throw BackgroundServiceError.requiresApproval
            }
            // SMAppService starts the agent on register; the legacy CLI waits
            // for the socket itself. Avoid a redundant kill/restart.
            if await waitForDaemon(seconds: 10) { return }
        }
        try? await backgroundService.kickstart()
        if await waitForDaemon(seconds: 20) { return }
        #if DEBUG
        if let daemon = backgroundService.bundledDaemonURL {
            try? await backgroundService.installLegacyAgent(daemon: daemon)
        }
        #endif
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
                    #if DEBUG
                    if let daemon = backgroundService.bundledDaemonURL {
                        try? await backgroundService.installLegacyAgent(daemon: daemon)
                    }
                    #else
                    showError((error as? LocalizedError)?.errorDescription ?? error.localizedDescription)
                    #endif
                }
            }
        } else {
            // After a reboot no daemon would come back: managed services would
            // keep their ports removed while nothing listens on them, and a
            // leftover failed transaction would never be recovered. Fail
            // closed on every step before unregistering.
            guard await refreshServices() else {
                showError("无法读取托管服务列表（fail-closed）。已保留开机自启动，请稍后重试。")
                return
            }
            do {
                let prep = try await client.call("system.prepareShutdown", as: ShutdownPreparation.self)
                if prep.safe == false {
                    showError("存在未完成的操作或事务（\(prep.message ?? "")）。请先处理后再关闭自启动。")
                    return
                }
            } catch {
                showError("无法确认安全状态（\(errorMessage(error))）。已保留开机自启动。")
                return
            }
            if !services.isEmpty {
                if !(await restoreAllManagedServices()) {
                    showError("恢复托管服务失败，已保留开机自启动。请先在服务列表中恢复后再关闭。")
                    return
                }
                guard await refreshServices(silent: true) else {
                    showError("无法确认托管服务状态（fail-closed）。已保留开机自启动。")
                    return
                }
                if !services.isEmpty {
                    showError("仍有托管服务未恢复，已保留开机自启动。")
                    return
                }
            }
            try? await backgroundService.unregister()
            await backgroundService.removeLegacyAgent()
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
        await withMaintenanceLock {
            guard !isBusyService else { return }
            isBusyService = true
            defer { isBusyService = false }
            await startServiceLocked()
        }
    }

    private func startServiceLocked() async {
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

    /// 「停止是否成功」由 stopServiceLocked 记录；重启前必须确认旧进程真的退出了。
    @ObservationIgnored private var lastStopConfirmed = true

    func stopService() async {
        await withMaintenanceLock {
            guard !isBusyService else { return }
            isBusyService = true
            defer { isBusyService = false }
            await stopServiceLocked()
        }
    }

    /// - Parameter force: 重启/升级流程在自身保护后立即拉起新进程，用 force
    ///   跳过托管服务检查；普通「停止」不 force，由后台 fail-closed 拒绝。
    private func stopServiceLocked(force: Bool = false) async {
        lastStopConfirmed = false
        if daemonReachable {
            do {
                let result = try await client.call(
                    "system.shutdown",
                    params: ["force": .bool(force)],
                    as: ShutdownResult.self
                )
                if result.ok == false {
                    showError("后台服务拒绝了停止请求，未执行后续操作。")
                    return
                }
            } catch {
                handle(error)
                return
            }
        } else {
            await backgroundService.killJob()
        }
        guard await waitForDaemonToStop(seconds: 15) else {
            showError("后台服务未能在 15 秒内退出。状态未更新，请稍后在诊断中重试。")
            return
        }
        daemonReachable = false
        lastStopConfirmed = true
        refreshLocalState()
    }

    func restartService() async {
        await withMaintenanceLock {
            guard !isBusyService else { return }
            isBusyService = true
            defer { isBusyService = false }
            await stopServiceLocked(force: true)
            // If the old process never exited, "start" would just re-probe the
            // still-running daemon and report success without a real restart.
            guard lastStopConfirmed else { return }
            await startServiceLocked()
        }
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
                    _ = await waitOperation(operationId, timeoutSeconds: 600)
                }
                await refreshStatus(silent: true)
            }
            await loadDoctor()
            guard doctorLoadState == .succeeded else {
                showError("无法完成修复后的自检：\(connectionError ?? "请求失败")。请稍后重试。")
                return
            }
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
        await withMaintenanceLock {
            guard !isBusyService else { return }
            isBusyService = true
            defer { isBusyService = false }
            await upgradeServiceLocked()
        }
    }

    private func upgradeServiceLocked() async {
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
    /// 严格 fail-closed：后台不可用、恢复失败、代理未停止、停止未确认，
    /// 任何一步失败都中止，绝不带着无法恢复的托管状态拆掉后台。
    func uninstallBackgroundService() async {
        await withMaintenanceLock {
            guard !isBusyService && !isUninstalling else { return }
            isBusyService = true
            defer { isBusyService = false }
            guard await prepareForRemoval(stopProxy: true) else { return }
            do {
                _ = try await client.call("system.shutdown", as: ShutdownResult.self)
            } catch {
                showError("无法通知后台服务退出：\(errorMessage(error))。已中止。")
                return
            }
            guard await waitForDaemonToStop(seconds: 15) else {
                showError("后台服务未退出，已中止移除。")
                return
            }
            do {
                try await backgroundService.unregister()
            } catch {
                showError("注销后台服务失败：\(errorMessage(error))。已中止。")
                return
            }
            await backgroundService.removeLegacyAgent()
            daemonReachable = false
            refreshLocalState()
            showNotice("已恢复托管服务并移除后台服务与开机启动。再次点击「启动」会重新注册。")
        }
    }

    /// 应用内一键卸载。fail-closed 状态机：
    /// 确认后台可用 → 恢复全部托管服务 → 确认为 0 → 停止并确认代理 →
    /// 优雅停止并确认进程退出 → 注销 LaunchAgent → 最后才允许删除数据。
    func uninstallApplication(mode: UninstallMode = .keepData) async {
        await withMaintenanceLock {
            guard !isUninstalling else { return }
            isUninstalling = true
            defer { isUninstalling = false }
            let directory = dataDir

            guard await prepareForRemoval(stopProxy: true) else { return }

            do {
                _ = try await client.call("system.shutdown", as: ShutdownResult.self)
            } catch {
                showError("无法通知后台服务退出：\(errorMessage(error))。已中止卸载。")
                return
            }
            guard await waitForDaemonToStop(seconds: 15) else {
                showError("后台服务未退出，已中止卸载（数据未删除）。")
                return
            }
            do {
                try await backgroundService.unregister()
            } catch {
                showError("注销后台服务失败：\(errorMessage(error))。已中止卸载。")
                return
            }
            await backgroundService.removeLegacyAgent()
            backgroundService.removeCLI()
            daemonReachable = false
            refreshLocalState()

            // Only after every protection step passed may data be deleted.
            var dataDeleted = false
            if mode == .full {
                do {
                    try FileManager.default.removeItem(atPath: directory)
                    dataDeleted = true
                } catch {
                    showError("删除数据目录失败（\(error.localizedDescription)）。应用与服务已移除，但数据仍保留于 \(directory)。请手动删除。")
                }
            }

            let appURL = Bundle.main.bundleURL
            if appURL.path.hasPrefix("/Applications") {
                do {
                    try FileManager.default.trashItem(at: appURL, resultingItemURL: nil)
                    NSApp.terminate(nil)
                } catch {
                    let kept = dataDeleted ? "数据与后台服务已移除" : "数据已保留"
                    showError("\(kept)，但移动应用失败：\(error.localizedDescription)。请手动把 OrbSleepManager.app 移到废纸篓。")
                }
            } else {
                let kept = dataDeleted ? "数据与后台服务已移除" : "数据已保留"
                showNotice("\(kept)。请手动把 OrbSleepManager.app 移到废纸篓以完成卸载。")
            }
        }
    }

    /// 卸载/移除后台服务前的公共保护链：任何一步失败都返回 false 并中止。
    private func prepareForRemoval(stopProxy: Bool) async -> Bool {
        // 1. The daemon must be reachable: without it we cannot see (let alone
        //    restore) managed services, and deleting state would destroy the
        //    only recovery material.
        if !daemonReachable {
            do {
                try await ensureServiceRunning()
            } catch {
                showError("后台服务不可用（\(errorMessage(error))）。为避免托管服务失去恢复能力，已中止卸载。请先修复后台服务。")
                return false
            }
        }

        // 2. Restore every managed service and require a clean board.
        if !(await restoreAllManagedServices()) { return false }
        await refreshServices()
        if !services.isEmpty {
            showError("仍有 \(services.count) 个托管服务未恢复，已中止卸载。请先恢复这些服务后再卸载。")
            return false
        }

        // 2b. The board must also be free of unresolved work: rollback_failed /
        // needs_repair transactions whose service record never existed, and
        // running operations. Their recovery material lives only in the data
        // directory this flow is about to remove — fail closed instead.
        do {
            let prep = try await client.call("system.prepareShutdown", as: ShutdownPreparation.self)
            if prep.safe == false {
                showError("存在未完成的操作或事务（\(prep.message ?? "")）。请先在「诊断与修复」中处理后再卸载。")
                return false
            }
        } catch {
            showError("无法确认卸载安全状态（\(errorMessage(error))）。已中止卸载。")
            return false
        }

        // 3. Stop OrbSleep's own proxy and verify it really stopped; otherwise
        //    an unmanaged proxy container would survive the daemon.
        if stopProxy {
            do {
                let value = try await client.call("proxy.stop", as: PlanApplyResult.self)
                if let operationId = value.operationId {
                    guard await waitOperation(operationId, timeoutSeconds: 180) else { return false }
                }
            } catch {
                showError("停止 Lazy TCP Proxy 失败：\(errorMessage(error))。已中止卸载。")
                return false
            }
            await refreshStatus(silent: true)
            if status?.proxy?.running == true {
                showError("代理容器仍在运行，已中止卸载。请在「诊断与修复」中处理后重试。")
                return false
            }
        }
        return true
    }

    /// 恢复全部托管服务；任何一次恢复失败（RPC 失败、操作失败、超时、
    /// 服务列表读取失败）都中止。
    private func restoreAllManagedServices() async -> Bool {
        guard await refreshServices() else {
            showError("无法读取托管服务列表（fail-closed）。已中止。")
            return false
        }
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
                    guard await waitOperation(operationId) else { return false }
                }
            } catch {
                showError("恢复 \(service.displayName) 失败：\(errorMessage(error))。已中止。")
                return false
            }
        }
        return true
    }

    /// 等待操作终结。失败、超时、RPC 异常一律返回 false 并给出原因，
    /// 绝不静默吞掉后继续删除步骤。
    @discardableResult
    private func waitOperation(_ id: String, timeoutSeconds: Int = 120) async -> Bool {
        let attempts = max(1, timeoutSeconds * 2)
        for _ in 0..<attempts {
            do {
                let op = try await client.call(
                    "operation.get",
                    params: ["id": .string(id)],
                    as: Operation.self
                )
                if op.statusValue.isTerminal {
                    guard op.statusValue == .succeeded || op.statusValue == .rolledBack else {
                        showError("操作未成功结束（\(op.statusValue.title)）：\(op.error ?? "无详情")。已中止后续步骤。")
                        return false
                    }
                    return true
                }
            } catch {
                showError("无法获取操作状态：\(errorMessage(error))。已中止后续步骤。")
                return false
            }
            try? await Task.sleep(nanoseconds: 500_000_000)
        }
        showError("操作在 \(timeoutSeconds) 秒内未完成，已中止后续步骤。")
        return false
    }

    private func errorMessage(_ error: Error) -> String {
        (error as? LocalizedError)?.errorDescription ?? error.localizedDescription
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

    /// 加载自检结果（含 verify=true 的深度校验）。只有请求真正成功才算
    /// 「已通过」：请求失败时检查列表可能是空的，不能把空列表当成全部通过。
    func loadDoctor() async {
        isLoadingDoctor = true
        defer { isLoadingDoctor = false }
        doctorLoadState = .loading
        do {
            let value = try await client.call(
                "system.doctor",
                params: ["verify": .bool(true)],
                as: DoctorResult.self
            )
            doctorChecks = value.checks ?? []
            markReachable()
            doctorLoadState = .succeeded
        } catch {
            doctorLoadState = .failed
            doctorChecks = []
            handle(error)
        }
    }

    /// 自检是否真正通过（必须加载成功且没有失败项）。
    var doctorPassed: Bool {
        doctorLoadState == .succeeded && !doctorChecks.isEmpty
            && doctorChecks.allSatisfy { $0.statusValue != .fail }
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
            // Surface recoveries the user must know about, exactly once.
            if value.restoreFailed == true, !didReportRestoreFailure {
                didReportRestoreFailure = true
                showError("上次备份恢复失败，后台已用现有数据库继续启动。恢复证据保留在数据目录（restore-failed-*.json），可在「日志与维护」中查看。")
            }
            if (value.needsRepairTransactions ?? 0) > 0, !didReportNeedsRepair {
                didReportNeedsRepair = true
                showError("有恢复失败的事务需要人工重试，请在「诊断与修复」中处理。")
            }
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

    func resolveRecovery(id: String? = nil) async {
        guard daemonReachable, !isBusyMaintenance else { return }
        isBusyMaintenance = true
        defer { isBusyMaintenance = false }
        do {
            var params: [String: JSONValue] = [:]
            if let id { params["id"] = .string(id) }
            let value = try await client.call(
                "system.recovery.resolve",
                params: params,
                as: RecoveryListResult.self
            )
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
    /// 新进程在打开 SQLite 前应用备份。恢复属于独占维护操作。
    func restoreBackup(_ entry: BackupEntry) async {
        await withMaintenanceLock {
            guard daemonReachable, !isBusyMaintenance else { return }
            isBusyMaintenance = true
            defer { isBusyMaintenance = false }
            do {
                let result = try await client.call(
                    "backup.restore",
                    params: ["id": .string(entry.id)],
                    as: RestoreResult.self
                )
                guard await waitForDaemonToStop(seconds: 15) else {
                    showError("后台服务未按预期退出，恢复未开始。请在诊断中确认后台状态。")
                    return
                }
                try await ensureServiceRunning()
                refreshLocalState()
                await refreshStatus()
                await refreshHealth()
                await refreshServices()
                await loadSettings()
                await loadBackups()
                if health?.restoreFailed == true {
                    showError("恢复失败：已保留恢复失败证据并继续使用现有数据库。")
                } else {
                    showNotice(result.message ?? "已从备份 \(entry.id) 恢复")
                }
            } catch {
                handleServiceError(error)
            }
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
        await withMaintenanceLock {
            guard case .available = updateStatus else { return }
            // 升级前事务安全检查：接入、恢复、代理配置修改进行中不能换包。
            if daemonReachable {
                do {
                    let prep = try await client.call("system.prepareUpdate", as: ShutdownPreparation.self)
                    if prep.safe == false {
                        showError("存在未完成操作或事务，已暂停升级：\(prep.message ?? "")")
                        return
                    }
                } catch {
                    showError("升级前检查失败（\(errorMessage(error))），已暂停升级。")
                    return
                }
            }
            updateStatus = .downloading(0)
            do {
                let package = try await updateManager.download(update) { [weak self] progress in
                    Task { @MainActor [weak self] in
                        self?.updateStatus = .downloading(progress)
                    }
                }
                updateStatus = .installing
                try await updateManager.install(package: package, expectedNewVersion: update.version, dataDir: dataDir)
            } catch {
                updateStatus = .failed(error.localizedDescription)
                showError((error as? LocalizedError)?.errorDescription ?? error.localizedDescription)
            }
        }
    }

    // MARK: 更新健康握手

    /// 更新后的健康握手：UpdateManager 在替换包之前写入 update-in-progress
    /// 标记，回滚脚本轮询 update-ok 文件。只有 GUI 正常启动且后台服务版本
    /// 与 App 一致时才写 update-ok，否则脚本在超时后自动回滚旧版本。
    private func completeUpdateHandshake() async {
        let fm = FileManager.default
        let dir = URL(fileURLWithPath: dataDir)
        let inProgress = dir.appendingPathComponent("update-in-progress.json")
        guard fm.fileExists(atPath: inProgress.path) else { return }
        struct Marker: Decodable {
            var expectedVersion: String
        }
        guard let data = try? Data(contentsOf: inProgress),
              let marker = try? JSONDecoder().decode(Marker.self, from: data),
              marker.expectedVersion == appVersion,
              status?.daemonVersion == appVersion else {
            // Mismatched marker: leave it; the rollback script will time out
            // and restore the old version.
            return
        }
        // Tell the relaunch script the update is healthy, then clean up.
        if !fm.createFile(atPath: dir.appendingPathComponent("update-ok").path, contents: Data()) {
            return
        }
        try? fm.removeItem(at: inProgress)
        // Remove the handshake file once the script had a chance to see it.
        try? await Task.sleep(nanoseconds: 10_000_000_000)
        try? fm.removeItem(at: dir.appendingPathComponent("update-ok"))
    }

    // MARK: 旧版安装迁移

    /// 从 0.1.x 手动安装迁移。任何一步失败都不会清理旧组件，也不显示
    /// 「迁移完成」：
    ///
    /// 备份（新 API 或退出后直接拷库）→ 事务安全检查 → 停止旧服务 →
    /// 移除旧 LaunchAgent（内容先留作回滚）→ 注册并启动包内服务 →
    /// 校验版本与 Socket → 通过后才清理旧二进制。
    ///
    /// 0.1.x 没有 backup.create / system.prepareShutdown / system.shutdown，
    /// 因此旧后台走专用路径：先经 launchd/pkill 停止，再做文件级备份。
    func migrateLegacyInstall() async {
        await withMaintenanceLock {
            guard !isMigrating else { return }
            isMigrating = true
            defer { isMigrating = false }
            await migrateLegacyInstallLocked()
        }
    }

    private func migrateLegacyInstallLocked() async {
        await refreshLegacyDetection()
        let legacy = legacyInstallation
        guard legacy.isPresent else {
            showNotice("未检测到旧版安装")
            return
        }

        // The old plist is the rollback material for every destructive step
        // below. Read it before touching launchd: once the job is booted out
        // there is no second chance to save it. A plist we cannot read must
        // stop the migration — continuing would destroy rollback material we
        // never captured.
        let savedPlist = backgroundService.readOldPlistData()
        if savedPlist == nil && legacy.plistExists {
            showError("无法读取旧 LaunchAgent 内容（可能是权限问题）。已中止迁移，旧服务保持原状。")
            return
        }
        // Restore the old agent after a mid-migration failure. Without this a
        // backup failure would leave the old service unloaded and unstarted.
        var legacyBootedOut = false
        func restoreLegacyAgent() async {
            if legacyBootedOut, let savedPlist {
                _ = await backgroundService.reinstallLegacyPlist(data: savedPlist)
                legacyBootedOut = false
            }
        }

        if await probeDaemon() && legacy.daemonExists {
            await refreshStatus(silent: true)
        }
        let socketRunsNewDaemon = daemonReachable && status?.daemonVersion == appVersion

        // 阶段 1：备份 + 事务安全检查。
        if socketRunsNewDaemon {
            // 新版后台在跑：用正式 API 备份并检查事务；失败一律中止。
            do {
                _ = try await client.call("backup.create", as: BackupCreateResult.self)
            } catch {
                showError("迁移中止：无法创建迁移备份（\(errorMessage(error))）。")
                return
            }
            do {
                let prep = try await client.call("system.prepareShutdown", as: ShutdownPreparation.self)
                if prep.safe == false {
                    showError("存在未完成事务，已暂停迁移：\(prep.message ?? "")。请先在「诊断与修复」中执行恢复。")
                    return
                }
            } catch {
                showError("迁移中止：安全检查失败（\(errorMessage(error))）。")
                return
            }
            _ = try? await client.call(
                "system.shutdown",
                params: ["force": .bool(true)],
                as: ShutdownResult.self
            )
            if !(await waitForDaemonToStop(seconds: 10)) {
                backgroundService.terminateLegacyDaemon()
                _ = await waitForDaemonToStop(seconds: 8)
            }
        } else {
            // 0.1.x 或不可达：先停旧服务（旧版没有优雅停止 API），
            // 退出后做文件级备份——0.1.x 无法通过 API 备份。
            if await probeDaemon() {
                backgroundService.terminateLegacyDaemon()
                if !(await waitForDaemonToStop(seconds: 8)) {
                    showError("旧后台服务未能退出，已中止迁移。")
                    return
                }
            }
            do {
                try await backgroundService.bootoutLegacyJob()
                legacyBootedOut = true
            } catch {
                showError("无法卸载旧 LaunchAgent（\(errorMessage(error))）。已中止迁移，旧服务保持原状。")
                return
            }
            if !(await waitForDaemonToStop(seconds: 5)) {
                showError("旧后台服务仍未退出，已中止迁移。")
                await restoreLegacyAgent()
                return
            }
            if !backupDataFilesForMigration() {
                await restoreLegacyAgent()
                return
            }
        }

        // 阶段 2：移除旧 LaunchAgent（内容已留作回滚材料）。
        do {
            try await backgroundService.bootoutLegacyJob()
            legacyBootedOut = true
        } catch {
            showError("无法卸载旧 LaunchAgent（\(errorMessage(error))）。已中止迁移。")
            await restoreLegacyAgent()
            return
        }
        do {
            try backgroundService.removeOldPlist()
        } catch {
            // A plist that survives deletion may reload the old daemon at the
            // next login alongside the new one. Roll back and stop here.
            showError("删除旧 LaunchAgent 失败（\(errorMessage(error))）。已中止迁移并恢复旧服务。")
            await restoreLegacyAgent()
            return
        }
        if !(await waitForDaemonToStop(seconds: 5)) {
            showError("旧后台服务在卸载启动项后仍未退出，已中止迁移并恢复旧服务。")
            await restoreLegacyAgent()
            return
        }

        // 阶段 3：注册并启动包内服务，校验版本。
        do {
            try await ensureServiceRunning()
        } catch {
            handleServiceError(error)
            if let savedPlist {
                _ = await backgroundService.reinstallLegacyPlist(data: savedPlist)
                showNotice("新版后台服务启动失败，已尝试恢复旧 LaunchAgent；旧组件未清理。")
            } else {
                showError("新版后台服务启动失败，迁移未完成（旧组件未清理）。")
            }
            await refreshLegacyDetection()
            return
        }
        await refreshStatus(silent: true)
        guard status?.daemonVersion == appVersion, backgroundService.installed else {
            showError("新版后台服务版本校验失败（当前 \(status?.daemonVersion ?? "未知")），迁移未完成，已恢复旧 LaunchAgent。")
            await restoreLegacyAgent()
            await refreshLegacyDetection()
            return
        }

        // 阶段 4：校验通过后才允许清理旧二进制。
        var removed: [String] = []
        do {
            removed = try await backgroundService.cleanupLegacyBinaries()
        } catch {
            showError("旧版二进制清理未完成：\(errorMessage(error))。可在「诊断与修复」中重试。")
        }
        await refreshLegacyDetection()
        await refreshStatus(silent: true)
        let suffix = removed.isEmpty ? "" : "，并移除 \(removed.count) 个旧二进制"
        showNotice("迁移完成：已切换到应用内后台服务\(suffix)。")
    }

    /// 迁移前的文件级备份（旧版后台无备份 API）：停机后拷贝数据库与设置，
    /// 并校验拷贝出的 SQLite 头部，失败即中止迁移。
    private func backupDataFilesForMigration() -> Bool {
        let fm = FileManager.default
        let root = URL(fileURLWithPath: dataDir)
        let dest = root
            .appendingPathComponent("backups", isDirectory: true)
            .appendingPathComponent("migration", isDirectory: true)
            .appendingPathComponent("pre-migration-\(Int(Date().timeIntervalSince1970))", isDirectory: true)
        do {
            try fm.createDirectory(at: dest, withIntermediateDirectories: true)
        } catch {
            showError("无法创建迁移备份目录：\(error.localizedDescription)。已中止迁移。")
            return false
        }
        var dbCopied = false
        for name in ["state.sqlite", "state.sqlite-wal", "state.sqlite-shm", "settings.json"] {
            let src = root.appendingPathComponent(name)
            guard fm.fileExists(atPath: src.path) else { continue }
            do {
                try fm.copyItem(at: src, to: dest.appendingPathComponent(name))
                if name == "state.sqlite" { dbCopied = true }
            } catch {
                showError("迁移备份失败（\(name)）：\(error.localizedDescription)。已中止迁移。")
                return false
            }
        }
        guard dbCopied else {
            showError("数据目录中没有 state.sqlite，未做迁移备份。已中止迁移。")
            return false
        }
        if let head = try? Data(contentsOf: dest.appendingPathComponent("state.sqlite")).prefix(16),
           head != Data("SQLite format 3\u{00}".utf8) {
            showError("迁移备份的数据库头部异常，已中止迁移。")
            return false
        }
        showNotice("已创建迁移备份：\(dest.path)")
        return true
    }

    /// 只清理旧版组件。仅当新版后台正常运行、版本一致且没有托管服务时才允许：
    /// 旧 LaunchAgent 可能还在承担托管职责。
    func cleanupLegacyComponents() async {
        await withMaintenanceLock {
            guard !isBusyMaintenance else { return }
            isBusyMaintenance = true
            defer { isBusyMaintenance = false }
            guard await probeDaemon() else {
                showError("后台服务未运行，暂不能清理旧组件。")
                return
            }
            await refreshStatus(silent: true)
            guard status?.daemonVersion == appVersion else {
                showError("后台服务版本（\(status?.daemonVersion ?? "未知")）与 App（\(appVersion)）不一致，暂不能清理旧组件。")
                return
            }
            await refreshServices()
            guard services.isEmpty else {
                showError("仍有 \(services.count) 个托管服务，暂不能清理旧组件。")
                return
            }
            do {
                try await backgroundService.bootoutLegacyJob()
            } catch {
                showError("无法卸载旧 LaunchAgent（\(errorMessage(error))）。已中止清理。")
                return
            }
            try? backgroundService.removeOldPlist()
            do {
                let removed = try await backgroundService.cleanupLegacyBinaries()
                await refreshLegacyDetection()
                showNotice(removed.isEmpty ? "没有需要清理的旧组件" : "已清理 \(removed.count) 个旧组件")
            } catch {
                showError("清理旧组件失败：\(errorMessage(error))")
            }
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
