import Foundation
import SwiftData
import Testing
@testable import BabyCore
@testable import BabyProtocol
@testable import BabyStore

/// Another caregiver's running timer, as the server sends it.
///
/// The server keeps one row per timer in `active_timers` and journals every
/// change to it as the whole canonical timer (`Server/internal/records/
/// snapshot.go`, `timers.go`). A stop does not delete the row: it tombstones
/// it — `payload` null, `tombstoned_at` set — and journals the finished feed
/// or sleep as its own entity in the same change set.
@MainActor
@Suite("Remote timers")
struct RemoteTimerApplyTests {
    static let start = "2026-09-21T12:00:00.000000Z"
    static let startDate = Date(timeIntervalSince1970: 1_789_992_000) // 2026-09-21T12:00:00Z

    static func sleepPayload(started: String = start) -> String {
        """
        {"baby_id":"\(Fixture.babyID.uuidString.lowercased())","timer_kind":"sleep","started_at":"\(started)",
         "time_zone_id":"Europe/Zurich","current_side":null,"current_side_started_at":null,
         "current_bottle_started_at":null,"completed_components":[],"session_id":null}
        """
    }

    static func feedPayload(started: String = start, side: String?, sideStarted: String?, components: String) -> String {
        """
        {"baby_id":"\(Fixture.babyID.uuidString.lowercased())","timer_kind":"feed","started_at":"\(started)",
         "time_zone_id":"Europe/Zurich","current_side":\(side.map { "\"\($0)\"" } ?? "null"),
         "current_side_started_at":\(sideStarted.map { "\"\($0)\"" } ?? "null"),"current_bottle_started_at":null,
         "completed_components":\(components),"session_id":null}
        """
    }

    static func timer(_ id: UUID, version: Int64, payload: String) -> JournalChange {
        SyncFixture.change(id, kind: "timer", version: version, payload: payload)
    }

    static func timerTombstone(_ id: UUID, version: Int64) -> JournalChange {
        SyncFixture.change(id, kind: "timer", version: version, tombstoned: true, payload: "null")
    }

    static func sleepCompletion(_ id: UUID, version: Int64 = 1) -> JournalChange {
        SyncFixture.change(id, kind: "sleep", version: version, payload: """
        {"baby_id":"\(Fixture.babyID.uuidString.lowercased())","started_at":"\(start)",
         "ended_at":"2026-09-21T13:00:00.000000Z","time_zone_id":"Europe/Zurich","note":null,"kind_override":null,
         "sleep_session_id":null}
        """)
    }

    static func liveSleeps(_ store: BabyStore) throws -> [SleepEntry] {
        try store.context.fetch(FetchDescriptor<SleepEntry>()).filter { !$0.isTombstoned }
    }

    // MARK: Appearing

    @Test func remoteRunningSleepRunsHereFromTheSameStart() throws {
        let store = try BabyStore.sharedFixture()
        let timerID = UUID()

        try store.applySyncPage(SyncFixture.page(changes: [Self.timer(timerID, version: 1, payload: Self.sleepPayload())]))

        let timer = try #require(store.activeTimerForTesting(timerID))
        #expect(timer.kind == .sleep)
        #expect(timer.babyID == Fixture.babyID)
        #expect(timer.startedAt == Self.startDate)
        #expect(timer.timeZoneID == "Europe/Zurich")
        #expect(timer.realmKey == store.activeRealm.encodedKey)
        #expect(try store.activeTimers(for: Fixture.babyID).map(\.id) == [timerID])
        #expect(store.drainRemoteTimerTransitions() == [.appeared(timerID: timerID)])
        #expect(try store.pendingOutbox().isEmpty)
    }

