// 更新回滚故障注入探针：用 xcrun swiftc 直接编译生产的 UpdateManager.swift
// 与协议模型，运行真实函数（不是复制一份逻辑）。`make swift-check` 调用。
//
// 覆盖 v0.2.7 审查里缺失的「新 App 部分复制失败后的旧版本恢复」：
// 安装位置在任何失败分支上都不得为空，也不得留下半套新包。

import Foundation

private func describe(_ error: Error) -> String {
    if let failure = error as? UpdateError, case .installFailed(let detail) = failure {
        return detail
    }
    return error.localizedDescription
}

private func fail(_ check: String, _ detail: String) -> Never {
    FileHandle.standardError.write("FAIL \(check): \(detail)\n".data(using: .utf8)!)
    exit(1)
}

private func scratch() throws -> URL {
    let url = FileManager.default.temporaryDirectory
        .appendingPathComponent("OrbSleepRollbackProbe-\(UUID().uuidString)", isDirectory: true)
    try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
    return url
}

@MainActor
private func makeBundle(at url: URL, version: String, identifier: String? = nil,
                        marker: String = "universal-binary-placeholder") throws {
    let bundleID = identifier ?? UpdateManager.expectedBundleID
    let fm = FileManager.default
    let contents = url.appendingPathComponent("Contents")
    try fm.createDirectory(at: contents.appendingPathComponent("MacOS"), withIntermediateDirectories: true)
    let plist: [String: Any] = [
        "CFBundleIdentifier": bundleID,
        "CFBundleShortVersionString": version,
        "CFBundleExecutable": "OrbSleepManager",
    ]
    let data = try PropertyListSerialization.data(fromPropertyList: plist, format: .xml, options: 0)
    try data.write(to: contents.appendingPathComponent("Info.plist"), options: .atomic)
    // 半套新包正是「可执行文件写到一半就断了」：用占位内容代表被截断的产物。
    try marker.data(using: .utf8)!.write(to: contents.appendingPathComponent("MacOS/OrbSleepManager"))
}

private func version(at url: URL) -> String {
    let plist = url.appendingPathComponent("Contents/Info.plist")
    guard let data = FileManager.default.contents(atPath: plist.path),
          let object = try? PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any] else {
        return "<无 Info.plist>"
    }
    return object["CFBundleShortVersionString"] as? String ?? "<无版本>"
}

private func names(in dir: URL) -> [String] {
    (try? FileManager.default.contentsOfDirectory(atPath: dir.path))?.sorted() ?? []
}

@main
@MainActor
enum UpdateRollbackProbe {
    static func main() {
        do {
            try partialCopyFailureRestoresPreviousVersion()
            try missingOldBundleKeepsInstallPathNonEmpty()
            try mismatchedRestoredIdentityIsRejected()
            try backupTrimKeepsNewestRollbackCandidate()
            try bashHelperSucceedsWhenHandshakeArrives()
            try bashHelperRollsBackWhenHandshakeTimesOut()
            try bashHelperKeepsInstallPathWhenOldMoveFails()
            try bashHelperEntersManualRecoveryWhenEveryMoveBackFails()
            try bashHelperRejectsBundleWithWrongVersion()
        } catch {
            fail("探针抛出未预期错误", describe(error))
        }
        print("OK 更新回滚故障注入探针全部通过")
    }

    // MARK: - 真实 Bash 回滚脚本故障注入

    /// 运行生产脚本本体（UpdateManager.relaunchScript()），用 PATH 上的 shim
    /// 控制 open/sleep/mv 的行为。脚本必须无论在哪一步失败都留下可恢复的证据。
    private struct HelperRun {
        var status: Int32
        var stderr: String
    }

