import Foundation

// MARK: - 通用 JSON 值

/// 用于承载协议中未建模的任意 JSON（例如 `container.inspect` 的原始 inspect 结构）。
enum JSONValue: Codable, Sendable, Hashable {
    case null
    case bool(Bool)
    case number(Double)
    case string(String)
    case array([JSONValue])
    case object([String: JSONValue])

    init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        if container.decodeNil() {
            self = .null
        } else if let value = try? container.decode(Bool.self) {
            self = .bool(value)
        } else if let value = try? container.decode(Double.self) {
            self = .number(value)
        } else if let value = try? container.decode(String.self) {
            self = .string(value)
        } else if let value = try? container.decode([JSONValue].self) {
            self = .array(value)
        } else if let value = try? container.decode([String: JSONValue].self) {
            self = .object(value)
        } else {
            throw DecodingError.dataCorruptedError(in: container, debugDescription: "无法识别的 JSON 值")
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        switch self {
        case .null:
            try container.encodeNil()
        case .bool(let value):
            try container.encode(value)
        case .number(let value):
            try container.encode(value)
        case .string(let value):
            try container.encode(value)
        case .array(let value):
            try container.encode(value)
        case .object(let value):
            try container.encode(value)
        }
    }
}

extension JSONValue {
    var stringValue: String? {
        if case .string(let value) = self { return value }
        return nil
    }

    var boolValue: Bool? {
        if case .bool(let value) = self { return value }
        return nil
    }

    var numberValue: Double? {
        if case .number(let value) = self { return value }
        return nil
    }

    var intValue: Int? {
        switch self {
        case .number(let value):
            return Int(value)
        case .string(let value):
            return Int(value)
        default:
            return nil
        }
    }

    var arrayValue: [JSONValue]? {
        if case .array(let value) = self { return value }
        return nil
    }

    var objectValue: [String: JSONValue]? {
        if case .object(let value) = self { return value }
        return nil
    }

    /// 格式化输出，用于展示原始 inspect JSON。
    func prettyPrinted() -> String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        guard let data = try? encoder.encode(self), let text = String(data: data, encoding: .utf8) else {
            return "{}"
        }
        return text
    }
}

// MARK: - 枚举

/// 原因 / 风险 / 警告的严重程度。未知值降级为 `.unknown`，不抛错。
enum Severity: String, Codable, Sendable, Hashable {
    case info
    case warning
    case blocker
    case unknown

    init(from decoder: Decoder) throws {
        let raw = (try? decoder.singleValueContainer().decode(String.self)) ?? ""
        self = Severity(rawValue: raw) ?? .unknown
    }
}

/// 容器接入判断结果。
enum Verdict: String, Codable, Sendable, Hashable {
    case eligible
    case needsReview = "needs_review"
    case excluded
    case unknown

    init(from decoder: Decoder) throws {
        let raw = (try? decoder.singleValueContainer().decode(String.self)) ?? ""
        self = Verdict(rawValue: raw) ?? .unknown
    }

    var title: String {
        switch self {
        case .eligible: return "可接入"
        case .needsReview: return "需确认"
        case .excluded: return "排除"
        case .unknown: return "未知"
        }
    }

    var isSelectable: Bool {
        self == .eligible || self == .needsReview
    }
}

/// 判断筛选（UI 用）。
enum VerdictFilter: String, CaseIterable, Identifiable, Hashable {
    case all
    case eligible
    case needsReview
    case excluded

    var id: String { rawValue }

    var title: String {
        switch self {
        case .all: return "全部"
        case .eligible: return "可接入"
        case .needsReview: return "需确认"
        case .excluded: return "排除"
        }
    }

    func matches(_ verdict: Verdict) -> Bool {
        switch self {
        case .all: return true
        case .eligible: return verdict == .eligible
        case .needsReview: return verdict == .needsReview
        case .excluded: return verdict == .excluded
        }
    }
}

/// Docker 原始容器状态。
enum ContainerState: String, Codable, Sendable, Hashable {
    case running
    case exited
    case created
    case paused
    case restarting
    case dead
    case removing
    case unknown

