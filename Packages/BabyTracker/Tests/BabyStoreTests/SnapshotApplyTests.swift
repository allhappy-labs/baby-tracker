import Foundation
import Testing
@testable import BabyCore
@testable import BabyProtocol
@testable import BabyStore

@MainActor
@Suite("Snapshot and recovery apply")
struct SnapshotApplyTests {
    /// The test the plan singles out. After a recovery epoch the server's
    /// history has been rebuilt, and a mutation this device already had
    /// acknowledged is absent from the new snapshot.
    ///
    /// It must NOT be re-dispatched. Re-sending it would re-apply an edit the
    /// server deliberately dropped — which is how a recovery turns into data
    /// coming back from the dead.
    @Test func recoveryEpochDoesNotReuploadAcknowledgedMutation() throws {
        let store = try BabyStore.sharedFixture()
        let feed = try store.logBottle(volumeML: 60, contents: .formula, for: Fixture.babyID)
        let m1 = try #require(try store.pendingOutbox().first { $0.entityID == feed.id }).id
        try store.applySyncPage(SyncFixture.page(outcomes: [SyncFixture.applied(m1)]))
        #expect(try store.persistedDispatch(mutationID: m1).deliveryState == .acknowledged)

        let snapshotID = UUID()
        let high = SyncFixture.cursor(sequence: 1, epoch: 2)
        store.beginSnapshot(snapshotID: snapshotID, highWater: high)
        try store.applySnapshotPage(SnapshotFixture.page(snapshotID: snapshotID, highWater: high, entities: []))
        try store.finishSnapshot(snapshotID: snapshotID)

        #expect(!(try store.nextDispatchBatch(limit: 10).contains { $0.id == m1 }))
        #expect(try store.syncCursor()?.recoveryEpoch == 2)
    }

    /// Work this device still owes survives a recovery: it was never
    /// acknowledged, so it still needs sending.
    @Test func unacknowledgedMutationsSurviveARecoverySnapshot() throws {
        let store = try BabyStore.sharedFixture()
        let feed = try store.logBottle(volumeML: 60, contents: .formula, for: Fixture.babyID)
        let pending = try #require(try store.pendingOutbox().first { $0.entityID == feed.id }).id

        let snapshotID = UUID()
        let high = SyncFixture.cursor(sequence: 1, epoch: 2)
        store.beginSnapshot(snapshotID: snapshotID, highWater: high)
        try store.applySnapshotPage(SnapshotFixture.page(snapshotID: snapshotID, highWater: high, entities: []))
        try store.finishSnapshot(snapshotID: snapshotID)

        #expect(try store.nextDispatchBatch(limit: 10).map(\.id) == [pending])
    }

    /// The cursor moves only when the snapshot is closed. A half-applied
    /// snapshot that claimed its high water would skip everything it missed.
    @Test func snapshotCursorMovesOnlyOnFinish() throws {
        let store = try BabyStore.sharedFixture()
        let snapshotID = UUID()
        let high = SyncFixture.cursor(sequence: 9, epoch: 3)
        store.beginSnapshot(snapshotID: snapshotID, highWater: high)

        try store.applySnapshotPage(SnapshotFixture.page(snapshotID: snapshotID, highWater: high, entities: []))
        #expect(try store.syncCursor() == nil)

        try store.finishSnapshot(snapshotID: snapshotID)
        #expect(try store.syncCursor() == high)
    }

    /// Finishing a snapshot nobody started, or a different one, changes
    /// nothing — a stale completion must not move the cursor.
    @Test func finishingAnUnknownSnapshotIsIgnored() throws {
        let store = try BabyStore.sharedFixture()
        let high = SyncFixture.cursor(sequence: 9, epoch: 3)
        store.beginSnapshot(snapshotID: UUID(), highWater: high)

        try store.finishSnapshot(snapshotID: UUID())

        #expect(try store.syncCursor() == nil)
    }

    @Test func snapshotEntitiesReplaceTheVisibleRow() throws {
        let store = try BabyStore.sharedFixture()
        let feed = try store.logBottle(volumeML: 60, contents: .formula, for: Fixture.babyID)
        let m1 = try #require(try store.pendingOutbox().first { $0.entityID == feed.id }).id
        try store.applySyncPage(SyncFixture.page(outcomes: [SyncFixture.applied(m1)]))

        let snapshotID = UUID()
        let high = SyncFixture.cursor(sequence: 4, epoch: 2)
        store.beginSnapshot(snapshotID: snapshotID, highWater: high)
        try store.applySnapshotPage(SnapshotFixture.page(
            snapshotID: snapshotID, highWater: high,
            entities: [SnapshotFixture.feed(feed.id, volumeML: 42, version: 7)]
        ))
        try store.finishSnapshot(snapshotID: snapshotID)

        #expect(try store.feedEntryForTesting(feed.id)?.totalBottleVolumeML == 42)
    }
}

enum SnapshotFixture {
    static func feed(_ entityID: UUID, volumeML: Double, version: Int64) -> CanonicalEntity {
        let stamp = "2026-09-21T12:00:00.000000Z"
        let json = """
        {
          "entity_kind": "feed",
          "meta": {"id":"\(entityID.uuidString.lowercased())","family_id":"\(SyncFixture.familyID.uuidString.lowercased())",
                   "entity_version":\(version),"creator_account_id":"\(SyncFixture.familyID.uuidString.lowercased())",
                   "last_editor_account_id":"\(SyncFixture.familyID.uuidString.lowercased())",
                   "client_mutation_time":"\(stamp)","accepted_at":"\(stamp)","tombstoned_at":null},
          "payload": {"components": [{"id":"\(UUID().uuidString.lowercased())","kind":"bottle",
                                      "volume_ml":\(volumeML),"contents":"formula"}],
                      "feeding_session_id": null}
        }
        """
        return try! JSONDecoder().decode(CanonicalEntity.self, from: Data(json.utf8))
    }

    static func page(
        snapshotID: UUID,
        highWater: Cursor,
        entities: [CanonicalEntity],
        nextPageToken: String? = nil
    ) -> SnapshotResponse {
        SnapshotResponse(
            serverInstanceID: SyncFixture.instanceID,
            recoveryEpoch: highWater.recoveryEpoch,
            snapshotID: snapshotID,
            highWater: highWater,
            entities: entities,
            nextPageToken: nextPageToken
        )
    }
}
