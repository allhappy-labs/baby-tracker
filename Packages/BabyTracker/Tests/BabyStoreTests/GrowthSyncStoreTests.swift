import Foundation
import SwiftData
import Testing
import BabyCore
import BabyProtocol
@testable import BabyStore

@MainActor
@Suite("Growth sync store")
struct GrowthSyncStoreTests {
    let instant = Date(timeIntervalSince1970: 1_790_000_000.008)

    func fixture() throws -> (BabyStore, Baby) {
        let store = try BabyStore.sharedFixture(dateProvider: TestDateProvider(instant))
        let baby = try store.addBaby(name: "Mira", birthDate: instant.addingTimeInterval(-40 * 86400),
                                     referenceSex: .girls, expectedDueDate: LocalDate("2026-09-01"))
        return (store, baby)
    }

    @Test func newOfflineSharedWritesQueueBothGrowthKinds() throws {
        let (store, baby) = try fixture()
        let row = try store.logMeasurement(for: baby.id, at: instant, timeZoneID: "UTC", values: MeasurementValues(weightKG: 4))
        let batch = try store.nextDispatchBatch(limit: 100)
        #expect(batch.map(\.entityKind.rawValue) == ["baby", "growth_profile", "measurement"])
        let mutation = try WireFixture.onlyMutation(in: store, for: row.id)
        #expect(mutation.payloadVersion == 1)
        #expect(WireFixture.field("logged_at", of: mutation.payload) == .string(SupplementTimestamp.format(instant)))
        #expect(WireFixture.field("length_cm", of: mutation.payload) == .null)
        try store.clearOutboxForTesting()
        try store.softDelete(row)
        let deletion = try WireFixture.onlyMutation(in: store, for: row.id)
        #expect(deletion.payload == .object(["baby_id": .string(baby.id.uuidString.lowercased())]))
    }

    @Test func journalSnapshotAndPendingOverlayPreserveRawValuesAndNullClearing() throws {
        let (source, baby) = try fixture()
        let measurement = try source.logMeasurement(for: baby.id, at: instant, timeZoneID: "UTC",
            values: MeasurementValues(weightKG: 4, lengthCM: 54, headCircumferenceCM: 35, lengthPosition: .lying))
        let destination = try BabyStore.sharedFixture(dateProvider: TestDateProvider(instant.addingTimeInterval(100)))
        let changes = try source.nextDispatchBatch(limit: 100).map { try WireFixture.change(from: $0) }
        try destination.applySyncPage(SyncFixture.page(changes: changes))
        let row = try #require(try destination.measurements(for: baby.id).first)
        let profile = try #require(try destination.growthProfile(for: baby.id))
        #expect(row.weightKG == 4 && row.lengthCM == 54 && row.headCircumferenceCM == 35)
        #expect(profile.expectedDueDateISO == "2026-09-01")
        #expect(SupplementTimestamp.format(row.loggedAt) == SupplementTimestamp.format(measurement.loggedAt))
        try destination.editMeasurement(row, at: instant, timeZoneID: "Europe/Zurich", values: MeasurementValues(weightKG: 5))
        try destination.setGrowthProfile(for: baby.id, referenceSex: .boys, expectedDueDate: nil)
        // A stale snapshot must replay the current local full-state edits.
        let snapshot = UUID()
        let high = SyncFixture.cursor(sequence: 10)
        destination.beginSnapshot(snapshotID: snapshot, highWater: high)
        try destination.applySnapshotPage(SnapshotFixture.page(snapshotID: snapshot, highWater: high, entities: changes.map(\.snapshotOrTombstone)))
        try destination.finishSnapshot(snapshotID: snapshot)
        #expect(row.weightKG == 5 && row.lengthCM == nil && row.headCircumferenceCM == nil && row.lengthPosition == nil)
        #expect(profile.referenceSex == .boys && profile.expectedDueDateISO == nil)
        #expect(try destination.syncCursor() == high)
        try source.clearOutboxForTesting()
        try source.applySyncPage(SyncFixture.page(changes: destination.nextDispatchBatch(limit: 100).map { try WireFixture.change(from: $0) }))
        #expect(measurement.lengthCM == nil && measurement.lengthPosition == nil && measurement.headCircumferenceCM == nil)
        #expect(try source.growthProfile(for: baby.id)?.expectedDueDateISO == nil)
    }

