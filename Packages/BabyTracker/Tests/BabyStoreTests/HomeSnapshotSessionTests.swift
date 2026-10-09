import Testing
import Foundation
import BabyCore
@testable import BabyStore

@MainActor @Suite struct HomeSnapshotSessionTests {
    @Test func aStoppedPartOpensABreakUntilTheLimit() throws {
        let (store, baby, clock) = try fixture()            // same helper as OnePartTimerTests
        let left = try store.startPart(.breast(.left), for: baby)
        clock.advance(by: 300)
        _ = try store.stopFeed(left)
        clock.advance(by: 40)
        #expect(try store.homeSnapshot(for: baby).feedBreak?.activeSeconds == 300)
        clock.advance(by: 1_800)
        #expect(try store.homeSnapshot(for: baby).feedBreak == nil)
    }

    @Test func aJoinedRunningPartExposesTheEarlierParts() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300))
        _ = try store.startPart(.breast(.right), for: baby)
        let snapshot = try store.homeSnapshot(for: baby)
        #expect(snapshot.feedBreak == nil)
        #expect(snapshot.runningFeedSession?.activeSeconds == 300)
    }

    private func local(_ day: Int, _ hour: Int, _ minute: Int) -> Date {
        Calendar.current.date(from: DateComponents(year: 2026, month: 10, day: day, hour: hour, minute: minute))!
    }

    @Test func aJoinedSleepAcrossMidnightCountsOnItsFirstDay() throws {
        let clock = TestDateProvider(local(1, 23, 59))
        let store = try BabyStore.inMemory(dateProvider: clock)
        store.breakLimits = { .standard }
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil).id
        let first = try store.logSleep(from: local(1, 23, 30), to: local(1, 23, 55), for: baby)
        let second = try store.logSleep(from: local(2, 0, 5), to: local(2, 1, 0), for: baby)
        #expect(second.sleepSessionID == first.id)
        #expect(try store.homeSnapshot(for: baby).todaySleepSeconds == 25 * 60 + 55 * 60)
        clock.advance(by: local(2, 8, 0).timeIntervalSince(clock.now))
        #expect(try store.homeSnapshot(for: baby).todaySleepSeconds == 0)
    }

    @Test func aFeedSessionAcrossMidnightCountsOnce() throws {
        let clock = TestDateProvider(local(1, 23, 59))
        let store = try BabyStore.inMemory(dateProvider: clock)
        store.breakLimits = { .standard }
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil).id
        let left = try store.logBreast(side: .left, from: local(1, 23, 50), to: local(1, 23, 58), for: baby)
        let right = try store.logBreast(side: .right, from: local(2, 0, 5), to: local(2, 0, 12), for: baby)
        #expect(right.feedingSessionID == left.id)
        #expect(try store.homeSnapshot(for: baby).todayFeedCount == 1)
        clock.advance(by: local(2, 8, 0).timeIntervalSince(clock.now))
        #expect(try store.homeSnapshot(for: baby).todayFeedCount == 0)
    }

    @Test func awakeIsMeasuredFromTheLastSleepPart() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopSleep(try store.startSleep(for: baby).advancing(clock, by: 1_200))
        clock.advance(by: 300)
        _ = try store.stopSleep(try store.startSleep(for: baby).advancing(clock, by: 600))
        #expect(try store.homeSnapshot(for: baby).awakeSince == clock.now)
    }
}