    init(from decoder: Decoder) throws {
        let raw = (try? decoder.singleValueContainer().decode(String.self)) ?? ""
        self = ContainerState(rawValue: raw) ?? .unknown
    }

    var title: String {
        switch self {
        case .running: return "运行中"
        case .exited: return "已退出"
        case .created: return "已创建"
        case .paused: return "已暂停"
        case .restarting: return "重启中"
        case .dead: return "已失效"
        case .removing: return "删除中"
        case .unknown: return "未知"
        }
    }
}

/// 托管服务状态。
enum ServiceStatus: String, Codable, Sendable, Hashable {
    case managedRunning = "managed_running"
    case managedIdle = "managed_idle"
    case managedStopped = "managed_stopped"
    case missing
    case warning
    case failed
    case unknown

    init(from decoder: Decoder) throws {
        let raw = (try? decoder.singleValueContainer().decode(String.self)) ?? ""
        self = ServiceStatus(rawValue: raw) ?? .unknown
    }

    var title: String {
        switch self {
        case .managedRunning: return "运行中"
        case .managedIdle: return "空闲"
        case .managedStopped: return "已休眠"
        case .missing: return "容器缺失"
        case .warning: return "警告"
        case .failed: return "失败"
        case .unknown: return "未知"
        }
    }
}

/// 操作状态。
enum OperationStatus: String, Codable, Sendable, Hashable {
    case running
    case succeeded
    case failed
    case rolledBack = "rolled_back"
    case rollbackFailed = "rollback_failed"
    case unknown

    init(from decoder: Decoder) throws {
        let raw = (try? decoder.singleValueContainer().decode(String.self)) ?? ""
        self = OperationStatus(rawValue: raw) ?? .unknown
    }

    var title: String {
        switch self {
        case .running: return "执行中"
        case .succeeded: return "成功"
        case .failed: return "失败"
        case .rolledBack: return "已回滚"
        case .rollbackFailed: return "回滚失败"
        case .unknown: return "未知"
        }
    }

    var isTerminal: Bool {
        self != .running && self != .unknown
    }
}

/// 操作类型。
enum OperationKind: String, Codable, Sendable, Hashable {
    case attach
    case detach
    case restore
    case proxySetup = "proxy_setup"
    case start
    case stop
    case unknown

    init(from decoder: Decoder) throws {
        let raw = (try? decoder.singleValueContainer().decode(String.self)) ?? ""
        self = OperationKind(rawValue: raw) ?? .unknown
    }

    var title: String {
        switch self {
        case .attach: return "接入"
        case .detach: return "移除托管"
        case .restore: return "恢复端口映射"
        case .proxySetup: return "代理安装"
        case .start: return "启动容器"
        case .stop: return "停止容器"
        case .unknown: return "未知操作"
        }
    }
}

/// 计划状态。
enum PlanStatus: String, Codable, Sendable, Hashable {
    case ready
    case blocked
    case unknown

    init(from decoder: Decoder) throws {
        let raw = (try? decoder.singleValueContainer().decode(String.self)) ?? ""
        self = PlanStatus(rawValue: raw) ?? .unknown
    }
}

/// 自检项状态。
enum DoctorStatus: String, Codable, Sendable, Hashable {
    case ok
    case warn
    case fail
    case unknown

    init(from decoder: Decoder) throws {
        let raw = (try? decoder.singleValueContainer().decode(String.self)) ?? ""
        self = DoctorStatus(rawValue: raw) ?? .unknown
    }
}

// MARK: - 基础结构

struct Reason: Codable, Sendable, Hashable {
    var code: String?
    var message: String?
    var severity: Severity?

    var text: String {
        if let message, !message.isEmpty { return message }
        if let code, !code.isEmpty { return code }
        return "未知原因"
    }

    var severityValue: Severity { severity ?? .info }
}

struct PortBinding: Codable, Sendable, Hashable {
    var hostIP: String?
    var hostPort: Int?
    var containerPort: Int?
    var proxyPort: Int?
    /// 协议字段名为 `protocol`，Swift 保留字映射为 `proto`。
    var proto: String?

