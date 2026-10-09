import BabyCore
import BabyProtocol
import Foundation
import SwiftData

public enum TimerIntent: Sendable, Equatable {
    case start(timerID: UUID, babyID: UUID, kind: TimerKind, side: BreastSide?)
    case finishBottle(timerID: UUID, babyID: UUID, componentID: UUID, finishedAt: Date, volumeML: Double, contents: BottleContents)
    case stop(timerID: UUID, babyID: UUID, completedRecordID: UUID, componentID: UUID?, stoppedAt: Date, cleanupShortBreastSegments: Bool)
    /// A correction to a running timer: a new start (nil when unchanged)
    /// and the finished parts whose values changed, under their own ids.
    /// `sessionID` carries the timer's current session link — v5 requires
    /// the key on every edit, not only on the start.
    case edit(timerID: UUID, babyID: UUID, startedAt: Date?, components: [FeedComponent], sessionID: UUID?)

    var timerID: UUID {
        switch self {
        case .start(let id, _, _, _), .finishBottle(let id, _, _, _, _, _),
             .stop(let id, _, _, _, _, _), .edit(let id, _, _, _, _): id
        }
    }
}

public enum TimerIntentResult: Sendable, Equatable {
    case folded(startMutationID: UUID)
    case compacted(completionMutationID: UUID)
    case discarded
    case dependencyBlocked(mutationID: UUID, dependsOn: UUID)
    case ready(mutationID: UUID)
    case superseded(mutationID: UUID, code: String)
}

public extension BabyStore {
    /// Opens a write boundary and reduces one timer command. Store methods that
    /// are already inside a boundary call `reduceTimerIntentLocked` instead —
    /// the App Group lock is not reentrant.
    @discardableResult
    func reduceTimerIntent(_ intent: TimerIntent) throws -> TimerIntentResult {
        let expected = try captureWriteContext()
        return try withRealmWrite(expected: expected) { try self.reduceTimerIntentLocked(intent) }
    }

    /// Applies one stable receipt for a `timer_start` and settles everything
    /// waiting on it.
    ///
    /// `already_applied` is only interpretable together with `original_status`;
    /// a receipt without one is refused rather than guessed at. Only `applied`
    /// releases dependents. `superseded`/`rejected` mean the server never held
    /// this timer, so the dependents become terminally superseded — never
    /// retargeted at whatever timer does exist under that entity ID.
    @discardableResult
    func resolveTimerStartReceipt(_ outcome: Outcome) throws -> TimerIntentResult {
        let original = try outcome.requiredOriginalStatus
        let expected = try captureWriteContext()
        return try withRealmWrite(expected: expected) {
            let start = try self.outboxEntry(outcome.mutationID)
            switch original {
            case .applied:
                start.deliveryState = .acknowledged
                return try self.settleDependents(of: start, code: nil)
            case .superseded:
                start.deliveryState = .superseded
                start.supersededCode = outcome.code ?? "timer_collision"
                return try self.settleDependents(of: start, code: "timer_collision")
            case .rejected:
                start.deliveryState = .superseded
                start.supersededCode = outcome.code ?? "timer_start_rejected"
                return try self.settleDependents(of: start, code: "timer_start_rejected")
            case .alreadyApplied:
                throw SyncPersistenceError.unstableTimerReceipt
            }
        }
    }
}

extension BabyStore {
    /// Decides what a timer command becomes, given how far its `timer_start`
    /// has got towards the server. Must run inside an open write boundary.
    ///
    /// The three cases are genuinely different intents, not an optimisation:
    ///
    /// - The start has never been sent, so it is still editable. A finished
    ///   bottle or a correction folds into its payload, and a stop collapses
    ///   the whole lifecycle into the one completed record it produced.
    ///   Nothing about the timer ever reaches the server.
    /// - The start may have been applied but no receipt came back. The
    ///   dependent command is persisted and stays locally visible, but is held
    ///   back until the exact receipt arrives — acting now could target another
    ///   caregiver's timer sharing that entity ID.
    /// - The start is acknowledged, so the command is dispatchable at once.
    @discardableResult
    func reduceTimerIntentLocked(_ intent: TimerIntent, completedRecord: (any ChangeLoggable)? = nil) throws -> TimerIntentResult {
        guard realmIsShared else { return .ready(mutationID: intent.timerID) }
        // This is an originating timer action, including a fold into an
        // undispatched start. ACK/dispatch bookkeeping never enters here.
        syncWriteScope?.originatedCareWrite = true
        let start = try timerStartRow(intent.timerID)

        switch (intent, start?.deliveryState ?? .acknowledged) {
        case (.start, _):
            return .ready(mutationID: try appendTimerCommand(intent).id)

        case (.finishBottle, .neverDispatched), (.edit, .neverDispatched):
            guard let start, let timer = try activeTimer(id: intent.timerID) else {
                return .ready(mutationID: try appendTimerCommand(intent).id)
            }
            return try foldIntoUndispatchedStart(start: start, timer: timer)

        case (.stop, .neverDispatched):
            guard let start else {
                return .ready(mutationID: try appendTimerCommand(intent).id)
            }
            guard let record = completedRecord else {
                context.delete(start)
                return .discarded
            }
            return try compactUndispatchedLifecycle(start: start, record: record)

        case (.finishBottle, .inFlightOrUncertain), (.stop, .inFlightOrUncertain),
             (.edit, .inFlightOrUncertain):
            guard let start else { return .ready(mutationID: try appendTimerCommand(intent).id) }
            let row = try appendTimerCommand(intent, dependencyMutationID: start.id)
            return .dependencyBlocked(mutationID: row.id, dependsOn: start.id)

        default:
            return .ready(mutationID: try appendTimerCommand(intent).id)
        }
    }

