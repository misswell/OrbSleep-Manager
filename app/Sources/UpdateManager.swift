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
    ///
    /// - Parameter expectedNewVersion: 更新源声明的目标版本。握手标记必须用
    ///   **新包** 的版本：此时 Bundle.main 仍返回旧包的缓存 Info.plist，用它
    ///   比对新版本会让健康握手永远失败、45 秒后必然回滚。
    /// - Parameter dataDir: OrbSleep 数据目录。握手标记与旧包备份都放在这里，
    ///   与 GUI（可能使用自定义 dataDir）读取同一位置。
    func install(package: URL, expectedNewVersion: String, dataDir: String) async throws {
        let appURL = Bundle.main.bundleURL
        let work = FileManager.default.temporaryDirectory
            .appendingPathComponent("OrbSleepInstall-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: work, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: work) }

        let newApp = try await extractApp(from: package, into: work)
        try await verifySignature(newApp)

        // The handshake must expect exactly the version that was installed:
        // appcast version, package version and marker have to agree.
        let newVersion = Bundle(url: newApp)?.infoDictionary?["CFBundleShortVersionString"] as? String ?? ""
        guard !newVersion.isEmpty, newVersion == expectedNewVersion else {
            throw UpdateError.invalidPackage(
                "新包版本 \(newVersion.isEmpty ? "未知" : newVersion) 与更新源版本 \(expectedNewVersion) 不一致，已中止安装"
            )
        }

        let supportDir = URL(fileURLWithPath: dataDir, isDirectory: true)
        let backupRoot = supportDir.appendingPathComponent("app-backup", isDirectory: true)
        try FileManager.default.createDirectory(at: backupRoot, withIntermediateDirectories: true)
        let timestamp = ISO8601DateFormatter().string(from: Date()).replacingOccurrences(of: ":", with: "-")
        let oldApp = backupRoot.appendingPathComponent("OrbSleepManager-\(currentVersion)-\(timestamp).app")

        do {
            try FileManager.default.moveItem(at: appURL, to: oldApp)
        } catch {
            throw UpdateError.installFailed("无法保留旧版本：\(error.localizedDescription)")
        }
        // Keep three rollback copies at install time (the freshly saved old
        // bundle is the newest); the health handshake trims to two once the
        // update is confirmed healthy, so the active rollback candidate is
        // never removed early.
        Self.cleanupAppBackups(in: backupRoot, keeping: 3)
        do {
            try FileManager.default.copyItem(at: newApp, to: appURL)
        } catch {
            try Self.restorePreviousApp(appURL: appURL, oldApp: oldApp, backupRoot: backupRoot,
                                        expectedVersion: currentVersion, cause: error)
        }

        // Health handshake markers: the GUI writes `update-ok` after it has
        // booted AND verified the bundled daemon version; the script rolls
        // back if it never appears. Stale state from a previous update is
        // cleared first.
        let okMarker = supportDir.appendingPathComponent("update-ok")
        let inProgress = supportDir.appendingPathComponent("update-in-progress.json")
        try? FileManager.default.removeItem(at: okMarker)
        let markerPayload: [String: String] = [
            "expectedVersion": newVersion,
            "startedAt": ISO8601DateFormatter().string(from: Date()),
        ]
        if let payload = try? JSONSerialization.data(withJSONObject: markerPayload, options: [.sortedKeys]) {
            try? payload.write(to: inProgress, options: .atomic)
        }

        let helper = FileManager.default.temporaryDirectory
            .appendingPathComponent("OrbSleep-relaunch-\(UUID().uuidString).sh")
        let failureMarker = supportDir.appendingPathComponent("app-update-failed.txt")
        try? FileManager.default.removeItem(at: failureMarker)
        let script = Self.relaunchScript()
        do {
            try script.write(to: helper, atomically: true, encoding: .utf8)
            try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: helper.path)
        } catch {
            // Same contract as a helper start failure: the app directory must
            // never be left empty or half-replaced.
            try Self.restorePreviousApp(appURL: appURL, oldApp: oldApp, backupRoot: backupRoot,
                                        expectedVersion: currentVersion, cause: error)
        }

        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/bin/bash")
        process.arguments = [
            helper.path, appURL.path, oldApp.path,
            "\(ProcessInfo.processInfo.processIdentifier)", okMarker.path,
            currentVersion, failureMarker.path,
        ]
        do {
            try process.run()
        } catch {
            // The helper never started, so nobody else will put the old bundle
            // back while appURL holds the new one.
            try Self.restorePreviousApp(appURL: appURL, oldApp: oldApp, backupRoot: backupRoot,
                                        expectedVersion: currentVersion, cause: error)
        }
        NSApp.terminate(nil)
    }

    /// The relaunch/rollback helper. Its contract is that the install path is
    /// never left empty: every destructive move is preceded by one that keeps
    /// both bundles recoverable, and EVERY move is checked — including the
    /// compensating move back.
    ///
    /// Arguments: APP OLD OLDPID OKMARK EXPECTED_VERSION FAILURE_MARKER.
    /// Exits 0 when the new build was healthy or the rollback succeeded; a
    /// non-zero code means a human is needed, and the failure marker records
    /// where both bundles are.
    ///
    /// The script is a separate function (not an inline literal) so the
    /// fault-injection probe can execute the real thing with shims on PATH.
    static func relaunchScript() -> String {
        return """
        #!/bin/bash
        set -u
        APP="$1"; OLD="$2"; OLDPID="$3"; OKMARK="$4"; EXPECTVER="$5"; FAILMARK="$6"
        rm -f "$OKMARK" || exit 1
        sleep 3
        open -n "$APP" >/dev/null 2>&1
        # Wait for the health handshake instead of a bare process check: a GUI
        # that starts but never reaches a healthy daemon still counts as failed.
        for _ in $(seq 1 45); do
            if [ -f "$OKMARK" ]; then exit 0; fi
            sleep 1
        done

        # Nothing could be put back. Leave BOTH bundles in place as evidence and
        # write down exactly where they are; never delete a usable copy.
        fail_recovery() {
            echo "OrbSleep 更新回滚失败：$1" >&2
            {
                echo "time=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
                echo "install=$APP"
                echo "saved=$OLD"
            } > "$FAILMARK" 2>/dev/null
            exit 3
        }

        RESCUE="$APP.rollback-$$"
        if ! mv "$APP" "$RESCUE"; then
            fail_recovery "无法移开新版本，安装位置保持原状（${APP}）"
        fi
        if ! mv "$OLD" "$APP"; then
            # Put the residue back so the install path is not empty.
            if ! mv "$RESCUE" "$APP"; then
                fail_recovery "旧版本与残留新版本都无法放回，请手动处理：install=${APP} saved=${OLD} rescue=${RESCUE}"
            fi
            fail_recovery "旧版本无法移回安装位置（备份仍在 ${OLD}）"
        fi
        # Prove the bundle at the install path really is the saved version:
        # Bundle.main caches Info.plist, only a disk read tells the truth.
        if ! /usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$APP/Contents/Info.plist" 2>/dev/null | grep -qx "$EXPECTVER"; then
            fail_recovery "恢复后的版本校验不通过（期望 ${EXPECTVER}），备份仍在 ${OLD}"
        fi
        rm -rf "$RESCUE"
        open -n "$APP" >/dev/null 2>&1
        """
    }

    /// Puts the saved old bundle back at the install path and never returns
    /// normally: it always throws the update failure the caller reported.
    ///
    /// A `copyItem` that dies half-way can leave an incomplete bundle AT the
    /// install path, which makes `moveItem(oldApp → appURL)` throw "file already
    /// exists". So the residue is moved aside first, the old bundle is restored,
    /// its identity re-read from disk, and only then is the residue dropped —
    /// the install path is never left empty or half-replaced.
    static func restorePreviousApp(appURL: URL, oldApp: URL, backupRoot: URL,
                                   expectedVersion: String, cause: Error) throws -> Never {
        let fm = FileManager.default
        var residue: URL?
        if fm.fileExists(atPath: appURL.path) {
            let aside = backupRoot.appendingPathComponent("OrbSleepManager.failed-\(UUID().uuidString).app")
            do {
                try fm.moveItem(at: appURL, to: aside)
                residue = aside
            } catch {
                throw UpdateError.installFailed("替换应用失败（\(cause.localizedDescription)），且无法移开安装位置上的残留包：\(error.localizedDescription)")
            }
        }
        do {
            try fm.moveItem(at: oldApp, to: appURL)
        } catch let moveError {
            // Put the residue back rather than leave the install path empty.
            // If even that fails, fall back to a copy of the saved bundle: the
            // backup lives on the same volume, so a copy can refill the install
            // path when a move could not. Only when both fail is a human needed,
            // and then the message names every surviving bundle.
            if let aside = residue {
                do {
                    try fm.moveItem(at: aside, to: appURL)
                } catch let residueError {
                    do {
                        try fm.copyItem(at: oldApp, to: appURL)
                    } catch let copyError {
                        throw UpdateError.installFailed(
                            "替换应用失败（\(cause.localizedDescription)），旧版本无法还原：\(moveError.localizedDescription)；残留放回失败：\(residueError.localizedDescription)；从备份复制失败：\(copyError.localizedDescription)。请手动处理：安装位置 \(appURL.path)，备份 \(oldApp.path)，残留 \(aside.path)"
                        )
                    }
                }
            }
            throw UpdateError.installFailed("替换应用失败（\(cause.localizedDescription)），旧版本还原也未成功：\(moveError.localizedDescription)")
        }
        let info = Self.bundleInfo(at: appURL)
        guard info?["CFBundleIdentifier"] as? String == Self.expectedBundleID,
              info?["CFBundleShortVersionString"] as? String == expectedVersion else {
            throw UpdateError.installFailed(
                "旧版本还原后校验失败（期望 \(expectedVersion)），请从更新源手动重新安装。备份保留在 \(oldApp.path)，安装位置保留在 \(appURL.path)"
            )
        }
        if let aside = residue { try? fm.removeItem(at: aside) }
        throw UpdateError.installFailed("替换应用失败，已恢复旧版本：\(cause.localizedDescription)")
    }

    /// Reads a bundle's Info.plist straight from disk. `Bundle(url:)` can hand
    /// back a cached Info.plist for a path that was just replaced, which would
    /// verify the wrong build.
    private static func bundleInfo(at url: URL) -> [String: Any]? {
        let plist = url.appendingPathComponent("Contents/Info.plist")
        guard let data = FileManager.default.contents(atPath: plist.path),
              let object = try? PropertyListSerialization.propertyList(from: data, format: nil) else { return nil }
        return object as? [String: Any]
    }

    /// Keeps the newest `keeping` rollback copies, deletes the rest. Abandoned
    /// new bundles (`*.failed-*.app`) are evidence and are never counted here.
    static func cleanupAppBackups(in backupRoot: URL, keeping: Int) {
        let fm = FileManager.default
        guard let entries = try? fm.contentsOfDirectory(at: backupRoot, includingPropertiesForKeys: [.contentModificationDateKey]) else {
            return
        }
        let apps = entries
            .filter { $0.pathExtension == "app" && !$0.lastPathComponent.contains(".failed-") }
            .sorted { (a, b) in
                // Rollback copies are named OrbSleepManager-<version>-<ISO8601>
                // .app, so the trailing timestamp is exact and sorts
                // monotonically. A bundle's content-modification date can predate
                // the copy (Finder preserves it), which would let the sweep drop
                // the rollback candidate it just saved.
                let ta = backupTimestamp(a), tb = backupTimestamp(b)
                if !ta.isEmpty, !tb.isEmpty, ta != tb { return ta > tb }
                let da = (try? a.resourceValues(forKeys: [.contentModificationDateKey]))?.contentModificationDate ?? .distantPast
                let db = (try? b.resourceValues(forKeys: [.contentModificationDateKey]))?.contentModificationDate ?? .distantPast
                return da > db
            }
        for stale in apps.dropFirst(max(0, keeping)) {
            try? fm.removeItem(at: stale)
        }
    }

    /// The `yyyy-MM-dd'T'HH-mm-ssZ` suffix of a rollback copy's name, or "" when
    /// the name does not carry one.
    private static func backupTimestamp(_ url: URL) -> String {
        let name = url.deletingPathExtension().lastPathComponent
        guard name.hasPrefix("OrbSleepManager-"), name.count >= 20 else { return "" }
        return String(name.suffix(20))
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
