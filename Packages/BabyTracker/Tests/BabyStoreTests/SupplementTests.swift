import Foundation
import SwiftData
import Testing
import BabyCore
import BabyProtocol
@testable import BabyStore

@MainActor
@Suite("Supplements")
struct SupplementTests {
    @Test func namesUseLoggingOrderAndFirstSpellingAndDisappearAfterLastDeletion() throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
        let store = try BabyStore.inMemory(dateProvider: clock)
        let baby = try store.addBaby(name: "Mira", birthDate: clock.now, referenceSex: .girls, expectedDueDate: nil)
        let first = try store.logSupplement(name: " Vitamin D ", for: baby.id)
        clock.advance(by: 1)
        let second = try store.logSupplement(name: "Iron", for: baby.id)
        clock.advance(by: 1)
        let backdated = try store.logSupplement(name: " vitamin d ", for: baby.id, at: clock.now.addingTimeInterval(-3600))
        #expect(first.name == "Vitamin D")
        #expect(backdated.name == "Vitamin D")
        #expect(try store.supplementNames(for: baby.id) == ["Vitamin D", "Iron"])
        #expect(try store.lastSupplement(for: baby.id)?.id == backdated.id)
        #expect(try store.entries(for: baby.id, from: .distantPast, to: .distantFuture).count == 3)
        try store.softDelete(first)
        try store.softDelete(backdated)
        #expect(try store.supplementNames(for: baby.id) == ["Iron"])
        #expect(try store.lastSupplement(for: baby.id)?.id == second.id)
    }

    @Test func invalidNamesDoNotWriteAndNFCNormalizes() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let babyID = UUID()
        for name in [" \n", "A\u{0001}B", String(repeating: "a", count: 65)] {
            #expect(throws: (any Error).self) { try store.logSupplement(name: name, for: babyID) }
        }
        #expect(try store.changeLog().isEmpty)
        let row = try store.logSupplement(name: "Cafe\u{301}", for: babyID)
        #expect(row.name == "Café")
        #expect(row.payloadFields()["ended_at"] == nil)
        #expect(row.payloadFields()["note"] is NSNull)
    }

    @Test func genericUpdateValidatesAndRollsBackName() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let row = try store.logSupplement(name: "Vitamin D", for: UUID())
        row.name = " "
        #expect(throws: (any Error).self) { try store.update(row) }
        #expect(try ModelContext(store.container).fetch(FetchDescriptor<SupplementEntry>()).first?.name == "Vitamin D")
    }

    @Test func remoteLoggingOrderAndTombstonesAndPendingEdits() throws {
        let store = try BabyStore.sharedFixture()
        let id = UUID()
        let payload = "{\"baby_id\":\"\(Fixture.babyID.uuidString)\",\"started_at\":\"2026-09-20T14:10:00Z\",\"logged_at\":\"2026-09-20T15:00:00Z\",\"time_zone_id\":\"Europe/Zurich\",\"name\":\"Vitamin D\",\"note\":null}"
        let change = SyncFixture.change(id, kind: "supplement", version: 1, payload: payload)
        try store.applySyncPage(SyncFixture.page(changes: [change]))
        let row = try #require(try store.lastSupplement(for: Fixture.babyID))
        #expect(row.id == id)
        #expect(row.loggedAt == (try WireFixture.date("2026-09-20T15:00:00Z")))
        #expect(row.startedAt != row.loggedAt)
        try store.editSupplement(row, name: "Iron", at: row.startedAt, note: "local")
        try store.applySyncPage(SyncFixture.page(changes: [change]))
        #expect(row.name == "Iron")
        #expect(row.note == "local")
        try store.clearOutboxForTesting()
        try store.applySyncPage(SyncFixture.page(changes: [SyncFixture.change(id, kind: "supplement", version: 2, tombstoned: true, payload: "null")]))
        #expect(try store.supplementNames(for: Fixture.babyID).isEmpty)
    }

    @Test func importIncludesSupplementAndRemapsParent() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let row = try store.logSupplement(name: "Vitamin D", for: baby.id)
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .preserveLocalIDs)
        let frozen = try store.freezeImportManifest(checkpointID: checkpoint)
        let payloadData = try #require(frozen.sourceBases[EntityKey(kind: "supplement", id: row.id)])
        let payload = try JSONDecoder().decode(JSONValue.self, from: payloadData)
        #expect(WireFixture.field("logged_at", of: payload) == .string(SupplementTimestamp.format(row.loggedAt)))
        let mapped = UUID()
        try store.remapBabiesAndDependents([baby.id: mapped])
        #expect(row.babyID == mapped)
    }

    @Test func sharedOutboxMatchesFixtureAndDeletePayload() throws {
        let store = try BabyStore.sharedFixture()
        let fixture = try WireFixture.mutation("supplement.json")
        let row = try store.logSupplement(name: "Vitamin D", for: Fixture.babyID, at: WireFixture.date("2026-09-20T14:10:00Z"))
        row.timeZoneID = "Europe/Zurich"
        row.loggedAt = try WireFixture.date("2026-09-20T15:00:00Z")
        try store.clearOutboxForTesting()
        try store.record(.create, on: row)
        let mutation = try WireFixture.onlyMutation(in: store, for: row.id)
        #expect(mutation.entityKind == .supplement)
        #expect(mutation.payloadVersion == 1)
        try WireFixture.expectSameShape(mutation.payload, fixture.payload)
        #expect(try ProtocolCanonicalizer.contentSHA256(entityKind: "supplement", payload: mutation.payload) == "ada019a1605551c30b8a3926f283b062b0939f229fbc3be9f1133df9ad5307c9")
        try store.clearOutboxForTesting()
        try store.softDelete(row)
        let deletion = try WireFixture.onlyMutation(in: store, for: row.id)
        #expect(deletion.payload == .object(["baby_id": .string(Fixture.babyID.uuidString.lowercased())]))
    }

    @Test func unknownRequiredEntityDoesNotAdvanceCursorButOptionalIsSkipped() throws {
        let store = try BabyStore.sharedFixture()
        let change = SyncFixture.change(UUID(), kind: "future_kind", version: 1, payload: "{}")
        #expect(throws: (any Error).self) { try store.applySyncPage(SyncFixture.page(changes: [change])) }
        #expect(try store.syncCursor() == nil)
        var object = try #require(JSONSerialization.jsonObject(with: JSONEncoder().encode(change)) as? [String: Any])
        object["optional"] = true
        let optional = try JSONDecoder().decode(JournalChange.self, from: JSONSerialization.data(withJSONObject: object))
        try store.applySyncPage(SyncFixture.page(changes: [optional]))
        #expect(try store.syncCursor() == SyncFixture.cursor(sequence: 1))
    }

    @Test func snapshotSupportsSupplementAndUnknownKindRollsBackWholePage() throws {
        let store = try BabyStore.sharedFixture()
        let mutation = try WireFixture.mutation("supplement.json")
        let entity = try WireFixture.change(from: mutation).snapshotOrTombstone
        let snapshotID = UUID()
        let high = SyncFixture.cursor(sequence: 4)
        store.beginSnapshot(snapshotID: snapshotID, highWater: high)
        let unknown = SyncFixture.change(UUID(), kind: "future_kind", version: 1, payload: "{}").snapshotOrTombstone
        #expect(throws: (any Error).self) {
            try store.applySnapshotPage(SnapshotFixture.page(snapshotID: snapshotID, highWater: high, entities: [entity, unknown]))
        }
        #expect(try store.context.fetch(FetchDescriptor<SupplementEntry>()).isEmpty)
        try store.applySnapshotPage(SnapshotFixture.page(snapshotID: snapshotID, highWater: high, entities: [entity]))
        try store.finishSnapshot(snapshotID: snapshotID)
        let row = try #require(try store.context.fetch(FetchDescriptor<SupplementEntry>()).first)
        #expect(row.name == "Vitamin D")
        #expect(row.loggedAt == (try WireFixture.date("2026-09-20T15:00:00Z")))
        #expect(try store.syncCursor() == high)
    }

    @Test func realmMoveAndPurgeIncludeSupplementsWithoutTouchingLocalRows() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let id = UUID()
        let moved = try store.logSupplement(name: "D", for: id)
        let checkpoint = UUID()
        try store.beginTransition(to: Fixture.sharedRealm, checkpointID: checkpoint)
        try store.commitTransition(checkpointID: checkpoint)
        #expect(moved.realmID == Fixture.sharedRealm)
        try store.setActiveRealm(.local)
        let local = try store.logSupplement(name: "Iron", for: id)
        #expect(try store.supplementNames(for: id) == ["Iron"])
        try store.purgeSharedRealm(Fixture.sharedRealm)
        #expect(try store.context.fetch(FetchDescriptor<SupplementEntry>()).map(\.id) == [local.id])
    }

    @Test func remoteLastUseComesFromLoggedAtAndIgnoresBackdatedEventTime() throws {
        let store = try BabyStore.sharedFixture()
        let fixture = try WireFixture.mutation("supplement.json")
        func change(name: String, started: String, accepted: String) throws -> JournalChange {
            let base = try WireFixture.change(from: fixture)
            let data = try JSONEncoder().encode(base)
            var object = try #require(JSONSerialization.jsonObject(with: data) as? [String: Any])
            let id = UUID().uuidString.lowercased()
            object["entity_id"] = id
            object["accepted_at"] = accepted
            var entity = try #require(object["snapshot_or_tombstone"] as? [String: Any])
            var meta = try #require(entity["meta"] as? [String: Any])
            meta["id"] = id; meta["accepted_at"] = accepted; entity["meta"] = meta
            var payload = try #require(entity["payload"] as? [String: Any])
            payload["baby_id"] = Fixture.babyID.uuidString.lowercased()
            payload["name"] = name; payload["started_at"] = started; payload["logged_at"] = accepted; entity["payload"] = payload
            object["snapshot_or_tombstone"] = entity
            return try JSONDecoder().decode(JournalChange.self, from: JSONSerialization.data(withJSONObject: object))
        }
        let first = try change(name: "Iron", started: "2026-09-21T12:00:00Z", accepted: "2026-09-21T12:00:01Z")
        let second = try change(name: "Vitamin D", started: "2026-09-20T12:00:00Z", accepted: "2026-09-21T12:00:02Z")
        try store.applySyncPage(SyncFixture.page(changes: [second, first]))
        #expect(try store.supplementNames(for: Fixture.babyID) == ["Vitamin D", "Iron"])
        #expect(try store.lastSupplement(for: Fixture.babyID)?.id == second.entityID)
        let third = try change(name: "vitamin d", started: "2026-09-19T12:00:00Z", accepted: "2026-09-21T12:00:03Z")
        try store.applySyncPage(SyncFixture.page(changes: [third]))
        #expect(try store.supplementNames(for: Fixture.babyID) == ["Vitamin D", "Iron"])
    }

    @Test func acknowledgementDoesNotReorderOfflineLogs() throws {
        let clock = TestDateProvider(try WireFixture.date("2026-09-20T15:00:00Z"))
        let store = try BabyStore.inMemory(dateProvider: clock)
        try store.activateRealmForTesting(Fixture.sharedRealm)
        let first = try store.logSupplement(name: "Vitamin D", for: Fixture.babyID)
        let firstLoggedAt = first.loggedAt
        let firstMutation = try WireFixture.onlyMutation(in: store, for: first.id)
        clock.advance(by: 60)
        let second = try store.logSupplement(name: "Iron", for: Fixture.babyID)
        let secondLoggedAt = second.loggedAt
        let secondMutation = try WireFixture.onlyMutation(in: store, for: second.id)
        // The older action is accepted last, after the second action already exists.
        try store.applySyncPage(SyncFixture.page(
            outcomes: [SyncFixture.applied(secondMutation.mutationID)],
            changes: [try supplementChange(secondMutation, version: 1, acceptedAt: "2026-09-21T16:00:00Z")]))
        try store.applySyncPage(SyncFixture.page(
            outcomes: [SyncFixture.applied(firstMutation.mutationID)],
            changes: [try supplementChange(firstMutation, version: 1, acceptedAt: "2026-09-21T17:00:00Z")]))
        #expect(first.loggedAt == firstLoggedAt)
        #expect(second.loggedAt == secondLoggedAt)
        #expect(try store.supplementNames(for: Fixture.babyID) == ["Iron", "Vitamin D"])
    }

    @Test func freshSnapshotOfEditedRowsPreservesOriginalLoggingOrder() throws {
        let clock = TestDateProvider(try WireFixture.date("2026-09-20T15:00:00Z"))
        let source = try BabyStore.inMemory(dateProvider: clock)
        try source.activateRealmForTesting(Fixture.sharedRealm)
        let first = try source.logSupplement(name: "Vitamin D", for: Fixture.babyID)
        let loggedAt = first.loggedAt
        clock.advance(by: 60)
        let second = try source.logSupplement(name: "Iron", for: Fixture.babyID)
        let secondMutation = try WireFixture.onlyMutation(in: source, for: second.id)
        try source.clearOutboxForTesting()
        clock.advance(by: 3600)
        try source.editSupplement(first, name: "Vitamin D", at: clock.now.addingTimeInterval(-7200), note: "edited")
        let edit = try WireFixture.onlyMutation(in: source, for: first.id)
        #expect(WireFixture.field("logged_at", of: edit.payload) == .string(SupplementTimestamp.format(loggedAt)))
        let destination = try BabyStore.sharedFixture()
        let snapshotID = UUID()
        let high = SyncFixture.cursor(sequence: 5)
        destination.beginSnapshot(snapshotID: snapshotID, highWater: high)
        try destination.applySnapshotPage(SnapshotFixture.page(snapshotID: snapshotID, highWater: high, entities: [
            try supplementChange(edit, version: 2, acceptedAt: "2026-09-21T17:00:00Z").snapshotOrTombstone,
            try supplementChange(secondMutation, version: 1, acceptedAt: "2026-09-21T16:00:00Z").snapshotOrTombstone,
        ]))
        try destination.finishSnapshot(snapshotID: snapshotID)
        #expect(try destination.supplementNames(for: Fixture.babyID) == ["Iron", "Vitamin D"])
        let rows = try destination.context.fetch(FetchDescriptor<SupplementEntry>())
        #expect(rows.first { $0.id == first.id }?.loggedAt == loggedAt)
        #expect(rows.first { $0.id == first.id }?.note == "edited")
    }

    @Test func staleEntryCannotBeEditedInAnotherRealm() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let row = try store.logSupplement(name: "Vitamin D", for: Fixture.babyID)
        let originalDate = row.startedAt
        let count = try store.changeLog().count
        let expected = try store.captureWriteContext()
        try store.setActiveRealm(Fixture.sharedRealm)
        #expect(throws: RealmWriteError.wrongRealm) {
            try store.editSupplement(row, name: "Iron", at: originalDate.addingTimeInterval(-60), note: "wrong realm")
        }
        #expect(throws: RealmWriteError.retryAfterTransition) {
            try store.editSupplement(row, name: "Iron", at: originalDate, note: nil, expectedWriteContext: expected)
        }
        let persisted = try #require(try ModelContext(store.container).fetch(FetchDescriptor<SupplementEntry>()).first)
        #expect(persisted.name == "Vitamin D")
        #expect(persisted.startedAt == originalDate)
        #expect(persisted.note == nil)
        #expect(persisted.realmID == .local)
        #expect(try store.pendingOutbox().isEmpty)
        #expect(try store.changeLog().count == count)
    }

    @Test func missingLoggedAtIsRejectedForExistingAndUnseenRemoteRows() throws {
        let store = try BabyStore.sharedFixture()
        let row = try store.logSupplement(name: "Vitamin D", for: Fixture.babyID)
        try store.clearOutboxForTesting()
        let payload = "{\"baby_id\":\"\(Fixture.babyID.uuidString)\",\"started_at\":\"2026-09-20T14:10:00Z\",\"time_zone_id\":\"UTC\",\"name\":\"Iron\",\"note\":null}"
        for id in [row.id, UUID()] {
            #expect(throws: (any Error).self) {
                try store.applySyncPage(SyncFixture.page(changes: [SyncFixture.change(id, kind: "supplement", version: 2, payload: payload)]))
            }
        }
        #expect(try store.syncCursor() == nil)
        #expect(try store.context.fetch(FetchDescriptor<SupplementEntry>()).count == 1)
    }

    @Test(arguments: [
        "2026-09-26T20:00:32.007000Z", "2026-09-26T20:00:32.008000Z",
        "2026-09-26T20:00:32.123456Z", "2026-09-26T20:00:32.999999Z",
        "1900-01-01T00:00:00.123456Z", "2100-12-31T23:59:59.999999Z",
    ])
    func supplementLoggingTimestampSurvivesCanonicalParseAndEdit(stamp: String) throws {
        let store = try BabyStore.sharedFixture()
        let id = UUID()
        let payload = "{\"baby_id\":\"\(Fixture.babyID.uuidString)\",\"started_at\":\"2026-09-20T14:10:00Z\",\"logged_at\":\"\(stamp)\",\"time_zone_id\":\"UTC\",\"name\":\"Vitamin D\",\"note\":null}"
        try store.applySyncPage(SyncFixture.page(changes: [SyncFixture.change(id, kind: "supplement", version: 1, payload: payload)]))
        let row = try #require(try store.lastSupplement(for: Fixture.babyID))
        try store.editSupplement(row, name: "Vitamin D", at: row.startedAt, note: "edited")
        let edit = try WireFixture.onlyMutation(in: store, for: id)
        #expect(WireFixture.field("logged_at", of: edit.payload) == .string(stamp))
        let canonicalPayload = try JSONDecoder().decode(JSONValue.self, from: Data(payload.utf8))
        #expect(try ProtocolCanonicalizer.contentSHA256(entityKind: "supplement", payload: edit.payload) ==
            ProtocolCanonicalizer.contentSHA256(entityKind: "supplement", payload: .object({
                guard case var .object(fields) = canonicalPayload else { return [:] }
                fields["note"] = .string("edited")
                return fields
            }())))
    }

    @Test func locallyLoggedFractionRoundsToMicrosecondsBeforeSync() throws {
        let second = try WireFixture.date("2026-09-26T20:00:32Z")
        let date = Date(timeIntervalSince1970: floor(second.timeIntervalSince1970) + 0.008)
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(date))
        let row = try store.logSupplement(name: "Vitamin D", for: UUID())
        #expect(row.payloadFields()["logged_at"] as? String == "2026-09-26T20:00:32.008000Z")
    }

    private func supplementChange(_ mutation: WireMutation, version: Int64, acceptedAt: String) throws -> JournalChange {
        let base = try WireFixture.change(from: mutation)
        var object = try #require(JSONSerialization.jsonObject(with: JSONEncoder().encode(base)) as? [String: Any])
        object["entity_version"] = version
        object["accepted_at"] = acceptedAt
        var entity = try #require(object["snapshot_or_tombstone"] as? [String: Any])
        var meta = try #require(entity["meta"] as? [String: Any])
        meta["entity_version"] = version
        meta["accepted_at"] = acceptedAt
        entity["meta"] = meta
        object["snapshot_or_tombstone"] = entity
        return try JSONDecoder().decode(JournalChange.self, from: JSONSerialization.data(withJSONObject: object))
    }

}
