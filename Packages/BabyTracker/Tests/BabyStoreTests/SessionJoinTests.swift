import Testing
import Foundation
import BabyCore
@testable import BabyStore

@MainActor @Suite struct SessionJoinTests {
    @Test func aFeedStartingExactlyAtTheLimitJoinsAndOneSecondLaterDoesNot() throws {
        let (store, baby, clock) = try fixture()
        let first = try store.logBottle(volumeML: 60, contents: .formula, for: baby)
        let atLimit = clock.now.addingTimeInterval(1_800)
        #expect(try store.continuedSessionID(.feed, for: baby, startingAt: atLimit) == first.effectiveFeedingSessionID)
        #expect(try store.continuedSessionID(.feed, for: baby, startingAt: atLimit.addingTimeInterval(1)) == nil)
    }

    @Test func aDiaperOrMedInTheGapDoesNotBlockButASleepDoes() throws {
        let (store, baby, clock) = try fixture()
        let first = try store.logBottle(volumeML: 60, contents: .formula, for: baby)
        clock.advance(by: 300)
        _ = try store.logDiaper(.wet, for: baby)
        _ = try store.logSupplement(name: "Vitamin D", for: baby)
        clock.advance(by: 300)
        #expect(try store.continuedSessionID(.feed, for: baby, startingAt: clock.now) == first.effectiveFeedingSessionID)
        _ = try store.logSleep(from: clock.now.addingTimeInterval(-200), to: clock.now.addingTimeInterval(-100), for: baby)
        #expect(try store.continuedSessionID(.feed, for: baby, startingAt: clock.now) == nil)
    }

    @Test func aRunningSleepInTheGapBlocksAFeedJoin() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.logBottle(volumeML: 60, contents: .formula, for: baby)
        clock.advance(by: 120)
        _ = try store.startSleep(for: baby)
        clock.advance(by: 120)
        #expect(try store.continuedSessionID(.feed, for: baby, startingAt: clock.now) == nil)
    }

    @Test func solidsNeverJoin() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.logSolids(foods: ["pear"], amount: .some, for: baby)
        clock.advance(by: 60)
        #expect(try store.continuedSessionID(.feed, for: baby, startingAt: clock.now) == nil)
    }

    @Test func sleepJoinsWithinFifteenMinutes() throws {
        let (store, baby, clock) = try fixture()
        let nap = try store.logSleep(from: clock.now.addingTimeInterval(-3_600), to: clock.now, for: baby)
        #expect(try store.continuedSessionID(.sleep, for: baby, startingAt: clock.now.addingTimeInterval(900)) == nap.effectiveSleepSessionID)
        #expect(try store.continuedSessionID(.sleep, for: baby, startingAt: clock.now.addingTimeInterval(901)) == nil)
    }
}
