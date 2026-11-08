import Foundation
import SwiftData
import BabyCore

public enum FeedingError: Error, Sendable, Equatable {
    case emptyFeed
}

public enum BottleTimerError: Error, Sendable, Equatable {
    case unfinishedBottle
    case noBottleRunning
    case emptyDuration
    case invalidVolume
    case timerChanged
}

public extension BabyStore {
    /// Pre-selects the side opposite the last completed breast feed.
    ///
    /// Equal start times use canonical UUID order so every family device and
    /// the server select the same completed breast feed.
    func suggestedSide(for babyID: UUID) throws -> BreastSide {
        let breast = FeedKind.breast.rawValue
        var descriptor = FetchDescriptor<FeedEntry>(
            predicate: #Predicate { $0.babyID == babyID && !$0.isTombstoned && $0.kindRaw == breast }
        )
        descriptor.sortBy = [SortDescriptor(\.startedAt, order: .forward)]

        guard let last = try context.fetch(descriptor).max(by: Self.feedOrder), let side = last.lastSideUsed else {
            return .left
        }
        return side.opposite
    }

    /// Finishes only the bottle segment. The feed remains active so another
    /// breast or bottle can follow, or Stop can finish the whole session.
    func finishBottle(volumeML: Double, contents: BottleContents, on timer: ActiveTimer,
                      expectedWriteContext: RealmWriteContext? = nil) throws {
        let expected = try expectedWriteContext ?? captureWriteContext()
        try withRealmWrite(expected: expected) {
            guard let startedAt = timer.currentBottleStartedAt else { throw BottleTimerError.noBottleRunning }
            guard volumeML > 0 else { throw BottleTimerError.invalidVolume }
            let finishedAt = self.now
            let seconds = Int(finishedAt.timeIntervalSince(startedAt))
            guard seconds > 0 else { throw BottleTimerError.emptyDuration }
            let componentID = UUID()
            timer.completedComponents.append(.bottle(id: componentID, volumeML: volumeML,
                                                       contents: contents, seconds: seconds,
                                                       startedAt: startedAt, endedAt: finishedAt))
            timer.currentBottleStartedAt = nil
            try self.reduceTimerIntentLocked(.finishBottle(
                timerID: timer.id, babyID: timer.babyID, componentID: componentID,
                finishedAt: finishedAt, volumeML: volumeML, contents: contents
            ))
        }
    }

    @discardableResult
    func stopFeed(_ timer: ActiveTimer, expectedWriteContext: RealmWriteContext? = nil) throws -> FeedEntry? {
        let expected = try expectedWriteContext ?? captureWriteContext()
        let timerID = timer.id
        let babyID = timer.babyID
        return try withRealmWrite(expected: expected) {
            guard timer.currentBottleStartedAt == nil else { throw BottleTimerError.unfinishedBottle }
            let stoppedAt = self.now
            let entry = FeedEntry(
                babyID: timer.babyID,
                kind: .breast,
                startedAt: timer.startedAt,
                timeZoneID: timer.timeZoneID,
                createdBy: self.deviceID
            )
            entry.realmKey = self.activeRealm.encodedKey
            var components = timer.completedComponents
            var finalBreastComponentID: UUID?
            if let side = timer.currentSide {
                let seconds = timer.currentSideSeconds(now: stoppedAt)
                if seconds > 0 {
                    let componentID = UUID()
                    finalBreastComponentID = componentID
                    components.append(.breast(id: componentID, side: side, seconds: seconds,
                                               startedAt: timer.currentSideStartedAt, endedAt: stoppedAt))
                }
            }
            components = Self.cleanedBreastComponents(components)
            entry.components = components
            entry.endedAt = stoppedAt
            entry.feedingSessionID = timer.sessionID

            // A feed made only of accidental taps has no record. The timer
            // still ends, including on the family server if its start was sent.
            guard !components.isEmpty else {
                self.context.delete(timer)
                try self.reduceTimerIntentLocked(.stop(
                    timerID: timerID,
                    babyID: babyID,
                    completedRecordID: entry.id,
                    componentID: finalBreastComponentID,
                    stoppedAt: stoppedAt,
                    cleanupShortBreastSegments: true
                ))
                return nil
            }
            if !components.contains(where: { $0.breastSide != nil }),
               components.contains(where: { $0.bottleContents != nil }) {
                entry.kind = .bottle
            }

            self.context.insert(entry)
            self.context.delete(timer)   // ActiveTimer is scratch state, not a record

            // The completion's protocol mutation comes from the reducer, not
            // from `recordLocked`: a wholly undispatched lifecycle compacts
            // into exactly one `create feed`, while an uncertain one sends
            // `timer_stop` and lets the server derive the record.
            try self.recordLocked(.create, on: entry, emitMutation: false)
            try self.reduceTimerIntentLocked(
                .stop(
                    timerID: timerID,
                    babyID: babyID,
                    completedRecordID: entry.id,
                    componentID: finalBreastComponentID,
                    stoppedAt: entry.endedAt ?? self.now,
                    cleanupShortBreastSegments: true
                ),
                completedRecord: entry
            )
            return entry
        }
    }

