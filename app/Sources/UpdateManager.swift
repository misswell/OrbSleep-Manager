import AppKit
import CryptoKit
import Foundation

enum UpdateError: LocalizedError {
    case feedNotConfigured
    case invalidFeed(String)
    case checksumMismatch
    case invalidPackage(String)
    case signatureInvalid(String)
    case installFailed(String)

    var errorDescription: String? {
        switch self {
        case .feedNotConfigured:
            return "未配置更新源。请在 Info.plist 设置 OSUpdateFeedURL，或设置环境变量 ORBSLEEP_UPDATE_FEED。"
        case .invalidFeed(let detail):
            return "更新源返回的内容无效：\(detail)"
        case .checksumMismatch:
            return "更新包 SHA256 校验失败，已中止安装"
        case .invalidPackage(let detail):
            return "更新包无效：\(detail)"
        case .signatureInvalid(let detail):
            return "新版本签名校验失败：\(detail)"
        case .installFailed(let detail):
            return "安装更新失败：\(detail)"
        }
    }
}

/// 更新源的 JSON 结构。
private struct UpdateFeed: Decodable {
    var version: String
    var notes: String?
    var downloadURL: String
    var sha256: String?
    var publishedAt: String?
    var sizeBytes: Int64?
}

/// 应用内更新：检查源、下载、校验、替换 App Bundle，并在新版本启动失败时回滚。
///
/// 安全要求：更新包必须带 SHA256、下载地址必须 HTTPS、新包必须通过
/// codesign 与 Gatekeeper 校验，且 Team Identifier / Bundle ID 必须是
/// OrbSleep 自己的——任何一项不满足都拒绝安装。
/// 不依赖 Shell 字符串拼接；所有外部命令都以固定参数通过 `ProcessRunner` 调用。
@MainActor
final class UpdateManager {
    /// 只接受本团队的签名。
    static let expectedTeamID = "U8U443D7ZL"
    static let expectedBundleID = "com.misswell.orbsleepmanager"

    static var feedURL: URL? {
        if let value = ProcessInfo.processInfo.environment["ORBSLEEP_UPDATE_FEED"],
           let url = URL(string: value) {
            return url
        }
        if let value = Bundle.main.infoDictionary?["OSUpdateFeedURL"] as? String,
           !value.isEmpty,
           let url = URL(string: value) {
            return url
        }
        return nil
    }

    var currentVersion: String {
        Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "0"
    }

    var feedDescription: String {
        Self.feedURL?.absoluteString ?? "未配置"
    }

    func checkForUpdates() async throws -> AppUpdate? {
        guard let feed = Self.feedURL else { throw UpdateError.feedNotConfigured }
        let (data, response) = try await URLSession.shared.data(from: feed)
        if let http = response as? HTTPURLResponse, http.statusCode != 200 {
            throw UpdateError.invalidFeed("HTTP \(http.statusCode)")
        }
        let decoded: UpdateFeed
        do {
            decoded = try JSONDecoder().decode(UpdateFeed.self, from: data)
        } catch {
            throw UpdateError.invalidFeed(error.localizedDescription)
        }
        guard Self.compareVersions(decoded.version, currentVersion) > 0 else {
            return nil
        }
        guard let sha = decoded.sha256, !sha.isEmpty else {
            throw UpdateError.invalidFeed("更新源缺少 SHA256，拒绝安装")
        }
        let downloadURL = decoded.downloadURL
        guard let url = URL(string: downloadURL),
              url.scheme == "https" || Self.isLocal(url) else {
            throw UpdateError.invalidFeed("下载地址必须是 HTTPS")
        }
        return AppUpdate(
            version: decoded.version,
            notes: decoded.notes,
            downloadURL: decoded.downloadURL,
            publishedAt: decoded.publishedAt,
            sha256: decoded.sha256,
            sizeBytes: decoded.sizeBytes
        )
    }

