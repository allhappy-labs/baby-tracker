import Testing
import Foundation
import BabyCore
@testable import BabyStore

@MainActor
private func noonLocal() -> Date {
    var parts = DateComponents()
    parts.year = 2026; parts.month = 6; parts.day = 15; parts.hour = 12
    return Calendar.current.date(from: parts)!
}

@MainActor
@Test func loggingSolidsWritesAFeedWithNeitherSidesNorVolume() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    let entry = try store.logSolids(foods: ["pear", "oat"], amount: .some, for: baby.id)

    #expect(entry.kind == .solids)
    #expect(entry.components.map(\.solidFoods) == [["pear", "oat"]])
    #expect(entry.components.map(\.solidsAmount) == [.some])
    #expect(entry.totalBreastSeconds == 0)
    #expect(entry.bottleComponents.isEmpty)
    #expect(entry.endedAt == clock.now)
    #expect(entry.isCombo == false)
}

@MainActor
@Test func solidsAreChangeLogged() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(noonLocal()))
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    _ = try store.logSolids(foods: ["pear"], amount: .taste, for: baby.id)

    #expect(try store.changeLog().last?.entityType == "FeedEntry")
    #expect(try store.changeLog().last?.operation == .create)
}

@MainActor
@Test func solidsAcceptABackdatedTime() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    let earlier = clock.now.addingTimeInterval(-2_400)
    let entry = try store.logSolids(foods: ["pear"], amount: .lots, for: baby.id, at: earlier)

    #expect(entry.startedAt == earlier)
}
