import Foundation
import SwiftData
import BabyCore

public extension BabyStore {
    /// Running timers, most recently started first. The dial shows the first;
    /// any others appear as live rows whose timer can be edited or stopped.
    func activeTimers(for babyID: UUID, in realm: RealmID? = nil) throws -> [ActiveTimer] {
        let realmKey = (realm ?? activeRealm).encodedKey
        var descriptor = FetchDescriptor<ActiveTimer>(
            predicate: #Predicate { $0.babyID == babyID && $0.realmKey == realmKey }
        )
        descriptor.sortBy = [SortDescriptor(\.startedAt, order: .reverse)]
        return try context.fetch(descriptor)
    }

    /// Persists the id of the Live Activity rendering this timer, so the
    /// activity can be ended later from whichever process stops the timer.
    /// Presentation-only: which Live Activity is rendering this timer. It is
    /// local state with no protocol projection, so it creates no mutation —
    /// but it still goes through the write boundary, because it is a write to
    /// a realm-scoped row that a migration or import must not race.
    func setLiveActivityID(_ id: String?, on timer: ActiveTimer, expectedWriteContext: RealmWriteContext? = nil) throws {
        let expected = try expectedWriteContext ?? captureWriteContext()
        // Presentation-only and local: which Live Activity is rendering a
        // timer is not family data, so a read-only caregiver still records it.
        try withRealmWrite(expected: expected, allowsReadOnly: true) {
            timer.liveActivityID = id
        }
    }

    /// Looks a running timer up by its own identifier within the active realm.
    func activeTimer(id: UUID) throws -> ActiveTimer? {
        let realmKey = activeRealm.encodedKey
        return try context.fetch(
            FetchDescriptor<ActiveTimer>(predicate: #Predicate { $0.id == id && $0.realmKey == realmKey })
        ).first
    }

    func activeTimer(kind: TimerKind, for babyID: UUID) throws -> ActiveTimer? {
        let raw = kind.rawValue
        let realmKey = activeRealm.encodedKey
        let descriptor = FetchDescriptor<ActiveTimer>(
            predicate: #Predicate { $0.babyID == babyID && $0.kindRaw == raw && $0.realmKey == realmKey }
        )
        return try context.fetch(descriptor).first
    }
}

/// Why a correction to a running timer was refused.
public enum TimerEditError: Error, Equatable {
    /// The timer is no longer running here — stopped on this phone, or on
    /// the other one and synced.
    case timerEnded
    /// The start is after now, or further back than the week a record may
    /// span. The editor never offers either; the store still refuses them.
    case startInFuture
    /// The start is after a side that began later than the timer did.
    case startAfterCurrentSide
    /// The start is after a bottle that began later than the timer did.
    case startAfterCurrentBottle
    /// Finished parts exceed the time before the current side or bottle began
    /// (or before now, when neither runs).
    case partsExceedElapsed
    /// A part is not one of this timer's finished parts, or is named with a
    /// different kind than it has.
    case unknownComponent
    case nothingChanged
}

/// A correction to a running timer, checked against the rules the server
/// applies (`editTimer`, `Server/internal/records/timers.go`) and resolved
/// into the timer it produces.
///
/// A value, not a store method, so an editor can ask "would this save?" of
/// its own copy with exactly the answer the store will give. Where the server
/// judges against the moment it accepts the edit, this judges against `now`,
/// which is earlier — so what passes here passes there.
public struct RunningTimerEdit: Sendable, Equatable {
    /// What goes on the wire: the new start, or nil when it is unchanged…
    public let changedStart: Date?
    /// …and only the parts whose values changed, in the timer's order.
    public let changedComponents: [FeedComponent]

    public let startedAt: Date
    /// Nil when no side runs.
    public let currentSideStartedAt: Date?
    /// Nil when no bottle segment runs.
    public let currentBottleStartedAt: Date?
    public let completedComponents: [FeedComponent]

    /// A week: the longest the server lets a completed record span, and its
    /// cap on a timer's finished breast time (`max_side_total_seconds`).
    static let window: TimeInterval = 7 * 24 * 3600