    private static func isLocal(_ url: URL) -> Bool {
        url.host == "localhost" || url.host == "127.0.0.1"
    }

    func download(
        _ update: AppUpdate,
        progress: @escaping @Sendable (Double) -> Void
    ) async throws -> URL {
        guard let string = update.downloadURL, let url = URL(string: string) else {
            throw UpdateError.invalidFeed("downloadURL 无效")
        }
        guard let expected = update.sha256, !expected.isEmpty else {
            throw UpdateError.invalidFeed("更新源缺少 SHA256，拒绝下载安装")
        }
        guard url.scheme == "https" || Self.isLocal(url) else {
            throw UpdateError.invalidFeed("下载地址必须是 HTTPS")
        }
        let destination = FileManager.default.temporaryDirectory
            .appendingPathComponent("OrbSleepUpdate-\(UUID().uuidString)")
            .appendingPathExtension(url.pathExtension.isEmpty ? "zip" : url.pathExtension)
        let loader = DownloadLoader(progress: progress, destination: destination)
        try await loader.start(url: url)
        let actual = try Self.sha256(of: destination)
        guard actual.caseInsensitiveCompare(expected) == .orderedSame else {
            try? FileManager.default.removeItem(at: destination)
            throw UpdateError.checksumMismatch
        }
        return destination
    }

