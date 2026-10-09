import Foundation
import Testing
import BabyCore
@testable import BabyStore

@MainActor
@Test func combineAndSeparatePreserveEveryFeed() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let first = try store.logBottle(volumeML: 40, contents: .formula, for: baby.id)
    clock.advance(by: 4_000)
    let second = try store.logBottle(volumeML: 30, contents: .formula, for: baby.id)
    clock.advance(by: 1_000)
    let third = try store.logBottle(volumeML: 20, contents: .formula, for: baby.id)
    try store.combineFeeds([first.id, second.id, third.id], for: baby.id)
    let groups = try store.feedingSessions(for: baby.id, from: first.startedAt.addingTimeInterval(-1), to: third.startedAt.addingTimeInterval(1))
    #expect(groups.count == 1)
    #expect(groups[0].feedIDs == [first.id, second.id, third.id])
    #expect(groups[0].measuredVolumeML == 90)
    #expect(try store.homeSnapshot(for: baby.id).todayFeedCount == 1)
    try store.separateFeed(at: second.id, for: baby.id)
    let split = try store.feedingSessions(for: baby.id, from: first.startedAt.addingTimeInterval(-1), to: third.startedAt.addingTimeInterval(1))
    #expect(split.count == 2)
    #expect(split.map(\.feedIDs) == [[first.id], [second.id, third.id]])
}

@MainActor
@Test func combineFeedsRejectsASolidsFeed() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let bottle = try store.logBottle(volumeML: 40, contents: .formula, for: baby.id)
    clock.advance(by: 4_000)
    let solids = try store.logSolids(foods: ["Banana"], amount: .taste, for: baby.id)
    #expect(throws: FeedingSessionError.solidsNeverJoin) {
        try store.combineFeeds([bottle.id, solids.id], for: baby.id)
    }
}

