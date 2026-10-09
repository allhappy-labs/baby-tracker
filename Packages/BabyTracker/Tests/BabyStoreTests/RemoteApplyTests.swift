import Foundation
import SwiftData
import Testing
@testable import BabyCore
@testable import BabyProtocol
@testable import BabyStore

@MainActor
@Suite("Remote apply and cursor")
struct RemoteApplyTests {
    @Test func appliedCleanupStopWithoutServerFeedWithdrawsProvisionalRecord() throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let timer = try store.startPart(.breast(.left), for: Fixture.babyID)
        let start = try #require(try store.pendingOutbox().first { $0.operation == .timerStart })
        try store.markDispatched(mutationIDs: [start.id])
        _ = try store.resolveTimerStartReceipt(Fixture.alreadyApplied(start.id, originalStatus: "applied"))

        clock.advance(by: 35)
        let local = try #require(try store.stopFeed(timer))
        let stop = try #require(try store.pendingOutbox().first { $0.operation == .timerStop })
        #expect(try store.lastFeed(for: Fixture.babyID)?.id == local.id)

        try store.applySyncPage(SyncFixture.page(outcomes: [SyncFixture.applied(stop.id)]))

        #expect(try store.lastFeed(for: Fixture.babyID) == nil)
    }

    // MARK: Fixed high-water cursor

    /// A high-water mark is fixed for the whole response, so it must not be
    /// recorded until every page has been applied. Recording it after page one
    /// would claim changes that never landed locally, and every later page
    /// would be skipped forever.
    @Test func fixedHighWaterCursorWaitsForAllPages() throws {
        let store = try BabyStore.sharedFixture()
        let high = SyncFixture.cursor(sequence: 20)

        try store.applySyncPage(SyncFixture.page(highWater: high, nextPageToken: "page-2"))
        #expect(try store.syncCursor() == nil)

        try store.applySyncPage(SyncFixture.page(highWater: high, nextPageToken: nil))
        #expect(try store.syncCursor() == high)
    }

    @Test func cursorNeverMovesBackwardsWithinAnEpoch() throws {
        let store = try BabyStore.sharedFixture()
        try store.applySyncPage(SyncFixture.page(highWater: SyncFixture.cursor(sequence: 20)))
        try store.applySyncPage(SyncFixture.page(highWater: SyncFixture.cursor(sequence: 10)))

        #expect(try store.syncCursor()?.sequence == 20)
    }

    /// A recovery epoch is the one case where the sequence legitimately goes
    /// backwards: the server's history has been rebuilt.
    @Test func newerRecoveryEpochMayResetTheSequence() throws {
        let store = try BabyStore.sharedFixture()
        try store.applySyncPage(SyncFixture.page(highWater: SyncFixture.cursor(sequence: 20, epoch: 1)))
        try store.applySyncPage(SyncFixture.page(highWater: SyncFixture.cursor(sequence: 3, epoch: 2)))

        #expect(try store.syncCursor()?.sequence == 3)
        #expect(try store.syncCursor()?.recoveryEpoch == 2)
    }

    // MARK: Base plus overlay

    /// The test the plan singles out. Two local edits to one feed; the server
    /// confirms the FIRST with its canonical value. The value on screen must
    /// remain the second edit — the one the user last chose and has already
    /// seen — and the second mutation must still be owed.
    @Test func m1ResponseAfterLocalM2DoesNotRegressOverlay() throws {
        let store = try BabyStore.sharedFixture()
        let feed = try store.logBottle(volumeML: 60, contents: .formula, for: Fixture.babyID)
        let m1 = try #require(try store.pendingOutbox().first { $0.entityID == feed.id }).id
        try store.markDispatched(mutationIDs: [m1])

        // The user edits again while the first edit is still in flight.
        let bottleID = try #require(feed.components.first?.id)
        feed.components = [.bottle(id: bottleID, volumeML: 90, contents: .formula)]
        try store.update(feed)
        let m2 = try #require(try store.pendingMutationIDs(for: feed.id).last)
        #expect(m2 != m1)

        try store.applySyncPage(SyncFixture.page(
            outcomes: [SyncFixture.applied(m1)],
            changes: [SyncFixture.feedChange(feed.id, volumeML: 60, version: 1)]
        ))

        #expect(try store.feedEntryForTesting(feed.id)?.totalBottleVolumeML == 90)
        #expect(try store.pendingMutationIDs(for: feed.id) == [m2])
    }

    /// Once the last overlay mutation is acknowledged, the canonical value is
    /// what shows — there is nothing left to hold it back.
    @Test func acknowledgingEveryOverlayLeavesTheCanonicalValue() throws {
        let store = try BabyStore.sharedFixture()
        let feed = try store.logBottle(volumeML: 60, contents: .formula, for: Fixture.babyID)
        let m1 = try #require(try store.pendingOutbox().first { $0.entityID == feed.id }).id

        try store.applySyncPage(SyncFixture.page(
            outcomes: [SyncFixture.applied(m1)],
            changes: [SyncFixture.feedChange(feed.id, volumeML: 75, version: 2)]
        ))

        #expect(try store.pendingMutationIDs(for: feed.id).isEmpty)
        #expect(try store.feedEntryForTesting(feed.id)?.totalBottleVolumeML == 75)
    }

    /// A rejected mutation takes its dependents with it, and stops being owed.
    @Test func rejectedReceiptSupersedesItsDependents() throws {
        let clock = TestDateProvider(.now)
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let intents = SyncIntentTestAdapter(store: store)
        let start = try intents.startFeed(side: .left, for: Fixture.babyID)
        try store.markDispatched(mutationIDs: [start.mutationID])
        clock.advance(by: 30)
        let stop = try intents.stopFeed(timerID: start.timerID)

        try store.applySyncPage(SyncFixture.page(outcomes: [SyncFixture.rejected(start.mutationID, code: "invalid_timestamp")]))

        #expect(try store.persistedDispatch(mutationID: stop.mutationID).deliveryState == .superseded)
        #expect(try store.nextDispatchBatch(limit: 10).isEmpty)
    }

    /// A receipt for something this device never queued is ignored rather than
    /// treated as an error: another device's mutation is not ours to act on.
    @Test func receiptForAnUnknownMutationIsIgnored() throws {
        let store = try BabyStore.sharedFixture()
        #expect(throws: Never.self) {
            try store.applySyncPage(SyncFixture.page(outcomes: [SyncFixture.applied(UUID())]))
        }
    }

    @Test func tombstoneFromTheServerHidesTheRow() throws {
        let store = try BabyStore.sharedFixture()
        let feed = try store.logBottle(volumeML: 60, contents: .formula, for: Fixture.babyID)
        let m1 = try #require(try store.pendingOutbox().first { $0.entityID == feed.id }).id
        try store.applySyncPage(SyncFixture.page(outcomes: [SyncFixture.applied(m1)]))

        try store.applySyncPage(SyncFixture.page(
            changes: [SyncFixture.feedChange(feed.id, volumeML: 60, version: 3, tombstoned: true)]
        ))

        #expect(try store.feedEntryForTesting(feed.id)?.isTombstoned == true)
    }

    /// Recovery can add a canonical record whose completion still exists on
    /// this phone but was hidden after its timer start was superseded.
    @Test func liveCanonicalRecordUnhidesAWithdrawnLocalCompletion() throws {
        let store = try BabyStore.sharedFixture()
        let feedID = UUID()
        let sleepID = UUID()
        let feed = FeedEntry(id: feedID, babyID: Fixture.babyID, kind: .breast,
                             startedAt: .now, timeZoneID: "UTC", realm: store.activeRealm)
        let sleep = SleepEntry(id: sleepID, babyID: Fixture.babyID,
                               startedAt: .now, timeZoneID: "UTC", realm: store.activeRealm)
        feed.isTombstoned = true
        sleep.isTombstoned = true
        store.context.insert(feed)
        store.context.insert(sleep)
        try store.context.save()

        try store.applySyncPage(SyncFixture.page(changes: [
            SyncFixture.change(feedID, kind: "feed", version: 1, payload: """
            {"baby_id":"\(Fixture.babyID.uuidString.lowercased())","kind":"breast",
             "started_at":"2026-09-27T12:58:51Z","ended_at":"2026-09-27T13:19:42Z","time_zone_id":"UTC",
             "components":[{"id":"\(UUID().uuidString.lowercased())","kind":"breast","side":"left","seconds":1250}]}
            """),
            SyncFixture.change(sleepID, kind: "sleep", version: 1, payload: """
            {"baby_id":"\(Fixture.babyID.uuidString.lowercased())",
             "started_at":"2026-09-27T12:08:07Z","ended_at":"2026-09-27T12:44:16Z","time_zone_id":"UTC"}
            """),
        ]))

        #expect(try store.feedEntryForTesting(feedID)?.isTombstoned == false)
        #expect(try store.sleepEntryForTesting(sleepID)?.isTombstoned == false)
    }

    /// An older build may have already advanced its cursor over the recovery
    /// journal while leaving these local rows hidden. A later launch must
    /// inspect its saved live bases even when the server has no new page.
    @Test func savedLiveBaseRepairsHiddenCompletionAfterCursorAdvanced() throws {
        let store = try BabyStore.sharedFixture()
        let feedID = UUID()
        let sleepID = UUID()
        try store.applySyncPage(SyncFixture.page(changes: [
            SyncFixture.change(feedID, kind: "feed", version: 1, payload: """
            {"baby_id":"\(Fixture.babyID.uuidString.lowercased())","kind":"breast",
             "started_at":"2026-09-27T12:58:51Z","ended_at":"2026-09-27T13:19:42Z","time_zone_id":"UTC",
             "components":[{"id":"\(UUID().uuidString.lowercased())","kind":"breast","side":"left","seconds":1250}]}
            """),
            SyncFixture.change(sleepID, kind: "sleep", version: 1, payload: """
            {"baby_id":"\(Fixture.babyID.uuidString.lowercased())",
             "started_at":"2026-09-27T12:08:07Z","ended_at":"2026-09-27T12:44:16Z","time_zone_id":"UTC"}
            """),
        ]))
        let feed = try #require(try store.feedEntryForTesting(feedID))
        let sleep = try #require(try store.sleepEntryForTesting(sleepID))
        feed.isTombstoned = true
        sleep.isTombstoned = true
        try store.context.save()

        #expect(try store.reconcileHiddenCanonicalCompletions() == 2)
        #expect(feed.isTombstoned == false)
        #expect(sleep.isTombstoned == false)
        #expect(try store.reconcileHiddenCanonicalCompletions() == 0)
    }

    @Test func reconciliationDoesNotUndoAnIntentionalLocalDelete() throws {
        let store = try BabyStore.sharedFixture()
        let feed = try store.logBottle(volumeML: 60, contents: .formula, for: Fixture.babyID)
        let create = try #require(try store.pendingOutbox().first { $0.entityID == feed.id }).id
        try store.applySyncPage(SyncFixture.page(
            outcomes: [SyncFixture.applied(create)],
            changes: [SyncFixture.feedChange(feed.id, volumeML: 60, version: 1)]
        ))
        try store.softDelete(feed)

        #expect(feed.isTombstoned)
        #expect(try store.reconcileHiddenCanonicalCompletions() == 0)
        #expect(feed.isTombstoned)
    }

    // MARK: Entities this device has never seen

    /// Another caregiver's new record. Before this, remote apply only updated
    /// rows that already existed, so nothing anyone else logged ever showed.
    @Test func changeForAnUnseenEntityCreatesItsRowInTheActiveRealm() throws {
        let store = try BabyStore.sharedFixture()
        let feedID = UUID()
        let componentID = UUID()

        try store.applySyncPage(SyncFixture.page(changes: [
            SyncFixture.change(feedID, kind: "feed", version: 1, payload: """
            {"baby_id":"\(Fixture.babyID.uuidString.lowercased())","kind":"bottle",
             "started_at":"2026-09-21T12:00:00Z","time_zone_id":"UTC",
             "components":[{"id":"\(componentID.uuidString.lowercased())","kind":"bottle","volume_ml":50,"contents":"breast_milk"}]}
            """),
        ]))

        let feed = try #require(try store.feedEntryForTesting(feedID))
        #expect(feed.realmKey == store.activeRealm.encodedKey)
        #expect(feed.babyID == Fixture.babyID)
        #expect(feed.components == [.bottle(id: componentID, volumeML: 50, contents: .breastMilk)])
        #expect(try store.pendingMutationIDs(for: feedID).isEmpty)
    }

    /// Fix round 1. Realms are separate worlds even when an ID repeats: a row
    /// with the same UUID in the local realm neither stops the shared row
    /// being created nor gets overwritten by the family's version.
    @Test func sameIDInAnotherRealmNeitherBlocksNorIsModified() throws {
        let store = try BabyStore.sharedFixture()
        let feedID = UUID()
        let local = FeedEntry(id: feedID, babyID: Fixture.babyID, kind: .bottle, startedAt: .now, timeZoneID: "UTC", realm: .local)
        local.note = "mine"
        store.context.insert(local)
        try store.context.save()

        try store.applySyncPage(SyncFixture.page(changes: [
            SyncFixture.change(feedID, kind: "feed", version: 1, payload: """
            {"baby_id":"\(Fixture.babyID.uuidString.lowercased())","kind":"bottle",
             "started_at":"2026-09-21T12:00:00Z","time_zone_id":"UTC","note":"family",
             "components":[{"id":"\(UUID().uuidString.lowercased())","kind":"bottle","volume_ml":50,"contents":"formula"}]}
            """),
        ]))

        let rows = try store.context.fetch(FetchDescriptor<FeedEntry>(predicate: #Predicate { $0.id == feedID }))
        let sharedKey = store.activeRealm.encodedKey
        let localKey = RealmID.local.encodedKey
        #expect(rows.first { $0.realmKey == sharedKey }?.note == "family")
        let untouched = try #require(rows.first { $0.realmKey == localKey })
        #expect(untouched.note == "mine")
        #expect(untouched.components.isEmpty)
    }

    /// Something created and deleted elsewhere before this device looked has
    /// nothing to show, and must not appear as a live row.
    @Test func tombstoneForAnUnseenEntityCreatesNothing() throws {
        let store = try BabyStore.sharedFixture()
        let feedID = UUID()

        try store.applySyncPage(SyncFixture.page(changes: [
            SyncFixture.change(feedID, kind: "feed", version: 2, tombstoned: true, payload: """
            {"baby_id":"\(Fixture.babyID.uuidString.lowercased())","kind":"bottle",
             "started_at":"2026-09-21T12:00:00Z","time_zone_id":"UTC","components":[]}
            """),
        ]))

        #expect(try store.feedEntryForTesting(feedID) == nil)
    }
}

enum SyncFixture {
    static let instanceID = UUID(uuidString: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")!
    static let familyID = UUID(uuidString: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")!

    static func cursor(sequence: Int64, epoch: Int64 = 1) -> Cursor {
        Cursor(serverInstanceID: instanceID, recoveryEpoch: epoch, familyID: familyID, sequence: sequence)
    }

    static func applied(_ mutationID: UUID) -> Outcome {
        decode("""
        {"mutation_id":"\(mutationID.uuidString.lowercased())","status":"applied","original_status":null,
         "canonical_entity":null,"related_entities":[],"cursor":null,"code":null,"overwrote_newer_version":false}
        """)
    }

    static func rejected(_ mutationID: UUID, code: String) -> Outcome {
        decode("""
        {"mutation_id":"\(mutationID.uuidString.lowercased())","status":"rejected","original_status":null,
         "canonical_entity":null,"related_entities":[],"cursor":null,"code":"\(code)","overwrote_newer_version":false}
        """)
    }

    static func feedChange(_ entityID: UUID, volumeML: Double, version: Int64, tombstoned: Bool = false) -> JournalChange {
        let stamp = "2026-09-21T12:00:00.000000Z"
        return decode("""
        {
          "cursor": {"server_instance_id":"\(instanceID.uuidString.lowercased())","recovery_epoch":1,
                     "family_id":"\(familyID.uuidString.lowercased())","sequence":\(version)},
          "entity_kind": "feed",
          "entity_id": "\(entityID.uuidString.lowercased())",
          "entity_version": \(version),
          "actor_account_id": "\(familyID.uuidString.lowercased())",
          "device_id": "\(instanceID.uuidString.lowercased())",
          "operation": "update",
          "accepted_at": "\(stamp)",
          "optional": false,
          "snapshot_or_tombstone": {
            "entity_kind": "feed",
            "meta": {"id":"\(entityID.uuidString.lowercased())","family_id":"\(familyID.uuidString.lowercased())",
                     "entity_version":\(version),"creator_account_id":"\(familyID.uuidString.lowercased())",
                     "last_editor_account_id":"\(familyID.uuidString.lowercased())",
                     "client_mutation_time":"\(stamp)","accepted_at":"\(stamp)",
                     "tombstoned_at":\(tombstoned ? "\"\(stamp)\"" : "null")},
            "payload": {"components": [{"id":"\(UUID().uuidString.lowercased())","kind":"bottle",
                                        "volume_ml":\(volumeML),"contents":"formula"}],
                        "feeding_session_id": null}
          }
        }
        """)
    }

    static func change(_ entityID: UUID, kind: String, version: Int64, tombstoned: Bool = false, payload: String) -> JournalChange {
        let stamp = "2026-09-21T12:00:00.000000Z"
        let id = entityID.uuidString.lowercased()
        let family = familyID.uuidString.lowercased()
        return decode("""
        {"cursor":{"server_instance_id":"\(instanceID.uuidString.lowercased())","recovery_epoch":1,"family_id":"\(family)","sequence":\(version)},
         "entity_kind":"\(kind)","entity_id":"\(id)","entity_version":\(version),
         "actor_account_id":"\(family)","device_id":"\(family)","operation":"create","accepted_at":"\(stamp)","optional":false,
         "snapshot_or_tombstone":{"entity_kind":"\(kind)",
           "meta":{"id":"\(id)","family_id":"\(family)","entity_version":\(version),"creator_account_id":"\(family)",
                   "last_editor_account_id":"\(family)","client_mutation_time":"\(stamp)","accepted_at":"\(stamp)",
                   "tombstoned_at":\(tombstoned ? "\"\(stamp)\"" : "null")},
           "payload":\(payload)}}
        """)
    }

    static func page(
        outcomes: [Outcome] = [],
        changes: [JournalChange] = [],
        highWater: Cursor = cursor(sequence: 1),
        nextPageToken: String? = nil
    ) -> SyncResponse {
        SyncResponse(
            serverInstanceID: instanceID, recoveryEpoch: highWater.recoveryEpoch,
            outcomes: outcomes, changes: changes, highWater: highWater, nextPageToken: nextPageToken
        )
    }

    private static func decode<T: Decodable>(_ json: String) -> T {
        try! JSONDecoder().decode(T.self, from: Data(json.utf8))
    }
}
