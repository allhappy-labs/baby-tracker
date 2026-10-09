import BabyCore
import BabyProtocol
import Foundation
import SwiftData

/// What remote apply did to a running timer, for the layer that owns Live
/// Activities to act on once a whole sync has landed.
public enum RemoteTimerTransition: Sendable, Equatable {
    case appeared(timerID: UUID)
    case changed(timerID: UUID)
    /// The id is captured before the row is deleted: afterwards there is
    /// nothing left to read it from.
    case ended(timerID: UUID, liveActivityID: String?)
}

public extension BabyStore {
    /// Hands over and forgets every timer transition since the last call.
    func drainRemoteTimerTransitions() -> [RemoteTimerTransition] {
        defer { remoteTimerTransitions = [] }
        return remoteTimerTransitions
    }
}

/// Applying the server's timers.
///
/// The server keeps one row per timer (`active_timers`) and journals every
/// change as the whole canonical timer in its one shape (payload version 5):
/// `baby_id`, `timer_kind`, `started_at`, `time_zone_id`, `current_side`,
/// `current_side_started_at`, `current_bottle_started_at`,
/// `completed_components` and `session_id` (`TimerStartPayload`).
/// A stop tombstones the row — payload null — and journals the finished feed
/// or sleep as a separate entity in the same change set (`stopTimer`,
/// `Server/internal/records/timers.go`). So a running timer here is a pure
/// function of its base, and a finished one is simply gone: its record
/// arrives through the ordinary feed/sleep path.
extension BabyStore {
    /// Brings the local `ActiveTimer` in line with the server's timer.
    ///
    /// Local intents still owed win outright. The timer on screen already
    /// reflects them — a switch, a bottle, a stop that deleted it — and the
    /// server copy predates them, so replaying it would move the side back or
    /// resurrect a stopped timer. Once they are settled the next page carries
    /// the server's result and this runs again. A tombstone is the exception:
    /// the timer has ended on the server, every owed command against it will
    /// come back `timer_ended`, and there is nothing left to show.
    func rematerializeTimer(_ timerID: UUID, basePayload: Data) throws {
        let existing = try activeTimer(id: timerID)
        let object = try? JSONSerialization.jsonObject(with: basePayload, options: .fragmentsAllowed)
        guard let fields = object as? [String: Any] else {
            if let existing { endTimerLocally(existing) }
            return
        }
        let owed = try liveOutboxRows().contains { $0.entityID == timerID && !$0.deliveryState.isTerminal }
        guard !owed, let state = try RemoteTimerState(fields: fields) else { return }

        if let existing {
            if state.write(to: existing) {
                remoteTimerTransitions.append(.changed(timerID: timerID))
            }
            return
        }
        let timer = ActiveTimer(
            id: timerID, babyID: state.babyID, kind: state.kind, startedAt: state.startedAt,
            timeZoneID: state.timeZoneID, realm: activeRealm
        )
        _ = state.write(to: timer)
        context.insert(timer)
        remoteTimerTransitions.append(.appeared(timerID: timerID))
    }

    /// Runs one apply, forgetting the transitions it recorded if it fails:
    /// the write boundary rolls the rows back, and announcing a timer that
    /// is not there would start a Live Activity for nothing.
    func discardingTimerTransitionsOnFailure<T>(_ body: () throws -> T) throws -> T {
        let mark = remoteTimerTransitions.count
        do {
            return try body()
        } catch {
            remoteTimerTransitions.removeSubrange(mark...)
            throw error
        }
    }

    /// Removes a timer the server says is not running, remembering which Live
    /// Activity was showing it.
    func endTimerLocally(_ timer: ActiveTimer) {
        remoteTimerTransitions.append(.ended(timerID: timer.id, liveActivityID: timer.liveActivityID))
        context.delete(timer)
    }

    /// Settles what a refused timer command leaves behind on this phone.
    ///
    /// - A start the server did not take (`superseded` because another
    ///   caregiver's timer for the same baby and kind was already live, or
    ///   `rejected`) never existed there, so the local timer goes. The
    ///   family's timer arrives through the journal and takes its place. A
    ///   start superseded by the very same timer — a retry the server already
    ///   holds — is kept.
    /// - A stop the server did not take (`timer_ended`: the other caregiver
    ///   stopped first) never created the record this phone derived from it.
    ///   That record is withdrawn; the record of the stop that won arrives
    ///   through the journal, so the feed or sleep shows once, not twice.
    ///   Whatever this phone joined to the withdrawn record follows it to the
    ///   winner's session (`followWinningCompletion`).
    func settleRefusedTimerCommand(_ row: SyncOutboxEntry, outcome: Outcome?) throws {
        guard row.entityKind == .timer else { return }
        switch row.operation {
        case .timerStart:
            if outcome?.canonicalEntity?.meta.id == row.entityID { return }
            if let timer = try activeTimer(id: row.entityID) { endTimerLocally(timer) }
        case .timerStop:
            guard case .object(let fields) = try row.decodedMutation().payload,
                  case .string(let raw) = fields["completed_record_id"],
                  let recordID = UUID(uuidString: raw) else { return }
            guard let withdrawn = try withdrawUnsentCompletion(recordID),
                  let winner = outcome?.relatedEntities.first(where: { $0.entityKind == withdrawn.kind.rawValue })
            else { return }
            try followWinningCompletion(of: withdrawn, to: winner)
        default:
            return
        }
    }