    public init(
        startedAt: Date,
        currentSideStartedAt: Date?,
        currentBottleStartedAt: Date? = nil,
        completedComponents: [FeedComponent],
        newStart: Date?,
        changedComponents: [FeedComponent],
        now: Date
    ) throws {
        var components = completedComponents
        for change in changedComponents {
            guard let index = components.firstIndex(where: { $0.id == change.id }),
                  Self.sameKind(components[index], change) else { throw TimerEditError.unknownComponent }
            components[index] = Self.preservingBounds(change, from: components[index])
        }
        let changed = components.indices.filter { components[$0] != completedComponents[$0] }.map { components[$0] }
        // Compared as the wire spells them: the server stores microseconds,
        // so a start "moved" by less than the wire can say is not a change.
        let movedStart = newStart.flatMap {
            MutationFactory.wireTime($0) == MutationFactory.wireTime(startedAt) ? nil : $0
        }
        guard movedStart != nil || !changed.isEmpty else { throw TimerEditError.nothingChanged }

        var start = startedAt
        var sideStart = currentSideStartedAt
        var bottleStart = currentBottleStartedAt
        if let movedStart {
            guard movedStart <= now, movedStart >= now.addingTimeInterval(-Self.window) else {
                throw TimerEditError.startInFuture
            }
            if let running = sideStart {
                if Self.tied(running, startedAt) {
                    // The side has run since the timer began, so it began
                    // when the timer did: moving one moves the other.
                    sideStart = movedStart
                } else if movedStart > running {
                    throw TimerEditError.startAfterCurrentSide
                }
            }
            if let running = bottleStart {
                if Self.tied(running, startedAt) {
                    bottleStart = movedStart
                } else if movedStart > running {
                    throw TimerEditError.startAfterCurrentBottle
                }
            }
            start = movedStart
        }
        let intervals = components.compactMap { component -> (Date, Date)? in
            guard let componentStart = component.startedAt, let componentEnd = component.endedAt else { return nil }
            return (componentStart, componentEnd)
        }
        if components.contains(where: { component in
            guard let componentStart = component.startedAt, let componentEnd = component.endedAt else { return false }
            return componentStart < start || componentEnd > now || componentEnd <= componentStart
        }) { throw TimerEditError.partsExceedElapsed }
        let ordered = intervals.sorted { $0.0 < $1.0 }
        for index in ordered.indices.dropFirst() where ordered[index].0 < ordered[index - 1].1 {
            throw TimerEditError.partsExceedElapsed
        }
        for component in components {
            guard let componentStart = component.startedAt, let componentEnd = component.endedAt else { continue }
            let seconds = Int(componentEnd.timeIntervalSince(componentStart))
            if let breastSeconds = component.durationSeconds, breastSeconds != seconds {
                throw TimerEditError.partsExceedElapsed
            }
            if let bottleSeconds = component.bottleSeconds, bottleSeconds != seconds {
                throw TimerEditError.partsExceedElapsed
            }
        }
        let breast = TimeInterval(components.compactMap(\.durationSeconds).reduce(0, +))
        let finished = breast + TimeInterval(components.compactMap(\.bottleSeconds).reduce(0, +))
        let activeStart = [sideStart, bottleStart].compactMap { $0 }.min() ?? now
        guard finished <= activeStart.timeIntervalSince(start), breast <= Self.window else {
            throw TimerEditError.partsExceedElapsed
        }

        self.changedStart = movedStart
        self.changedComponents = changed
        self.startedAt = start
        self.currentSideStartedAt = sideStart
        self.currentBottleStartedAt = bottleStart
        self.completedComponents = components
    }

    /// The latest start an edit may choose: now, or the start of an active
    /// side or bottle that began later. A step tied to the timer start moves
    /// with it, so it does not limit the picker.
    public static func latestStart(
        startedAt: Date,
        currentSideStartedAt: Date?,
        currentBottleStartedAt: Date? = nil,
        now: Date
    ) -> Date {
        let untiedStarts = [currentSideStartedAt, currentBottleStartedAt]
            .compactMap { $0 }
            .filter { !tied($0, startedAt) }
        return max(startedAt, untiedStarts.min() ?? now)
    }

    /// Whether the running side began with the timer, and so moves with its
    /// start. False when no side runs.
    public static func sideMovesWithStart(startedAt: Date, currentSideStartedAt: Date?) -> Bool {
        currentSideStartedAt.map { tied($0, startedAt) } ?? false
    }

    /// Compared as the wire spells them (microseconds), as the server does.
    private static func tied(_ sideStart: Date, _ start: Date) -> Bool {
        MutationFactory.wireTime(sideStart) == MutationFactory.wireTime(start)
    }

    private static func sameKind(_ a: FeedComponent, _ b: FeedComponent) -> Bool {
        switch (a, b) {
        case (.breast, .breast), (.bottle, .bottle), (.solids, .solids): true
        default: false
        }
    }

