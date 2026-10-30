import Foundation

/// Serializes exclusive maintenance work (uninstall, migration, backup
/// restore, upgrade, context switch, service lifecycle).
///
/// Boolean busy flags cannot provide mutual exclusion across suspension
/// points: two tasks can both pass a `guard !isBusy` check before either
/// sets the flag, or interleave halfway through an operation. Every
/// exclusive operation takes this gate for its whole duration; read-only
/// refreshes never take it.
actor MaintenanceGate {
    private var locked = false
    private var waiters: [CheckedContinuation<Void, Never>] = []

    func acquire() async {
        if !locked {
            locked = true
            return
        }
        await withCheckedContinuation { waiters.append($0) }
        // Ownership was transferred by release(); locked stays true for us.
    }

    func release() {
        if let next = waiters.first {
            waiters.removeFirst()
            next.resume()
        } else {
            locked = false
        }
    }
}
