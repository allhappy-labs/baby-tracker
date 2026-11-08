import Foundation
import SwiftData
import Testing
import BabyCore
import BabyProtocol
@testable import BabyStore

@MainActor @Suite("Pump sync")
struct PumpSyncTests {
    private func insertPump(_ store: BabyStore, side: PumpSide = .both, startedAt: Date, endedAt: Date? = nil) throws -> PumpEntry {
        let pump = PumpEntry(startedAt: startedAt, endedAt: endedAt, timeZoneID: "Europe/Zurich", side: side,
                             createdBy: store.deviceID, realm: store.activeRealm)
        store.context.insert(pump)
        try store.record(.create, on: pump)
        return pump
    }

    private static func payload(startedAt: String? = "2026-09-21T11:50:00.000000Z", endedAt: String = "null",
                                side: String = "\"left\"", volume: String = "null", note: String = "null") -> String {
        var fields = ["\"ended_at\":\(endedAt)", "\"time_zone_id\":\"Europe/Zurich\"", "\"side\":\(side)",
                      "\"volume_ml\":\(volume)", "\"note\":\(note)"]
        if let startedAt { fields.append("\"started_at\":\"\(startedAt)\"") }
        return "{" + fields.joined(separator: ",") + "}"
    }

    @Test func theOutboxMatchesTheFixtureAndADeleteNamesNoBaby() throws {
        let store = try BabyStore.sharedFixture()
        let fixture = try WireFixture.mutation("pump.json")
        let pump = PumpEntry(id: fixture.entityID, startedAt: try WireFixture.date("2026-09-20T14:10:00Z"),
                             endedAt: try WireFixture.date("2026-09-20T14:30:00Z"), timeZoneID: "Europe/Zurich",
                             side: .both, realm: store.activeRealm)
        pump.volumeML = 90.5
        store.context.insert(pump)
        try store.record(.create, on: pump)
        let mutation = try WireFixture.onlyMutation(in: store, for: pump.id)
        #expect(mutation.entityKind == .pump)
        #expect(mutation.payloadVersion == 1)
        try WireFixture.expectSameShape(mutation.payload, fixture.payload)
        #expect(try ProtocolCanonicalizer.contentSHA256(entityKind: "pump", payload: mutation.payload) == "337fa8f308f7605672a8f1e8aa228b18feb0f4518c44f42a61eea93fb9aac9aa")
        try store.clearOutboxForTesting()
        try store.softDelete(pump)
        let deletion = try WireFixture.onlyMutation(in: store, for: pump.id)
        #expect(deletion.operation == .delete)
        #expect(deletion.payload == .object([:]))
    }

    /// A running pump goes out with nulls, never without the keys, and never
    /// names a baby.
    @Test func aRunningPumpSendsNullsAndNoBaby() throws {
        let store = try BabyStore.sharedFixture()
        let pump = try insertPump(store, startedAt: store.now.addingTimeInterval(-300))
        let mutation = try WireFixture.onlyMutation(in: store, for: pump.id)
        guard case let .object(fields) = mutation.payload else { Issue.record("payload is not an object"); return }
        #expect(fields["ended_at"] == .null)
        #expect(fields["volume_ml"] == .null)
        #expect(fields["note"] == .null)
        #expect(Set(fields.keys) == ["started_at", "ended_at", "time_zone_id", "side", "volume_ml", "note"])
    }

