import Foundation
import SwiftData
import Testing
import BabyCore
import BabyProtocol
@testable import BabyStore

@MainActor @Suite("Immunization sync")
struct ImmunizationSyncTests {
    /// The shared fixture's baby is born "now"; the fixture shot is dated
    /// 2026-09-20, so the baby is moved back before it.
    private func sharedStore() throws -> BabyStore {
        let store = try BabyStore.sharedFixture()
        let baby = try #require(try store.baby(Fixture.babyID))
        baby.birthDate = try WireFixture.date("2026-01-12T00:00:00Z")
        baby.birthDateISO = "2026-01-12"
        try store.context.save()
        return store
    }

    private static func payload(vaccine: String? = "Prevenar 13", givenAt: String? = "2026-09-20T09:30:00Z",
                                dose: String = "2", note: String = "null") -> String {
        var fields = ["\"baby_id\":\"\(Fixture.babyID.uuidString.lowercased())\"", "\"time_zone_id\":\"Europe/Zurich\"",
                      "\"dose\":\(dose)", "\"note\":\(note)"]
        if let vaccine { fields.append("\"vaccine\":\"\(vaccine)\"") }
        if let givenAt { fields.append("\"given_at\":\"\(givenAt)\"") }
        return "{" + fields.joined(separator: ",") + "}"
    }