    private static func runHelper(appURL: URL, oldApp: URL, expectedVersion: String,
                                  failMarker: URL, mvFails: Set<Int> = [],
                                  openWritesOK: Bool) throws -> HelperRun {
        let root = try scratch()
        defer { try? FileManager.default.removeItem(at: root) }
        let bin = root.appendingPathComponent("bin", isDirectory: true)
        try FileManager.default.createDirectory(at: bin, withIntermediateDirectories: true)
        let state = root.appendingPathComponent("state", isDirectory: true)
        try FileManager.default.createDirectory(at: state, withIntermediateDirectories: true)
        let okMarker = root.appendingPathComponent("update-ok")

        // mv: counts invocations and fails the configured ones, then delegates
        // to the real binary.
        let mvShim = """
        #!/bin/bash
        count_file="$SHIM_STATE/mv.count"
        n=$(( $(cat "$count_file" 2>/dev/null || echo 0) + 1 ))
        echo "$n" > "$count_file"
        for f in $MV_FAIL; do
            if [ "$f" = "$n" ]; then
                echo "shim: injected mv failure #$n" >&2
                exit 1
            fi
        done
        exec /bin/mv "$@"
        """
        // sleep is a no-op so the 45-iteration handshake loop completes instantly.
        let sleepShim = "#!/bin/bash\nexit 0\n"
        // open only simulates a healthy launch when asked to.
        let openShim = """
        #!/bin/bash
        if [ "${OPEN_WRITES_OK:-0}" = "1" ] && [ -n "${OKMARK:-}" ]; then
            : > "$OKMARK"
        fi
        exit 0
        """
        for (name, body) in [("mv", mvShim), ("sleep", sleepShim), ("open", openShim)] {
            let path = bin.appendingPathComponent(name)
            try body.write(to: path, atomically: true, encoding: .utf8)
            try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: path.path)
        }

