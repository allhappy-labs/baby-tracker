import Testing
import Foundation
import BabyCore
import BabyStore
@testable import LoggingFeature

@MainActor
@Test func bottleDefaultsToThePreviousBottle() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    _ = try store.logBottle(volumeML: 120, contents: .formula, for: baby.id)
    clock.advance(by: 60)
    _ = try store.logBottle(volumeML: 120, contents: .formula, for: baby.id)
    clock.advance(by: 60)
    _ = try store.logBottle(volumeML: 90, contents: .breastMilk, for: baby.id)

    let defaults = LogDefaults(store: store)
    // The previous bottle, not the most common one.
    #expect(try defaults.lastBottleVolumeML(for: baby.id) == 90)
    #expect(try defaults.lastBottleContents(for: baby.id) == .breastMilk)
}

@MainActor
@Test func bottleDefaultsAreSensibleWithNoHistory() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let defaults = LogDefaults(store: store)

    #expect(try defaults.lastBottleVolumeML(for: baby.id) == 120)
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

    #expect(try LogDefaults(store: store).lastBottleVolumeML(for: baby.id) == 120)
}

/// A bottle saved at 0 ml is never the default: every total leaves a
/// zero-volume bottle out, and a Lock Screen stop would save another.
@MainActor
@Test func aZeroVolumeLastBottleFallsBackToTheDefault() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    _ = try store.logBottle(volumeML: 90, contents: .formula, for: baby.id)
    clock.advance(by: 60)
    _ = try store.logBottle(volumeML: 0, contents: .formula, for: baby.id)

    #expect(try LogDefaults(store: store).lastBottleVolumeML(for: baby.id) == 120)
}

/// An older phone's feed can hold several bottles: the last one is the
/// previous bottle.
@MainActor
@Test func theLastBottleOfAFeedWithSeveralIsThePreviousBottle() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000)))
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let feed = FeedEntry(babyID: baby.id, kind: .bottle, startedAt: store.now, timeZoneID: "UTC")
    feed.components = [
        .bottle(id: UUID(), volumeML: 40, contents: .formula),
        .bottle(id: UUID(), volumeML: 70, contents: .breastMilk),
    ]
    feed.endedAt = store.now
    store.context.insert(feed)
    try store.record(.create, on: feed)

    #expect(try LogDefaults(store: store).lastBottleVolumeML(for: baby.id) == 70)
}