    /// 替换当前 App Bundle，保留旧版本；新版本必须在 45 秒内通过健康握手
    /// （GUI 正常启动且后台服务版本一致后写入 update-ok），否则自动回滚。
    func install(package: URL) async throws {
        let appURL = Bundle.main.bundleURL
        let work = FileManager.default.temporaryDirectory
            .appendingPathComponent("OrbSleepInstall-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: work, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: work) }

        let newApp = try await extractApp(from: package, into: work)
        try await verifySignature(newApp)

        let supportDir = FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/Application Support/OrbSleep", isDirectory: true)
        let backupRoot = supportDir.appendingPathComponent("app-backup", isDirectory: true)
        try FileManager.default.createDirectory(at: backupRoot, withIntermediateDirectories: true)
        let timestamp = ISO8601DateFormatter().string(from: Date()).replacingOccurrences(of: ":", with: "-")
        let oldApp = backupRoot.appendingPathComponent("OrbSleepManager-\(currentVersion)-\(timestamp).app")

        do {
            try FileManager.default.moveItem(at: appURL, to: oldApp)
        } catch {
            throw UpdateError.installFailed("无法保留旧版本：\(error.localizedDescription)")
        }
        do {
            try FileManager.default.copyItem(at: newApp, to: appURL)
        } catch {
            try? FileManager.default.moveItem(at: oldApp, to: appURL)
            throw UpdateError.installFailed("替换应用失败：\(error.localizedDescription)")
        }

        // Health handshake markers: the GUI writes `update-ok` after it has
        // booted AND verified the bundled daemon version; the script rolls
        // back if it never appears. Stale state from a previous update is
        // cleared first.
        let okMarker = supportDir.appendingPathComponent("update-ok")
        let inProgress = supportDir.appendingPathComponent("update-in-progress.json")
        try? FileManager.default.removeItem(at: okMarker)
        let markerPayload: [String: String] = [
            "expectedVersion": Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "",
            "startedAt": ISO8601DateFormatter().string(from: Date()),
        ]
        if let payload = try? JSONSerialization.data(withJSONObject: markerPayload, options: [.sortedKeys]) {
            try? payload.write(to: inProgress, options: .atomic)
        }

        let helper = FileManager.default.temporaryDirectory
            .appendingPathComponent("OrbSleep-relaunch-\(UUID().uuidString).sh")
        let script = """
        #!/bin/bash
        set -u
        APP="$1"; OLD="$2"; OLDPID="$3"; OKMARK="$4"
        rm -f "$OKMARK"
        sleep 3
        open -n "$APP" >/dev/null 2>&1
        # Wait for the health handshake instead of a bare process check: a GUI
        # that starts but never reaches a healthy daemon still counts as failed.
        for _ in $(seq 1 45); do
            if [ -f "$OKMARK" ]; then exit 0; fi
            sleep 1
        done
        rm -rf "$APP"
        mv "$OLD" "$APP"
        open -n "$APP" >/dev/null 2>&1
        """
        try script.write(to: helper, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: helper.path)

        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/bin/bash")
        process.arguments = [helper.path, appURL.path, oldApp.path, "\(ProcessInfo.processInfo.processIdentifier)", okMarker.path]
        try process.run()
        NSApp.terminate(nil)
    }

    // MARK: - 包处理

    private func extractApp(from package: URL, into work: URL) async throws -> URL {
        let fm = FileManager.default
        let ext = package.pathExtension.lowercased()
        let extractDir = work.appendingPathComponent("extract", isDirectory: true)
        try fm.createDirectory(at: extractDir, withIntermediateDirectories: true)

        if ext == "dmg" {
            let mount = work.appendingPathComponent("mnt", isDirectory: true)
            try fm.createDirectory(at: mount, withIntermediateDirectories: true)
            try await run("/usr/bin/hdiutil", ["attach", package.path, "-nobrowse", "-quiet", "-mountpoint", mount.path])
            defer { Task { try? await run("/usr/bin/hdiutil", ["detach", mount.path, "-quiet"]) } }
            guard let app = try fm.contentsOfDirectory(at: mount, includingPropertiesForKeys: nil)
                .first(where: { $0.pathExtension == "app" }) else {
                throw UpdateError.invalidPackage("DMG 中没有 .app")
            }
            let copy = extractDir.appendingPathComponent(app.lastPathComponent)
            try await run("/usr/bin/ditto", [app.path, copy.path])
            return copy
        }

        if ext == "zip" {
            try await run("/usr/bin/ditto", ["-x", "-k", package.path, extractDir.path])
            if let app = findApp(in: extractDir) { return app }
            throw UpdateError.invalidPackage("ZIP 中没有 .app")
        }
        if ext == "app" {
            let copy = extractDir.appendingPathComponent(package.lastPathComponent)
            try await run("/usr/bin/ditto", [package.path, copy.path])
            return copy
        }
        throw UpdateError.invalidPackage("仅支持 .zip / .dmg / .app")
    }

    private func findApp(in directory: URL) -> URL? {
        let fm = FileManager.default
        guard let entries = try? fm.contentsOfDirectory(at: directory, includingPropertiesForKeys: nil) else {
            return nil
        }
        if let app = entries.first(where: { $0.pathExtension == "app" }) { return app }
        for entry in entries where (try? entry.resourceValues(forKeys: [.isDirectoryKey]))?.isDirectory == true {
            if let app = findApp(in: entry) { return app }
        }
        return nil
    }

    /// 签名与身份校验：结构有效（codesign --verify）之外，还必须是我们
    /// 自己的 Developer ID 签名、自己的 Bundle ID，并通过 Gatekeeper。
    private func verifySignature(_ app: URL) async throws {
        do {
            try await run("/usr/bin/codesign", ["--verify", "--deep", "--strict", app.path])
        } catch {
            throw UpdateError.signatureInvalid(error.localizedDescription)
        }
        let display = try await run("/usr/bin/codesign", ["-dv", "--verbose=4", app.path])
        guard display.contains("TeamIdentifier=\(Self.expectedTeamID)") else {
            throw UpdateError.signatureInvalid("签名身份不是 \(Self.expectedTeamID)")
        }
        guard let info = Bundle(url: app)?.infoDictionary,
              info["CFBundleIdentifier"] as? String == Self.expectedBundleID else {
            throw UpdateError.signatureInvalid("Bundle ID 不是 \(Self.expectedBundleID)")
        }
        // The new package must actually be newer than what is running.
        if let version = Bundle(url: app)?.infoDictionary?["CFBundleShortVersionString"] as? String {
            guard Self.compareVersions(version, currentVersion) > 0 else {
                throw UpdateError.signatureInvalid("新包版本 \(version) 不高于当前 \(currentVersion)")
            }
        }
        do {
            // Notarized builds pass; a rejected Gatekeeper assessment means
            // unsigned or tampered code.
            try await run("/usr/sbin/spctl", ["--assess", "--type", "execute", app.path])
        } catch {
            throw UpdateError.signatureInvalid("Gatekeeper 校验未通过")
        }
    }

    private func run(_ executable: String, _ arguments: [String]) async throws -> String {
        let result = try await ProcessRunner.run(
            executable: URL(fileURLWithPath: executable),
            arguments: arguments
        )
        guard result.succeeded else {
            throw UpdateError.installFailed(result.output.trimmingCharacters(in: .whitespacesAndNewlines))
        }
        return result.output
    }

    // MARK: - 版本与校验

    static func compareVersions(_ lhs: String, _ rhs: String) -> Int {
        let left = lhs.split(separator: ".").map { Int($0) ?? 0 }
        let right = rhs.split(separator: ".").map { Int($0) ?? 0 }
        let count = max(left.count, right.count)
        for index in 0..<count {
            let l = index < left.count ? left[index] : 0
            let r = index < right.count ? right[index] : 0
            if l != r { return l < r ? -1 : 1 }
        }
        return 0
    }

    /// 流式 SHA256：不把整个更新包读进内存。
    static func sha256(of url: URL) throws -> String {
        let handle = try FileHandle(forReadingFrom: url)
        defer { try? handle.close() }
        var hasher = SHA256()
        while let chunk = try handle.read(upToCount: 1 << 20), !chunk.isEmpty {
            hasher.update(data: chunk)
        }
        return hasher.finalize().map { String(format: "%02x", $0) }.joined()
    }
}

/// 带进度的下载器。
private final class DownloadLoader: NSObject, URLSessionDownloadDelegate, @unchecked Sendable {
    private let progress: @Sendable (Double) -> Void
    private let destination: URL
    private let lock = NSLock()
    private var continuation: CheckedContinuation<Void, Error>?
    private var finished = false
    private var session: URLSession?