        let scriptURL = root.appendingPathComponent("relaunch.sh")
        try UpdateManager.relaunchScript().write(to: scriptURL, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: scriptURL.path)

        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/bin/bash")
        process.arguments = [
            scriptURL.path, appURL.path, oldApp.path, "12345",
            okMarker.path, expectedVersion, failMarker.path,
        ]
        var env = ProcessInfo.processInfo.environment
        env["PATH"] = bin.path + ":" + (env["PATH"] ?? "/usr/bin:/bin:/usr/sbin:/sbin")
        env["SHIM_STATE"] = state.path
        env["MV_FAIL"] = mvFails.sorted().map(String.init).joined(separator: " ")
        env["OKMARK"] = okMarker.path
        env["OPEN_WRITES_OK"] = openWritesOK ? "1" : "0"
        process.environment = env
        let errPipe = Pipe()
        process.standardError = errPipe
        process.standardOutput = Pipe()
        try process.run()
        process.waitUntilExit()
        let data = errPipe.fileHandleForReading.readDataToEndOfFile()
        return HelperRun(status: process.terminationStatus, stderr: String(data: data, encoding: .utf8) ?? "")
    }

    /// 握手成功：脚本只能退出 0，安装位置保持新版本，不做任何回滚。
    private static func bashHelperSucceedsWhenHandshakeArrives() throws {
        let root = try scratch()
        defer { try? FileManager.default.removeItem(at: root) }
        let appURL = root.appendingPathComponent("OrbSleepManager.app")
        let backupRoot = root.appendingPathComponent("app-backup")
        try FileManager.default.createDirectory(at: backupRoot, withIntermediateDirectories: true)
        try makeBundle(at: appURL, version: "0.9.9")
        let oldApp = backupRoot.appendingPathComponent("OrbSleepManager-0.2.8-saved.app")
        try makeBundle(at: oldApp, version: "0.2.8")

        let run = try runHelper(appURL: appURL, oldApp: oldApp, expectedVersion: "0.2.8",
                                  failMarker: root.appendingPathComponent("app-update-failed.txt"), openWritesOK: true)
        if run.status != 0 {
            fail("握手成功必须退出 0", "status \(run.status) \(run.stderr)")
        }
        if version(at: appURL) != "0.9.9" {
            fail("握手成功时不得回滚", "安装位置版本 \(version(at: appURL))")
        }
        if !FileManager.default.fileExists(atPath: oldApp.path) {
            fail("握手成功时不得消耗旧包备份", "备份不见了")
        }
    }

    /// 45 秒健康握手超时：脚本必须回滚到旧版本，并清理移开的残留。
    private static func bashHelperRollsBackWhenHandshakeTimesOut() throws {
        let root = try scratch()
        defer { try? FileManager.default.removeItem(at: root) }
        let fm = FileManager.default
        let appURL = root.appendingPathComponent("OrbSleepManager.app")
        let backupRoot = root.appendingPathComponent("app-backup")
        try fm.createDirectory(at: backupRoot, withIntermediateDirectories: true)
        try makeBundle(at: appURL, version: "0.9.9")
        let oldApp = backupRoot.appendingPathComponent("OrbSleepManager-0.2.8-saved.app")
        try makeBundle(at: oldApp, version: "0.2.8")

        let run = try runHelper(appURL: appURL, oldApp: oldApp, expectedVersion: "0.2.8",
                                  failMarker: root.appendingPathComponent("app-update-failed.txt"), openWritesOK: false)
        if run.status != 0 {
            fail("超时回滚必须成功退出", "status \(run.status) \(run.stderr)")
        }
        if version(at: appURL) != "0.2.8" {
            fail("超时后安装位置必须是旧版本", "实际 \(version(at: appURL))")
        }
        if fm.fileExists(atPath: oldApp.path) {
            fail("旧包必须被移回安装位置", "备份仍在 \(oldApp.path)")
        }
        let leftovers = names(in: root).filter { $0.contains(".rollback-") }
        if !leftovers.isEmpty {
            fail("成功回滚后必须清理临时目录", leftovers.joined(separator: "、"))
        }
        if fm.fileExists(atPath: root.appendingPathComponent("app-update-failed.txt").path) {
            fail("成功回滚不得写人工恢复标记", "标记存在")
        }
    }

    /// 旧包移回失败（第二次 mv 失败）：安装位置必须保住半套新包，并进入明确的
    /// 人工恢复状态，而不是静默空掉。
    private static func bashHelperKeepsInstallPathWhenOldMoveFails() throws {
        let root = try scratch()
        defer { try? FileManager.default.removeItem(at: root) }
        let fm = FileManager.default
        let appURL = root.appendingPathComponent("OrbSleepManager.app")
        let backupRoot = root.appendingPathComponent("app-backup")
        try fm.createDirectory(at: backupRoot, withIntermediateDirectories: true)
        try makeBundle(at: appURL, version: "0.9.9", marker: "truncated")
        let oldApp = backupRoot.appendingPathComponent("OrbSleepManager-0.2.8-saved.app")
        try makeBundle(at: oldApp, version: "0.2.8")

        // 第一次 mv（移开新包）成功，第二次（放回旧包）失败，第三次（放回残留）成功。
        let run = try runHelper(appURL: appURL, oldApp: oldApp, expectedVersion: "0.2.8",
                                  failMarker: root.appendingPathComponent("app-update-failed.txt"),
                                  mvFails: [2], openWritesOK: false)
        if run.status != 3 {
            fail("旧包移回失败必须进入人工恢复状态", "status \(run.status) \(run.stderr)")
        }
        if !fm.fileExists(atPath: appURL.path) {
            fail("安装位置不得为空", "残留与旧包都不在安装位置")
        }
        if version(at: appURL) != "0.9.9" {
            fail("无法放回旧包时必须保住残留", "实际 \(version(at: appURL))")
        }
        let marker = root.appendingPathComponent("app-update-failed.txt")
        guard let text = try? String(contentsOf: marker, encoding: .utf8) else {
            fail("必须写入人工恢复标记", "标记缺失")
        }
        if !text.contains(oldApp.path) {
            fail("人工恢复标记必须记录备份位置", text)
        }
    }

    /// 第二次和补偿用的第三次 mv 都失败：安装位置无法避免地空了，但两个包都
    /// 不得被删除，必须留下人工恢复标记与准确路径。
    private static func bashHelperEntersManualRecoveryWhenEveryMoveBackFails() throws {
        let root = try scratch()
        defer { try? FileManager.default.removeItem(at: root) }
        let fm = FileManager.default
        let appURL = root.appendingPathComponent("OrbSleepManager.app")
        let backupRoot = root.appendingPathComponent("app-backup")
        try fm.createDirectory(at: backupRoot, withIntermediateDirectories: true)
        try makeBundle(at: appURL, version: "0.9.9", marker: "truncated")
        let oldApp = backupRoot.appendingPathComponent("OrbSleepManager-0.2.8-saved.app")
        try makeBundle(at: oldApp, version: "0.2.8")

        let run = try runHelper(appURL: appURL, oldApp: oldApp, expectedVersion: "0.2.8",
                                  failMarker: root.appendingPathComponent("app-update-failed.txt"),
                                  mvFails: [2, 3], openWritesOK: false)
        if run.status != 3 {
            fail("双重失败必须进入人工恢复状态", "status \(run.status) \(run.stderr)")
        }
        if !run.stderr.contains("手动处理") {
            fail("必须提示人工处理", run.stderr)
        }
        // 两个包都还在：一个在备份位置，一个在 rescue 位置。
        if !fm.fileExists(atPath: oldApp.path) {
            fail("备份包不得被删除", oldApp.path)
        }
        let rescue = try fm.contentsOfDirectory(atPath: root.path).filter { $0.contains(".rollback-") }
        if rescue.isEmpty {
            fail("残留新包不得被删除", "没有 rollback 目录")
        }
        let marker = root.appendingPathComponent("app-update-failed.txt")
        guard let text = try? String(contentsOf: marker, encoding: .utf8) else {
            fail("必须写入人工恢复标记", "标记缺失")
        }
        if !text.contains(oldApp.path) || !text.contains(appURL.path) {
            fail("人工恢复标记必须记录两个位置", text)
        }
    }

    /// 放回安装位置的包版本不符：必须进入人工恢复状态并保留备份，不能报告成功。
    private static func bashHelperRejectsBundleWithWrongVersion() throws {
        let root = try scratch()
        defer { try? FileManager.default.removeItem(at: root) }
        let fm = FileManager.default
        let appURL = root.appendingPathComponent("OrbSleepManager.app")
        let backupRoot = root.appendingPathComponent("app-backup")
        try fm.createDirectory(at: backupRoot, withIntermediateDirectories: true)
        try makeBundle(at: appURL, version: "0.9.9")
        let oldApp = backupRoot.appendingPathComponent("OrbSleepManager-0.2.8-saved.app")
        try makeBundle(at: oldApp, version: "0.0.1")

        let run = try runHelper(appURL: appURL, oldApp: oldApp, expectedVersion: "0.2.8",
                                  failMarker: root.appendingPathComponent("app-update-failed.txt"), openWritesOK: false)
        if run.status != 3 {
            fail("版本不符必须进入人工恢复状态", "status \(run.status) \(run.stderr)")
        }
        if !run.stderr.contains("版本校验不通过") {
            fail("必须说明版本校验失败", run.stderr)
        }
        if !fm.fileExists(atPath: appURL.path) {
            fail("版本不符时不得清空安装位置", "安装位置已空")
        }
    }

    /// 半套新包占住了安装位置：先把残留移开，再把旧包放回，最后才丢弃残留。
    private static func partialCopyFailureRestoresPreviousVersion() throws {
        let root = try scratch()
        defer { try? FileManager.default.removeItem(at: root) }
        let fm = FileManager.default
        let appURL = root.appendingPathComponent("OrbSleepManager.app")
        let backupRoot = root.appendingPathComponent("app-backup")
        try fm.createDirectory(at: backupRoot, withIntermediateDirectories: true)
        try makeBundle(at: appURL, version: "0.9.9", marker: "truncated")
        let oldApp = backupRoot.appendingPathComponent("OrbSleepManager-0.2.8-2026-11-01T01-02-03Z.app")
        try makeBundle(at: oldApp, version: "0.2.8")

        let cause = NSError(domain: "probe", code: 1, userInfo: [NSLocalizedDescriptionKey: "copyItem 中途失败"])
        do {
            // restorePreviousApp 是 Never：它只能以错误退出。
            try UpdateManager.restorePreviousApp(appURL: appURL, oldApp: oldApp, backupRoot: backupRoot,
                                                 expectedVersion: "0.2.8", cause: cause)
        } catch let error as UpdateError {
            guard case .installFailed(let detail) = error, detail.contains("已恢复旧版本") else {
                fail("回滚成功时的错误文案", describe(error))
            }
        }

        if version(at: appURL) != "0.2.8" {
            fail("安装位置必须是旧版本", "实际 \(version(at: appURL))")
        }
        if fm.fileExists(atPath: oldApp.path) {
            fail("旧包必须被移回安装位置", "备份位置仍有 \(oldApp.lastPathComponent)")
        }
        // 恢复成功后不得留下移开的残留包。
        let leftovers = names(in: backupRoot).filter { $0.contains(".failed-") }
        if !leftovers.isEmpty {
            fail("成功回滚后必须清理移开的残留", leftovers.joined(separator: "、"))
        }
    }

    /// 旧包本身不见了：宁可保住半套新包，也不能让安装位置变空。
    private static func missingOldBundleKeepsInstallPathNonEmpty() throws {
        let root = try scratch()
        defer { try? FileManager.default.removeItem(at: root) }
        let fm = FileManager.default
        let appURL = root.appendingPathComponent("OrbSleepManager.app")
        let backupRoot = root.appendingPathComponent("app-backup")
        try fm.createDirectory(at: backupRoot, withIntermediateDirectories: true)
        try makeBundle(at: appURL, version: "0.9.9", marker: "truncated")
        let oldApp = backupRoot.appendingPathComponent("OrbSleepManager-0.2.8-2026-11-01T01-02-04Z.app")

        let cause = NSError(domain: "probe", code: 2, userInfo: [NSLocalizedDescriptionKey: "copyItem 中途失败"])
        do {
            try UpdateManager.restorePreviousApp(appURL: appURL, oldApp: oldApp, backupRoot: backupRoot,
                                                 expectedVersion: "0.2.8", cause: cause)
        } catch let error as UpdateError {
            guard case .installFailed(let detail) = error, detail.contains("旧版本还原也未成功") else {
                fail("旧包缺失时的错误文案", describe(error))
            }
        }
        if !fm.fileExists(atPath: appURL.path) {
            fail("安装位置不得为空", "新包残留和旧包都不见了")
        }
        if version(at: appURL) != "0.9.9" {
            fail("还原失败时必须把残留放回原位", "实际 \(version(at: appURL))")
        }
    }

    /// 放回安装位置的包必须重新读盘校验身份：`Bundle(url:)` 会命中刚被替换路径
    /// 的缓存，只有从磁盘读 Info.plist 才能证明恢复的是那一版。
    private static func mismatchedRestoredIdentityIsRejected() throws {
        let root = try scratch()
        defer { try? FileManager.default.removeItem(at: root) }
        let fm = FileManager.default
        let appURL = root.appendingPathComponent("OrbSleepManager.app")
        let backupRoot = root.appendingPathComponent("app-backup")
        try fm.createDirectory(at: backupRoot, withIntermediateDirectories: true)
        try makeBundle(at: appURL, version: "0.9.9", marker: "truncated")
        let oldApp = backupRoot.appendingPathComponent("OrbSleepManager-0.2.8-2026-11-01T01-02-05Z.app")
        try makeBundle(at: oldApp, version: "0.0.1")

        let cause = NSError(domain: "probe", code: 3, userInfo: [NSLocalizedDescriptionKey: "copyItem 中途失败"])
        do {
            try UpdateManager.restorePreviousApp(appURL: appURL, oldApp: oldApp, backupRoot: backupRoot,
                                                 expectedVersion: "0.2.8", cause: cause)
        } catch let error as UpdateError {
            guard case .installFailed(let detail) = error, detail.contains("旧版本还原后校验失败") else {
                fail("身份校验的错误文案", describe(error))
            }
        }
        if !fm.fileExists(atPath: appURL.path) {
            fail("身份不符时不得清空安装位置", "安装位置已空")
        }
    }

    /// 裁剪按备份文件名里的时间戳排序：Finder 会保留内容的修改时间，用它的
    /// contentModificationDate 排序会把刚存下的回滚候选当成最旧的先删掉。
    private static func backupTrimKeepsNewestRollbackCandidate() throws {
        let root = try scratch()
        defer { try? FileManager.default.removeItem(at: root) }
        let fm = FileManager.default
        let backupRoot = root.appendingPathComponent("app-backup")
        try fm.createDirectory(at: backupRoot, withIntermediateDirectories: true)
        let stamps = ["2026-10-01T00-00-01Z", "2026-10-01T00-00-02Z", "2026-10-01T00-00-03Z", "2026-10-01T00-00-04Z"]
        let base = Date(timeIntervalSince1970: 1_700_000_000)
        for (index, stamp) in stamps.enumerated() {
            let url = backupRoot.appendingPathComponent("OrbSleepManager-0.2.\(index)-\(stamp).app")
            try makeBundle(at: url, version: "0.2.\(index)")
            // 修改时间与文件名顺序完全相反。
            let inverted = base.addingTimeInterval(Double(stamps.count - index) * 3600)
            try fm.setAttributes([.modificationDate: inverted], ofItemAtPath: url.path)
        }
        let evidence = backupRoot.appendingPathComponent("OrbSleepManager.failed-abc.app")
        try makeBundle(at: evidence, version: "0.9.9")

        UpdateManager.cleanupAppBackups(in: backupRoot, keeping: 2)

        let kept = Set(names(in: backupRoot))
        let want: Set<String> = ["OrbSleepManager-0.2.3-2026-10-01T00-00-04Z.app",
                                 "OrbSleepManager-0.2.2-2026-10-01T00-00-03Z.app",
                                 "OrbSleepManager.failed-abc.app"]
        if kept != want {
            fail("按文件名时间戳保留最新两份", "留下 \(kept.sorted().joined(separator: "、"))")
        }
    }
}