    /// The side, when it started, and every completed component — under the
    /// IDs the server sent, because the stop will name them.
    @Test func remoteFeedCarriesSideAndComponentsUnderTheirOwnIDs() throws {
        let store = try BabyStore.sharedFixture()
        let timerID = UUID()
        let left = UUID()
        let bottle = UUID()

        try store.applySyncPage(SyncFixture.page(changes: [Self.timer(timerID, version: 3, payload: Self.feedPayload(
            side: "right", sideStarted: "2026-09-21T12:10:00.000000Z",
            components: """
            [{"id":"\(left.uuidString.lowercased())","kind":"breast","side":"left","seconds":300},
             {"id":"\(bottle.uuidString.lowercased())","kind":"bottle","volume_ml":50,"contents":"breast_milk"}]
            """
        ))]))

        let timer = try #require(store.activeTimerForTesting(timerID))
        #expect(timer.kind == .feed)
        #expect(timer.currentSide == .right)
        #expect(timer.currentSideStartedAt == Self.startDate.addingTimeInterval(600))
        #expect(timer.completedComponents == [
            .breast(id: left, side: .left, seconds: 300),
            .bottle(id: bottle, volumeML: 50, contents: .breastMilk),
        ])
    }

    /// A timer in a shape older than the server's one (scalar
    /// `completed_sides` and pending bottle, no `session_id`) is not read:
    /// the server sends only the current shape. The page still applies and
    /// the cursor still moves, so sync is not held up by it.
    @Test func olderShapeTimerIsNotReadAndDoesNotHoldUpSync() throws {
        let store = try BabyStore.sharedFixture()
        let timerID = UUID()
        let page = SyncFixture.page(changes: [Self.timer(timerID, version: 2, payload: """
        {"baby_id":"\(Fixture.babyID.uuidString.lowercased())","timer_kind":"feed","started_at":"\(Self.start)",
         "time_zone_id":"UTC","current_side":"left","current_side_started_at":"2026-09-21T12:02:00.000000Z",
         "completed_sides":[{"side":"right","seconds":120}],"pending_volume_ml":30,"pending_contents":"formula"}
        """)])

        try store.applySyncPage(page)

        #expect(store.activeTimerForTesting(timerID) == nil)
        #expect(store.drainRemoteTimerTransitions().isEmpty)
        #expect(try store.syncCursor() == page.highWater)
    }

    /// The other caregiver switched sides: the running timer here follows,
    /// and the change is announced so a Live Activity can be refreshed.
    @Test func laterRemoteVersionUpdatesTheRunningTimer() throws {
        let store = try BabyStore.sharedFixture()
        let timerID = UUID()
        let left = UUID()
        try store.applySyncPage(SyncFixture.page(changes: [Self.timer(timerID, version: 1, payload: Self.feedPayload(
            side: "left", sideStarted: Self.start, components: "[]"
        ))]))
        _ = store.drainRemoteTimerTransitions()

        try store.applySyncPage(SyncFixture.page(changes: [Self.timer(timerID, version: 2, payload: Self.feedPayload(
            side: "right", sideStarted: "2026-09-21T12:05:00.000000Z",
            components: #"[{"id":"\#(left.uuidString.lowercased())","kind":"breast","side":"left","seconds":300}]"#
        ))]))

        let timer = try #require(store.activeTimerForTesting(timerID))
        #expect(timer.currentSide == .right)
        #expect(timer.completedComponents == [.breast(id: left, side: .left, seconds: 300)])
        #expect(try store.activeTimers(for: Fixture.babyID).count == 1)
        #expect(store.drainRemoteTimerTransitions() == [.changed(timerID: timerID)])
    }

    /// A v5 start names the session it continues; a later edit that sends
    /// `session_id: null` detaches it.
    @Test func aRemoteTimerSessionIsAppliedAndDetachable() throws {
        let store = try BabyStore.sharedFixture()
        let timerID = UUID()
        let sessionID = UUID()

        try store.applySyncPage(SyncFixture.page(changes: [Self.timer(timerID, version: 1, payload: """
        {"baby_id":"\(Fixture.babyID.uuidString.lowercased())","timer_kind":"feed","started_at":"\(Self.start)",
         "time_zone_id":"Europe/Zurich","current_side":"left","current_side_started_at":"\(Self.start)",
         "current_bottle_started_at":null,"completed_components":[],"session_id":"\(sessionID.uuidString.lowercased())"}
        """)]))

        let timer = try #require(store.activeTimerForTesting(timerID))
        #expect(timer.sessionID == sessionID)
        #expect(store.drainRemoteTimerTransitions() == [.appeared(timerID: timerID)])

        try store.applySyncPage(SyncFixture.page(changes: [Self.timer(timerID, version: 2, payload: """
        {"baby_id":"\(Fixture.babyID.uuidString.lowercased())","timer_kind":"feed","started_at":"\(Self.start)",
         "time_zone_id":"Europe/Zurich","current_side":"left","current_side_started_at":"\(Self.start)",
         "current_bottle_started_at":null,"completed_components":[],"session_id":null}
        """)]))

        let detached = try #require(store.activeTimerForTesting(timerID))
        #expect(detached.sessionID == nil)
        #expect(store.drainRemoteTimerTransitions() == [.changed(timerID: timerID)])
    }

    // MARK: Ending

    /// The other caregiver stopped it: the timer goes, and the finished sleep
    /// arrives once, as its own entity.
    @Test func remoteStopEndsTheTimerAndLeavesExactlyOneEntry() throws {
        let store = try BabyStore.sharedFixture()
        let timerID = UUID()
        let sleepID = UUID()
        try store.applySyncPage(SyncFixture.page(changes: [Self.timer(timerID, version: 1, payload: Self.sleepPayload())]))
        let timer = try #require(store.activeTimerForTesting(timerID))
        try store.setLiveActivityID("activity-1", on: timer)
        _ = store.drainRemoteTimerTransitions()

        try store.applySyncPage(SyncFixture.page(changes: [
            Self.timerTombstone(timerID, version: 2),
            Self.sleepCompletion(sleepID),
        ]))

        #expect(store.activeTimerForTesting(timerID) == nil)
        #expect(try Self.liveSleeps(store).map(\.id) == [sleepID])
        #expect(store.drainRemoteTimerTransitions() == [.ended(timerID: timerID, liveActivityID: "activity-1")])
    }

    /// Both caregivers stopped the same nap before either synced. The server
    /// keeps the first stop and answers the second `superseded`/`timer_ended`
    /// with the first stop's record (`endedTimerOutcome`, timers.go). The
    /// record this phone derived was never created on the server, so it goes,
    /// and the family's one arrives — one nap, not two.
    @Test func losingStopKeepsOnlyTheFamilysRecord() throws {
        let clock = TestDateProvider(Self.startDate)
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let timerID = UUID()
        let winningSleep = UUID()
        try store.applySyncPage(SyncFixture.page(changes: [Self.timer(timerID, version: 1, payload: Self.sleepPayload())]))
        clock.advance(by: 3_600)
        let local = try store.stopSleep(try #require(store.activeTimerForTesting(timerID)))
        let stop = try #require(try store.pendingOutbox().first { $0.operation == .timerStop })
        try store.markDispatched(mutationIDs: [stop.id])

        try store.applySyncPage(SyncFixture.page(
            outcomes: [Self.superseded(stop.id, code: "timer_ended")],
            changes: [Self.timerTombstone(timerID, version: 2), Self.sleepCompletion(winningSleep)]
        ))

        #expect(try Self.liveSleeps(store).map(\.id) == [winningSleep])
        #expect(try store.sleepEntryForTesting(local.id)?.isTombstoned == true)
        #expect(store.activeTimerForTesting(timerID) == nil)
    }

    /// This phone stopped the nap and, inside the break, started the next
    /// one joined to its own record — whose stop then lost. The refusal
    /// names the winning record before the journal delivers it, and the next
    /// nap follows that record's session at once. Its start was never sent,
    /// so the new link folds into the start rather than adding an edit.
    @Test func napJoinedToALosingStopFollowsTheWinnerBeforeItArrives() throws {
        let clock = TestDateProvider(Self.startDate)
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        store.breakLimits = { .standard }
        let timerID = UUID()
        let winningSleep = UUID()
        try store.applySyncPage(SyncFixture.page(changes: [Self.timer(timerID, version: 1, payload: Self.sleepPayload())]))
        clock.advance(by: 3_600)
        let running = try #require(store.activeTimerForTesting(timerID))
        let local = try store.stopSleep(running)
        let stop = try #require(try store.pendingOutbox().first { $0.operation == .timerStop })
        try store.markDispatched(mutationIDs: [stop.id])
        clock.advance(by: 60)
        let next = try store.startSleep(for: Fixture.babyID)
        #expect(next.sessionID == local.id)

        try store.applySyncPage(SyncFixture.page(outcomes: [Self.superseded(
            stop.id, code: "timer_ended",
            related: [Self.sleepCompletion(winningSleep).snapshotOrTombstone]
        )]))

        #expect(try store.sleepEntryForTesting(local.id)?.isTombstoned == true)
        #expect(next.sessionID == winningSleep)
        let owed = try store.pendingOutbox().filter { !$0.deliveryState.isTerminal }
        #expect(owed.map(\.operation) == [.timerStart])
        let start = try #require(owed.first).decodedMutation()
        #expect(WireFixture.field("session_id", of: start.payload) == .string(winningSleep.uuidString.lowercased()))
    }

    /// Both caregivers started a nap for the same baby at once. The server
    /// allows one live timer per baby and kind and answers the later start
    /// `superseded` with the timer that won (`startTimer`, timers.go). The
    /// loser's own timer never existed on the server, so it goes and the
    /// winner's runs in its place.
    @Test func losingStartGivesWayToTheFamilysTimer() throws {
        let store = try BabyStore.sharedFixture()
        let mine = try store.startSleep(for: Fixture.babyID)
        let mineID = mine.id
        try store.setLiveActivityID("mine", on: mine)
        let start = try #require(try store.pendingOutbox().first { $0.operation == .timerStart })
        try store.markDispatched(mutationIDs: [start.id])
        let winner = UUID()

        try store.applySyncPage(SyncFixture.page(
            outcomes: [Self.superseded(start.id, code: nil, canonical: Self.timer(winner, version: 1, payload: Self.sleepPayload()).snapshotOrTombstone)],
            changes: [Self.timer(winner, version: 1, payload: Self.sleepPayload())]
        ))

        #expect(try store.activeTimers(for: Fixture.babyID).map(\.id) == [winner])
        #expect(store.activeTimerForTesting(mineID) == nil)
        #expect(store.drainRemoteTimerTransitions() == [
            .ended(timerID: mineID, liveActivityID: "mine"),
            .appeared(timerID: winner),
        ])
    }

    /// A completed nap belongs to this phone even if its timer start lost a
    /// race with a different timer. The completion must become an ordinary
    /// record create, while the family's winning timer keeps running.
    @Test func completedSleepSurvivesSupersededStart() throws {
        let clock = TestDateProvider(Self.startDate)
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let mine = try store.startSleep(for: Fixture.babyID)
        let start = try #require(try store.pendingOutbox().first { $0.operation == .timerStart })
        try store.markDispatched(mutationIDs: [start.id])
        clock.advance(by: 600)
        let completed = try store.stopSleep(mine)
        let winner = UUID()

        try store.applySyncPage(SyncFixture.page(
            outcomes: [Self.superseded(start.id, code: nil, canonical: Self.timer(winner, version: 1, payload: Self.sleepPayload()).snapshotOrTombstone)],
            changes: [Self.timer(winner, version: 1, payload: Self.sleepPayload())]
        ))

        #expect(try Self.liveSleeps(store).map(\.id) == [completed.id])
        let create = try #require(try store.pendingOutbox().first { $0.entityID == completed.id && $0.operation == .create && !$0.deliveryState.isTerminal })
        #expect(try store.activeTimers(for: Fixture.babyID).map(\.id) == [winner])

        // Replayed receipts cannot queue the same completion twice. Once the
        // create is accepted, the canonical row remains a single entry.
        try store.applySyncPage(SyncFixture.page(outcomes: [Self.superseded(start.id, code: nil)]))
        #expect(try store.pendingOutbox().filter { $0.entityID == completed.id && $0.operation == .create }.count == 1)
        try store.markDispatched(mutationIDs: [create.id])
        try store.applySyncPage(SyncFixture.page(
            outcomes: [SyncFixture.applied(create.id)],
            changes: [Self.sleepCompletion(completed.id)]
        ))
        #expect(try Self.liveSleeps(store).map(\.id) == [completed.id])
    }

    @Test func completedFeedSurvivesSupersededStart() throws {
        let clock = TestDateProvider(Self.startDate)
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let mine = try store.startPart(.breast(.left), for: Fixture.babyID)
        let start = try #require(try store.pendingOutbox().first { $0.operation == .timerStart })
        try store.markDispatched(mutationIDs: [start.id])
        clock.advance(by: 600)
        let completed = try #require(try store.stopFeed(mine))
        let winner = UUID()

        try store.applySyncPage(SyncFixture.page(
            outcomes: [Self.superseded(start.id, code: nil, canonical: Self.timer(winner, version: 1, payload: Self.feedPayload(side: "right", sideStarted: Self.start, components: "[]")).snapshotOrTombstone)],
            changes: [Self.timer(winner, version: 1, payload: Self.feedPayload(side: "right", sideStarted: Self.start, components: "[]"))]
        ))

        let liveFeeds = try store.context.fetch(FetchDescriptor<FeedEntry>()).filter { !$0.isTombstoned }
        #expect(liveFeeds.map(\.id) == [completed.id])
        #expect(try store.pendingOutbox().contains { $0.entityID == completed.id && $0.operation == .create && !$0.deliveryState.isTerminal })
        #expect(try store.activeTimers(for: Fixture.babyID).map(\.id) == [winner])
    }

    // MARK: This phone mid-change

    /// A stop in flight here: the server's still-running copy must not bring
    /// the timer back while the stop is owed.
    @Test func pendingStopIsNotUndoneByTheServersRunningCopy() throws {
        let clock = TestDateProvider(Self.startDate)
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let timerID = UUID()
        try store.applySyncPage(SyncFixture.page(changes: [Self.timer(timerID, version: 1, payload: Self.sleepPayload())]))
        clock.advance(by: 60)
        try store.stopSleep(try #require(store.activeTimerForTesting(timerID)))

        try store.applySyncPage(SyncFixture.page(changes: [Self.timer(timerID, version: 1, payload: Self.sleepPayload())]))

        #expect(store.activeTimerForTesting(timerID) == nil)
    }

    /// The other caregiver corrected the start and a finished part: the timer
    /// here follows under the same part ids, and the change is announced so
    /// this phone's Live Activity shows the corrected start.
    @Test func remoteEditChangesTheTimerUnderTheSameIDs() throws {
        let store = try BabyStore.sharedFixture()
        let timerID = UUID()
        let left = UUID()
        try store.applySyncPage(SyncFixture.page(changes: [Self.timer(timerID, version: 2, payload: Self.feedPayload(
            side: "right", sideStarted: "2026-09-21T12:10:00.000000Z",
            components: #"[{"id":"\#(left.uuidString.lowercased())","kind":"breast","side":"left","seconds":600}]"#
        ))]))
        _ = store.drainRemoteTimerTransitions()

        try store.applySyncPage(SyncFixture.page(changes: [Self.timer(timerID, version: 3, payload: Self.feedPayload(
            started: "2026-09-21T11:58:00.000000Z", side: "right", sideStarted: "2026-09-21T12:10:00.000000Z",
            components: #"[{"id":"\#(left.uuidString.lowercased())","kind":"breast","side":"left","seconds":300}]"#
        ))]))

        let timer = try #require(store.activeTimerForTesting(timerID))
        #expect(timer.startedAt == Self.startDate.addingTimeInterval(-120))
        #expect(timer.currentSideStartedAt == Self.startDate.addingTimeInterval(600))
        #expect(timer.completedComponents == [.breast(id: left, side: .left, seconds: 300)])
        #expect(store.drainRemoteTimerTransitions() == [.changed(timerID: timerID)])
    }

    /// This phone corrected the start and has not been told yet. The server's
    /// copy from before the edit must not put the old start back.
    @Test func pendingEditIsNotOverwrittenByAnOlderRemoteVersion() throws {
        let clock = TestDateProvider(Self.startDate)
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let intents = SyncIntentTestAdapter(store: store)
        let started = try intents.startFeed(side: .left, for: Fixture.babyID)
        try store.markDispatched(mutationIDs: [started.mutationID])
        try store.applySyncPage(SyncFixture.page(outcomes: [SyncFixture.applied(started.mutationID)]))
        let timer = try #require(store.activeTimerForTesting(started.timerID))
        clock.advance(by: 600)
        try store.editRunningTimer(id: timer.id, startedAt: Self.startDate.addingTimeInterval(-300), components: [])

        try store.applySyncPage(SyncFixture.page(changes: [Self.timer(started.timerID, version: 1, payload: Self.feedPayload(
            side: "left", sideStarted: Self.start, components: "[]"
        ))]))

        #expect(timer.startedAt == Self.startDate.addingTimeInterval(-300))
        #expect(try store.pendingOutbox().contains { $0.operation == .update })
        #expect(store.drainRemoteTimerTransitions().isEmpty)
    }

    /// The server refused the edit — here because the other phone's copy has
    /// no such part (`timer_component_not_found`). The refusal is about this
    /// one mutation, so the row is settled rather than quarantined, and the
    /// timer goes back to what the server holds.
    @Test func refusedEditPutsTheServersTimerBack() throws {
        let clock = TestDateProvider(Self.startDate)
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let intents = SyncIntentTestAdapter(store: store)
        let started = try intents.startFeed(side: .left, for: Fixture.babyID)
        try store.markDispatched(mutationIDs: [started.mutationID])
        let serverCopy = Self.timer(started.timerID, version: 1, payload: Self.feedPayload(
            side: "left", sideStarted: Self.start, components: "[]"
        ))
        try store.applySyncPage(SyncFixture.page(outcomes: [SyncFixture.applied(started.mutationID)], changes: [serverCopy]))
        _ = store.drainRemoteTimerTransitions()
        let timer = try #require(store.activeTimerForTesting(started.timerID))
        clock.advance(by: 600)
        try store.editRunningTimer(id: timer.id, startedAt: Self.startDate.addingTimeInterval(-300), components: [])
        let edit = try #require(try store.pendingOutbox().first { $0.operation == .update })
        try store.markDispatched(mutationIDs: [edit.id])

        try store.applySyncPage(SyncFixture.page(outcomes: [
            Self.refused(edit.id, status: "rejected", code: "timer_component_not_found", canonical: serverCopy.snapshotOrTombstone),
        ]))

        #expect(timer.startedAt == Self.startDate)
        #expect(timer.currentSideStartedAt == Self.startDate)
        let row = try store.persistedDispatch(mutationID: edit.id)
        #expect(row.deliveryState == .superseded)
        #expect(try store.outboxEntry(edit.id).supersededCode == "timer_component_not_found")
        #expect(store.drainRemoteTimerTransitions() == [.changed(timerID: started.timerID)])
    }

    /// A page that fails part-way is rolled back whole. What it would have
    /// announced goes with it, or a Live Activity would be started for a
    /// timer that is not there. Driven through the wrapper every apply entry
    /// point uses, because which touched entity a page rematerializes first
    /// is set order, so a failing page cannot be made to fail *after* a timer
    /// deterministically.
    @Test func failedApplyAnnouncesNothing() throws {
        let store = try BabyStore.sharedFixture()
        let kept = UUID()
        store.remoteTimerTransitions = [.appeared(timerID: kept)]

        #expect(throws: CancellationError.self) {
            try store.discardingTimerTransitionsOnFailure {
                store.remoteTimerTransitions.append(.appeared(timerID: UUID()))
                throw CancellationError()
            }
        }

        #expect(store.drainRemoteTimerTransitions() == [.appeared(timerID: kept)])
    }

    // MARK: Snapshot

    /// A joiner's first snapshot: a nap already running shows as running, and
    /// one already finished does not come back.
    @Test func snapshotBringsRunningTimersButNotFinishedOnes() throws {
        let store = try BabyStore.sharedFixture()
        let running = UUID()
        let finished = UUID()
        let snapshotID = UUID()
        store.beginSnapshot(snapshotID: snapshotID, highWater: SyncFixture.cursor(sequence: 9))

        try store.applySnapshotPage(SnapshotResponse(
            serverInstanceID: SyncFixture.instanceID, recoveryEpoch: 1, snapshotID: snapshotID,
            highWater: SyncFixture.cursor(sequence: 9),
            entities: [
                Self.timer(running, version: 1, payload: Self.sleepPayload()).snapshotOrTombstone,
                Self.timerTombstone(finished, version: 2).snapshotOrTombstone,
            ],
            nextPageToken: nil
        ))
        #expect(store.activeTimerForTesting(running) == nil)
        try store.finishSnapshot(snapshotID: snapshotID)

        #expect(try store.activeTimers(for: Fixture.babyID).map(\.id) == [running])
        #expect(store.drainRemoteTimerTransitions() == [.appeared(timerID: running)])
    }

    static func superseded(_ mutationID: UUID, code: String?, canonical: CanonicalEntity? = nil,
                           related: [CanonicalEntity] = []) -> Outcome {
        refused(mutationID, status: "superseded", code: code, canonical: canonical, related: related)
    }

    static func refused(_ mutationID: UUID, status: String, code: String?, canonical: CanonicalEntity? = nil,
                        related: [CanonicalEntity] = []) -> Outcome {
        let canonicalJSON = canonical.map { String(decoding: try! JSONEncoder().encode($0), as: UTF8.self) } ?? "null"
        let relatedJSON = String(decoding: try! JSONEncoder().encode(related), as: UTF8.self)
        return try! JSONDecoder().decode(Outcome.self, from: Data("""
        {"mutation_id":"\(mutationID.uuidString.lowercased())","status":"\(status)","original_status":null,
         "canonical_entity":\(canonicalJSON),"related_entities":\(relatedJSON),"cursor":null,
         "code":\(code.map { "\"\($0)\"" } ?? "null"),"overwrote_newer_version":false}
        """.utf8))
    }
}
