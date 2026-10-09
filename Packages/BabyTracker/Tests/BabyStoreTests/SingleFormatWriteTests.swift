import Foundation
import SwiftData
import Testing
@testable import BabyCore
@testable import BabyProtocol
@testable import BabyStore

/// The client writes one shape per kind: feeds and timers as components,
/// every operation at the kind's current payload version. The server stores
/// nothing older (schema 27), and every phone starts from a fresh install
/// on the baseline schema, which has no scalar feed or timer columns.
@MainActor
@Suite("One format per kind")
struct SingleFormatWriteTests {
    // MARK: Payload versions

    static let currentVersions: [(WireEntityKind, Int64)] = [
        (.feed, 4), (.timer, 5), (.sleep, 2), (.baby, 1), (.diaper, 1),
        (.supplement, 1), (.growthProfile, 1), (.measurement, 1),
    ]

    /// The version is the kind's, not something read off the payload: a
    /// delete names only the baby, and still declares the current version.
    @Test func everyOperationDeclaresTheKindsCurrentVersion() throws {
        let payloads: [WireOperation: [String: Any]] = [
            .create: ["baby_id": Fixture.babyID.uuidString.lowercased()],
            .update: ["baby_id": Fixture.babyID.uuidString.lowercased()],
            .delete: ["baby_id": Fixture.babyID.uuidString.lowercased()],
        ]
        for (kind, version) in Self.currentVersions {
            #expect(kind.currentPayloadVersion == version, "\(kind)")
            for (operation, payload) in payloads {
                let mutation = try MutationFactory.mutation(
                    entityID: UUID(), entityKind: kind, operation: operation,
                    observedEntityVersion: 1, at: .now, payload: payload
                )
                #expect(mutation.payloadVersion == version, "\(kind) \(operation)")
            }
        }
    }

    @Test func aFeedAndASleepDeleteDeclareTheirCurrentVersions() throws {
        let store = try BabyStore.sharedFixture()
        let feed = try store.logBottle(volumeML: 90, contents: .formula, for: Fixture.babyID)
        let sleep = try store.logSleep(from: store.now.addingTimeInterval(-600), to: store.now, for: Fixture.babyID)
        try store.clearOutboxForTesting()

        try store.softDelete(feed)
        try store.softDelete(sleep)

        let feedDelete = try WireFixture.onlyMutation(in: store, for: feed.id)
        #expect(feedDelete.operation == .delete)
        #expect(feedDelete.payloadVersion == 4)
        let sleepDelete = try WireFixture.onlyMutation(in: store, for: sleep.id)
        #expect(sleepDelete.operation == .delete)
        #expect(sleepDelete.payloadVersion == 2)
    }

    // MARK: Content lives in components

