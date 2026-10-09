import Testing
import Foundation
import BabyCore
@testable import BabyStore

/// The store calls History's session sheet and part editors make: add a part
/// to a chosen session, delete a session or one of its parts, find the
/// session to join, and check a completed part's start.
@MainActor @Suite struct SessionEditingTests {
    /// A day's clock times as dates, so a test reads like the timeline it
    /// builds. The store's clock is moved to the evening first, so every
    /// part is in the past.
    private func day(_ clock: TestDateProvider) -> (Int, Int) -> Date {
        let midnight = clock.now
        clock.advance(by: 20 * 3_600)
        return { hour, minute in midnight.addingTimeInterval(TimeInterval(hour * 3_600 + minute * 60)) }
    }

    private func liveFeeds(_ store: BabyStore, _ baby: UUID) throws -> [FeedEntry] {
        try store.sessionFeeds(for: baby)
    }

    // MARK: - Add part

    @Test func addFeedPartJoinsTheGivenSessionEvenPastTheBreakLimit() throws {
        let (store, baby, clock) = try fixture()
        let left = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-7200), to: clock.now.addingTimeInterval(-6600), for: baby)
        let bottle = try store.addFeedPart(.bottle(volumeML: 60, contents: .formula, at: clock.now), toSession: left.id, for: baby)
        #expect(bottle.effectiveFeedingSessionID == left.effectiveFeedingSessionID)
        #expect(try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture).count == 1)
    }

    @Test func addFeedPartIsOneCreateCarryingTheSessionLink() throws {
        let (store, baby, clock) = try fixture()
        let at = day(clock)
        let left = try store.logBreast(side: .left, from: at(7, 0), to: at(7, 10), for: baby)
        let right = try store.addFeedPart(.breast(side: .right, from: at(9, 0), to: at(9, 12)), toSession: left.id, for: baby)
        #expect(right.feedingSessionID == left.id)
        #expect(right.components.map(\.breastSide) == [.right])
        let logged = try store.changeLog().filter { $0.entityID == right.id }
        #expect(logged.map(\.operation) == [.create])
    }

    @Test func addFeedPartRefusesASolidsSession() throws {
        let (store, baby, clock) = try fixture()
        let solids = try store.logSolids(foods: ["pear"], amount: .some, for: baby)
        #expect(throws: FeedingSessionError.solidsNeverJoin) {
            try store.addFeedPart(.bottle(volumeML: 60, contents: .formula, at: clock.now), toSession: solids.id, for: baby)
        }
        #expect(try liveFeeds(store, baby).count == 1)
    }

    @Test func addSleepPartJoinsTheGivenSessionEvenPastTheBreakLimit() throws {
        let (store, baby, clock) = try fixture()
        let at = day(clock)
        let nap = try store.logSleep(from: at(9, 0), to: at(10, 0), for: baby)
        let later = try store.addSleepPart(from: at(13, 0), to: at(14, 0), toSession: nap.id, for: baby)
        #expect(later.effectiveSleepSessionID == nap.effectiveSleepSessionID)
        #expect(try store.sleepSessions(for: baby, from: .distantPast, to: .distantFuture).count == 1)
    }

    // MARK: - Delete

    @Test func deleteSessionTombstonesEveryMember() throws {
        let (store, baby, clock) = try fixture()
        let at = day(clock)
        let left = try store.logBreast(side: .left, from: at(10, 0), to: at(10, 12), for: baby)
        let right = try store.logBreast(side: .right, from: at(10, 13), to: at(10, 25), for: baby)
        let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: at(10, 30))
        #expect([right, bottle].allSatisfy { $0.effectiveFeedingSessionID == left.id })

        try store.deleteSession(.feed, rootID: left.id, for: baby)

        #expect(try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture).isEmpty)
        #expect(try liveFeeds(store, baby).isEmpty)
        #expect([left, right, bottle].allSatisfy { $0.isTombstoned })
    }

    @Test func deleteSessionFreesARunningPartThatJoinedIt() throws {
        let (store, baby, clock) = try fixture()
        let left = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-900), to: clock.now.addingTimeInterval(-300), for: baby)
        let running = try store.startPart(.breast(.right), for: baby)
        #expect(running.sessionID == left.id)

        try store.deleteSession(.feed, rootID: left.id, for: baby)

        #expect(left.isTombstoned)
        #expect(try store.activeTimer(kind: .feed, for: baby)?.sessionID == nil)
    }

    @Test func deletingTheFirstPartPromotesTheNextAsRoot() throws {
        let (store, baby, clock) = try fixture()
        let at = day(clock)
        let left = try store.logBreast(side: .left, from: at(10, 0), to: at(10, 12), for: baby)
        let right = try store.logBreast(side: .right, from: at(10, 13), to: at(10, 25), for: baby)
        let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: at(10, 30))
        #expect([right, bottle].allSatisfy { $0.effectiveFeedingSessionID == left.id })

        try store.deleteFeedPart(left.id, for: baby)

        let sessions = try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture)
        #expect(sessions.map(\.feedIDs) == [[right.id, bottle.id]])
        #expect(sessions.map(\.id) == [right.id])
        let live = try liveFeeds(store, baby)
        #expect(Set(live.map(\.id)) == [right.id, bottle.id])
        #expect(live.allSatisfy { $0.effectiveFeedingSessionID == right.id })
        #expect(left.isTombstoned)
    }

    @Test func deletingTheFirstPartRelinksARunningPartToTheNewRoot() throws {
        let (store, baby, clock) = try fixture()
        let left = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-1_500), to: clock.now.addingTimeInterval(-900), for: baby)
        let right = try store.logBreast(side: .right, from: clock.now.addingTimeInterval(-800), to: clock.now.addingTimeInterval(-200), for: baby)
        let running = try store.startPart(.bottle(.formula), for: baby)
        #expect(running.sessionID == left.id)

        try store.deleteFeedPart(left.id, for: baby)

        #expect(right.feedingSessionID == nil)
        #expect(try store.activeTimer(kind: .feed, for: baby)?.sessionID == right.id)
    }

    @Test func deletingTheFirstSleepPromotesTheNextAsRoot() throws {
        let (store, baby, clock) = try fixture()
        let at = day(clock)
        let first = try store.logSleep(from: at(13, 0), to: at(13, 40), for: baby)
        let second = try store.logSleep(from: at(13, 45), to: at(14, 30), for: baby)
        let third = try store.logSleep(from: at(14, 35), to: at(15, 0), for: baby)
        #expect([second, third].allSatisfy { $0.effectiveSleepSessionID == first.id })

        try store.deleteSleepPart(first.id, for: baby)

        let sessions = try store.sleepSessions(for: baby, from: .distantPast, to: .distantFuture)
        #expect(sessions.map(\.sleepIDs) == [[second.id, third.id]])
        #expect([second, third].allSatisfy { $0.effectiveSleepSessionID == second.id })
        #expect(first.isTombstoned)
    }

    @Test func deletingAMiddlePartKeepsTheSessionAndItsRoot() throws {
        let (store, baby, clock) = try fixture()
        let at = day(clock)
        let left = try store.logBreast(side: .left, from: at(10, 0), to: at(10, 12), for: baby)
        let right = try store.logBreast(side: .right, from: at(10, 13), to: at(10, 25), for: baby)
        let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: at(10, 30))

        try store.deleteFeedPart(right.id, for: baby)

        let sessions = try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture)
        #expect(sessions.map(\.feedIDs) == [[left.id, bottle.id]])
        #expect(sessions.map(\.id) == [left.id])
        #expect(right.isTombstoned)
    }

    @Test func removeComponentDropsOneAndDeletesTheEntryWithTheLast() throws {
        let (store, baby, clock) = try fixture()
        let at = day(clock)
        // An older one-entry feed holding two parts, as History's session
        // sheet lists them (H-R2).
        let left = FeedComponent.breast(id: UUID(), side: .left, seconds: 600, startedAt: at(10, 0), endedAt: at(10, 10))
        let bottle = FeedComponent.bottle(id: UUID(), volumeML: 60, contents: .formula, startedAt: at(10, 15))
        let feed = FeedEntry(babyID: baby, kind: .breast, startedAt: at(10, 0), timeZoneID: "UTC", realm: store.activeRealm)
        feed.components = [left, bottle]
        feed.endedAt = at(10, 15)
        store.context.insert(feed)
        try store.record(.create, on: feed)

        try store.removeComponent(left.id, fromFeed: feed.id, for: baby)

        #expect(feed.components == [bottle])
        #expect(feed.kind == .bottle)
        #expect(feed.startedAt == at(10, 15))
        #expect(feed.endedAt == at(10, 15))
        #expect(!feed.isTombstoned)
        #expect(try liveFeeds(store, baby).map(\.id) == [feed.id])

        try store.removeComponent(bottle.id, fromFeed: feed.id, for: baby)

        #expect(feed.isTombstoned)
        #expect(try liveFeeds(store, baby).isEmpty)
    }

    // MARK: - Previous session

    @Test func previousSessionSkipsSolidsAndIsNilWhenNoneEarlier() throws {
        let (store, baby, clock) = try fixture()
        let at = day(clock)
        let early = try store.logBreast(side: .left, from: at(7, 30), to: at(7, 45), for: baby)
        _ = try store.logSolids(foods: ["pear"], amount: .some, for: baby, at: at(9, 0))
        let late = try store.logBreast(side: .right, from: at(10, 23), to: at(10, 40), for: baby)
        #expect(late.feedingSessionID == nil)

        let previous = try store.previousSession(.feed, before: late.id, for: baby)

        #expect(previous == PreviousSession(rootID: early.id, lastMemberID: early.id, startedAt: at(7, 30), endedAt: at(7, 45)))
        #expect(try store.previousSession(.feed, before: early.id, for: baby) == nil)
    }

    @Test func previousSleepSessionFindsTheEarlierSleep() throws {
        let (store, baby, clock) = try fixture()
        let at = day(clock)
        let morning = try store.logSleep(from: at(9, 0), to: at(9, 40), for: baby)
        let wake = try store.logSleep(from: at(9, 50), to: at(10, 30), for: baby)
        let afternoon = try store.logSleep(from: at(13, 0), to: at(14, 0), for: baby)
        #expect(wake.effectiveSleepSessionID == morning.id)
        #expect(afternoon.sleepSessionID == nil)

        let previous = try store.previousSession(.sleep, before: afternoon.id, for: baby)

        #expect(previous == PreviousSession(rootID: morning.id, lastMemberID: wake.id, startedAt: at(9, 0), endedAt: at(10, 30)))
        #expect(try store.previousSession(.sleep, before: morning.id, for: baby) == nil)
    }

    // MARK: - Part start check

    @Test func checkPartStartRefusesAStartBeforeThePreviousPartEnds() throws {
        let (store, baby, clock) = try fixture()
        let at = day(clock)
        let left = try store.logBreast(side: .left, from: at(10, 0), to: at(10, 12), for: baby)
        let right = try store.logBreast(side: .right, from: at(10, 15), to: at(10, 30), for: baby)
        #expect(right.effectiveFeedingSessionID == left.id)

        #expect(try store.previousPartEnd(ofFeed: right.id, for: baby) == at(10, 12))
        #expect(try store.previousPartEnd(ofFeed: left.id, for: baby) == nil)
        #expect(throws: PartError.startBeforePreviousPart) {
            try store.checkPartStart(at(10, 10), ofFeed: right.id, for: baby)
        }
        try store.checkPartStart(at(10, 12), ofFeed: right.id, for: baby)
        try store.checkPartStart(at(9, 0), ofFeed: left.id, for: baby)
    }

    /// H-R15: a part moved past the next part's start would reorder its
    /// session behind the root, where Split does nothing.
    @Test func checkPartStartRefusesAStartAfterTheNextPartBegan() throws {
        let (store, baby, clock) = try fixture()
        let at = day(clock)
        let left = try store.logBreast(side: .left, from: at(10, 0), to: at(10, 12), for: baby)
        let right = try store.logBreast(side: .right, from: at(10, 15), to: at(10, 30), for: baby)
        let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: at(10, 40))
        #expect(bottle.effectiveFeedingSessionID == left.id)

        #expect(try store.nextPartStart(ofFeed: left.id, for: baby) == at(10, 15))
        #expect(try store.nextPartStart(ofFeed: right.id, for: baby) == at(10, 40))
        #expect(try store.nextPartStart(ofFeed: bottle.id, for: baby) == nil)
        #expect(throws: PartError.startAfterNextPart) {
            try store.checkPartStart(at(10, 16), ofFeed: left.id, for: baby)
        }
        #expect(throws: PartError.startAfterNextPart) {
            try store.checkPartStart(at(10, 41), ofFeed: right.id, for: baby)
        }
        try store.checkPartStart(at(10, 15), ofFeed: left.id, for: baby)
        try store.checkPartStart(at(10, 40), ofFeed: right.id, for: baby)
        try store.checkPartStart(at(19, 0), ofFeed: bottle.id, for: baby)
    }

    /// H-R14 and H-R15: a sleep part has the same two bounds.
    @Test func checkSleepPartStartRefusesBothBounds() throws {
        let (store, baby, clock) = try fixture()
        let at = day(clock)
        let first = try store.logSleep(from: at(9, 0), to: at(9, 40), for: baby)
        let second = try store.logSleep(from: at(9, 50), to: at(10, 30), for: baby)
        let third = try store.logSleep(from: at(10, 40), to: at(11, 0), for: baby)
        #expect(second.effectiveSleepSessionID == first.id)
        #expect(third.effectiveSleepSessionID == first.id)

        #expect(try store.previousPartEnd(ofSleep: first.id, for: baby) == nil)
        #expect(try store.previousPartEnd(ofSleep: second.id, for: baby) == at(9, 40))
        #expect(try store.nextPartStart(ofSleep: second.id, for: baby) == at(10, 40))
        #expect(try store.nextPartStart(ofSleep: third.id, for: baby) == nil)
        #expect(throws: PartError.startBeforePreviousPart) {
            try store.checkSleepPartStart(at(9, 30), ofSleep: second.id, for: baby)
        }
        #expect(throws: PartError.startAfterNextPart) {
            try store.checkSleepPartStart(at(10, 41), ofSleep: second.id, for: baby)
        }
        #expect(throws: PartError.startAfterNextPart) {
            try store.checkSleepPartStart(at(9, 51), ofSleep: first.id, for: baby)
        }
        try store.checkSleepPartStart(at(9, 40), ofSleep: second.id, for: baby)
        try store.checkSleepPartStart(at(10, 40), ofSleep: second.id, for: baby)
        try store.checkSleepPartStart(at(8, 0), ofSleep: first.id, for: baby)
        #expect(throws: SleepSessionError.missingSleep) {
            try store.checkSleepPartStart(at(8, 0), ofSleep: UUID(), for: baby)
        }
    }

    // MARK: - A session id that is also a member's id

    /// B was joined into A while parts that continued B kept B's id as
    /// their session key — the other phone's running part, say, stopped
    /// before the Join reached it. `b.id` then names two things: the live
    /// member B of session A, and the session {x, bottle}. A caller holding
    /// a session id means the second.
    private func collidingOrphan(_ store: BabyStore, _ baby: UUID, _ clock: TestDateProvider) throws
        -> (a: FeedEntry, b: FeedEntry, x: FeedEntry, bottle: FeedEntry) {
        let a = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-4 * 3_600),
                                    to: clock.now.addingTimeInterval(-4 * 3_600 + 600), for: baby)
        let b = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-900),
                                    to: clock.now.addingTimeInterval(-300), for: baby)
        try store.combineFeeds([a.id, b.id], for: baby)
        let x = try store.logBreast(side: .right, from: clock.now.addingTimeInterval(-240),
                                    to: clock.now.addingTimeInterval(-120), for: baby)
        x.feedingSessionID = b.id
        try store.context.save()
        let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: clock.now.addingTimeInterval(-60))
        let sessions = try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture)
        #expect(sessions.map(\.feedIDs) == [[a.id, b.id], [x.id, bottle.id]])
        #expect(sessions.map(\.id) == [a.id, b.id])
        return (a, b, x, bottle)
    }

    @Test func deleteSessionByAnIdThatIsAlsoAMembersDeletesThatSession() throws {
        let (store, baby, clock) = try fixture()
        let (a, b, x, bottle) = try collidingOrphan(store, baby, clock)

        try store.deleteSession(.feed, rootID: b.id, for: baby)

        #expect(x.isTombstoned && bottle.isTombstoned)
        #expect(!a.isTombstoned && !b.isTombstoned)
    }

    @Test func addPartAndPreviousSessionTakeTheSessionAnIdNames() throws {
        let (store, baby, clock) = try fixture()
        let (a, b, _, _) = try collidingOrphan(store, baby, clock)

        #expect(try store.previousSession(.feed, before: b.id, for: baby)?.rootID == a.id)
        let added = try store.addFeedPart(.bottle(volumeML: 30, contents: .formula, at: clock.now), toSession: b.id, for: baby)
        #expect(added.effectiveFeedingSessionID == b.id)
    }

    @Test func deleteSleepSessionByAnIdThatIsAlsoAMembersDeletesThatSession() throws {
        let (store, baby, clock) = try fixture()
        let a = try store.logSleep(from: clock.now.addingTimeInterval(-8_200), to: clock.now.addingTimeInterval(-4_600), for: baby)
        let b = try store.logSleep(from: clock.now.addingTimeInterval(-3_000), to: clock.now.addingTimeInterval(-1_800), for: baby)
        try store.combineSleeps([a.id, b.id], for: baby)
        let x = try store.logSleep(from: clock.now.addingTimeInterval(-1_500), to: clock.now.addingTimeInterval(-600), for: baby)
        x.sleepSessionID = b.id
        try store.context.save()
        #expect(try store.sleepSessions(for: baby, from: .distantPast, to: .distantFuture).map(\.sleepIDs) == [[a.id, b.id], [x.id]])

        try store.deleteSession(.sleep, rootID: b.id, for: baby)

        #expect(x.isTombstoned)
        #expect(!a.isTombstoned && !b.isTombstoned)
    }
}