    @Test func theOutboxMatchesTheFixtureAndTheDeletePayload() throws {
        let store = try sharedStore()
        let fixture = try WireFixture.mutation("immunization.json")
        let row = try store.logImmunization(vaccine: "Prevenar 13", dose: 2, for: Fixture.babyID,
                                            at: WireFixture.date("2026-09-20T09:30:00Z"))
        row.timeZoneID = "Europe/Zurich"
        try store.clearOutboxForTesting()
        try store.record(.create, on: row)
        let mutation = try WireFixture.onlyMutation(in: store, for: row.id)
        #expect(mutation.entityKind == .immunization)
        #expect(mutation.payloadVersion == 1)
        try WireFixture.expectSameShape(mutation.payload, fixture.payload)
        #expect(try ProtocolCanonicalizer.contentSHA256(entityKind: "immunization", payload: mutation.payload)
                == "463b3c456f76db16d2621074d6faff332dd64c13b1d77705f313faff9111f552")
        try store.clearOutboxForTesting()
        try store.softDelete(row)
        let deletion = try WireFixture.onlyMutation(in: store, for: row.id)
        #expect(deletion.payload == .object(["baby_id": .string(Fixture.babyID.uuidString.lowercased())]))
    }

    /// A shot without a dose or note goes out with nulls, never without the keys.
    @Test func aShotWithoutADoseSendsNull() throws {
        let store = try sharedStore()
        let row = try store.logImmunization(vaccine: "MMR", dose: nil, for: Fixture.babyID,
                                            at: WireFixture.date("2026-09-20T09:30:00Z"))
        let mutation = try WireFixture.onlyMutation(in: store, for: row.id)
        guard case let .object(fields) = mutation.payload else { Issue.record("payload is not an object"); return }
        #expect(fields["dose"] == .null)
        #expect(fields["note"] == .null)
        #expect(Set(fields.keys) == ["baby_id", "given_at", "time_zone_id", "vaccine", "dose", "note"])
    }

    @Test func aRemoteShotIsCreatedUpdatedAndTombstoned() throws {
        let store = try sharedStore()
        let id = UUID()
        let created = SyncFixture.change(id, kind: "immunization", version: 1, payload: Self.payload(note: "\"left thigh\""))
        try store.applySyncPage(SyncFixture.page(changes: [created]))
        let row = try #require(try store.lastImmunization(for: Fixture.babyID))
        #expect(row.id == id)
        #expect(row.vaccine == "Prevenar 13")
        #expect(row.dose == 2)
        #expect(row.note == "left thigh")
        #expect(row.givenAt == (try WireFixture.date("2026-09-20T09:30:00Z")))
        #expect(row.timeZoneID == "Europe/Zurich")
        #expect(row.realmID == Fixture.sharedRealm)
        let meta = created.snapshotOrTombstone.meta
        #expect(row.createdBy == meta.creatorAccountID.uuidString.lowercased())
        #expect(row.lastModifiedAt == (try BabyStore.wireDate(meta.acceptedAt)))
        #expect(try store.vaccineNames() == ["Prevenar 13"])
        #expect(try store.nextDose(of: "Prevenar 13", for: Fixture.babyID) == 3)

        let updated = SyncFixture.change(id, kind: "immunization", version: 2,
                                         payload: Self.payload(vaccine: "Prevenar 20", givenAt: "2026-09-19T08:00:00Z", dose: "null"))
        try store.applySyncPage(SyncFixture.page(changes: [updated]))
        #expect(row.vaccine == "Prevenar 20")
        #expect(row.dose == nil)
        #expect(row.note == nil)
        #expect(row.givenAt == (try WireFixture.date("2026-09-19T08:00:00Z")))

        // A pending local edit stays over the server's base when the page repeats.
        try store.editImmunization(row, vaccine: "Prevenar 20", dose: 5, at: row.givenAt, note: "local")
        try store.applySyncPage(SyncFixture.page(changes: [updated]))
        #expect(row.dose == 5)
        #expect(row.note == "local")
        try store.clearOutboxForTesting()

        try store.applySyncPage(SyncFixture.page(changes: [
            SyncFixture.change(id, kind: "immunization", version: 3, tombstoned: true, payload: "null"),
        ]))
        #expect(try store.immunizations(for: Fixture.babyID).isEmpty)
        #expect(try store.vaccineNames().isEmpty)
    }

    @Test func aSnapshotCarriesTheShot() throws {
        let store = try sharedStore()
        let entity = try WireFixture.change(from: WireFixture.mutation("immunization.json")).snapshotOrTombstone
        let snapshotID = UUID()
        let high = SyncFixture.cursor(sequence: 4)
        store.beginSnapshot(snapshotID: snapshotID, highWater: high)
        let unknown = SyncFixture.change(UUID(), kind: "future_kind", version: 1, payload: "{}").snapshotOrTombstone
        #expect(throws: (any Error).self) {
            try store.applySnapshotPage(SnapshotFixture.page(snapshotID: snapshotID, highWater: high, entities: [entity, unknown]))
        }
        #expect(try store.context.fetch(FetchDescriptor<ImmunizationEntry>()).isEmpty)
        try store.applySnapshotPage(SnapshotFixture.page(snapshotID: snapshotID, highWater: high, entities: [entity]))
        try store.finishSnapshot(snapshotID: snapshotID)
        let row = try #require(try store.context.fetch(FetchDescriptor<ImmunizationEntry>()).first)
        #expect(row.vaccine == "Prevenar 13")
        #expect(row.dose == 2)
        #expect(row.givenAt == (try WireFixture.date("2026-09-20T09:30:00Z")))
        #expect(try store.syncCursor() == high)
    }

    // Review Focus 2: a shot this phone cannot read refuses the whole page.
    @Test(arguments: ["no given_at", "bad given_at", "no vaccine", "blank vaccine", "fractional dose", "string dose", "dose 11"])
    func aMalformedRemoteShotRefusesThePage(_ fault: String) throws {
        let store = try sharedStore()
        let bad: String = switch fault {
        case "no given_at": Self.payload(givenAt: nil)
        case "bad given_at": Self.payload(givenAt: "yesterday")
        case "no vaccine": Self.payload(vaccine: nil)
        case "blank vaccine": Self.payload(vaccine: "   ")
        case "fractional dose": Self.payload(dose: "2.5")
        case "string dose": Self.payload(dose: "\"2\"")
        default: Self.payload(dose: "11")
        }
        let med = "{\"baby_id\":\"\(Fixture.babyID.uuidString)\",\"started_at\":\"2026-09-20T14:10:00Z\",\"logged_at\":\"2026-09-20T15:00:00Z\",\"time_zone_id\":\"Europe/Zurich\",\"name\":\"Vitamin D\",\"note\":null}"
        let page = SyncFixture.page(changes: [
            SyncFixture.change(UUID(), kind: "supplement", version: 1, payload: med),
            SyncFixture.change(UUID(), kind: "immunization", version: 1, payload: bad),
        ])
        #expect(throws: (any Error).self) { try store.applySyncPage(page) }
        #expect(try store.syncCursor() == nil)
        #expect(try store.context.fetch(FetchDescriptor<ImmunizationEntry>()).isEmpty)
        #expect(try store.context.fetch(FetchDescriptor<SupplementEntry>()).isEmpty)
    }

    @Test func realmMoveAndPurgeIncludeShotsWithoutTouchingLocalRows() throws {
        let clock = TestDateProvider(.now)
        let store = try BabyStore.inMemory(dateProvider: clock)
        let first = try store.addBaby(name: "Mira", birthDate: clock.now.addingTimeInterval(-86_400), referenceSex: .girls, expectedDueDate: nil)
        let moved = try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: first.id)
        let checkpoint = UUID()
        try store.beginTransition(to: Fixture.sharedRealm, checkpointID: checkpoint)
        try store.commitTransition(checkpointID: checkpoint)
        #expect(moved.realmID == Fixture.sharedRealm)
        try store.setActiveRealm(.local)
        let second = try store.addBaby(name: "Noah", birthDate: clock.now.addingTimeInterval(-86_400), referenceSex: .boys, expectedDueDate: nil)
        let local = try store.logImmunization(vaccine: "MMR", dose: 1, for: second.id)
        // Names are the active realm's, never the other family's.
        #expect(try store.vaccineNames() == ["MMR"])
        try store.purgeSharedRealm(Fixture.sharedRealm)
        #expect(try store.context.fetch(FetchDescriptor<ImmunizationEntry>()).map(\.id) == [local.id])
    }

    @Test func importCapturesTheShotAndRemapsItsBaby() throws {
        let clock = TestDateProvider(.now)
        let store = try BabyStore.inMemory(dateProvider: clock)
        let baby = try store.addBaby(name: "Mira", birthDate: clock.now.addingTimeInterval(-86_400), referenceSex: .girls, expectedDueDate: nil)
        let row = try store.logImmunization(vaccine: "Prevenar 13", dose: 2, for: baby.id)
        let deleted = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby.id)
        try store.softDelete(deleted)
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .preserveLocalIDs)
        let frozen = try store.freezeImportManifest(checkpointID: checkpoint)
        let payloadData = try #require(frozen.sourceBases[EntityKey(kind: "immunization", id: row.id)])
        let payload = try JSONDecoder().decode(JSONValue.self, from: payloadData)
        #expect(WireFixture.field("vaccine", of: payload) == .string("Prevenar 13"))
        #expect(WireFixture.field("dose", of: payload) == .number(2))
        #expect(frozen.sourceBases[EntityKey(kind: "immunization", id: deleted.id)] == nil)
        let mapped = UUID()
        try store.remapBabiesAndDependents([baby.id: mapped])
        #expect(row.babyID == mapped)
    }
}
