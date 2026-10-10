import Foundation

/// What a background push did, in the terms iOS asks for
/// (`UIBackgroundFetchResult`), without this package depending on UIKit.
public enum BackgroundWakeResult: Sendable, Equatable {
    case newData, noData, failed
}

/// Answers a background push (`{"aps":{"content-available":1}}`) with one
/// ordinary sync pull.
///
/// The push carries no data and no hint of what changed: it only says "the
/// server has something for you". The pull goes through the same engine path a
/// live hint does. iOS gives a background push about 30 seconds and penalises
/// an app that does not call the completion handler in time, so the answer is
/// bounded by `deadline` whatever the network does.
@MainActor
public final class BackgroundWakeHandler {
    private let sync: @MainActor () async -> Bool
    private let deadline: Duration

    /// - Parameter sync: one `.backgroundPush` sync; `true` when the device
    ///   ended up to date.
    public init(sync: @escaping @MainActor () async -> Bool, deadline: Duration = .seconds(25)) {
        self.sync = sync
        self.deadline = deadline
    }

    /// One `.backgroundPush` sync, answered within the deadline (iOS allows about 30 s).
    public func receiveSilentPush() async -> BackgroundWakeResult {
        let sync = self.sync
        switch await MainActorDeadline.first(within: deadline, { await sync() }) {
        case true?: return .newData
        case false?, nil: return .failed
        }
    }
}

/// Runs main-actor work against a deadline: whichever finishes first answers.
///
/// Past the deadline the work is cancelled and the caller gets `nil` at once.
/// The work may still be winding down — the sync engine finishes a pass it has
/// started — but nobody waits for it. Not a task group: a group waits for every
/// child, and a sync that ignores cancellation would hold the answer hostage.
enum MainActorDeadline {
    @MainActor
    static func first<T: Sendable>(within deadline: Duration, _ work: @escaping @MainActor () async -> T) async -> T? {
        let race = Race<T>()
        let worker = Task { @MainActor in race.finish(await work()) }
        let timer = Task {
            try? await Task.sleep(for: deadline)
            race.finish(nil)
        }
        let result = await withCheckedContinuation { race.install($0) }
        timer.cancel()
        if result == nil { worker.cancel() }
        return result
    }

    /// The first answer wins; later ones are dropped. Locked rather than
    /// actor-isolated, because the timer finishes it from off the main actor.
    private final class Race<T: Sendable>: @unchecked Sendable {
        private let lock = NSLock()
        private var continuation: CheckedContinuation<T?, Never>?
        private var answer: T??

        func install(_ continuation: CheckedContinuation<T?, Never>) {
            lock.lock()
            if let answer {
                lock.unlock()
                continuation.resume(returning: answer)
                return
            }
            self.continuation = continuation
            lock.unlock()
        }

        func finish(_ value: T?) {
            lock.lock()
            guard answer == nil else { lock.unlock(); return }
            answer = .some(value)
            let waiting = continuation
            continuation = nil
            lock.unlock()
            waiting?.resume(returning: value)
        }
    }
}