    @Test func malformedRemoteGrowthRollsBackWholePageAndCursor() throws {
        let store = try BabyStore.sharedFixture()
        let valid = SyncFixture.change(GrowthProfileID.forBaby(Fixture.babyID), kind: "growth_profile", version: 1,
            payload: "{\"baby_id\":\"\(Fixture.babyID.uuidString)\",\"reference_sex\":\"girls\",\"expected_due_date\":null}")
        let bad = SyncFixture.change(UUID(), kind: "measurement", version: 1,
            payload: "{\"baby_id\":\"\(Fixture.babyID.uuidString)\",\"measured_at\":\"2026-09-20T14:10:00Z\",\"time_zone_id\":\"UTC\",\"weight_kg\":-1}")
        #expect(throws: (any Error).self) { try store.applySyncPage(SyncFixture.page(changes: [valid, bad])) }
        #expect(try store.context.fetch(FetchDescriptor<GrowthProfile>()).isEmpty)
        #expect(try store.context.fetch(FetchDescriptor<MeasurementCheckIn>()).isEmpty)
        #expect(try store.syncCursor() == nil)
    }

    @Test func importRemapsProfileIdentityPreservesMeasurementAndMovesThenPurgesGrowth() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(instant))
        let baby = try store.addBaby(name: "Mira", birthDate: instant.addingTimeInterval(-40 * 86400), referenceSex: .girls, expectedDueDate: nil)
        let originalID = baby.id
        let profile = try #require(try store.growthProfile(for: baby.id))
        let reading = try store.logMeasurement(for: baby.id, at: instant, timeZoneID: "UTC", values: MeasurementValues(weightKG: 4))
        let readingID = reading.id
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .matchShared)
        let frozen = try store.freezeImportManifest(checkpointID: checkpoint)
        #expect(Set(try frozen.records().map(\.entityKind)) == ["growth_profile", "measurement"])
        let mapped = UUID()
        try store.saveMapping([originalID: mapped], checkpointID: checkpoint)
        try store.rewriteAndMoveWorkspace(checkpointID: checkpoint, to: Fixture.sharedRealm)
        #expect(profile.id == GrowthProfileID.forBaby(mapped))
        #expect(profile.babyID == mapped && profile.realmID == Fixture.sharedRealm)
        #expect(reading.id == readingID && reading.babyID == mapped && reading.realmID == Fixture.sharedRealm)
        let batch = try store.nextDispatchBatch(limit: 100)
        #expect(Set(batch.map(\.entityKind.rawValue)) == ["growth_profile", "measurement"])
        #expect(batch.first { $0.entityKind.rawValue == "growth_profile" }?.entityID == GrowthProfileID.forBaby(mapped))
        #expect(batch.allSatisfy { WireFixture.field("baby_id", of: $0.payload) == .string(mapped.uuidString.lowercased()) })
        try store.setActiveRealm(.local)
        let local = try store.addBaby(name: "Local", birthDate: instant, referenceSex: .boys, expectedDueDate: nil)
        let localMeasurement = try store.logMeasurement(for: local.id, at: instant, timeZoneID: "UTC", values: MeasurementValues(weightKG: 3))
        try store.purgeSharedRealm(Fixture.sharedRealm)
        #expect(try store.context.fetch(FetchDescriptor<GrowthProfile>()).map(\.babyID) == [local.id])
        #expect(try store.context.fetch(FetchDescriptor<MeasurementCheckIn>()).map(\.id) == [localMeasurement.id])
    }
    @Test func conflictingProfileReceiptReplacesBaseWithoutAJournalReplay() throws {
        let (store, baby) = try fixture()
        let profile = try #require(try store.growthProfile(for: baby.id))
        let mutation = try WireFixture.onlyMutation(in: store, for: profile.id)
        let payload = "{\"baby_id\":\"\(baby.id.uuidString)\",\"reference_sex\":\"boys\",\"expected_due_date\":null}"
        let canonical = SyncFixture.change(profile.id, kind: "growth_profile", version: 2, payload: payload).snapshotOrTombstone
        var object = try #require(JSONSerialization.jsonObject(with: JSONEncoder().encode(SyncFixture.rejected(mutation.id, code: "entity_exists"))) as? [String: Any])
        object["canonical_entity"] = try JSONSerialization.jsonObject(with: JSONEncoder().encode(canonical))
        let outcome = try JSONDecoder().decode(Outcome.self, from: JSONSerialization.data(withJSONObject: object))
        try store.applySyncPage(SyncFixture.page(outcomes: [outcome]))
        #expect(profile.referenceSex == .boys && profile.expectedDueDateISO == nil)
        #expect(try store.observedVersion(of: profile.id, kind: mutation.entityKind) == 2)
        #expect(try store.pendingMutationIDs(for: profile.id).isEmpty)
    }

    @Test func frozenImportFollowupGrowthEditsUseMappedParentAndProfileID() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(instant))
        let baby = try store.addBaby(name: "Mira", birthDate: instant.addingTimeInterval(-86400), referenceSex: .girls, expectedDueDate: nil)
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .matchShared)
        _ = try store.freezeImportManifest(checkpointID: checkpoint)
        let profile = try #require(try store.growthProfile(for: baby.id))
        let kind = try #require(WireEntityKind(rawValue: "growth_profile"))
        let edit = try MutationFactory.mutation(entityID: profile.id, entityKind: kind, operation: .update,
            observedEntityVersion: 0, at: instant, payload: profile.payloadFields())
        try store.appendPostImportIntent(edit, checkpointID: checkpoint)
        let mapped = UUID()
        try store.saveMapping([baby.id: mapped], checkpointID: checkpoint)
        try store.rewriteAndMoveWorkspace(checkpointID: checkpoint, to: Fixture.sharedRealm)
        let queued = try #require(try store.nextDispatchBatch(limit: 100).last)
        #expect(queued.mutationID == edit.mutationID)
        #expect(queued.entityID == GrowthProfileID.forBaby(mapped))
        #expect(WireFixture.field("baby_id", of: queued.payload) == .string(mapped.uuidString.lowercased()))
    }

    @Test func remoteGrowthMetadataAndRealmIsolation() throws {
        let (store, baby) = try fixture()
        let row = try store.logMeasurement(for: baby.id, at: instant, timeZoneID: "UTC", values: MeasurementValues(weightKG: 4))
        let mutation = try WireFixture.onlyMutation(in: store, for: row.id)
        let entity = try WireFixture.change(from: mutation)
        try store.clearOutboxForTesting()
        try store.applySyncPage(SyncFixture.page(changes: [entity]))
        #expect(row.createdBy == entity.snapshotOrTombstone.meta.creatorAccountID.uuidString.lowercased())
        #expect(row.lastModifiedAt == (try BabyStore.wireDate(entity.snapshotOrTombstone.meta.acceptedAt)))
        try store.setActiveRealm(.local)
        let local = MeasurementCheckIn(id: row.id, babyID: baby.id, measuredAt: instant, timeZoneID: "UTC", loggedAt: instant,
                                      values: try MeasurementValues(weightKG: 9))
        store.context.insert(local)
        try store.context.save()
        try store.setActiveRealm(Fixture.sharedRealm)
        #expect(local.realmID == .local && row.realmID == Fixture.sharedRealm)
        try store.applySyncPage(SyncFixture.page(changes: [SyncFixture.change(row.id, kind: "measurement", version: 2, tombstoned: true, payload: "null")]))
        #expect(row.isTombstoned)
        #expect(!local.isTombstoned && local.weightKG == 9)
    }

    @Test func growthMigrationSummaryIncludesBothKinds() throws {
        let (store, baby) = try fixture()
        _ = try store.logMeasurement(for: baby.id, at: instant, timeZoneID: "UTC", values: MeasurementValues(weightKG: 4))
        let kinds = try store.migratedEntityKinds().map(\.rawValue)
        #expect(kinds.contains("growthProfile") && kinds.contains("measurement"))
    }

    @Test func rejectedLateMeasurementAfterChildDeletionIsHiddenWithoutLosingRawRow() throws {
        let (store, baby) = try fixture()
        let row = try store.logMeasurement(for: baby.id, at: instant, timeZoneID: "UTC", values: MeasurementValues(weightKG: 4))
        let mutation = try WireFixture.onlyMutation(in: store, for: row.id)
        try store.applySyncPage(SyncFixture.page(outcomes: [SyncFixture.rejected(mutation.id, code: "baby_deleted")],
            changes: [SyncFixture.change(baby.id, kind: "baby", version: 2, tombstoned: true, payload: "null")]))
        #expect(row.isTombstoned)
        #expect(try store.measurements(for: baby.id).isEmpty)
        #expect(try store.context.fetch(FetchDescriptor<MeasurementCheckIn>()).map(\.id) == [row.id])
    }

    @Test func overlayDeleteOfAnotherKindWithSameUUIDDoesNotTombstoneProfile() throws {
        let (store, baby) = try fixture()
        let profile = try #require(try store.growthProfile(for: baby.id))
        let canonical = try WireFixture.change(from: WireFixture.onlyMutation(in: store, for: profile.id))
        try store.clearOutboxForTesting()
        let row = MeasurementCheckIn(id: profile.id, babyID: baby.id, measuredAt: instant, timeZoneID: "UTC", loggedAt: instant,
                                      values: try MeasurementValues(weightKG: 4), realm: store.activeRealm)
        store.context.insert(row)
        try store.context.save()
        try store.softDelete(row)
        try store.applySyncPage(SyncFixture.page(changes: [canonical]))
        #expect(!profile.isTombstoned)
        #expect(row.isTombstoned)
    }

    @Test(arguments: [WireEntityKind.growthProfile, .measurement], [false, true])
    func olderLostResponseReceiptCannotReplaceNewerSnapshot(kind: WireEntityKind, tombstoned: Bool) throws {
        let (store, baby) = try fixture()
        let profile = try #require(try store.growthProfile(for: baby.id))
        let reading = try store.logMeasurement(for: baby.id, at: instant, timeZoneID: "UTC", values: MeasurementValues(weightKG: 4))
        let id = kind == .growthProfile ? profile.id : reading.id
        let pending = try WireFixture.onlyMutation(in: store, for: id)
        try store.markDispatched(mutationIDs: [pending.id])
        // The server accepted v1, but this phone never received its response.
        let oldCanonical = try WireFixture.change(from: pending).snapshotOrTombstone
        var fields = try #require(JSONSerialization.jsonObject(with: JSONEncoder().encode(pending.payload)) as? [String: Any])
        if kind == .growthProfile {
            fields["reference_sex"] = "boys"
            fields["expected_due_date"] = NSNull()
        } else {
            fields["weight_kg"] = 6
        }
        let payload = String(decoding: try JSONSerialization.data(withJSONObject: fields), as: UTF8.self)
        let newer = SyncFixture.change(id, kind: kind.rawValue, version: 2, tombstoned: tombstoned,
                                       payload: tombstoned ? "null" : payload).snapshotOrTombstone
        let snapshotID = UUID()
        let high = SyncFixture.cursor(sequence: 10)
        store.beginSnapshot(snapshotID: snapshotID, highWater: high)
        try store.applySnapshotPage(SnapshotFixture.page(snapshotID: snapshotID, highWater: high, entities: [newer]))
        try store.finishSnapshot(snapshotID: snapshotID)
        #expect(try store.observedVersion(of: id, kind: kind) == 2)
        #expect(try store.pendingMutationIDs(for: id) == [pending.id])
        // Retrying delivers the original v1 receipt, with no newer journal
        // because the snapshot has already advanced the cursor beyond v2.
        var receipt = try #require(JSONSerialization.jsonObject(with: JSONEncoder().encode(SyncFixture.applied(pending.id))) as? [String: Any])
        receipt["status"] = "already_applied"
        receipt["original_status"] = "applied"
        receipt["canonical_entity"] = try JSONSerialization.jsonObject(with: JSONEncoder().encode(oldCanonical))
        let outcome = try JSONDecoder().decode(Outcome.self, from: JSONSerialization.data(withJSONObject: receipt))
        try store.applySyncPage(SyncFixture.page(outcomes: [outcome], highWater: high))
        #expect(try store.observedVersion(of: id, kind: kind) == 2)
        #expect(try store.outboxEntry(pending.id).deliveryState == .acknowledged)
        #expect(try store.pendingMutationIDs(for: id).isEmpty)
        #expect(try store.syncCursor() == high)
        if kind == .growthProfile {
            #expect(profile.isTombstoned == tombstoned)
            if !tombstoned { #expect(profile.referenceSex == .boys && profile.expectedDueDateISO == nil) }
        } else {
            #expect(reading.isTombstoned == tombstoned)
            if !tombstoned { #expect(reading.weightKG == 6) }
            #expect(SupplementTimestamp.format(reading.loggedAt) == SupplementTimestamp.format(instant))
        }
    }

}
