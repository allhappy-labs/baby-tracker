import Testing
import Foundation
import SwiftData
import BabyCore
@testable import BabyStore

// Every test here is on the main actor, like every store suite. Building and
// saving live models off it ran them on the cooperative pool while
// `BaselineStoreTests` opened an older-schema store, which is the pairing
// that can abort the test binary inside Core Data.

@MainActor @Test func inMemoryContainerAcceptsEveryModel() throws {
    let container = try BabySchema.container(inMemory: true)
    let context = ModelContext(container)

    let baby = Baby(name: "Mira", birthDate: .now)
    context.insert(baby)

    let feed = FeedEntry(babyID: baby.id, kind: .breast, startedAt: .now, timeZoneID: "Europe/Berlin")
    feed.components = [.breast(id: UUID(), side: .left, seconds: 1_080)]
    context.insert(feed)

    try context.save()

    let feeds = try context.fetch(FetchDescriptor<FeedEntry>())
    #expect(feeds.count == 1)
    #expect(feeds[0].kind == .breast)
    #expect(feeds[0].components.first?.durationSeconds == 1_080)
}

@MainActor @Test func typedAccessorsReadAndWriteTheRawColumn() {
    let feed = FeedEntry(babyID: UUID(), kind: .bottle, startedAt: .now, timeZoneID: "UTC")

    #expect(feed.kindRaw == "bottle")

    feed.kind = .solids
    #expect(feed.kindRaw == "solids")
    #expect(feed.kind == .solids)
}

@MainActor @Test func comboFeedIsDetectedByFieldsNotByKind() {
    let feed = FeedEntry(babyID: UUID(), kind: .breast, startedAt: .now, timeZoneID: "UTC")
    #expect(feed.isCombo == false)

    feed.components = [
        .breast(id: UUID(), side: .left, seconds: 300),
        .bottle(id: UUID(), volumeML: 60, contents: .formula),
    ]
    #expect(feed.isCombo)
    // kind still records how it started; it does not gate which fields are set.
    #expect(feed.kind == .breast)
}

@MainActor @Test func entriesStartUndeletedAndStampTheirOwnTimezone() {
    let sleep = SleepEntry(babyID: UUID(), startedAt: .now, timeZoneID: "America/New_York")

    #expect(sleep.isTombstoned == false)
    #expect(sleep.timeZoneID == "America/New_York")
    #expect(sleep.endedAt == nil)
}

/// Moved from the deleted `FeedComponentMigrationTests`: the projections read
/// every component in order, and two bottles stay two.
@MainActor @Test func projectionsReadEveryOrderedComponentWithoutCollapsingBottles() {
    let feed = FeedEntry(
        id: UUID(uuidString: "20000000-0000-4000-8000-000000000001")!,
        babyID: UUID(uuidString: "10000000-0000-4000-8000-000000000001")!,
        kind: .breast,
        startedAt: Date(timeIntervalSince1970: 1_700_000_000),
        timeZoneID: "Europe/Zurich"
    )
    let ids = (1...4).map { UUID(uuidString: "30000000-0000-4000-8000-00000000000\($0)")! }
    feed.components = [
        .breast(id: ids[0], side: .left, seconds: 480),
        .breast(id: ids[1], side: .right, seconds: 360),
        .bottle(id: ids[2], volumeML: 50, contents: .breastMilk),
        .bottle(id: ids[3], volumeML: 30, contents: .formula),
    ]

    #expect(feed.components.map(\.id) == ids)
    #expect(feed.totalBreastSeconds == 840)
    #expect(feed.totalBottleVolumeML == 80)
    #expect(feed.lastSideUsed == .right)
    #expect(feed.isCombo)
    #expect(feed.bottleComponents.count == 2)
    #expect(feed.bottleComponents.map(\.bottleContents) == [.breastMilk, .formula])
}

/// Moved from the deleted `GrowthModelMigrationTests`: growth rows keep their
/// raw values, realm and calendar date through a save.
@MainActor @Test func growthModelsPersistRawValuesRealmAndCalendarDate() throws {
    let container = try BabySchema.container(inMemory: true)
    let context = ModelContext(container)
    let babyID = UUID(uuidString: "10000000-0000-4000-8000-000000000001")!
    let realm = RealmID.local
    let measuredAt = Date(timeIntervalSince1970: 1_780_000_000)
    let loggedAt = measuredAt.addingTimeInterval(3600.125)
    let profile = GrowthProfile(babyID: babyID, referenceSex: .girls,
        expectedDueDate: try LocalDate("2026-08-31"), modifiedAt: loggedAt, realm: realm)
    let measurement = MeasurementCheckIn(babyID: babyID, measuredAt: measuredAt,
        timeZoneID: "Pacific/Auckland", loggedAt: loggedAt,
        values: try MeasurementValues(weightKG: 7.125, lengthCM: 65.5, lengthPosition: .standing),
        createdBy: "device-a", realm: realm)
    let measurementID = measurement.id
    context.insert(profile)
    context.insert(measurement)
    try context.save()
    let fresh = ModelContext(container)
    let savedProfile = try #require(try fresh.fetch(FetchDescriptor<GrowthProfile>()).first)
    #expect(savedProfile.id.uuidString.lowercased() == "2742cf11-d3ad-51bb-bbfa-24147f4b0aae")
    #expect(savedProfile.babyID == babyID)
    #expect(savedProfile.referenceSex == .girls)
    #expect(savedProfile.expectedDueDate?.iso8601 == "2026-08-31")
    #expect(savedProfile.realmID == realm)
    let saved = try #require(try fresh.fetch(FetchDescriptor<MeasurementCheckIn>()).first)
    #expect(saved.id == measurementID)
    #expect(saved.babyID == babyID)
    #expect(saved.measuredAt == measuredAt)
    #expect(saved.loggedAt == loggedAt)
    #expect(saved.timeZoneID == "Pacific/Auckland")
    #expect(saved.weightKG == 7.125)
    #expect(saved.lengthCM == 65.5)
    #expect(saved.headCircumferenceCM == nil)
    #expect(saved.lengthPosition == .standing)
    #expect(saved.createdBy == "device-a")
    #expect(saved.realmID == realm)
    #expect(saved.lastModifiedAt == loggedAt)
    #expect(!saved.isTombstoned)
}
