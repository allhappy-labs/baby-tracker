import Foundation
import BabyCore

/// An activity's start and, once it has one, its end — the one place the
/// app judges an interval. Pure, so every panel and its tests agree.
public struct TimeSpan: Equatable, Sendable {
    public enum Problem: Equatable, Sendable {
        case endBeforeStart, inTheFuture, tooShort
    }

    public var start: Date
    /// Nil: no end — a bottle given at a moment, or a part still running.
    public var end: Date?

    public init(start: Date, end: Date?) {
        self.start = start
        self.end = end
    }

    public var seconds: Int? {
        end.map { max(0, Int($0.timeIntervalSince(start).rounded())) }
    }

    /// "18m", read-only beside the rows. Never edited directly.
    public var lengthText: String? { seconds.map(ElapsedFormatter.precise) }

    /// Nil when the span can be saved. `latest` is now, or later for an entry
    /// already stored ahead of the clock. `minimumSeconds` is 30 for a
    /// breast side, as `logBreast` refuses shorter.
    public func problem(latest: Date, minimumSeconds: Int = 1) -> Problem? {
        if start > latest { return .inTheFuture }
        guard let end else { return nil }
        if end <= start { return .endBeforeStart }
        if end > latest { return .inTheFuture }
        if let seconds, seconds < minimumSeconds { return .tooShort }
        return nil
    }
}

/// Every string the time rows show. The three messages keep the Past entry
/// sheet's keys, so their translations carry over.
public enum TimeSpanCopy {
    public static let start = LoggingL10n.text("Start")
    public static let end = LoggingL10n.text("End")
    public static let length = LoggingL10n.text("Length")
    public static let running = LoggingL10n.text("Running")
    public static let addEnd = LoggingL10n.text("Add end time")
    public static let removeEnd = LoggingL10n.text("Remove end time")
    public static let endBeforeStart = LoggingL10n.text("End must be after start.")
    public static let inTheFuture = LoggingL10n.text("Time can’t be in the future.")
    public static let breastTooShort = LoggingL10n.text("A breast feed must be at least 30 seconds.")

    public static func message(_ problem: TimeSpan.Problem) -> String {
        switch problem {
        case .endBeforeStart: endBeforeStart
        case .inTheFuture: inTheFuture
        case .tooShort: breastTooShort
        }
    }
}
