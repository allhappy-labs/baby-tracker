import Testing
import Foundation
import BabyCore
@testable import BabyStore

@MainActor @Suite struct LogAfterTheFactTests {
    private func fixture() throws -> (BabyStore, UUID, TestDateProvider) {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
        let store = try BabyStore.inMemory(dateProvider: clock)
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        return (store, baby.id, clock)
    }

    @Test func aBottleWithAnEndStoresItsLength() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-900)
        let feed = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: start,
                                       endedAt: clock.now, note: "slow")
        #expect(feed.components.first?.bottleSeconds == 900)
        #expect(feed.components.first?.startedAt == start)
        #expect(feed.components.first?.endedAt == clock.now)
        #expect(feed.startedAt == start)
        #expect(feed.endedAt == clock.now)
        #expect(feed.note == "slow")
    }

    @Test func aBreastSideKeepsItsNote() throws {
        let (store, baby, clock) = try fixture()
        let feed = try store.logBreast(side: .right, from: clock.now.addingTimeInterval(-600), to: clock.now,
                                       for: baby, note: "sleepy")
        #expect(feed.note == "sleepy")
        #expect(try store.changeLog().last?.operation == .create)
    }

    @Test func aSleepKeepsItsKindAndNote() throws {
        let (store, baby, clock) = try fixture()
        let sleep = try store.logSleep(from: clock.now.addingTimeInterval(-3600), to: clock.now, for: baby,
                                       kindOverride: .night, note: "car")
        #expect(sleep.kindOverride == .night)
        #expect(sleep.note == "car")
    }
}