    init(progress: @escaping @Sendable (Double) -> Void, destination: URL) {
        self.progress = progress
        self.destination = destination
    }

    func start(url: URL) async throws {
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            lock.lock()
            self.continuation = continuation
            lock.unlock()
            let session = URLSession(configuration: .default, delegate: self, delegateQueue: nil)
            self.session = session
            session.downloadTask(with: url).resume()
        }
    }

    private func finish(_ result: Result<Void, Error>) {
        lock.lock()
        guard !finished else { lock.unlock(); return }
        finished = true
        let continuation = self.continuation
        self.continuation = nil
        lock.unlock()
        session?.invalidateAndCancel()
        switch result {
        case .success: continuation?.resume()
        case .failure(let error): continuation?.resume(throwing: error)
        }
    }

    func urlSession(
        _ session: URLSession,
        downloadTask: URLSessionDownloadTask,
        didWriteData bytesWritten: Int64,
        totalBytesWritten: Int64,
        totalBytesExpectedToWrite: Int64
    ) {
        guard totalBytesExpectedToWrite > 0 else { return }
        progress(Double(totalBytesWritten) / Double(totalBytesExpectedToWrite))
    }

    func urlSession(_ session: URLSession, downloadTask: URLSessionDownloadTask, didFinishDownloadingTo location: URL) {
        do {
            if FileManager.default.fileExists(atPath: destination.path) {
                try FileManager.default.removeItem(at: destination)
            }
            try FileManager.default.moveItem(at: location, to: destination)
            finish(.success(()))
        } catch {
            finish(.failure(error))
        }
    }

    func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
        if let error { finish(.failure(error)) }
    }
}