    @Test func loggedFeedsKeepTheirContentInComponents() throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
        let store = try BabyStore.inMemory(dateProvider: clock)
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil).id

        let bottle = try store.logBottle(volumeML: 120, contents: .formula, for: baby)
        try store.setBottleVolume(90, onFeed: bottle.id)
        let solids = try store.logSolids(foods: ["pear"], amount: .some, for: baby)
        let breast = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-600), to: clock.now, for: baby)

        #expect(bottle.totalBottleVolumeML == 90)
        #expect(solids.components.first?.solidFoods == ["pear"])
        #expect(breast.totalBreastSeconds == 600)
    }

    @Test func aTimedFeedKeepsItsPartsInComponents() throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
        let store = try BabyStore.inMemory(dateProvider: clock)
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil).id

        // A bottle finished mid-feed, then edited while the feed still runs.
        let timer = ActiveTimer(babyID: baby, kind: .feed, startedAt: clock.now, timeZoneID: "UTC")
        timer.currentBottleStartedAt = clock.now
        store.context.insert(timer)
        try store.context.save()
        clock.advance(by: 120)
        try store.finishBottle(volumeML: 60, contents: .breastMilk, on: timer)
        let bottleID = try #require(timer.completedComponents.first?.id)
        try store.editRunningTimer(id: timer.id, startedAt: nil, components: [
            .bottle(id: bottleID, volumeML: 70, contents: .formula),
        ])
        #expect(timer.completedComponents.map(\.volumeML) == [70])

        let feed = try #require(try store.stopFeed(timer))
        #expect(feed.totalBottleVolumeML == 70)
    }

    /// A feed and a timer arriving from the family land in components.
    @Test func remoteApplyWritesComponents() throws {
        let sender = try BabyStore.sharedFixture()
        let feed = try sender.logBottle(volumeML: 80, contents: .formula, for: Fixture.babyID)
        let receiver = try BabyStore.sharedFixture()
        try WireFixture.deliver(try WireFixture.onlyMutation(in: sender, for: feed.id), to: receiver)

        let applied = try #require(try receiver.feedEntryForTesting(feed.id))
        #expect(applied.totalBottleVolumeML == 80)

        let state = try #require(try RemoteTimerState(fields: [
            "baby_id": Fixture.babyID.uuidString.lowercased(),
            "timer_kind": "feed",
            "started_at": "2026-09-20T12:00:00Z",
            "time_zone_id": "UTC",
            "current_side": "right",
            "current_side_started_at": "2026-09-20T12:05:00Z",
            "completed_components": [
                ["id": UUID().uuidString.lowercased(), "kind": "breast", "side": "left", "seconds": 300],
                ["id": UUID().uuidString.lowercased(), "kind": "bottle", "volume_ml": 50, "contents": "breast_milk"],
            ],
            "session_id": NSNull(),
        ]))
        let timer = ActiveTimer(babyID: Fixture.babyID, kind: .feed, startedAt: .now, timeZoneID: "UTC")
        #expect(state.write(to: timer))
        #expect(timer.completedComponents.count == 2)
    }

    /// Moved from the deleted `LegacyFeedUpgradeTests`: a feed that has
    /// components is sent exactly as it is.
    @Test func compositeFeedIsUnchanged() throws {
        let store = try BabyStore.sharedFixture()
        let feed = FeedEntry(babyID: Fixture.babyID, kind: .breast, startedAt: .now, timeZoneID: "UTC", realm: store.activeRealm)
        let composite: [FeedComponent] = [
            .bottle(id: UUID(), volumeML: 90, contents: .formula),
            .breast(id: UUID(), side: .right, seconds: 120),
        ]
        feed.components = composite
        store.context.insert(feed)
        try store.record(.create, on: feed)

        let sent = try WireFeedComponent.decodeComponents(
            "components", of: try WireFixture.onlyMutation(in: store, for: feed.id).payload
        )
        #expect(sent.map(\.id) == composite.map(\.id))
        #expect(sent.map(\.kind) == [.bottle, .breast])
    }

    // MARK: A feed with nothing in it

    /// A row with no components reads as a feed with nothing in it.
    @Test func aFeedWithNoComponentsReadsAsEmpty() throws {
        let feed = FeedEntry(babyID: Fixture.babyID, kind: .breast, startedAt: .now, timeZoneID: "UTC")

        #expect(!feed.isCombo)
        #expect(feed.totalBreastSeconds == 0)
        #expect(feed.totalBottleVolumeML == 0)
        #expect(feed.lastSideUsed == nil)
        #expect(WireFixture.field("components", of: try MutationFactory.jsonValue(feed.payloadFields())) == .array([]))
        #expect(!feed.hasWireContent)
        #expect(BabyStore.projectSessions([feed]).first?.activeSeconds == 0)
    }

    /// Moved from the deleted `LegacyFeedUpgradeTests`. A feed with nothing
    /// to send would be refused, so it stays on the phone and never enters
    /// the outbox — not on create, not on a later edit.
    @Test func feedWithNothingToSendNeverEntersTheOutbox() throws {
        let store = try BabyStore.sharedFixture()
        let feed = Self.emptyFeed(in: store.activeRealm)
        store.context.insert(feed)
        try store.record(.create, on: feed)
        feed.note = "edited"
        try store.update(feed)

        #expect(try store.pendingOutbox().filter { $0.entityID == feed.id }.isEmpty)
        #expect(try store.feedEntryForTesting(feed.id) != nil)
        #expect(try store.measuredBottleSteps(for: Fixture.babyID, from: .distantPast, to: .distantFuture).isEmpty)
    }

    /// Moved from the deleted `LegacyFeedUpgradeTests`. Nor is it frozen
    /// into the owner's import, so the resolver is never promised a record
    /// the server would refuse.
    @Test func feedWithNothingToSendIsLeftOutOfTheImport() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let empty = Self.emptyFeed(babyID: baby.id, in: .local)
        store.context.insert(empty)
        try store.record(.create, on: empty)
        let kept = try store.logBottle(volumeML: 120, contents: .breastMilk, for: baby.id)
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .preserveLocalIDs)

        let manifest = try store.freezeImportManifest(checkpointID: checkpoint)
        try store.saveMapping([:], checkpointID: checkpoint)
        try store.rewriteAndMoveWorkspace(checkpointID: checkpoint, to: ImportFixture.realm)

        #expect(manifest.sourceBases[EntityKey(kind: "feed", id: empty.id)] == nil)
        #expect(Set(try manifest.records().map(\.recordID)) == [kept.id, GrowthProfileID.forBaby(baby.id)])
        let queued = try store.liveOutboxRows(in: ImportFixture.realm).map(\.entityID)
        #expect(Set(queued) == [kept.id, GrowthProfileID.forBaby(baby.id)])
        // Still the owner's, in the family realm, just not sent.
        #expect(try store.feedEntryForTesting(empty.id)?.realmKey == ImportFixture.realm.encodedKey)
    }

    // MARK: Helpers

    static func emptyFeed(babyID: UUID = Fixture.babyID, in realm: RealmID) -> FeedEntry {
        FeedEntry(babyID: babyID, kind: .breast, startedAt: Date(timeIntervalSince1970: 1_790_236_800), timeZoneID: "Europe/Zurich", realm: realm)
    }
}
