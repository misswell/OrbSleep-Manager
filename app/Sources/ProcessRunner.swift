import Foundation

/// Result of a finished subprocess.
struct ProcessResult: Sendable {
    let exitStatus: Int32
    let output: String

    var succeeded: Bool { exitStatus == 0 }
}

/// Runs subprocesses without blocking the caller's actor and without the
/// classic pipe deadlock: stdout/stderr are consumed while the process runs,
/// so a full pipe buffer cannot deadlock it against our wait.
///
/// All GUI process execution goes through this runner; no shell strings are
/// built anywhere and arguments are always passed as fixed arrays.
enum ProcessRunner {
    static func run(executable: URL, arguments: [String]) async throws -> ProcessResult {
        try await Task.detached(priority: .userInitiated) {
            let process = Process()
            process.executableURL = executable
            process.arguments = arguments
            let pipe = Pipe()
            process.standardOutput = pipe
            process.standardError = pipe
            do {
                try process.run()
            } catch {
                return ProcessResult(exitStatus: -1, output: error.localizedDescription)
            }
            let data = pipe.fileHandleForReading.readDataToEndOfFile()
            process.waitUntilExit()
            return ProcessResult(
                exitStatus: process.terminationStatus,
                output: String(decoding: data, as: UTF8.self)
            )
        }.value
    }

    /// Runs a command and reports only whether it exited successfully.
    /// Used for probes like `launchctl print`, where a non-zero exit carries
    /// the actual answer.
    static func succeeded(executable: URL, arguments: [String]) async -> Bool {
        guard let result = try? await run(executable: executable, arguments: arguments) else {
            return false
        }
        return result.succeeded
    }
}