    /// At completion, forget accidental breast taps and combine the same side
    /// around a forgotten tap. Bottles remain separators and keep their IDs.
    private static func cleanedBreastComponents(_ components: [FeedComponent]) -> [FeedComponent] {
        var cleaned: [FeedComponent] = []
        for component in components {
            if let seconds = component.durationSeconds {
                guard seconds >= 30 else { continue }
                if let previous = cleaned.last, previous.breastSide == component.breastSide,
                   let previousSeconds = previous.durationSeconds, let side = component.breastSide {
                    // Preserve distinct timed intervals: combining across a
                    // pause would invent active time and lose exact bounds.
                    if previous.startedAt == nil && component.startedAt == nil {
                        cleaned[cleaned.count - 1] = .breast(
                            id: previous.id, side: side, seconds: previousSeconds + seconds
                        )
                        continue
                    }
                }
            }
            cleaned.append(component)
        }
        return cleaned
    }

    /// Bottles have no timer — volume and contents are known at log time.
    @discardableResult
    func logBottle(
        volumeML: Double,
        contents: BottleContents,
        for babyID: UUID,
        at date: Date? = nil,
        endedAt end: Date? = nil,
        note: String? = nil
    ) throws -> FeedEntry {
        let stamp = date ?? now
        let entry = bottleEntry(volumeML: volumeML, contents: contents, at: stamp, endedAt: end, for: babyID)
        entry.note = note
        entry.feedingSessionID = try continuedSessionID(.feed, for: babyID, startingAt: stamp)

        context.insert(entry)
        try record(.create, on: entry)
        return entry
    }

    /// Solids have no timer and no volume — they are a moment plus a
    /// description. `foods` is free text so it does not need a curated list.
    @discardableResult
    func logSolids(
        foods: [String],
        amount: SolidsAmount,
        for babyID: UUID,
        at date: Date? = nil,
        note: String? = nil
    ) throws -> FeedEntry {
        let entry = solidsEntry(foods: foods, amount: amount, at: date ?? now, for: babyID)
        entry.note = note

        context.insert(entry)
        try record(.create, on: entry)
        return entry
    }

    @discardableResult
    func startSleep(for babyID: UUID, expectedWriteContext: RealmWriteContext? = nil) throws -> ActiveTimer {
        if let running = try activeTimer(kind: .sleep, for: babyID) {
            return running
        }
        let expected = try expectedWriteContext ?? captureWriteContext()
        return try withRealmWrite(expected: expected) {
            let timer = ActiveTimer(
                babyID: babyID,
                kind: .sleep,
                startedAt: self.now,
                timeZoneID: TimeZone.current.identifier,
                realm: self.activeRealm
            )
            timer.sessionID = try self.continuedSessionID(.sleep, for: babyID, startingAt: timer.startedAt)
            self.context.insert(timer)
            try self.reduceTimerIntentLocked(
                .start(timerID: timer.id, babyID: babyID, kind: .sleep, side: nil)
            )
            return timer
        }
    }

    @discardableResult
    func stopSleep(_ timer: ActiveTimer, expectedWriteContext: RealmWriteContext? = nil) throws -> SleepEntry {
        let expected = try expectedWriteContext ?? captureWriteContext()
        let timerID = timer.id
        let babyID = timer.babyID
        return try withRealmWrite(expected: expected) {
            let entry = SleepEntry(
                babyID: timer.babyID,
                startedAt: timer.startedAt,
                timeZoneID: timer.timeZoneID,
                createdBy: self.deviceID
            )
            entry.realmKey = self.activeRealm.encodedKey
            entry.endedAt = self.now
            entry.sleepSessionID = timer.sessionID

            self.context.insert(entry)
            self.context.delete(timer)
            try self.recordLocked(.create, on: entry, emitMutation: false)
            try self.reduceTimerIntentLocked(
                .stop(
                    timerID: timerID,
                    babyID: babyID,
                    completedRecordID: entry.id,
                    componentID: nil,
                    stoppedAt: entry.endedAt ?? self.now,
                    cleanupShortBreastSegments: false
                ),
                completedRecord: entry
            )
            return entry
        }
    }
}

extension BabyStore {
    /// A bottle, not yet inserted or linked to a session — shared by
    /// `logBottle` and `addFeedPart`, which differ only in that.
    func solidsEntry(foods: [String], amount: SolidsAmount, at stamp: Date, for babyID: UUID) -> FeedEntry {
        let entry = FeedEntry(
            babyID: babyID,
            kind: .solids,
            startedAt: stamp,
            timeZoneID: TimeZone.current.identifier,
            createdBy: deviceID,
            realm: activeRealm
        )
        entry.components = [.solids(id: UUID(), foods: foods, amount: amount)]
        entry.endedAt = stamp
        return entry
    }

    func bottleEntry(volumeML: Double, contents: BottleContents, at stamp: Date, endedAt end: Date? = nil,
                     for babyID: UUID) -> FeedEntry {
        let entry = FeedEntry(
            babyID: babyID,
            kind: .bottle,
            startedAt: stamp,
            timeZoneID: TimeZone.current.identifier,
            createdBy: deviceID,
            realm: activeRealm
        )
        let seconds = end.map { Int($0.timeIntervalSince(stamp).rounded()) }.flatMap { $0 > 0 ? $0 : nil }
        entry.components = [.bottle(id: UUID(), volumeML: volumeML, contents: contents, seconds: seconds,
                                    startedAt: stamp, endedAt: seconds == nil ? nil : end)]
        entry.endedAt = seconds == nil ? stamp : end
        return entry
    }
}
