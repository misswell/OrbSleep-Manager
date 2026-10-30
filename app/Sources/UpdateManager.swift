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
/// 不依赖 Shell 字符串拼接；所有外部命令都以固定参数通过 `Process` 调用。
@MainActor
final class UpdateManager {
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
        return AppUpdate(
            version: decoded.version,
            notes: decoded.notes,
            downloadURL: decoded.downloadURL,
            publishedAt: decoded.publishedAt,
            sha256: decoded.sha256,
            sizeBytes: decoded.sizeBytes
        )
    }

    func download(
        _ update: AppUpdate,
        progress: @escaping @Sendable (Double) -> Void
    ) async throws -> URL {
        guard let string = update.downloadURL, let url = URL(string: string) else {
            throw UpdateError.invalidFeed("downloadURL 无效")
        }
        let destination = FileManager.default.temporaryDirectory
            .appendingPathComponent("OrbSleepUpdate-\(UUID().uuidString)")
            .appendingPathExtension(url.pathExtension.isEmpty ? "zip" : url.pathExtension)
        let loader = DownloadLoader(progress: progress, destination: destination)
        try await loader.start(url: url)
        if let expected = update.sha256, !expected.isEmpty {
            let actual = try Self.sha256(of: destination)
            guard actual.caseInsensitiveCompare(expected) == .orderedSame else {
                try? FileManager.default.removeItem(at: destination)
                throw UpdateError.checksumMismatch
            }
        }
        return destination
    }

    /// 替换当前 App Bundle，保留旧版本；若新版本在 12 秒内未运行则自动回滚。
    func install(package: URL) async throws {
        let appURL = Bundle.main.bundleURL
        let work = FileManager.default.temporaryDirectory
            .appendingPathComponent("OrbSleepInstall-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: work, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: work) }

        let newApp = try await extractApp(from: package, into: work)
        try verifySignature(newApp)

        let backupRoot = FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/Application Support/OrbSleep/app-backup", isDirectory: true)
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

        let helper = FileManager.default.temporaryDirectory
            .appendingPathComponent("OrbSleep-relaunch-\(UUID().uuidString).sh")
        let script = """
        #!/bin/bash
        set -u
        APP="$1"; OLD="$2"; OLDPID="$3"
        sleep 3
        open -n "$APP" >/dev/null 2>&1
        sleep 12
        NEW=$(pgrep -f "$APP/Contents/MacOS/OrbSleepManager" | grep -v "^$OLDPID$" | head -1)
        if [ -n "$NEW" ]; then exit 0; fi
        rm -rf "$APP"
        mv "$OLD" "$APP"
        open -n "$APP" >/dev/null 2>&1
        """
        try script.write(to: helper, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: helper.path)

        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/bin/bash")
        process.arguments = [helper.path, appURL.path, oldApp.path, "\(ProcessInfo.processInfo.processIdentifier)"]
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
            try run("/usr/bin/hdiutil", ["attach", package.path, "-nobrowse", "-quiet", "-mountpoint", mount.path])
            defer { try? run("/usr/bin/hdiutil", ["detach", mount.path, "-quiet"]) }
            guard let app = try fm.contentsOfDirectory(at: mount, includingPropertiesForKeys: nil)
                .first(where: { $0.pathExtension == "app" }) else {
                throw UpdateError.invalidPackage("DMG 中没有 .app")
            }
            let copy = extractDir.appendingPathComponent(app.lastPathComponent)
            try run("/usr/bin/ditto", [app.path, copy.path])
            return copy
        }

        if ext == "zip" {
            try run("/usr/bin/ditto", ["-x", "-k", package.path, extractDir.path])
            if let app = findApp(in: extractDir) { return app }
            throw UpdateError.invalidPackage("ZIP 中没有 .app")
        }
        if ext == "app" {
            let copy = extractDir.appendingPathComponent(package.lastPathComponent)
            try run("/usr/bin/ditto", [package.path, copy.path])
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

    private func verifySignature(_ app: URL) throws {
        do {
            try run("/usr/bin/codesign", ["--verify", "--deep", "--strict", app.path])
        } catch {
            throw UpdateError.signatureInvalid(error.localizedDescription)
        }
    }

    @discardableResult
    private func run(_ executable: String, _ arguments: [String]) throws -> String {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: executable)
        process.arguments = arguments
        let pipe = Pipe()
        process.standardOutput = pipe
        process.standardError = pipe
        try process.run()
        process.waitUntilExit()
        let data = pipe.fileHandleForReading.readDataToEndOfFile()
        let output = String(decoding: data, as: UTF8.self)
        guard process.terminationStatus == 0 else {
            throw UpdateError.installFailed(output.trimmingCharacters(in: .whitespacesAndNewlines))
        }
        return output
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

    static func sha256(of url: URL) throws -> String {
        let data = try Data(contentsOf: url)
        return SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
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
