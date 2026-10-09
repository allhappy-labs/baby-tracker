import Foundation
import SwiftData
import BabyCore

public enum FeedPart: Equatable, Sendable { case breast(BreastSide), bottle(BottleContents) }
public enum PartError: Error, Equatable {
    case tooShort, startBeforePreviousPart, missingFeed, missingComponent, notASingleBottle, invalidVolume
    /// An edited part's start moved past the start of the part after it in
    /// its session, which would reorder the session (ruling H-R15).
    case startAfterNextPart
}

/// What `startPartStoppingRunning` did: the part now running, and the timed
/// bottle it had to stop first, if any — saved at the usual amount, which
/// nobody was asked to confirm.
@MainActor
public struct StartedPart {
    public let timer: ActiveTimer
    public let stoppedBottle: FeedEntry?
}

public extension BabyStore {
    /// One breast side or one bottle. Starting another part while one runs
    /// stops it first; the new part joins whatever session the rules say.
    @discardableResult
    func startPart(_ part: FeedPart, for babyID: UUID) throws -> ActiveTimer {
        try startPartStoppingRunning(part, for: babyID).timer
    }

    /// `startPart`, reporting a running timed bottle its safety net stopped.
    /// A caller outside the app — where a bottle's amount cannot be asked for
    /// — uses this to leave the same "Set amount" prompt a Lock Screen Stop
    /// leaves.
    ///
    /// `expectedWriteContext`: the realm generation the caller checked; every
    /// write here is refused if the realm has moved since.
    func startPartStoppingRunning(_ part: FeedPart, for babyID: UUID,
                                  expectedWriteContext: RealmWriteContext? = nil) throws -> StartedPart {
        var stoppedBottle: FeedEntry?
        if let running = try activeTimer(kind: .feed, for: babyID) {
            switch part {
            case .breast(let side) where running.currentSide == side && running.currentBottleStartedAt == nil:
                return StartedPart(timer: running, stoppedBottle: nil)
            case .bottle where running.currentBottleStartedAt != nil:
                return StartedPart(timer: running, stoppedBottle: nil)
            default:
                if let bottleSeconds = running.currentBottleSeconds(now: now), bottleSeconds <= 0 {
                    // A bottle tapped and abandoned inside the same second —
                    // `finishBottle` would throw `emptyDuration` on it, same
                    // as a breast tap under 30 s is discarded at stop. Clear
                    // it so `stopFeed` sees nothing left on the timer.
                    try discardZeroLengthBottle(on: running, expectedWriteContext: expectedWriteContext)
                    _ = try stopFeed(running, expectedWriteContext: expectedWriteContext)
                } else if running.currentBottleStartedAt != nil {
                    stoppedBottle = try stopBottle(running, volumeML: try LogDefaults(store: self).mostUsedBottleVolumeML(for: babyID),
                                                   expectedWriteContext: expectedWriteContext)
                } else {
                    _ = try stopFeed(running, expectedWriteContext: expectedWriteContext)
                }
            }
        }
        let expected = try expectedWriteContext ?? captureWriteContext()
        let timer = try withRealmWrite(expected: expected) {
            let startedAt = self.now
            let timer = ActiveTimer(babyID: babyID, kind: .feed, startedAt: startedAt,
                                    timeZoneID: TimeZone.current.identifier, realm: self.activeRealm)
            timer.sessionID = try self.continuedSessionID(.feed, for: babyID, startingAt: startedAt)
            var side: BreastSide?
            switch part {
            case .breast(let breastSide):
                side = breastSide
                timer.currentSide = breastSide
                timer.currentSideStartedAt = startedAt
            case .bottle(let contents):
                timer.currentBottleStartedAt = startedAt
                timer.bottleContents = contents
            }
            self.context.insert(timer)
            try self.reduceTimerIntentLocked(.start(timerID: timer.id, babyID: babyID, kind: .feed, side: side))
            return timer
        }
        return StartedPart(timer: timer, stoppedBottle: stoppedBottle)
    }

    @discardableResult
    func stopBottle(_ timer: ActiveTimer, volumeML: Double, expectedWriteContext: RealmWriteContext? = nil) throws -> FeedEntry? {
        // A bottle this device didn't start carries no contents (`startBottle`
        // sets none); the best guess is what was logged last, not the generic
        // fallback — the same reasoning `LogDefaults` already applies to a
        // fresh Bottle sheet.
        let contents = try timer.bottleContents ?? LogDefaults(store: self).lastBottleContents(for: timer.babyID)
        try finishBottle(volumeML: volumeML, contents: contents, on: timer, expectedWriteContext: expectedWriteContext)
        return try stopFeed(timer, expectedWriteContext: expectedWriteContext)
    }