    @Test func aRemotePumpIsCreatedUpdatedAndTombstoned() throws {
        let store = try BabyStore.sharedFixture()
        let id = UUID()
        let created = SyncFixture.change(id, kind: "pump", version: 1, payload: Self.payload())
        try store.applySyncPage(SyncFixture.page(changes: [created]))
        let row = try #require(try store.context.fetch(FetchDescriptor<PumpEntry>()).first)
        #expect(row.id == id)
        #expect(row.side == .left)
        #expect(row.endedAt == nil)
        #expect(row.isRunning)
        #expect(row.startedAt == (try WireFixture.date("2026-09-21T11:50:00Z")))
        #expect(row.timeZoneID == "Europe/Zurich")
        #expect(row.realmID == Fixture.sharedRealm)
        let meta = created.snapshotOrTombstone.meta
        #expect(row.createdBy == meta.creatorAccountID.uuidString.lowercased())
        #expect(row.lastModifiedAt == (try BabyStore.wireDate(meta.acceptedAt)))

        let stopped = SyncFixture.change(id, kind: "pump", version: 2,
                                         payload: Self.payload(endedAt: "\"2026-09-21T12:10:00.000000Z\"", side: "\"both\"",
                                                               volume: "75.25", note: "\"evening\""))
        try store.applySyncPage(SyncFixture.page(changes: [stopped]))
        #expect(row.endedAt == (try WireFixture.date("2026-09-21T12:10:00Z")))
        #expect(row.side == .both)
        #expect(row.volumeML == 75.25)
        #expect(row.note == "evening")
        #expect(!row.isRunning)

        // The amount and the note taken off on the other phone come off here.
        let cleared = SyncFixture.change(id, kind: "pump", version: 3,
                                         payload: Self.payload(endedAt: "\"2026-09-21T12:10:00.000000Z\"", side: "\"both\""))
        try store.applySyncPage(SyncFixture.page(changes: [cleared]))
        #expect(row.volumeML == nil)
        #expect(row.note == nil)
        #expect(row.endedAt != nil)

        try store.applySyncPage(SyncFixture.page(changes: [
            SyncFixture.change(id, kind: "pump", version: 4, tombstoned: true, payload: "null"),
        ]))
        #expect(row.isTombstoned)
    }

    @Test func aSnapshotCarriesAPump() throws {
        let store = try BabyStore.sharedFixture()
        let entity = try WireFixture.change(from: WireFixture.mutation("pump.json")).snapshotOrTombstone
        let snapshotID = UUID()
        let high = SyncFixture.cursor(sequence: 2)
        store.beginSnapshot(snapshotID: snapshotID, highWater: high)
        try store.applySnapshotPage(SnapshotFixture.page(snapshotID: snapshotID, highWater: high, entities: [entity]))
        try store.finishSnapshot(snapshotID: snapshotID)
        let row = try #require(try store.context.fetch(FetchDescriptor<PumpEntry>()).first)
        #expect(row.side == .both)
        #expect(row.volumeML == 90.5)
        #expect(row.endedAt == (try WireFixture.date("2026-09-20T14:30:00Z")))
        #expect(try store.syncCursor() == high)
    }

    /// A pump this phone cannot read refuses the whole page: the cursor does
    /// not advance and nothing is half-applied.
    @Test(arguments: ["no started_at", "bad side", "string volume", "bad ended_at"])
    func aMalformedRemotePumpRefusesThePage(_ fault: String) throws {
        let store = try BabyStore.sharedFixture()
        let bad: String = switch fault {
        case "no started_at": Self.payload(startedAt: nil)
        case "bad side": Self.payload(side: "\"middle\"")
        case "string volume": Self.payload(volume: "\"90\"")
        default: Self.payload(endedAt: "\"yesterday\"")
        }
        let page = SyncFixture.page(changes: [
            SyncFixture.change(UUID(), kind: "pump", version: 1, payload: Self.payload()),
            SyncFixture.change(UUID(), kind: "pump", version: 1, payload: bad),
        ])
        #expect(throws: (any Error).self) { try store.applySyncPage(page) }
        #expect(try store.syncCursor() == nil)
        #expect(try store.context.fetch(FetchDescriptor<PumpEntry>()).isEmpty)
    }