    enum CodingKeys: String, CodingKey {
        case hostIP
        case hostPort
        case containerPort
        case proxyPort
        case proto = "protocol"
    }

    var hostAddress: String {
        let ip = hostIP ?? ""
        return ip.isEmpty ? "*" : ip
    }

    var protoText: String {
        let value = proto ?? ""
        return value.isEmpty ? "tcp" : value
    }
}

struct ProxyMapping: Codable, Sendable, Hashable {
    var listenPort: Int?
    var targetPort: Int?
}

struct NetworkInfo: Codable, Sendable, Hashable {
    var name: String?
    var ip: String?
    var gateway: String?
}

struct MountInfo: Codable, Sendable, Hashable {
    var type: String?
    var name: String?
    var source: String?
    var destination: String?
    var readOnly: Bool?
}

struct Healthcheck: Codable, Sendable, Hashable {
    var configured: Bool?
    var status: String?
    var test: String?
}

struct Classification: Codable, Sendable, Hashable {
    var verdict: Verdict?
    var reasons: [Reason]?
    var warnings: [Reason]?
    var proxyCandidates: [PortBinding]?

    var verdictValue: Verdict { verdict ?? .unknown }
    var allReasons: [Reason] { (reasons ?? []) + (warnings ?? []) }
}

struct PortRange: Codable, Sendable, Hashable {
    var start: Int?
    var end: Int?
}

struct DockerContextInfo: Codable, Sendable, Hashable {
    var name: String?
    var endpoint: String?
    var kind: String?
}

// MARK: - 容器与扫描

struct Container: Codable, Sendable, Hashable, Identifiable {
    var id: String
    var name: String
    var image: String?
    var state: ContainerState?
    var statusText: String?
    var createdAt: String?
    var startedAt: String?
    var composeProject: String?
    var composeService: String?
    var composeWorkingDir: String?
    var composeConfigFiles: [String]?
    var composeContainerNumber: Int?
    var oneoff: Bool?
    var networkMode: String?
    var networks: [NetworkInfo]?
    var restartPolicy: String?
    var autoRemove: Bool?
    var privileged: Bool?
    var readonlyRootfs: Bool?
    var healthcheck: Healthcheck?
    var mounts: [MountInfo]?
    var ports: [PortBinding]?
    var managedServiceId: String?
    var classification: Classification?

    var displayName: String {
        name.isEmpty ? String(id.prefix(12)) : name
    }

    var verdict: Verdict {
        classification?.verdictValue ?? .unknown
    }

    var reasons: [Reason] { classification?.reasons ?? [] }
    var warnings: [Reason] { classification?.warnings ?? [] }
    var allReasons: [Reason] { reasons + warnings }
    var proxyCandidates: [PortBinding] { classification?.proxyCandidates ?? [] }

    var isManaged: Bool {
        guard let managedServiceId else { return false }
        return !managedServiceId.isEmpty
    }

    var projectServiceText: String {
        let project = (composeProject ?? "").trimmingCharacters(in: .whitespaces)
        let service = (composeService ?? "").trimmingCharacters(in: .whitespaces)
        if project.isEmpty && service.isEmpty { return "—" }
        if project.isEmpty { return service }
        if service.isEmpty { return project }
        return "\(project) / \(service)"
    }

    var portsText: String {
        PortsFormatter.original(ports ?? [])
    }
}

struct ScanCounts: Codable, Sendable, Hashable {
    var eligible: Int?
    var needsReview: Int?
    var excluded: Int?
    var managed: Int?
}

struct ScanResult: Codable, Sendable, Hashable {
    var scannedAt: String?
    var context: DockerContextInfo?
    var containers: [Container]?
    var counts: ScanCounts?
}

// MARK: - 计划

struct PlanConfirmation: Codable, Sendable, Hashable {
    var allowLanExposure: Bool?
}