    func timerStartRow(_ timerID: UUID) throws -> SyncOutboxEntry? {
        try liveOutboxRows().first {
            $0.entityID == timerID && $0.operation == .timerStart && !$0.deliveryState.isTerminal
        }
    }

    private func settleDependents(of start: SyncOutboxEntry, code: String?) throws -> TimerIntentResult {
        let dependents = try liveOutboxRows().filter { $0.dependencyMutationID == start.id }
        guard let last = dependents.last else {
            if let code { return .superseded(mutationID: start.id, code: code) }
            return .ready(mutationID: start.id)
        }
        guard let code else {
            // Applied: the dependency is satisfied, so the held commands may go.
            for row in dependents { row.dependencyMutationID = nil }
            return .ready(mutationID: last.id)
        }
        for row in dependents {
            row.deliveryState = .superseded
            row.supersededCode = code
        }
        return .superseded(mutationID: last.id, code: code)
    }

    /// The start is still editable, so the new state is written into its own
    /// payload and no second mutation is ever created.
    private func foldIntoUndispatchedStart(start: SyncOutboxEntry, timer: ActiveTimer) throws -> TimerIntentResult {
        let refreshed = try MutationFactory.mutation(
            id: start.id,
            entityID: timer.id,
            entityKind: .timer,
            operation: .timerStart,
            observedEntityVersion: try observedVersion(of: timer.id, kind: .timer),
            at: now,
            payload: MutationFactory.timerPayload(timer)
        )
        try start.replacePayload(try JSONEncoder().encode(refreshed))
        return .folded(startMutationID: start.id)
    }

    /// Nothing about this timer ever left the device, so the server is told
    /// only about the record the timer produced.
    private func compactUndispatchedLifecycle(start: SyncOutboxEntry, record: any ChangeLoggable) throws -> TimerIntentResult {
        context.delete(start)
        guard let kind = MutationFactory.entityKind(forTypeName: record.entityTypeName) else {
            throw SyncPersistenceError.unknownMutation(record.entityID)
        }
        let mutation = try MutationFactory.mutation(
            entityID: record.entityID,
            entityKind: kind,
            operation: .create,
            observedEntityVersion: try observedVersion(of: record.entityID, kind: kind),
            at: now,
            payload: record.payloadFields()
        )
        return .compacted(completionMutationID: try appendOutboxRow(mutation).id)
    }

    @discardableResult
    func appendTimerCommand(_ intent: TimerIntent, dependencyMutationID: UUID? = nil) throws -> SyncOutboxEntry {
        let operation: WireOperation
        var payload: [String: Any]
        switch intent {
        case .start(let timerID, _, _, _):
            operation = .timerStart
            guard let timer = try activeTimer(id: timerID) else {
                throw SyncPersistenceError.unknownMutation(timerID)
            }
            payload = MutationFactory.timerPayload(timer)
        case .finishBottle(_, let babyID, let componentID, let finishedAt, let volumeML, let contents):
            operation = .timerFinishBottle
            payload = [
                "baby_id": babyID.uuidString.lowercased(),
                "finished_at": MutationFactory.wireTime(finishedAt),
                "component_id": componentID.uuidString.lowercased(),
                "volume_ml": volumeML,
                "contents": WireFeedComponent.wireContents(contents),
            ]
        case .stop(_, let babyID, let completedRecordID, let componentID, let stoppedAt, let cleanupShortBreastSegments):
            operation = .timerStop
            payload = [
                "baby_id": babyID.uuidString.lowercased(),
                "completed_record_id": completedRecordID.uuidString.lowercased(),
                "stopped_at": MutationFactory.wireTime(stoppedAt),
            ]
            payload["component_id"] = componentID?.uuidString.lowercased()
            if cleanupShortBreastSegments {
                payload["cleanup_short_breast_segments"] = true
            }
        case .edit(_, let babyID, let startedAt, let components, let sessionID):
            // A patch: `started_at` is always present, null meaning "leave
            // it", and `components` names only the parts that changed — a
            // whole-timer replace would undo a switch or bottle the other
            // phone made meanwhile. `session_id` is required at v5, so every
            // edit carries the timer's current link, null when there is none.
            operation = .update
            payload = [
                "baby_id": babyID.uuidString.lowercased(),
                "started_at": startedAt.map { MutationFactory.wireTime($0) as Any } ?? NSNull(),
                "components": components.map(MutationFactory.componentPayload),
                "session_id": sessionID?.uuidString.lowercased() as Any? ?? NSNull(),
            ]
        }
        let mutation = try MutationFactory.mutation(
            entityID: intent.timerID,
            entityKind: .timer,
            operation: operation,
            observedEntityVersion: try observedVersion(of: intent.timerID, kind: .timer),
            at: now,
            payload: payload
        )
        return try appendOutboxRow(mutation, dependencyMutationID: dependencyMutationID)
    }
}
