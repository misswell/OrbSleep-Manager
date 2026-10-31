import Darwin
import Foundation

/// 与 orbsleepd 通信时可能出现的错误。
enum CoreError: Error, Sendable, Equatable, LocalizedError {
    /// Socket 不存在或连接失败。
    case unreachable(String)
    /// 连接中途断开。
    case disconnected(String)
    /// 请求超时。
    case timeout(String)
    /// 后台助手返回的业务错误。
    case remote(code: String, message: String)
    /// 响应格式不符合协议。
    case invalidResponse(String)

    var errorDescription: String? {
        switch self {
        case .unreachable(let message):
            return "无法连接 orbsleepd：\(message)"
        case .disconnected(let message):
            return "与 orbsleepd 的连接已断开：\(message)"
        case .timeout(let message):
            return "请求超时：\(message)"
        case .remote(_, let message):
            return message
        case .invalidResponse(let message):
            return "响应格式无效：\(message)"
        }
    }

    var isConnectionProblem: Bool {
        switch self {
        case .unreachable, .disconnected:
            return true
        default:
            return false
        }
    }

    var isTimeout: Bool {
        if case .timeout = self { return true }
        return false
    }
}

/// 基于 Unix Domain Socket + 换行分隔 JSON 的协议客户端。
///
/// 所有 socket 状态都由一个串行 `DispatchQueue` 保护：
/// - 连接按需建立，断开后下一次请求自动重连；
/// - 请求按 `id` 与响应匹配，支持并发；
/// - 读取由独立线程驱动，按行切分，单行上限 8MB；
/// - `async/await` 由 `CheckedContinuation` 桥接，UI 侧再切回 MainActor。
final class CoreClient: @unchecked Sendable {
    static let shared = CoreClient()

    /// 单行响应上限（8MB）。
    static let maxLineBytes = 8 * 1024 * 1024
    static let protocolVersion = 1
    static let defaultTimeout: TimeInterval = 15

    static var defaultDataDirectory: String {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/Application Support/OrbSleep", isDirectory: true)
            .path
    }

    static var defaultSocketPath: String {
        if let override = ProcessInfo.processInfo.environment["ORBSLEEP_SOCKET"], !override.isEmpty {
            return override
        }
        return defaultDataDirectory + "/daemon.sock"
    }

    /// 根据方法名决定超时。事务恢复在后台是同步执行的（最长 15 分钟），
    /// GUI 若沿用默认 15 秒会提前报超时、误导用户操作，因此单独放宽。
    static func timeout(for method: String) -> TimeInterval {
        switch method {
        case "scan", "plan.create":
            return 60
        case "system.recover", "system.recovery.resolve":
            return 960
        case "backup.create":
            return 120
        default:
            return defaultTimeout
        }
    }

    let socketPath: String

    private let queue = DispatchQueue(label: "com.misswell.orbsleepmanager.coreclient")
    private var fileDescriptor: Int32 = -1
    private var generation = 0
    private var pending: [String: PendingRequest] = [:]

    private struct PendingRequest {
        let continuation: CheckedContinuation<Data, any Error>
        let method: String
    }

    init(socketPath: String = CoreClient.defaultSocketPath) {
        self.socketPath = socketPath
    }

    // MARK: - 公开 API

    /// 发送一次请求并解码结果。失败抛出 `CoreError`。
    func call<T: Decodable & Sendable>(
        _ method: String,
        params: [String: JSONValue] = [:],
        timeout: TimeInterval? = nil,
        as type: T.Type = T.self
    ) async throws -> T {
        let data = try await sendRequest(
            method: method,
            params: params,
            timeout: timeout ?? Self.timeout(for: method)
        )
        do {
            return try JSONDecoder().decode(T.self, from: data)
        } catch {
            throw CoreError.invalidResponse("\(method) 响应无法解析：\(error.localizedDescription)")
        }
    }

    /// 轻量连通性检查。
    func ping() async throws -> PingResult {
        try await call("ping", timeout: 5, as: PingResult.self)
    }

    /// 断开当前连接（下一次请求会重新连接）。
    func resetConnection() {
        queue.sync {
            failAllLocked(error: .disconnected("连接已被重置"))
            closeLocked()
        }
    }

    // MARK: - 请求发送