struct PlanItem: Codable, Sendable, Hashable, Identifiable {
    var containerId: String?
    var containerName: String?
    var composeProject: String?
    var composeService: String?
    var composeFile: String?
    var removals: [PortBinding]?
    var proxyMappings: [ProxyMapping]?
    var bindings: [PortBinding]?
    var requiresLanConfirmation: Bool?
    var risks: [Reason]?
    var diff: String?
    var steps: [String]?

    var id: String {
        if let containerId, !containerId.isEmpty { return containerId }
        if let containerName, !containerName.isEmpty { return containerName }
        return "\(composeProject ?? "")/\(composeService ?? "")"
    }

    var displayName: String {
        if let containerName, !containerName.isEmpty { return containerName }
        if let composeService, !composeService.isEmpty { return composeService }
        return id
    }
}

struct Plan: Codable, Sendable, Hashable, Identifiable {
    var id: String
    var createdAt: String?
    var status: PlanStatus?
    var blockers: [Reason]?
    var warnings: [Reason]?
    var items: [PlanItem]?
    var confirmation: PlanConfirmation?

    var statusValue: PlanStatus { status ?? .unknown }
    var itemList: [PlanItem] { items ?? [] }
    var blockerList: [Reason] { blockers ?? [] }
    var warningList: [Reason] { warnings ?? [] }

    var needsLanConfirmation: [PlanItem] {
        itemList.filter { $0.requiresLanConfirmation == true }
    }

    var isBlocked: Bool {
        statusValue == .blocked || !blockerList.isEmpty
    }
}

// MARK: - 操作

struct LogLine: Codable, Sendable, Hashable {
    var at: String?
    var level: String?
    var message: String?
}

struct Operation: Codable, Sendable, Hashable, Identifiable {
    var id: String
    var kind: OperationKind?
    var status: OperationStatus?
    var startedAt: String?
    var finishedAt: String?
    var error: String?
    var backupDir: String?
    var affectedServiceIds: [String]?
    var log: [LogLine]?

    var statusValue: OperationStatus { status ?? .running }
    var kindValue: OperationKind { kind ?? .unknown }
    var logLines: [LogLine] { log ?? [] }

    var errorText: String? {
        guard let error, !error.isEmpty else { return nil }
        return error
    }
}

// MARK: - 服务

struct Service: Codable, Sendable, Hashable, Identifiable {
    var id: String
    var containerId: String?
    var containerName: String?
    var composeProject: String?
    var composeService: String?
    var composeFile: String?
    var status: ServiceStatus?
    var bindings: [PortBinding]?
    var idleTimeoutSecs: Int?
    var startTimeoutSecs: Int?
    var activeConnections: Int?
    var lastActiveAt: String?
    var createdAt: String?
    var lastError: String?
    var excludedFromSleep: Bool?

    var statusValue: ServiceStatus { status ?? .unknown }

    var displayName: String {
        if let containerName, !containerName.isEmpty { return containerName }
        if let composeService, !composeService.isEmpty { return composeService }
        return String(id.prefix(12))
    }

    var projectServiceText: String {
        let project = (composeProject ?? "").trimmingCharacters(in: .whitespaces)
        let service = (composeService ?? "").trimmingCharacters(in: .whitespaces)
        if project.isEmpty && service.isEmpty { return "—" }
        if project.isEmpty { return service }
        if service.isEmpty { return project }
        return "\(project) / \(service)"
    }

    var errorText: String? {
        guard let lastError, !lastError.isEmpty else { return nil }
        return lastError
    }
}

// MARK: - 状态

struct ProxyStatus: Codable, Sendable, Hashable {
    var installed: Bool?
    var running: Bool?
    var adminReachable: Bool?
    var metricsReachable: Bool?
    var containerName: String?
    var containerIP: String?
    var publishedRange: PortRange?
    var error: String?

    var errorText: String? {
        guard let error, !error.isEmpty else { return nil }
        return error
    }
}

struct StatusCounts: Codable, Sendable, Hashable {
    var managed: Int?
    var running: Int?
    var idle: Int?
    var stopped: Int?
    var missing: Int?
    var eligible: Int?
    var needsReview: Int?
    var excluded: Int?

