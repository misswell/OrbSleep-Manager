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
        } catch {
            fail("探针抛出未预期错误", describe(error))
        }
        print("OK 更新回滚故障注入探针全部通过")
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
