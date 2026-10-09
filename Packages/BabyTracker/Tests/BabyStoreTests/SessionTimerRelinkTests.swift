import Testing
import Foundation
import BabyCore
@testable import BabyProtocol
@testable import BabyStore

/// Join and Split move a running part's session link with the session it
/// continues, as deleting a session's first part does: when the part stops,
/// it lands in the session the parent now sees, never in one keyed by a
/// joined-away member or in the earlier half of a split.
@MainActor @Suite struct SessionTimerRelinkTests {
    // MARK: - Feeds

    @Test func joiningMovesARunningPartToTheEarlierSession() throws {
        let (store, baby, clock) = try fixture()
        let a = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-4 * 3_600),
                                    to: clock.now.addingTimeInterval(-4 * 3_600 + 600), for: baby)
        let b = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-900),
                                    to: clock.now.addingTimeInterval(-300), for: baby)
        let running = try store.startPart(.breast(.right), for: baby)
        #expect(running.sessionID == b.id)

        try store.combineFeeds([a.id, b.id], for: baby)

        #expect(try store.activeTimer(kind: .feed, for: baby)?.sessionID == a.id)
        let stopped = try #require(try store.stopFeed(running.advancing(clock, by: 300)))
        #expect(stopped.effectiveFeedingSessionID == a.id)
        let sessions = try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture)
        #expect(sessions.map(\.feedIDs) == [[a.id, b.id, stopped.id]])
    }

    @Test func deletingAJoinedPartKeepsTheRunningPartOnItsSession() throws {
        let (store, baby, clock) = try fixture()
        let a = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-4 * 3_600),
                                    to: clock.now.addingTimeInterval(-4 * 3_600 + 600), for: baby)
        let b = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-900),
                                    to: clock.now.addingTimeInterval(-300), for: baby)
        let running = try store.startPart(.breast(.right), for: baby)
        try store.combineFeeds([a.id, b.id], for: baby)

        try store.deleteFeedPart(b.id, for: baby)

        #expect(b.isTombstoned)
        #expect(try store.activeTimer(kind: .feed, for: baby)?.sessionID == a.id)
        let stopped = try #require(try store.stopFeed(running.advancing(clock, by: 300)))
        let sessions = try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture)
        #expect(sessions.map(\.feedIDs) == [[a.id, stopped.id]])
    }

    @Test func splittingMovesARunningPartToTheLaterHalf() throws {
        let (store, baby, clock) = try fixture()
        let left = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-1_500),
                                       to: clock.now.addingTimeInterval(-1_200), for: baby)
        let right = try store.logBreast(side: .right, from: clock.now.addingTimeInterval(-900),
                                        to: clock.now.addingTimeInterval(-300), for: baby)
        #expect(right.feedingSessionID == left.id)
        let running = try store.startPart(.breast(.left), for: baby)
        #expect(running.sessionID == left.id)

        try store.separateFeed(at: right.id, for: baby)

        #expect(try store.activeTimer(kind: .feed, for: baby)?.sessionID == right.id)
        let stopped = try #require(try store.stopFeed(running.advancing(clock, by: 300)))
        let sessions = try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture)
        #expect(sessions.map(\.feedIDs) == [[left.id], [right.id, stopped.id]])
    }

    // MARK: - Sleeps

    @Test func joiningMovesARunningSleepToTheEarlierSession() throws {
        let (store, baby, clock) = try fixture()
        let a = try store.logSleep(from: clock.now.addingTimeInterval(-8_200), to: clock.now.addingTimeInterval(-4_600), for: baby)
        let b = try store.logSleep(from: clock.now.addingTimeInterval(-3_000), to: clock.now.addingTimeInterval(-300), for: baby)
        let running = try store.startSleep(for: baby)
        #expect(running.sessionID == b.id)

        try store.combineSleeps([a.id, b.id], for: baby)

        #expect(try store.activeTimer(kind: .sleep, for: baby)?.sessionID == a.id)
        let stopped = try store.stopSleep(running.advancing(clock, by: 600))
        #expect(stopped.effectiveSleepSessionID == a.id)
        let sessions = try store.sleepSessions(for: baby, from: .distantPast, to: .distantFuture)
        #expect(sessions.map(\.sleepIDs) == [[a.id, b.id, stopped.id]])
    }

    @Test func splittingMovesARunningSleepToTheLaterHalf() throws {
        let (store, baby, clock) = try fixture()
        let a = try store.logSleep(from: clock.now.addingTimeInterval(-8_200), to: clock.now.addingTimeInterval(-4_600), for: baby)
        let b = try store.logSleep(from: clock.now.addingTimeInterval(-3_000), to: clock.now.addingTimeInterval(-300), for: baby)
        try store.combineSleeps([a.id, b.id], for: baby)
        let running = try store.startSleep(for: baby)
        #expect(running.sessionID == a.id)

        try store.separateSleep(at: b.id, for: baby)

        #expect(try store.activeTimer(kind: .sleep, for: baby)?.sessionID == b.id)
        let stopped = try store.stopSleep(running.advancing(clock, by: 600))
        let sessions = try store.sleepSessions(for: baby, from: .distantPast, to: .distantFuture)
        #expect(sessions.map(\.sleepIDs) == [[a.id], [b.id, stopped.id]])
    }

    // MARK: - Sync

    /// The relink is the same session-only timer edit a delete's relink
    /// sends, so the other phone's copy of the timer follows the session.
    @Test func joinAndSplitSendTheTimersNewSessionToTheOtherPhone() throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        store.breakLimits = { .standard }
        let baby = Fixture.babyID
        let a = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-4 * 3_600),
                                    to: clock.now.addingTimeInterval(-4 * 3_600 + 600), for: baby)
        let b = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-900),
                                    to: clock.now.addingTimeInterval(-300), for: baby)
        let running = try store.startPart(.breast(.right), for: baby)
        let start = try #require(try store.pendingOutbox().first { $0.operation == .timerStart })
        try store.markDispatched(mutationIDs: [start.id])
        _ = try store.resolveTimerStartReceipt(Fixture.alreadyApplied(start.id, originalStatus: "applied"))

        try store.combineFeeds([a.id, b.id], for: baby)
        #expect(try timerEdits(store, running.id) == [a.id])

        try store.separateFeed(at: b.id, for: baby)
        #expect(try timerEdits(store, running.id) == [a.id, b.id])
    }

    /// The session links the timer's queued edits carry, oldest first.
    private func timerEdits(_ store: BabyStore, _ timerID: UUID) throws -> [UUID?] {
        try store.pendingOutbox()
            .filter { $0.entityID == timerID && $0.operation == .update }
            .map { row in
                guard case .string(let id)? = WireFixture.field("session_id", of: try row.decodedMutation().payload) else {
                    return nil
                }
                return UUID(uuidString: id)
            }
    }
}