    var pendingCount: Int { (eligible ?? 0) + (needsReview ?? 0) }
}

struct LastErrorInfo: Codable, Sendable, Hashable {
    var at: String?
    var message: String?
}

struct DaemonStatus: Codable, Sendable, Hashable {
    var daemonVersion: String?
    var dataDir: String?
    var context: DockerContextInfo?
    var dockerReachable: Bool?
    var proxy: ProxyStatus?
    var counts: StatusCounts?
    var lastScanAt: String?
    var lastError: LastErrorInfo?
    var launchAgentInstalled: Bool?
}

// MARK: - 设置

struct Settings: Codable, Sendable, Hashable {
    var idleTimeoutSecs: Int?
    var startTimeoutSecs: Int?
    var proxyPortStart: Int?
    var proxyPortEnd: Int?
    var adminPort: Int?
    var webPort: Int?
    var autoScanOnLaunch: Bool?
    var enableLowPortHelper: Bool?
    var logRetentionDays: Int?
    var backupDir: String?
    var dataDir: String?
    var socketPath: String?
    var proxyDialMode: String?
    var excludeImagePatterns: [String]?
    var launchAgentInstalled: Bool?
}

/// `settings.update` 的补丁：只编码有值的字段。
struct SettingsPatch: Codable, Sendable, Hashable {
    var idleTimeoutSecs: Int?
    var startTimeoutSecs: Int?
    var proxyPortStart: Int?
    var proxyPortEnd: Int?
    var adminPort: Int?
    var webPort: Int?
    var autoScanOnLaunch: Bool?
    var enableLowPortHelper: Bool?
    var logRetentionDays: Int?
    var proxyDialMode: String?
    var excludeImagePatterns: [String]?

    var isEmpty: Bool {
        idleTimeoutSecs == nil
            && startTimeoutSecs == nil
            && proxyPortStart == nil
            && proxyPortEnd == nil
            && adminPort == nil
            && webPort == nil
            && autoScanOnLaunch == nil
            && enableLowPortHelper == nil
            && logRetentionDays == nil
            && proxyDialMode == nil
            && excludeImagePatterns == nil
    }
}

// MARK: - 自检

struct DoctorCheck: Codable, Sendable, Hashable {
    var id: String?
    var title: String?
    var status: DoctorStatus?
    var detail: String?
    var remediation: String?

    var statusValue: DoctorStatus { status ?? .unknown }

    var displayTitle: String {
        guard let title, !title.isEmpty else { return id ?? "检查项" }
        return title
    }

    var remediationText: String? {
        guard let remediation, !remediation.isEmpty else { return nil }
        return remediation
    }
}

// MARK: - RPC 结果包装

struct PingResult: Codable, Sendable {
    var pong: Bool?
    var version: String?
}

struct DoctorResult: Codable, Sendable {
    var checks: [DoctorCheck]?
}

struct ServicesResult: Codable, Sendable {
    var services: [Service]?
}

struct OperationsResult: Codable, Sendable {
    var operations: [Operation]?
}

struct ContainerInspectResult: Codable, Sendable {
    var container: Container?
    var inspect: JSONValue?
}

struct PlanApplyResult: Codable, Sendable {
    var operationId: String?
}

struct ServiceActionResult: Codable, Sendable {
    var operationId: String?
    var ok: Bool?
}

/// `agent.install` / `agent.uninstall` 的返回。
struct AgentStatusResult: Codable, Sendable {
    var installed: Bool?
}

/// `logs.tail` 的返回。
struct LogsResult: Codable, Sendable {
    var lines: [LogLine]?
    var source: String?
}

struct LogSourcesResult: Codable, Sendable {
    var sources: [String]?
}

/// `system.shutdown` 的返回。
struct ShutdownResult: Codable, Sendable {
    var ok: Bool?
    var shuttingDown: Bool?
}

// MARK: - UI 辅助

enum PortsFormatter {
    static func host(_ ip: String?) -> String {
        let value = ip ?? ""
        return value.isEmpty ? "*" : value
    }

