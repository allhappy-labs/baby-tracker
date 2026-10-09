import Foundation
import Testing
@testable import BabyCore
@testable import BabyProtocol
@testable import BabyStore

@MainActor
@Suite("Shared timer intents and receipts")
struct SharedTimerTests {
    @Test func allBriefUndispatchedFeedLeavesNoTimerRecordOrMutation() throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let timer = try store.startPart(.breast(.left), for: Fixture.babyID)

        clock.advance(by: 3)
        #expect(try store.stopFeed(timer) == nil)

        #expect(try store.activeTimer(id: timer.id) == nil)
        #expect(try store.lastFeed(for: Fixture.babyID) == nil)
        #expect(try store.pendingOutbox().isEmpty)
    }

    @Test func allBriefDispatchedFeedSendsDependentStopWithoutLocalRecord() throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let intents = SyncIntentTestAdapter(store: store)
        let start = try intents.startFeed(side: .left, for: Fixture.babyID)
        try store.markDispatched(mutationIDs: [start.mutationID])
        let timer = try #require(store.activeTimerForTesting(start.timerID))

        clock.advance(by: 3)
        #expect(try store.stopFeed(timer) == nil)

        let stop = try #require(try store.pendingOutbox().first { $0.operation == .timerStop })
        #expect(stop.dependencyMutationID == start.mutationID)
        let payload = try #require(try decodedMutation(store, id: stop.id).payload.objectValue)
        #expect(payload["cleanup_short_breast_segments"] == .bool(true))
        #expect(try store.lastFeed(for: Fixture.babyID) == nil)
    }

    @Test func sleepStopDoesNotRequestBreastCleanup() throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let timer = try store.startSleep(for: Fixture.babyID)
        let start = try #require(try store.pendingOutbox().first { $0.operation == .timerStart })
        try store.markDispatched(mutationIDs: [start.id])
        _ = try store.resolveTimerStartReceipt(Fixture.alreadyApplied(start.id, originalStatus: "applied"))

        clock.advance(by: 60)
        _ = try store.stopSleep(timer)

        let stop = try #require(try store.pendingOutbox().first { $0.operation == .timerStop })
        let payload = try #require(try decodedMutation(store, id: stop.id).payload.objectValue)
        #expect(payload["cleanup_short_breast_segments"] == nil)
    }

    @Test func uncertainStartDoesNotDispatchOrRetargetDependentStop() throws {
        let clock = TestDateProvider(.now)
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let intents = SyncIntentTestAdapter(store: store)
        let start = try intents.startFeed(side: .left, for: Fixture.babyID)
        try store.markDispatched(mutationIDs: [start.mutationID])
        let frozen = try store.persistedDispatch(mutationID: start.mutationID)
        clock.advance(by: 30)
        let stop = try intents.stopFeed(timerID: start.timerID)

        #expect(frozen.frozenPayload == (try store.outboxEntry(start.mutationID).payload))
        #expect(try store.outboxEntry(stop.mutationID).dependencyMutationID == start.mutationID)
        #expect(try store.nextDispatchBatch(limit: 10).map(\.id) == [start.mutationID])
    }

    /// Each row gets its own store and exactly one stable receipt: applying two
    /// mutually exclusive receipts to one start would prove nothing.
    @Test(arguments: ["applied", "superseded", "rejected"])
    func alreadyAppliedOriginalStatusesReleaseOnlyAppliedStart(originalStatus: String) throws {
        let clock = TestDateProvider(.now)
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let intents = SyncIntentTestAdapter(store: store)
        let start = try intents.startFeed(side: .left, for: Fixture.babyID)
        try store.markDispatched(mutationIDs: [start.mutationID])
        #expect(try store.persistedDispatch(mutationID: start.mutationID).deliveryState == .inFlightOrUncertain)

        clock.advance(by: 30)
        let dependent = try intents.stopFeed(timerID: start.timerID)
        #expect(try store.outboxEntry(dependent.mutationID).dependencyMutationID == start.mutationID)
        #expect(try store.nextDispatchBatch(limit: 10).map(\.id) == [start.mutationID])

        let result = try store.resolveTimerStartReceipt(Fixture.alreadyApplied(start.mutationID, originalStatus: originalStatus))

        switch originalStatus {
        case "applied":
            #expect(result == .ready(mutationID: dependent.mutationID))
            #expect(try store.nextDispatchBatch(limit: 10).map(\.id) == [dependent.mutationID])
        case "superseded", "rejected":
            let code = originalStatus == "superseded" ? "timer_collision" : "timer_start_rejected"
            #expect(result == .superseded(mutationID: dependent.mutationID, code: code))
            #expect(try store.persistedDispatch(mutationID: dependent.mutationID).deliveryState == .superseded)
            #expect(try store.nextDispatchBatch(limit: 10).isEmpty)
        default:
            Issue.record("Unexpected original receipt status in table")
        }
    }

    @Test func whollyNeverDispatchedTimerCompactsToOneCompletion() throws {
        let clock = TestDateProvider(.now)
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let intents = SyncIntentTestAdapter(store: store)
        let start = try intents.startFeed(side: .left, for: Fixture.babyID)
        clock.advance(by: 30)
        _ = try intents.stopFeed(timerID: start.timerID)

        let batch = try store.nextDispatchBatch(limit: 10)
        #expect(batch.map(\.operation) == [.create])
        #expect(batch.map(\.entityKind) == [.feed])
    }

    /// An edit on a never-dispatched start folds into it (no second mutation),
    /// and the compaction on stop carries the edited value through — not the
    /// value the timer started with.
    @Test func editThenStopOnAnUndispatchedTimerCompactsWithTheEditedStart() throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let intents = SyncIntentTestAdapter(store: store)
        let start = try intents.startFeed(side: .left, for: Fixture.babyID)
        let timer = try #require(store.activeTimerForTesting(start.timerID))
        let moved = timer.startedAt.addingTimeInterval(-120)
        clock.advance(by: 60)

        try store.editRunningTimer(id: timer.id, startedAt: moved, components: [])
        _ = try intents.stopFeed(timerID: timer.id)

        let batch = try store.nextDispatchBatch(limit: 10)
        #expect(batch.map(\.operation) == [.create])
        let feed = try #require(batch.first?.payload.objectValue)
        #expect(feed["started_at"] == .string(MutationFactory.wireTime(moved)))
    }

    /// The exact wire shape a start and a stop send — the live-fire check the
    /// deleted switchSide/attachBottle-specific payload test used to pin,
    /// re-pinned against the two operations the client still sends.
    @Test func timerStartAndStopUseExactWireShapes() throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let intents = SyncIntentTestAdapter(store: store)
        let start = try intents.startFeed(side: .left, for: Fixture.babyID)
        let startMutation = try decodedMutation(store, id: start.mutationID)
        let startPayload = try #require(startMutation.payload.objectValue)
        #expect(Set(startPayload.keys) == ["baby_id", "timer_kind", "started_at", "time_zone_id", "completed_components",
                                           "current_side", "current_side_started_at", "current_bottle_started_at", "session_id"])
        #expect(startPayload["current_bottle_started_at"] == .null)
        #expect(startMutation.payloadVersion == 5)

        try store.markDispatched(mutationIDs: [start.mutationID])
        _ = try store.resolveTimerStartReceipt(Fixture.alreadyApplied(start.mutationID, originalStatus: "applied"))

        clock.advance(by: 60)
        let stop = try intents.stopFeed(timerID: start.timerID)
        let stopPayload = try #require(try decodedMutation(store, id: stop.mutationID).payload.objectValue)
        #expect(Set(stopPayload.keys) == ["baby_id", "completed_record_id", "stopped_at", "component_id", "cleanup_short_breast_segments"])
    }

    /// The exact wire shape a timed bottle's finish sends, dispatched once
    /// its start is acknowledged.
    @Test func timedBottleFinishUsesExactWireShape() throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let timer = try store.startPart(.bottle(.formula), for: Fixture.babyID)
        let start = try #require(try store.pendingOutbox().first { $0.operation == .timerStart })
        try store.markDispatched(mutationIDs: [start.id])
        _ = try store.resolveTimerStartReceipt(Fixture.alreadyApplied(start.id, originalStatus: "applied"))

        clock.advance(by: 90)
        try store.finishBottle(volumeML: 80, contents: .formula, on: timer)

        let finish = try #require(try store.pendingOutbox().first { $0.operation == .timerFinishBottle })
        let payload = try #require(try decodedMutation(store, id: finish.id).payload.objectValue)
        #expect(Set(payload.keys) == ["baby_id", "finished_at", "component_id", "volume_ml", "contents"])
    }

    /// Every timer this phone starts is version 5, and the server refuses any
    /// later command declaring a lower version than the timer it targets
    /// (`unsupported_payload_version`). Stop and finish-bottle payloads carry
    /// no `session_id`, so the version cannot be read off their keys.
    @Test func everyTimerCommandDeclaresVersionFive() throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let sleep = try store.startSleep(for: Fixture.babyID)
        let bottle = try store.startPart(.bottle(.formula), for: Fixture.babyID)
        for start in try store.pendingOutbox() where start.operation == .timerStart {
            #expect(try decodedMutation(store, id: start.id).payloadVersion == 5)
            try store.markDispatched(mutationIDs: [start.id])
            _ = try store.resolveTimerStartReceipt(Fixture.alreadyApplied(start.id, originalStatus: "applied"))
        }

        clock.advance(by: 90)
        try store.editRunningTimer(id: sleep.id, startedAt: sleep.startedAt.addingTimeInterval(-60), components: [])
        try store.finishBottle(volumeML: 80, contents: .formula, on: bottle)
        _ = try store.stopFeed(bottle)
        _ = try store.stopSleep(sleep)

        let commands = try store.pendingOutbox().filter { $0.entityKind == .timer && $0.operation != .timerStart }
        #expect(Set(commands.map(\.operation)) == [.update, .timerFinishBottle, .timerStop])
        #expect(commands.count == 4)
        for command in commands {
            #expect(try decodedMutation(store, id: command.id).payloadVersion == 5, "\(command.operationRaw)")
        }
    }

    // MARK: Editing a running timer

    /// Nothing has left the phone yet, so the edit is written into the start
    /// itself rather than sent as a second mutation.
    @Test func editOnAnUndispatchedStartFoldsIntoIt() throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let intents = SyncIntentTestAdapter(store: store)
        let start = try intents.startFeed(side: .left, for: Fixture.babyID)
        let timer = try #require(store.activeTimerForTesting(start.timerID))
        clock.advance(by: 600)
        let moved = timer.startedAt.addingTimeInterval(-120)

        try store.editRunningTimer(id: timer.id, startedAt: moved, components: [])

        #expect(try store.pendingOutbox().map(\.id) == [start.mutationID])
        let payload = try #require(try decodedMutation(store, id: start.mutationID).payload.objectValue)
        #expect(payload["started_at"] == .string(MutationFactory.wireTime(moved)))
        #expect(payload["current_side_started_at"] == .string(MutationFactory.wireTime(moved)))
    }

    /// The start may or may not have reached the server, so the edit waits
    /// for its receipt rather than acting on a timer that may not be there.
    @Test func editBehindAStartInFlightWaitsForItsReceipt() throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let intents = SyncIntentTestAdapter(store: store)
        let start = try intents.startFeed(side: .left, for: Fixture.babyID)
        try store.markDispatched(mutationIDs: [start.mutationID])
        clock.advance(by: 600)
        let timer = try #require(store.activeTimerForTesting(start.timerID))

        try store.editRunningTimer(id: timer.id, startedAt: timer.startedAt.addingTimeInterval(-60), components: [])

        let edit = try #require(try store.pendingOutbox().first { $0.operation == .update })
        #expect(edit.dependencyMutationID == start.mutationID)
        #expect(try store.nextDispatchBatch(limit: 10).map(\.id) == [start.mutationID])

        _ = try store.resolveTimerStartReceipt(Fixture.alreadyApplied(start.mutationID, originalStatus: "applied"))
        #expect(try store.nextDispatchBatch(limit: 10).map(\.id) == [edit.id])
    }

    /// A start the server refused never existed there, so an edit waiting on
    /// it can never apply and goes down with it.
    @Test func aRefusedStartTakesAHeldEditDownWithIt() throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let intents = SyncIntentTestAdapter(store: store)
        let start = try intents.startFeed(side: .left, for: Fixture.babyID)
        try store.markDispatched(mutationIDs: [start.mutationID])
        clock.advance(by: 600)
        let timer = try #require(store.activeTimerForTesting(start.timerID))
        try store.editRunningTimer(id: timer.id, startedAt: timer.startedAt.addingTimeInterval(-60), components: [])
        let edit = try #require(try store.pendingOutbox().first { $0.operation == .update })

        let result = try store.resolveTimerStartReceipt(Fixture.alreadyApplied(start.mutationID, originalStatus: "rejected"))

        #expect(result == .superseded(mutationID: edit.id, code: "timer_start_rejected"))
        #expect(try store.persistedDispatch(mutationID: edit.id).deliveryState == .superseded)
        #expect(try store.nextDispatchBatch(limit: 10).isEmpty)
    }

    @Test func unstableAlreadyAppliedReceiptIsRefused() throws {
        let store = try BabyStore.sharedFixture()
        let intents = SyncIntentTestAdapter(store: store)
        let start = try intents.startFeed(side: .left, for: Fixture.babyID)
        try store.markDispatched(mutationIDs: [start.mutationID])
        let unstable = try JSONDecoder().decode(Outcome.self, from: Data("""
        {"mutation_id":"\(start.mutationID.uuidString.lowercased())","status":"already_applied","original_status":null,
         "canonical_entity":null,"related_entities":[],"cursor":null,"code":null,"overwrote_newer_version":false}
        """.utf8))

        #expect(throws: OutcomeError.unstableReceipt(start.mutationID)) {
            try store.resolveTimerStartReceipt(unstable)
        }
    }
}

@MainActor
private func decodedMutation(_ store: BabyStore, id: UUID) throws -> WireMutation {
    try JSONDecoder().decode(WireMutation.self, from: store.outboxEntry(id).payload)
}

private extension JSONValue {
    var objectValue: [String: JSONValue]? {
        guard case .object(let value) = self else { return nil }
        return value
    }
}