    /// Clears a running bottle without finishing it, leaving `stopFeed` to
    /// discard the timer exactly as it would an accidental breast tap.
    /// `ActiveTimer` is scratch state, not change-logged, so this writes
    /// directly without a mutation of its own; `stopFeed`'s own `.stop`
    /// intent closes the timer out.
    private func discardZeroLengthBottle(on timer: ActiveTimer, expectedWriteContext: RealmWriteContext? = nil) throws {
        let expected = try expectedWriteContext ?? captureWriteContext()
        try withRealmWrite(expected: expected) {
            timer.currentBottleStartedAt = nil
            timer.bottleContents = nil
        }
    }

    /// Breaks the part's link to its session. Stopping it afterward starts a
    /// feed of its own, even if the gap would otherwise have joined it.
    /// `startedAt: nil` leaves the timer's start untouched — only the
    /// session link changes. `components: []` because the wire edit patches
    /// finished parts by id (`TimerIntentReducer.appendTimerCommand`,
    /// `.edit` case): an empty array means none changed, where re-sending
    /// every completed component could overwrite a concurrent caregiver's
    /// edit to one of them.
    func detachFromSession(_ timer: ActiveTimer) throws {
        let expected = try captureWriteContext()
        try withRealmWrite(expected: expected) {
            guard timer.sessionID != nil else { return }
            timer.sessionID = nil
            try self.reduceTimerIntentLocked(.edit(timerID: timer.id, babyID: timer.babyID,
                                                   startedAt: nil, components: [], sessionID: nil))
        }
    }

    /// One timed breast component, entered after the fact, joined like any
    /// other part. Durations under 30 s are not worth a record.
    @discardableResult
    func logBreast(side: BreastSide, from start: Date, to end: Date, for babyID: UUID) throws -> FeedEntry {
        let seconds = try Self.breastSeconds(from: start, to: end)
        let expected = try captureWriteContext()
        return try withRealmWrite(expected: expected) {
            let entry = self.breastEntry(side: side, seconds: seconds, from: start, to: end, for: babyID)
            entry.feedingSessionID = try self.continuedSessionID(.feed, for: babyID, startingAt: start)
            self.context.insert(entry)
            try self.recordLocked(.create, on: entry)
            return entry
        }
    }

    /// Rewrites the single bottle component's volume on a feed logged earlier.
    /// Refuses a feed with no bottle component or more than one — which one
    /// would this volume belong to? — and a volume that is not positive.
    func setBottleVolume(_ volumeML: Double, onFeed feedID: UUID) throws {
        guard volumeML > 0 else { throw PartError.invalidVolume }
        let realmKey = activeRealm.encodedKey
        guard let feed = try context.fetch(FetchDescriptor<FeedEntry>(predicate: #Predicate {
            $0.id == feedID && $0.realmKey == realmKey && !$0.isTombstoned
        })).first else { throw PartError.missingFeed }
        guard feed.components.filter({ $0.bottleContents != nil }).count == 1 else {
            throw PartError.notASingleBottle
        }
        feed.components = feed.components.map { component in
            guard case let .bottle(id, _, contents, seconds, startedAt, endedAt) = component else { return component }
            return .bottle(id: id, volumeML: volumeML, contents: contents, seconds: seconds, startedAt: startedAt, endedAt: endedAt)
        }
        try update(feed)
    }
}

extension BabyStore {
    /// Durations under 30 s are not worth a record.
    static func breastSeconds(from start: Date, to end: Date) throws -> Int {
        let seconds = Int(end.timeIntervalSince(start))
        guard seconds >= 30 else { throw PartError.tooShort }
        return seconds
    }

    /// A completed breast part, not yet inserted or linked to a session —
    /// shared by `logBreast` and `addFeedPart`, which differ only in that.
    /// Built inside the caller's write boundary, so the realm it is stamped
    /// with is the one the write commits to.
    func breastEntry(side: BreastSide, seconds: Int, from start: Date, to end: Date, for babyID: UUID) -> FeedEntry {
        let entry = FeedEntry(babyID: babyID, kind: .breast, startedAt: start,
                              timeZoneID: TimeZone.current.identifier, createdBy: deviceID)
        entry.realmKey = activeRealm.encodedKey
        entry.components = [.breast(id: UUID(), side: side, seconds: seconds, startedAt: start, endedAt: end)]
        entry.endedAt = end
        return entry
    }
}
