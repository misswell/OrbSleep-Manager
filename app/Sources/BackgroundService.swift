import Darwin
import Foundation
import ServiceManagement

/// Errors surfaced by the background-service management layer.
enum BackgroundServiceError: LocalizedError {
    case missingBundledExecutable(String)
    case commandFailed(String, Int32, String)
    case requiresApproval
    case appIsTranslocated
    case daemonDidNotStart

    var errorDescription: String? {
        switch self {
        case .missingBundledExecutable(let name):
            return "应用包中缺少 \(name)；请重新安装完整的 OrbSleep Manager"
        case .commandFailed(let command, let status, let output):
            let detail = output.trimmingCharacters(in: .whitespacesAndNewlines)
            return detail.isEmpty
                ? "\(command) 执行失败（退出码 \(status)）"
                : "\(command) 执行失败（退出码 \(status)）：\(detail)"
        case .requiresApproval:
            return "后台服务需要在「系统设置 → 通用 → 登录项」中允许 OrbSleep Manager"
        case .appIsTranslocated:
            return "应用正在从隔离位置运行。请先把 OrbSleepManager.app 拖到「应用程序」文件夹，再打开并初始化。"
        case .daemonDidNotStart:
            return "后台服务没有在预期时间内响应；请在「诊断」中查看详情"
        }
    }
}

/// Owns the launchd registration for the bundled `orbsleepd`.
///
/// The primary path is `SMAppService`, which registers the LaunchAgent that
/// lives at `Contents/Library/LaunchAgents/` inside the app bundle. Everything
/// the daemon needs ships inside `OrbSleepManager.app`, so no binary is copied
/// to `/usr/local/bin`, no `sudo` runs, and no shell command is required.
///
/// Development builds that are not signed with a Developer ID identity cannot
/// be registered through `SMAppService`; in that case the bundled `orbsleepctl`
/// writes the legacy `~/Library/LaunchAgents` plist. Both paths are driven by
/// the GUI, never by the user's terminal.
@MainActor
final class BackgroundService {
    static let agentLabel = "com.misswell.orbsleep.daemon.app"
    static let agentPlistName = "com.misswell.orbsleep.daemon.app.plist"

    /// 0.1.x manual-install label; used only for detection and migration.
    static let legacyLabel = "com.misswell.orbsleep.daemon"
    static let legacyPlistName = "com.misswell.orbsleep.daemon.plist"

    /// Executable shipped at `Contents/Resources/orbsleepd`.
    var bundledDaemonURL: URL? {
        Bundle.main.url(forResource: "orbsleepd", withExtension: nil)
    }

    /// Executable shipped at `Contents/Resources/orbsleepctl`.
    var bundledCLIURL: URL? {
        Bundle.main.url(forResource: "orbsleepctl", withExtension: nil)
    }

    var agentPlistURL: URL {
        Bundle.main.bundleURL
            .appendingPathComponent("Contents/Library/LaunchAgents", isDirectory: true)
            .appendingPathComponent(Self.agentPlistName)
    }

    var hasBundledAgentPlist: Bool {
        FileManager.default.fileExists(atPath: agentPlistURL.path)
    }

    /// Gatekeeper runs quarantined apps from a read-only translocated path;
    /// SMAppService cannot register from there.
    var isTranslocated: Bool {
        Bundle.main.bundleURL.path.contains("AppTranslocation")
    }

    /// Per-user plist written by the development fallback (current label).
    var legacyPlistURL: URL {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/LaunchAgents", isDirectory: true)
            .appendingPathComponent(Self.agentPlistName)
    }

    var legacyAgentInstalled: Bool {
        FileManager.default.fileExists(atPath: legacyPlistURL.path)
    }

    /// 0.1.x plist written by the manual install flow.
    var oldPlistURL: URL {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/LaunchAgents", isDirectory: true)
            .appendingPathComponent(Self.legacyPlistName)
    }

    // MARK: - SMAppService

    private var appService: SMAppService {
        SMAppService.agent(plistName: Self.agentPlistName)
    }

    var registrationStatus: SMAppService.Status {
        appService.status
    }

    /// True when launchd knows about the embedded agent (enabled or waiting
    /// for the user to approve it in System Settings).
    var isRegistered: Bool {
        switch registrationStatus {
        case .enabled, .requiresApproval:
            return true
        default:
            return false
        }
    }

    var needsApproval: Bool {
        registrationStatus == .requiresApproval
    }

    /// Registered through SMAppService or through the legacy plist.
    var installed: Bool {
        isRegistered || legacyAgentInstalled
    }

    var statusText: String {
        if needsApproval {
            return "等待系统授权"
        }
        if isRegistered {
            return "已注册"
        }
        if legacyAgentInstalled {
            return "已注册（用户级 LaunchAgent）"
        }
        return "未注册"
    }

    func register() throws {
        guard hasBundledAgentPlist else {
            throw BackgroundServiceError.missingBundledExecutable(Self.agentPlistName)
        }
        try appService.register()
    }