    /// The running editor changes a step's duration or bottle details, not
    /// its observed finish instant. When duration changes, move its start so
    /// the v4 bounds still describe precisely that many active seconds.
    private static func preservingBounds(_ change: FeedComponent, from old: FeedComponent) -> FeedComponent {
        switch change {
        case let .breast(id, side, seconds, suppliedStart, suppliedEnd):
            let end = suppliedEnd ?? old.endedAt
            let start: Date?
            if let suppliedStart { start = suppliedStart }
            else if seconds != old.durationSeconds, let end { start = end.addingTimeInterval(TimeInterval(-seconds)) }
            else { start = old.startedAt }
            return .breast(id: id, side: side, seconds: seconds, startedAt: start, endedAt: end)
        case let .bottle(id, volume, contents, suppliedSeconds, suppliedStart, suppliedEnd):
            let seconds = suppliedSeconds ?? old.bottleSeconds
            let end = suppliedEnd ?? old.endedAt
            let start: Date?
            if let suppliedStart { start = suppliedStart }
            else if seconds != old.bottleSeconds, let end, let seconds {
                start = end.addingTimeInterval(TimeInterval(-seconds))
            } else { start = old.startedAt }
            return .bottle(id: id, volumeML: volume, contents: contents, seconds: seconds,
                           startedAt: start, endedAt: end)
        case .solids: return change
        }
    }
}

public extension BabyStore {
    /// When the session a running timer joined last ended — the end of the
    /// part before it — or nil when it joined none. The timer's start can't
    /// move before it (`editRunningTimer`).
    func previousPartEnd(of timer: ActiveTimer) throws -> Date? {
        guard let session = timer.sessionID else { return nil }
        return timer.kind == .feed
            ? Self.projectSessions(try sessionFeeds(for: timer.babyID)).first { $0.id == session }?.endedAt
            : Self.projectSleepSessions(try sessionSleeps(for: timer.babyID)).first { $0.id == session }?.endedAt
    }

    /// The time asleep in the earlier parts of the sleep a running sleep
    /// continues — what its clock adds to its own — or 0 for a feed, or for a
    /// sleep that continues none.
    func earlierAsleepSeconds(of timer: ActiveTimer) throws -> Int {
        guard timer.kind == .sleep, let session = timer.sessionID else { return 0 }
        return Self.projectSleepSessions(try sessionSleeps(for: timer.babyID))
            .first { $0.id == session }?.asleepSeconds ?? 0
    }

    /// Corrects a running timer's start and/or its finished parts.
    ///
    /// `startedAt` nil leaves the start alone; `components` names parts by
    /// id, and a part named with its current value is no change. The timer is
    /// re-read inside the write boundary — the other phone may have stopped
    /// it since the editor opened — and the rules are judged against that.
    /// In a shared realm the correction goes out as a patch through the
    /// timer reducer; in the local realm it is only written here.
    ///
    /// The Live Activity is not pushed from here: `LiveActivityController.
    /// update` is async, and, as with a side switch, the caller pushes it
    /// once this returns.
    func editRunningTimer(
        id: UUID,
        startedAt: Date?,
        components: [FeedComponent],
        expectedWriteContext: RealmWriteContext? = nil
    ) throws {
        let expected = try expectedWriteContext ?? captureWriteContext()
        try withRealmWrite(expected: expected) {
            guard let timer = try self.activeTimer(id: id) else { throw TimerEditError.timerEnded }
            if let newStart = startedAt, let previousEnd = try self.previousPartEnd(of: timer),
               newStart < previousEnd {
                throw PartError.startBeforePreviousPart
            }
            let edit = try RunningTimerEdit(
                startedAt: timer.startedAt,
                currentSideStartedAt: timer.currentSide == nil ? nil : timer.currentSideStartedAt,
                currentBottleStartedAt: timer.currentBottleStartedAt,
                completedComponents: timer.completedComponents,
                newStart: startedAt,
                changedComponents: components,
                now: self.now
            )
            timer.startedAt = edit.startedAt
            if let sideStart = edit.currentSideStartedAt {
                timer.currentSideStartedAt = sideStart
            } else if edit.changedStart != nil {
                // No side runs, so the column is the timer's start — what
                // remote apply writes (`RemoteTimerState.write`) — and it
                // follows a moved start rather than keeping a stale side's.
                timer.currentSideStartedAt = edit.startedAt
            }
            timer.currentBottleStartedAt = edit.currentBottleStartedAt
            timer.completedComponents = edit.completedComponents
            try self.reduceTimerIntentLocked(.edit(
                timerID: timer.id,
                babyID: timer.babyID,
                startedAt: edit.changedStart,
                components: edit.changedComponents,
                sessionID: timer.sessionID
            ))
        }
    }
}