    private func sendRequest(method: String, params: [String: JSONValue], timeout: TimeInterval) async throws -> Data {
        let id = UUID().uuidString
        return try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Data, any Error>) in
            queue.async { [weak self] in
                guard let self else {
                    continuation.resume(throwing: CoreError.disconnected("客户端已释放"))
                    return
                }
                do {
                    try self.ensureConnectedLocked()
                    let line = try Self.encodeRequest(method: method, id: id, params: params)
                    self.pending[id] = PendingRequest(continuation: continuation, method: method)
                    do {
                        try self.writeLocked(line)
                    } catch {
                        if let request = self.pending.removeValue(forKey: id) {
                            let reason = CoreError.disconnected("写入请求失败：\(error.localizedDescription)")
                            request.continuation.resume(throwing: reason)
                        }
                        self.failAllLocked(error: .disconnected("写入请求失败，连接已关闭"))
                        self.closeLocked()
                        return
                    }
                } catch {
                    continuation.resume(throwing: self.mapConnectionError(error))
                    return
                }
                self.queue.asyncAfter(deadline: .now() + timeout) { [weak self] in
                    self?.timeoutFired(id: id, method: method, timeout: timeout)
                }
            }
        }
    }

    private func timeoutFired(id: String, method: String, timeout: TimeInterval) {
        guard let request = pending.removeValue(forKey: id) else { return }
        request.continuation.resume(
            throwing: CoreError.timeout("\(method) 超过 \(Int(timeout)) 秒没有响应")
        )
    }

    private static func encodeRequest(method: String, id: String, params: [String: JSONValue]) throws -> Data {
        let object: [String: JSONValue] = [
            "v": .number(Double(protocolVersion)),
            "id": .string(id),
            "method": .string(method),
            "params": .object(params)
        ]
        do {
            var data = try JSONEncoder().encode(JSONValue.object(object))
            data.append(0x0A)
            return data
        } catch {
            throw CoreError.invalidResponse("请求编码失败：\(error.localizedDescription)")
        }
    }

    // MARK: - 连接管理（调用方必须持有 queue）

    private func ensureConnectedLocked() throws {
        if fileDescriptor >= 0 { return }
        guard FileManager.default.fileExists(atPath: socketPath) else {
            throw CoreError.unreachable("未找到 Socket 文件 \(socketPath)")
        }
        let newDescriptor = socket(AF_UNIX, SOCK_STREAM, 0)
        guard newDescriptor >= 0 else {
            throw CoreError.unreachable("创建 Socket 失败：\(Self.errnoText())")
        }
        var noSigPipe: Int32 = 1
        _ = setsockopt(
            newDescriptor,
            SOL_SOCKET,
            SO_NOSIGPIPE,
            &noSigPipe,
            socklen_t(MemoryLayout<Int32>.size)
        )
        do {
            try Self.connect(descriptor: newDescriptor, path: socketPath)
        } catch {
            close(newDescriptor)
            throw error
        }
        fileDescriptor = newDescriptor
        generation += 1
        startReader(descriptor: newDescriptor, generation: generation)
    }

    private static func connect(descriptor: Int32, path: String) throws {
        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        let pathBytes = Array(path.utf8)
        let capacity = MemoryLayout.size(ofValue: address.sun_path)
        guard pathBytes.count < capacity else {
            throw CoreError.unreachable("Socket 路径过长（上限 \(capacity - 1) 字节）")
        }
        withUnsafeMutableBytes(of: &address.sun_path) { buffer in
            buffer.copyBytes(from: pathBytes)
        }
        let offset = MemoryLayout<sockaddr_un>.offset(of: \.sun_path) ?? 2
        let length = socklen_t(offset + pathBytes.count + 1)
        let result = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) { socketAddress in
                Darwin.connect(descriptor, socketAddress, length)
            }
        }
        if result != 0 {
            let code = errno
            throw CoreError.unreachable(Self.text(forErrno: code))
        }
    }

    private func writeLocked(_ data: Data) throws {
        var offset = 0
        let total = data.count
        while offset < total {
            let written = data.withUnsafeBytes { raw -> Int in
                guard let base = raw.baseAddress else { return -1 }
                return Darwin.send(
                    fileDescriptor,
                    base.advanced(by: offset),
                    total - offset,
                    0
                )
            }
            if written > 0 {
                offset += written
                continue
            }
            if written < 0 && errno == EINTR { continue }
            throw CoreError.disconnected(Self.errnoText())
        }
    }

    private func closeLocked() {
        if fileDescriptor >= 0 {
            close(fileDescriptor)
            fileDescriptor = -1
        }
        generation += 1
    }

    private func failAllLocked(error: CoreError) {
        let requests = pending
        pending.removeAll()
        for (_, request) in requests {
            request.continuation.resume(throwing: error)
        }
    }

    private func mapConnectionError(_ error: Error) -> CoreError {
        if let core = error as? CoreError { return core }
        return .unreachable(error.localizedDescription)
    }

    // MARK: - 读取

    private func startReader(descriptor: Int32, generation: Int) {
        Thread.detachNewThread { [weak self] in
            Thread.current.name = "OrbSleep.CoreClient.Reader"
            self?.readLoop(descriptor: descriptor, generation: generation)
        }
    }

    private func readLoop(descriptor: Int32, generation: Int) {
        var carry = Data()
        // `scanned` 记录 carry 头部已确认不含换行的字节数，避免每次读取都从头扫描（O(n²)）。
        var scanned = 0
        var buffer = [UInt8](repeating: 0, count: 64 * 1024)

        while true {
            let count = buffer.withUnsafeMutableBytes { raw -> Int in
                guard let base = raw.baseAddress else { return -1 }
                return Darwin.recv(descriptor, base, raw.count, 0)
            }
            if count > 0 {
                carry.append(contentsOf: buffer[0..<count])
                guard processLines(carry: &carry, scanned: &scanned) else { return }
                // 处理完完整行后，carry 里剩下的就是未结束的当前行。
                if carry.count > Self.maxLineBytes {
                    queue.sync {
                        self.failAllLocked(
                            error: .disconnected("响应行超过 \(Self.maxLineBytes) 字节上限，连接已关闭")
                        )
                        self.closeLocked()
                    }
                    return
                }
            } else if count == 0 {
                break
            } else {
                if errno == EINTR { continue }
                break
            }
        }

        queue.sync {
            guard self.generation == generation else { return }
            self.failAllLocked(error: .disconnected("后台助手关闭了连接"))
            self.closeLocked()
        }
    }

    /// 从 carry 中切出所有完整的行，每次只扫描尚未扫描过的部分。
    /// 返回 `false` 表示行超长，连接已被关闭。
    private func processLines(carry: inout Data, scanned: inout Int) -> Bool {
        while true {
            let searchStart = carry.startIndex + scanned
            guard searchStart < carry.endIndex else { return true }
            guard let newlineIndex = carry[searchStart...].firstIndex(of: 0x0A) else {
                scanned = carry.count
                return true
            }
            let lineData = Data(carry[carry.startIndex..<newlineIndex])
            carry.removeSubrange(carry.startIndex...newlineIndex)
            scanned = 0
            if lineData.count > Self.maxLineBytes {
                queue.sync {
                    self.failAllLocked(
                        error: .disconnected("响应行超过 \(Self.maxLineBytes) 字节上限，连接已关闭")
                    )
                    self.closeLocked()
                }
                return false
            }
            if !lineData.isEmpty {
                queue.sync { self.handleLineLocked(lineData) }
            }
        }
    }

    private func handleLineLocked(_ data: Data) {
        guard let value = try? JSONDecoder().decode(JSONValue.self, from: data),
              case .object(let object) = value,
              let id = object["id"]?.stringValue,
              let request = pending.removeValue(forKey: id) else {
            return
        }

        let succeeded = object["ok"]?.boolValue ?? false
        if succeeded {
            let result = object["result"] ?? .null
            do {
                let encoded = try JSONEncoder().encode(result)
                request.continuation.resume(returning: encoded)
            } catch {
                request.continuation.resume(
                    throwing: CoreError.invalidResponse("\(request.method) 结果无法重新编码")
                )
            }
        } else {
            let errorObject = object["error"]?.objectValue ?? [:]
            let code = errorObject["code"]?.stringValue ?? "internal"
            let message = errorObject["message"]?.stringValue ?? "后台返回未知错误"
            request.continuation.resume(throwing: CoreError.remote(code: code, message: message))
        }
    }

    // MARK: - errno 辅助

    private static func errnoText() -> String {
        text(forErrno: errno)
    }

    private static func text(forErrno code: Int32) -> String {
        if let message = strerror(code) {
            return String(cString: message)
        }
        return "errno \(code)"
    }
}