    static func original(_ ports: [PortBinding]) -> String {
        guard !ports.isEmpty else { return "无端口" }
        return ports.map { port in
            "\(host(port.hostIP)):\(port.hostPort ?? 0) → 容器 \(port.containerPort ?? 0)/\(port.protoText)"
        }.joined(separator: "\n")
    }

    static func proxied(_ ports: [PortBinding]) -> String {
        guard !ports.isEmpty else { return "无端口" }
        return ports.map { port in
            let prefix = "\(host(port.hostIP)):\(port.hostPort ?? 0)"
            let container = "容器 \(port.containerPort ?? 0)/\(port.protoText)"
            if let proxyPort = port.proxyPort {
                return "\(prefix) ⇢ 代理 \(proxyPort) ⇢ \(container)"
            }
            return "\(prefix) → \(container)"
        }.joined(separator: "\n")
    }

    static func oneLine(_ ports: [PortBinding]) -> String {
        guard !ports.isEmpty else { return "无端口" }
        return ports.map { port in
            "\(host(port.hostIP)):\(port.hostPort ?? 0)→\(port.containerPort ?? 0)"
        }.joined(separator: ", ")
    }

    static func mapping(_ mapping: ProxyMapping) -> String {
        "代理监听 \(mapping.listenPort ?? 0) → 容器 \(mapping.targetPort ?? 0)"
    }
}

// MARK: - 健康 / 恢复 / 环境 / 备份

struct Health: Codable, Sendable, Hashable {
    var daemonVersion: String?
    var protocolVersion: Int?
    var dataDir: String?
    var socketPath: String?
    var uptimeSecs: Int?
    var dockerReachable: Bool?
    var dockerContext: DockerContextInfo?
    var proxy: ProxyStatus?
    var counts: StatusCounts?
    var pendingTransactions: Int?
    var runningOperations: Int?
    var launchAgentLoaded: Bool?
    var legacyLaunchAgent: Bool?
    var legacyBinaries: [String]?
    var lastError: LastErrorInfo?
}

struct RecoveryTransaction: Codable, Sendable, Hashable, Identifiable {
    var id: String
    var kind: String?
    var operationId: String?
    var status: String?
    var phase: String?
    var startedAt: String?
    var updatedAt: String?
    var error: String?
}

struct RecoveryListResult: Codable, Sendable {
    var transactions: [RecoveryTransaction]?
    var resolved: Int?
    var message: String?
}

struct ShutdownPreparation: Codable, Sendable {
    var safe: Bool?
    var runningOperations: [Operation]?
    var pendingTransactions: [RecoveryTransaction]?
    var message: String?
}

struct DockerContextEntry: Codable, Sendable, Hashable, Identifiable {
    var name: String
    var endpoint: String?
    var kind: String?
    var current: Bool?
    var reachable: Bool?
    var source: String?

    var id: String { endpoint ?? name }
    var displayName: String { current == true ? "\(name)（当前）" : name }
}

struct DockerContextsResult: Codable, Sendable {
    var contexts: [DockerContextEntry]?
    var selected: String?
}

struct BackupEntry: Codable, Sendable, Hashable, Identifiable {
    var id: String
    var path: String?
    var createdAt: String?
    var sizeBytes: Int64?
    var kind: String?
    var note: String?
}

struct BackupListResult: Codable, Sendable {
    var backups: [BackupEntry]?
}

struct BackupCreateResult: Codable, Sendable {
    var backup: BackupEntry?
}

struct LogsExportResult: Codable, Sendable {
    var path: String?
    var lines: Int?
}

/// 应用更新信息（更新中心使用）。
struct AppUpdate: Codable, Sendable, Hashable {
    var version: String
    var notes: String?
    var downloadURL: String?
    var publishedAt: String?
    var sha256: String?
    var sizeBytes: Int64?
}

/// `backup.restore` 的返回。
struct RestoreResult: Codable, Sendable {
    var ok: Bool?
    var restartRequired: Bool?
    var message: String?
    var backupPath: String?
}
