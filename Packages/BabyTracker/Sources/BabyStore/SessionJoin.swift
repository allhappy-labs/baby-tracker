import Foundation
import SwiftData
import BabyCore

extension BabyStore {
    /// Decided once, when a part starts, and stored on the timer or entry so
    /// every device that later completes it agrees.
    func continuedSessionID(_ kind: SessionKind, for babyID: UUID, startingAt start: Date) throws -> UUID? {
        let limit = breakLimits().seconds(for: kind)
        switch kind {
        case .feed:
            let feeds = try sessionFeeds(for: babyID).filter { $0.kind != .solids }
            let previous = Self.projectSessions(feeds)
                .filter { $0.endedAt <= start }
                .max { $0.endedAt < $1.endedAt }
            guard let previous, start.timeIntervalSince(previous.endedAt) <= limit else { return nil }
            guard try !sleepFalls(between: previous.endedAt, and: start, for: babyID) else { return nil }
            return previous.id
        case .sleep:
            let previous = try sleepSessions(for: babyID, from: .distantPast, to: .distantFuture)
                .compactMap { session in session.endedAt.map { (session, $0) } }
                .filter { $0.1 <= start }
                .max { $0.1 < $1.1 }
            guard let previous, start.timeIntervalSince(previous.1) <= limit else { return nil }
            return previous.0.id
        }
    }

    /// True when a sleep — completed or still running — occupies any part of
    /// `(lower, upper)`. A sleep in a feed gap means the baby was put down
    /// between the two feed parts, so the next part starts its own session
    /// even if it arrives within the break limit.
    private func sleepFalls(between lower: Date, and upper: Date, for babyID: UUID) throws -> Bool {
        let sleeps = try sessionSleeps(for: babyID)
        if sleeps.contains(where: { $0.startedAt < upper && ($0.endedAt ?? now) > lower }) { return true }
        if let running = try activeTimer(kind: .sleep, for: babyID), running.startedAt < upper { return true }
        return false
    }
}
