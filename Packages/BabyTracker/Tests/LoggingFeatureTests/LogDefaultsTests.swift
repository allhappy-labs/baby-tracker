import Testing
import Foundation
import BabyCore
import BabyStore
@testable import LoggingFeature

@MainActor
@Test func bottleDefaultsToTheMostFrequentVolume() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    _ = try store.logBottle(volumeML: 120, contents: .formula, for: baby.id)
    _ = try store.logBottle(volumeML: 120, contents: .formula, for: baby.id)
    _ = try store.logBottle(volumeML: 90, contents: .breastMilk, for: baby.id)

    let defaults = LogDefaults(store: store)
    #expect(try defaults.mostUsedBottleVolumeML(for: baby.id) == 120)
    // Contents follows the most recent, not the most frequent — it changes
    // more often than volume does.
    #expect(try defaults.lastBottleContents(for: baby.id) == .breastMilk)
}

@MainActor
@Test func bottleDefaultsAreSensibleWithNoHistory() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let defaults = LogDefaults(store: store)

    #expect(try defaults.mostUsedBottleVolumeML(for: baby.id) == 120)
    #expect(try defaults.lastBottleContents(for: baby.id) == .formula)
}

@MainActor
@Test func breastFeedsDoNotPolluteBottleDefaults() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    let timer = try store.startPart(.breast(.left), for: baby.id)
    clock.advance(by: 900)
    _ = try store.stopFeed(timer)

    let defaults = LogDefaults(store: store)
    #expect(try defaults.mostUsedBottleVolumeML(for: baby.id) == 120)
}
