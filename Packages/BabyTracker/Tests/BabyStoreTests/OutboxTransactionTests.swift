import Foundation
import Testing
@testable import BabyCore
@testable import BabyProtocol
@testable import BabyStore

@MainActor
@Suite("Outbox transactions")
struct OutboxTransactionTests {
    @Test func sharedWriteCreatesExactlyOneOutboxRowWithItsEntity() throws {
        let store = try BabyStore.sharedFixture()
        let entry = try store.logDiaper(.wet, for: Fixture.babyID)
        let outbox = try store.pendingOutbox()

        #expect(outbox.count == 1)
        #expect(outbox.first?.entityID == entry.id)
        #expect(outbox.first?.operation == .create)
        #expect(outbox.first?.entityKind == .diaper)
        #expect(outbox.first?.deliveryState == .neverDispatched)
    }

    @Test func localOnlyWriteCreatesNoOutboxRow() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        _ = try store.logDiaper(.wet, for: baby.id)

        #expect(try store.pendingOutbox().isEmpty)
    }

    @Test func frozenPayloadAndHashSurviveDispatchAndReload() throws {
        let store = try BabyStore.sharedFixture()
        let entry = try store.logDiaper(.dirty, for: Fixture.babyID)
        let mutationID = try #require(try store.pendingOutbox().first { $0.entityID == entry.id }).id

        try store.markDispatched(mutationIDs: [mutationID])
        let first = try store.persistedDispatch(mutationID: mutationID)
        // Marking again must be a no-op rather than a re-freeze.
        try store.markDispatched(mutationIDs: [mutationID])
        let second = try store.persistedDispatch(mutationID: mutationID)

        #expect(first == second)
        #expect(first.deliveryState == .inFlightOrUncertain)
        #expect(!first.payloadHash.isEmpty)
    }

    @Test func staleGenerationWriteLeavesEntityAndOutboxUnchanged() throws {
        let store = try BabyStore.sharedFixture()
        let stale = try store.captureWriteContext()
        // Any transition bumps the generation, so the captured context is now stale.
        try store.beginTransition(to: .local, checkpointID: UUID())

        #expect(throws: RealmWriteError.retryAfterTransition) {
            _ = try store.logDiaper(.wet, for: Fixture.babyID, expectedWriteContext: stale)
        }
        #expect(try store.diaperCountForTesting() == 0)
        #expect(try store.pendingOutbox().isEmpty)
    }

    /// A row whose operation this build no longer knows (a `timer_switch_side`
    /// left uncertain by the build before the one-part timers) must not stop
    /// the rest of the outbox: it and everything waiting on it are
    /// quarantined, and the rows behind it still go out.
    @Test func aLegacyTimerCommandIsQuarantinedAndTheRestStillDispatch() throws {
        let store = try BabyStore.sharedFixture()
        let diaper = try store.logDiaper(.wet, for: Fixture.babyID)
        let normal = try #require(try store.pendingOutbox().first { $0.entityID == diaper.id })

        let timerID = UUID()
        var legacyJSON = try #require(try JSONSerialization.jsonObject(with: normal.payload) as? [String: Any])
        let legacyID = UUID()
        legacyJSON["mutation_id"] = legacyID.uuidString.lowercased()
        legacyJSON["entity_id"] = timerID.uuidString.lowercased()
        legacyJSON["entity_kind"] = "timer"
        legacyJSON["operation"] = "timer_switch_side"
        let legacy = SyncOutboxEntry(
            id: legacyID, realm: store.activeRealm, entityID: timerID, entityKind: .timer,
            operation: .create, payload: try JSONSerialization.data(withJSONObject: legacyJSON),
            sequence: -2, createdAt: .now
        )
        legacy.operationRaw = "timer_switch_side"
        legacy.deliveryState = .inFlightOrUncertain

        var dependentJSON = legacyJSON
        let dependentID = UUID()
        dependentJSON["mutation_id"] = dependentID.uuidString.lowercased()
        dependentJSON["operation"] = "timer_stop"
        let dependent = SyncOutboxEntry(
            id: dependentID, realm: store.activeRealm, entityID: timerID, entityKind: .timer,
            operation: .timerStop, payload: try JSONSerialization.data(withJSONObject: dependentJSON),
            sequence: -1, createdAt: .now, dependencyMutationID: legacyID
        )
        store.context.insert(legacy)
        store.context.insert(dependent)
        try store.context.save()

        let batch = try store.nextDispatchBatch(limit: 10)

        #expect(batch.map(\.mutationID) == [normal.id])
        #expect(try store.outboxEntry(legacyID).deliveryState == .quarantined)
        #expect(try store.outboxEntry(dependentID).deliveryState == .quarantined)
        #expect(try store.outboxEntry(normal.id).deliveryState == .neverDispatched)
        // The next cycle sees the same, without tripping over the legacy row.
        #expect(try store.nextDispatchBatch(limit: 10).map(\.mutationID) == [normal.id])
    }

    /// A mutation an older build queued in a payload shape the server no
    /// longer accepts — a feed update at version 3, before
    /// `feeding_session_id` — is quarantined like an unknown command. Until
    /// then it has no overlay, so a sync page for the same feed still
    /// applies. A delete from that build (feed at version 2) names only its
    /// baby, is accepted at any version up to the current one, and goes out.
    @Test func anOlderShapeMutationIsQuarantinedAndSyncStillApplies() throws {
        let store = try BabyStore.sharedFixture()
        let feed = try store.logBottle(volumeML: 60, contents: .formula, for: Fixture.babyID)
        let create = try #require(try store.pendingOutbox().first { $0.entityID == feed.id })
        try store.applySyncPage(SyncFixture.page(outcomes: [SyncFixture.applied(create.id)]))

        func olderRow(entityID: UUID, operation: WireOperation, version: Int64, payload: JSONValue, sequence: Int64) throws -> UUID {
            let mutation = WireMutation(
                mutationID: UUID(), entityID: entityID, entityKind: .feed, operation: operation,
                payloadVersion: version, observedEntityVersion: 1, clientMutationTime: "2026-09-21T12:00:00.000Z",
                importID: nil, payload: payload
            )
            let row = SyncOutboxEntry(
                id: mutation.mutationID, realm: store.activeRealm, entityID: entityID, entityKind: .feed,
                operation: operation, payload: try JSONEncoder().encode(mutation), sequence: sequence, createdAt: .now
            )
            row.deliveryState = .inFlightOrUncertain
            store.context.insert(row)
            return row.id
        }
        let babyID = JSONValue.string(Fixture.babyID.uuidString.lowercased())
        let olderUpdate = try olderRow(entityID: feed.id, operation: .update, version: 3, payload: .object([
            "baby_id": babyID, "kind": .string("bottle"), "started_at": .string("2026-09-21T12:00:00.000Z"),
            "time_zone_id": .string("UTC"),
            "components": .array([.object([
                "id": .string(UUID().uuidString.lowercased()), "kind": .string("bottle"),
                "volume_ml": .number(999), "contents": .string("formula"),
            ])]),
        ]), sequence: 100)
        let olderDelete = try olderRow(entityID: UUID(), operation: .delete, version: 2,
                                       payload: .object(["baby_id": babyID]), sequence: 101)
        try store.context.save()

        try store.applySyncPage(SyncFixture.page(changes: [SyncFixture.feedChange(feed.id, volumeML: 42, version: 2)]))
        #expect(try store.feedEntryForTesting(feed.id)?.totalBottleVolumeML == 42)

        let batch = try store.nextDispatchBatch(limit: 10)

        #expect(batch.map(\.mutationID) == [olderDelete])
        #expect(try store.outboxEntry(olderUpdate).deliveryState == .quarantined)
        #expect(try store.nextDispatchBatch(limit: 10).map(\.mutationID) == [olderDelete])
    }

    @Test func failedEncodeRollsBackBothEntityAndOutbox() throws {
        let store = try BabyStore.sharedFixture()
        #expect(throws: (any Error).self) {
            _ = try store.logDiaperFailingEncodeForTesting(for: Fixture.babyID)
        }
        #expect(try store.diaperCountForTesting() == 0)
        #expect(try store.pendingOutbox().isEmpty)
    }
}