    func unregister() async throws {
        try await appService.unregister()
    }

    func openLoginItemsSettings() {
        SMAppService.openSystemSettingsLoginItems()
    }

    // MARK: - 登录后自动打开 GUI（独立于后台服务）

    private var mainAppService: SMAppService { .mainApp }

    var launchesAtLogin: Bool {
        mainAppService.status == .enabled
    }

    var mainAppNeedsApproval: Bool {
        mainAppService.status == .requiresApproval
    }

    func setLaunchAtLogin(_ enabled: Bool) async throws {
        if enabled {
            if mainAppService.status != .enabled {
                try mainAppService.register()
            }
        } else if mainAppService.status != .notRegistered {
            try await mainAppService.unregister()
        }
    }

    // MARK: - Launchctl helpers (GUI-initiated only)

    /// Starts the registered job with launchd.
    func kickstart() throws {
        try runLaunchctl(["kickstart", "-k", "gui/\(getuid())/\(Self.agentLabel)"])
    }

    /// Stops the job without unregistering it. The daemon exits successfully,
    /// so `KeepAlive.SuccessfulExit = false` does not restart it.
    func killJob() {
        try? runLaunchctl(["kill", "SIGTERM", "gui/\(getuid())/\(Self.agentLabel)"])
    }

    func isJobLoaded() -> Bool {
        (try? runLaunchctl(["print", "gui/\(getuid())/\(Self.agentLabel)"], allowFailure: true)) != nil
    }

    // MARK: - Legacy fallback (development builds)

    /// Registers a user LaunchAgent by running the bundled CLI. Used only when
    /// `SMAppService` rejects an unsigned development build.
    func installLegacyAgent(daemon: URL) throws {
        guard let cli = bundledCLIURL else {
            throw BackgroundServiceError.missingBundledExecutable("orbsleepctl")
        }
        _ = try runProcess(cli, ["install-agent", "--binary", daemon.path])
    }

    func removeLegacyAgent() {
        guard let cli = bundledCLIURL else { return }
        _ = try? runProcess(cli, ["uninstall-agent"])
    }

    // MARK: - 0.1.x migration helpers

    /// Detects the old manual install (/usr/local/bin + old LaunchAgent label).
    func detectLegacy() -> LegacyInstallation {
        let fm = FileManager.default
        let daemon = "/usr/local/bin/orbsleepd"
        let ctl = "/usr/local/bin/orbsleepctl"
        var rootOwned = false
        for path in [daemon, ctl] {
            guard let attrs = try? fm.attributesOfItem(atPath: path),
                  let owner = attrs[.ownerAccountID] as? NSNumber else { continue }
            if owner.intValue == 0 { rootOwned = true }
        }
        return LegacyInstallation(
            daemonPath: daemon,
            ctlPath: ctl,
            plistPath: oldPlistURL.path,
            daemonExists: fm.fileExists(atPath: daemon),
            ctlExists: fm.fileExists(atPath: ctl),
            plistExists: fm.fileExists(atPath: oldPlistURL.path),
            launchdLoaded: isLegacyJobLoaded(),
            rootOwned: rootOwned
        )
    }

    func isLegacyJobLoaded() -> Bool {
        (try? runLaunchctl(["print", "gui/\(getuid())/\(Self.legacyLabel)"], allowFailure: true)) != nil
    }

    /// Terminates a manually-started 0.1.x daemon that is not under launchd.
    func terminateLegacyDaemon() {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/pkill")
        process.arguments = ["-TERM", "-f", "/usr/local/bin/orbsleepd"]
        try? process.run()
        process.waitUntilExit()
    }

    /// Stops the 0.1.x LaunchAgent so the new bundled service can take over.
    func bootoutLegacyJob() {
        _ = try? runLaunchctl(["bootout", "gui/\(getuid())/\(Self.legacyLabel)"])
        if FileManager.default.fileExists(atPath: oldPlistURL.path) {
            _ = try? runLaunchctl(["bootout", "gui/\(getuid())", oldPlistURL.path])
        }
    }

    func removeOldPlist() throws {
        if FileManager.default.fileExists(atPath: oldPlistURL.path) {
            try FileManager.default.removeItem(at: oldPlistURL)
        }
    }

    /// Removes 0.1.x binaries. User-owned files are deleted directly; root-owned
    /// files go through a one-shot macOS administrator authorization dialog.
    func cleanupLegacyBinaries() throws -> [String] {
        let paths = ["/usr/local/bin/orbsleepd", "/usr/local/bin/orbsleepctl"]
        var removed: [String] = []
        var rootOwned: [String] = []
        for path in paths {
            guard FileManager.default.fileExists(atPath: path) else { continue }
            if FileManager.default.isWritableFile(atPath: path) {
                try FileManager.default.removeItem(atPath: path)
                removed.append(path)
            } else {
                rootOwned.append(path)
            }
        }
        if !rootOwned.isEmpty {
            try removeWithAdministrator(paths: rootOwned)
            removed.append(contentsOf: rootOwned)
        }
        return removed
    }