    /// A part started — or a record logged — inside the break after this
    /// phone's completion of a timer joined that completion's session, under
    /// its record ID. When another caregiver's stop of the same timer won,
    /// that record is withdrawn and the ID names nothing: the joined part
    /// would end as a session of its own on every phone. So every link to the
    /// withdrawn record moves to the session of the record that won.
    ///
    /// The server answers a losing stop with the winning record in
    /// `related_entities` (`endedTimerOutcome`, timers.go), and replays that
    /// receipt whole on a retry, so its session is known here even when the
    /// record itself has not reached this phone yet.
    ///
    /// A running timer gets a timer edit with the new `session_id` — folded
    /// into its start if that was never sent. A stored record gets an
    /// ordinary update, as combining sessions does.
    private func followWinningCompletion(of withdrawn: WithdrawnCompletion, to winner: CanonicalEntity) throws {
        guard winner.meta.tombstonedAt == nil, case .object(let fields)? = winner.payload else { return }
        let linkKey = withdrawn.kind == .sleep ? "sleep_session_id" : "feeding_session_id"
        var session = winner.meta.id
        if case .string(let raw) = fields[linkKey], let linked = UUID(uuidString: raw) { session = linked }
        let stale = withdrawn.id
        guard session != stale else { return }
        let babyID = withdrawn.babyID
        let realmKey = activeRealm.encodedKey

        if let timer = try activeTimer(kind: withdrawn.kind == .sleep ? .sleep : .feed, for: babyID),
           timer.sessionID == stale {
            timer.sessionID = session
            try reduceTimerIntentLocked(.edit(timerID: timer.id, babyID: babyID,
                                              startedAt: nil, components: [], sessionID: session))
        }
        if withdrawn.kind == .sleep {
            for sleep in try context.fetch(FetchDescriptor<SleepEntry>(predicate: #Predicate {
                $0.babyID == babyID && $0.realmKey == realmKey && !$0.isTombstoned && $0.sleepSessionID == stale
            })) {
                sleep.sleepSessionID = session
                sleep.lastModifiedAt = now
                try recordLocked(.update, on: sleep)
            }
        } else {
            for feed in try context.fetch(FetchDescriptor<FeedEntry>(predicate: #Predicate {
                $0.babyID == babyID && $0.realmKey == realmKey && !$0.isTombstoned && $0.feedingSessionID == stale
            })) {
                feed.feedingSessionID = session
                feed.lastModifiedAt = now
                try recordLocked(.update, on: feed)
            }
        }
    }

    /// The server may have seen a shorter canonical timer than this phone did.
    /// If its accepted cleanup stop produced no feed, withdraw this phone's
    /// provisional completion; a later canonical feed change can still restore
    /// it if one actually exists.
    func settleAppliedShortFeedCleanup(_ row: SyncOutboxEntry, outcome: Outcome) throws {
        guard row.entityKind == .timer, row.operation == .timerStop,
              case .object(let fields) = try row.decodedMutation().payload,
              fields["cleanup_short_breast_segments"] == .bool(true),
              case .string(let raw) = fields["completed_record_id"],
              let recordID = UUID(uuidString: raw),
              !outcome.relatedEntities.contains(where: { $0.meta.id == recordID }) else { return }
        try withdrawUnsentCompletion(recordID)
    }

    /// This phone finished a different timer whose start lost the server race.
    /// Its stop cannot be sent, but its completed record is still an actual
    /// session. Send that record by itself under the original record ID.
    func preserveCompletionOfSupersededStart(_ stop: SyncOutboxEntry) throws {
        guard case .object(let fields) = try stop.decodedMutation().payload,
              case .string(let raw) = fields["completed_record_id"],
              let recordID = UUID(uuidString: raw) else { return }
        let realmKey = activeRealm.encodedKey
        guard !(try liveOutboxRows().contains { $0.entityID == recordID && $0.operation == .create }) else { return }
        if let sleep = try context.fetch(FetchDescriptor<SleepEntry>(
            predicate: #Predicate { $0.id == recordID && $0.realmKey == realmKey }
        )).first {
            sleep.isTombstoned = false
            try appendEntityMutation(.create, on: sleep)
        } else if let feed = try context.fetch(FetchDescriptor<FeedEntry>(
            predicate: #Predicate { $0.id == recordID && $0.realmKey == realmKey }
        )).first {
            feed.isTombstoned = false
            try appendEntityMutation(.create, on: feed)
        }
    }

    /// A local completion `withdrawUnsentCompletion` took back.
    struct WithdrawnCompletion {
        let id: UUID
        let babyID: UUID
        let kind: WireEntityKind
    }

    /// A completion the server never created: no base exists for it. One it
    /// did create — a retried stop that in fact won — is left alone. Returns
    /// the record it withdrew, if any.
    @discardableResult
    private func withdrawUnsentCompletion(_ recordID: UUID) throws -> WithdrawnCompletion? {
        let realmKey = activeRealm.encodedKey
        let known = try [WireEntityKind.feed, .sleep].contains { kind in
            let storageKey = "\(realmKey)|\(kind.rawValue)|\(recordID.uuidString)"
            return try !context.fetch(
                FetchDescriptor<CanonicalEntityBase>(predicate: #Predicate { $0.storageKey == storageKey })
            ).isEmpty
        }
        guard !known else { return nil }
        var withdrawn: WithdrawnCompletion?
        if let sleep = try context.fetch(
            FetchDescriptor<SleepEntry>(predicate: #Predicate { $0.id == recordID && $0.realmKey == realmKey })
        ).first {
            sleep.isTombstoned = true
            withdrawn = WithdrawnCompletion(id: recordID, babyID: sleep.babyID, kind: .sleep)
        }
        if let feed = try context.fetch(
            FetchDescriptor<FeedEntry>(predicate: #Predicate { $0.id == recordID && $0.realmKey == realmKey })
        ).first {
            feed.isTombstoned = true
            withdrawn = WithdrawnCompletion(id: recordID, babyID: feed.babyID, kind: .feed)
        }
        return withdrawn
    }
}

/// One canonical timer payload, read.
struct RemoteTimerState {
    let babyID: UUID
    let kind: TimerKind
    let startedAt: Date
    let timeZoneID: String
    let currentSide: BreastSide?
    let currentSideStartedAt: Date?
    let currentBottleStartedAt: Date?
    let components: [FeedComponent]
    /// Nil when the timer belongs to no session yet.
    let sessionID: UUID?

    /// Nil when a column a timer cannot exist without is missing, which is
    /// also how a timer in an older shape (scalar `completed_sides`, no
    /// `session_id`) reads: the server sends only the current one.
    init?(fields: [String: Any]) throws {
        guard let babyID = (fields["baby_id"] as? String).flatMap(UUID.init(uuidString:)),
              let kind = (fields["timer_kind"] as? String).flatMap(TimerKind.init(rawValue:)),
              let started = fields["started_at"] as? String,
              let timeZoneID = fields["time_zone_id"] as? String,
              fields["completed_components"] != nil,
              let session = fields["session_id"] else { return nil }
        self.babyID = babyID
        self.kind = kind
        self.startedAt = try BabyStore.wireDate(started)
        self.timeZoneID = timeZoneID
        currentSide = (fields["current_side"] as? String).flatMap(BreastSide.init(rawValue:))
        currentSideStartedAt = try (fields["current_side_started_at"] as? String).map(BabyStore.wireDate)
        currentBottleStartedAt = try (fields["current_bottle_started_at"] as? String).map(BabyStore.wireDate)
        sessionID = (session as? String).flatMap(UUID.init(uuidString:))
        // The server's own part IDs, kept exactly.
        components = try WireFeedComponent.decodeComponents("completed_components", of: MutationFactory.jsonValue(fields))
            .map(BabyStore.localComponent)
    }

    /// Writes the state onto a timer, touching only columns that differ, and
    /// says whether anything did. `liveActivityID` has no wire projection and
    /// is never touched.
    func write(to timer: ActiveTimer) -> Bool {
        var changed = false
        func set<Value: Equatable>(_ keyPath: ReferenceWritableKeyPath<ActiveTimer, Value>, _ value: Value) {
            guard timer[keyPath: keyPath] != value else { return }
            timer[keyPath: keyPath] = value
            changed = true
        }
        // The wire keeps microseconds and this phone's own clock does not
        // round the same way, so an echo of this phone's own timer differs
        // by a fraction of a millisecond. That is not a change.
        func setDate(_ keyPath: ReferenceWritableKeyPath<ActiveTimer, Date>, _ value: Date) {
            guard abs(timer[keyPath: keyPath].timeIntervalSince(value)) >= 0.001 else { return }
            timer[keyPath: keyPath] = value
            changed = true
        }
        set(\.babyID, babyID)
        set(\.kindRaw, kind.rawValue)
        setDate(\.startedAt, startedAt)
        set(\.timeZoneID, timeZoneID)
        set(\.currentSideRaw, currentSide?.rawValue)
        setDate(\.currentSideStartedAt, currentSideStartedAt ?? startedAt)
        set(\.currentBottleStartedAt, currentBottleStartedAt)
        set(\.completedComponents, components)
        set(\.sessionID, sessionID)
        return changed
    }
}