    @Test func realmMoveAndPurgeIncludePumpsWithoutTouchingLocalRows() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let moved = try insertPump(store, startedAt: store.now.addingTimeInterval(-600), endedAt: store.now)
        let checkpoint = UUID()
        try store.beginTransition(to: Fixture.sharedRealm, checkpointID: checkpoint)
        try store.commitTransition(checkpointID: checkpoint)
        #expect(moved.realmID == Fixture.sharedRealm)
        #expect(try store.migratedEntityKinds().contains(.pump))
        try store.setActiveRealm(.local)
        let local = try insertPump(store, startedAt: store.now.addingTimeInterval(-300), endedAt: store.now)
        try store.purgeSharedRealm(Fixture.sharedRealm)
        #expect(try store.context.fetch(FetchDescriptor<PumpEntry>()).map(\.id) == [local.id])
    }

    // Review Focus 4. A pump has no baby for the import resolver to map. It
    // travels as a plain create (no import id), never as a manifest record
    // the server would refuse.
    @Test func importSendsPumpsAsPlainCreates() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        _ = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let pump = try insertPump(store, startedAt: store.now.addingTimeInterval(-900), endedAt: store.now.addingTimeInterval(-300))
        let deleted = try insertPump(store, startedAt: store.now.addingTimeInterval(-9000), endedAt: store.now.addingTimeInterval(-8000))
        try store.softDelete(deleted)
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .preserveLocalIDs)
        let frozen = try store.freezeImportManifest(checkpointID: checkpoint)
        #expect(frozen.sourceBases[EntityKey(kind: "pump", id: pump.id)] == nil)
        #expect(try frozen.records().allSatisfy { $0.entityKind != "pump" })
        try store.enqueueLocalPumps(realm: Fixture.sharedRealm)
        let rows = try store.liveOutboxRows(in: Fixture.sharedRealm).filter { $0.entityKind == .pump }
        #expect(rows.map(\.entityID) == [pump.id])
        let mutation = try JSONDecoder().decode(WireMutation.self, from: try #require(rows.first).payload)
        #expect(mutation.entityKind == .pump)
        #expect(mutation.operation == .create)
        #expect(mutation.importID == nil)
        #expect(mutation.observedEntityVersion == 0)
    }

    /// A pump still running when the family is created moves with the
    /// workspace and goes as one create with no end, beside a finished one.
    @Test func aPumpRunningAtFamilyCreationGoesUpStillRunning() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let finished = try store.logPump(side: .left, from: store.now.addingTimeInterval(-9000), to: store.now.addingTimeInterval(-8000), volumeML: 40, note: nil)
        let running = try store.startPump(side: .both, at: store.now.addingTimeInterval(-600))
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .preserveLocalIDs)
        _ = try store.freezeImportManifest(checkpointID: checkpoint)
        try store.saveMapping([baby.id: baby.id], checkpointID: checkpoint)
        try store.rewriteAndMoveWorkspace(checkpointID: checkpoint, to: Fixture.sharedRealm)
        #expect(running.realmID == Fixture.sharedRealm && finished.realmID == Fixture.sharedRealm)
        #expect(try store.runningPump()?.id == running.id)
        let rows = try store.liveOutboxRows(in: Fixture.sharedRealm).filter { $0.entityKind == .pump }
        #expect(Set(rows.map(\.entityID)) == [running.id, finished.id])
        #expect(rows.allSatisfy { $0.operation == .create })
        let sent = try JSONDecoder().decode(WireMutation.self, from: try #require(rows.first { $0.entityID == running.id }).payload)
        #expect(WireFixture.field("ended_at", of: sent.payload) == .null)
    }

    // Review Focus 1. The other phone's pump reached the server first. This
    // phone had already stopped its own: that session is real and goes
    // again, finished.
    @Test func aLosingStartAlreadyStoppedIsSentAgainFinished() throws {
        let store = try BabyStore.sharedFixture()
        let mine = try insertPump(store, side: .right, startedAt: store.now.addingTimeInterval(-1200))
        let start = try #require(try store.liveOutboxRows().first { $0.entityID == mine.id })
        mine.endedAt = store.now
        mine.volumeML = 60
        try store.record(.update, on: mine)
        let winner = UUID()
        try store.applySyncPage(SyncFixture.page(outcomes: [Self.supersededByRunningPump(start.id, winner: winner)]))
        let rows = try store.context.fetch(FetchDescriptor<PumpEntry>())
        #expect(rows.first { $0.id == winner }?.isRunning == true)
        #expect(rows.first { $0.id == winner }?.side == .left)
        #expect(!mine.isTombstoned)
        #expect(mine.endedAt != nil)
        #expect(mine.volumeML == 60)
        let pending = try store.nextDispatchBatch(limit: 20).filter { $0.entityID == mine.id }
        #expect(pending.count == 1)
        #expect(pending.first?.operation == .create)
        #expect(pending.first?.observedEntityVersion == 0)
        #expect(pending.first.map { WireFixture.field("ended_at", of: $0.payload) != .null } == true)
        #expect(pending.first.map { WireFixture.field("volume_ml", of: $0.payload) == .number(60) } == true)
    }

    // Review Focus 1. Still running on this phone: it gives way, and the
    // winner shows.
    @Test func aLosingStartStillRunningGivesWayToTheWinner() throws {
        let store = try BabyStore.sharedFixture()
        let mine = try insertPump(store, startedAt: store.now.addingTimeInterval(-300))
        let start = try #require(try store.liveOutboxRows().first { $0.entityID == mine.id })
        let winner = UUID()
        try store.applySyncPage(SyncFixture.page(outcomes: [Self.supersededByRunningPump(start.id, winner: winner)]))
        #expect(mine.isTombstoned)
        let running = try store.context.fetch(FetchDescriptor<PumpEntry>()).filter(\.isRunning)
        #expect(running.map(\.id) == [winner])
        #expect(try store.nextDispatchBatch(limit: 20).filter { $0.entityID == mine.id }.isEmpty)
    }

    // The other parent stopped the session and entered 90 ml; this phone had
    // not synced and stopped it too. The server keeps the finished session and
    // says so: this phone shows that, not its own late Stop without an amount.
    @Test func aStaleStopGivesWayToTheSessionTheOtherParentFinished() throws {
        let store = try BabyStore.sharedFixture()
        let id = UUID()
        try store.applySyncPage(SyncFixture.page(changes: [SyncFixture.change(id, kind: "pump", version: 1, payload: Self.payload())]))
        let pump = try #require(try store.pump(id: id))
        try store.stopPump(pump)
        let stop = try #require(try store.liveOutboxRows().first { $0.entityID == id })
        #expect(pump.volumeML == nil)
        let finished = Self.payload(endedAt: "\"2026-09-21T12:10:00.000000Z\"", volume: "90")
        try store.applySyncPage(SyncFixture.page(outcomes: [
            Self.outcome(stop.id, status: "superseded", code: "pump_already_stopped", entity: id, version: 2, payload: finished),
        ]))
        #expect(pump.endedAt == (try WireFixture.date("2026-09-21T12:10:00Z")))
        #expect(pump.volumeML == 90)
        #expect(try store.nextDispatchBatch(limit: 20).filter { $0.entityID == id }.isEmpty)
    }

    static func outcome(_ mutationID: UUID, status: String, code: String, entity: UUID, version: Int, payload: String) -> Outcome {
        let stamp = "2026-09-21T12:10:00.000000Z"
        let family = SyncFixture.familyID.uuidString.lowercased()
        let json = """
        {"mutation_id":"\(mutationID.uuidString.lowercased())","status":"\(status)","original_status":null,
         "canonical_entity":{"entity_kind":"pump","meta":{"id":"\(entity.uuidString.lowercased())","family_id":"\(family)",
           "entity_version":\(version),"creator_account_id":"\(family)","last_editor_account_id":"\(family)",
           "client_mutation_time":"\(stamp)","accepted_at":"\(stamp)","tombstoned_at":null},
           "payload":\(payload)},
         "related_entities":[],"cursor":null,"code":"\(code)","overwrote_newer_version":false}
        """
        return try! JSONDecoder().decode(Outcome.self, from: Data(json.utf8))
    }

    static func supersededByRunningPump(_ mutationID: UUID, winner: UUID) -> Outcome {
        let stamp = "2026-09-21T12:00:00.000000Z"
        let family = SyncFixture.familyID.uuidString.lowercased()
        let json = """
        {"mutation_id":"\(mutationID.uuidString.lowercased())","status":"superseded","original_status":null,
         "canonical_entity":{"entity_kind":"pump","meta":{"id":"\(winner.uuidString.lowercased())","family_id":"\(family)",
           "entity_version":1,"creator_account_id":"\(family)","last_editor_account_id":"\(family)",
           "client_mutation_time":"\(stamp)","accepted_at":"\(stamp)","tombstoned_at":null},
           "payload":{"started_at":"2026-09-21T11:50:00.000000Z","ended_at":null,"time_zone_id":"Europe/Zurich","side":"left","volume_ml":null,"note":null}},
         "related_entities":[],"cursor":null,"code":"pump_running","overwrote_newer_version":false}
        """
        return try! JSONDecoder().decode(Outcome.self, from: Data(json.utf8))
    }
}