    private func removeWithAdministrator(paths: [String]) throws {
        let quoted = paths
            .map { "'" + $0.replacingOccurrences(of: "'", with: "'\\''") + "'" }
            .joined(separator: " ")
        let script = "do shell script \"rm -f \(quoted)\" with administrator privileges"
        _ = try runProcess(URL(fileURLWithPath: "/usr/bin/osascript"), ["-e", script])
    }

    // MARK: - Command line tools (optional, user-level, no sudo)

    static var cliInstallURL: URL {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent(".local/bin", isDirectory: true)
            .appendingPathComponent("orbsleepctl")
    }

    var cliInstalled: Bool {
        FileManager.default.fileExists(atPath: Self.cliInstallURL.path)
    }

    /// Installs `orbsleepctl` as a symlink in `~/.local/bin`. This is an
    /// optional advanced entry point; the GUI never depends on it.
    func installCLI() throws {
        guard let cli = bundledCLIURL else {
            throw BackgroundServiceError.missingBundledExecutable("orbsleepctl")
        }
        let destination = Self.cliInstallURL
        let directory = destination.deletingLastPathComponent()
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        if FileManager.default.fileExists(atPath: destination.path) {
            try FileManager.default.removeItem(at: destination)
        }
        try FileManager.default.createSymbolicLink(at: destination, withDestinationURL: cli)
    }

    func removeCLI() {
        let destination = Self.cliInstallURL
        guard FileManager.default.fileExists(atPath: destination.path) else { return }
        // Only remove the link when it points at our embedded CLI.
        let target = try? FileManager.default.destinationOfSymbolicLink(atPath: destination.path)
        if let target, target == bundledCLIURL?.path {
            try? FileManager.default.removeItem(at: destination)
        } else if target == nil {
            // A regular file was placed there by the user; leave it alone.
        }
    }

    // MARK: - Process helpers

    @discardableResult
    private func runLaunchctl(_ arguments: [String], allowFailure: Bool = false) throws -> String {
        try runProcess(URL(fileURLWithPath: "/bin/launchctl"), arguments, allowFailure: allowFailure)
    }

    @discardableResult
    private func runProcess(_ executable: URL, _ arguments: [String], allowFailure: Bool = false) throws -> String {
        let process = Process()
        process.executableURL = executable
        process.arguments = arguments
        let pipe = Pipe()
        process.standardOutput = pipe
        process.standardError = pipe
        do {
            try process.run()
        } catch {
            throw BackgroundServiceError.commandFailed(executable.lastPathComponent, -1, error.localizedDescription)
        }
        process.waitUntilExit()
        let data = pipe.fileHandleForReading.readDataToEndOfFile()
        let output = String(decoding: data, as: UTF8.self)
        guard process.terminationStatus == 0 || allowFailure else {
            throw BackgroundServiceError.commandFailed(
                ([executable.lastPathComponent] + arguments).joined(separator: " "),
                process.terminationStatus,
                output
            )
        }
        return output
    }
}

// MARK: - 状态与迁移模型

/// 0.1.x manual installation detected on this machine.
struct LegacyInstallation: Sendable, Equatable {
    var daemonPath: String
    var ctlPath: String
    var plistPath: String
    var daemonExists: Bool
    var ctlExists: Bool
    var plistExists: Bool
    var launchdLoaded: Bool
    var rootOwned: Bool

    var isPresent: Bool {
        daemonExists || ctlExists || plistExists || launchdLoaded
    }

    var summary: String {
        var parts: [String] = []
        if daemonExists { parts.append("/usr/local/bin/orbsleepd") }
        if ctlExists { parts.append("/usr/local/bin/orbsleepctl") }
        if plistExists { parts.append("旧 LaunchAgent plist") }
        if launchdLoaded { parts.append("旧启动项正在运行") }
        if parts.isEmpty { return "未检测到旧版安装" }
        return parts.joined(separator: "、")
    }

    static let none = LegacyInstallation(
        daemonPath: "/usr/local/bin/orbsleepd",
        ctlPath: "/usr/local/bin/orbsleepctl",
        plistPath: "",
        daemonExists: false,
        ctlExists: false,
        plistExists: false,
        launchdLoaded: false,
        rootOwned: false
    )
}

/// Full lifecycle state, richer than a single `daemonReachable` flag.
enum BackgroundServiceState: String, Sendable, Equatable {
    case notInstalled
    case awaitingApproval
    case starting
    case running
    case stopped
    case failed
    case versionMismatch
    case upgrading
    case recoveryRequired

    var title: String {
        switch self {
        case .notInstalled: return "未安装"
        case .awaitingApproval: return "等待授权"
        case .starting: return "正在启动"
        case .running: return "运行中"
        case .stopped: return "已停止"
        case .failed: return "启动失败"
        case .versionMismatch: return "版本不匹配"
        case .upgrading: return "正在升级"
        case .recoveryRequired: return "恢复未完成"
        }
    }
}
