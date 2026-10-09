import Foundation
import SwiftData
import Testing
@testable import BabyCore
@testable import BabyProtocol
@testable import BabyStore

/// Baby, sleep and diaper records in the server's wire shape.
///
/// The server decodes every mutation payload with unknown fields disallowed
/// and required keys enforced, so a payload in the local column spelling is
/// refused and the record never reaches the other phone. The shared fixtures
/// in `Server/api/fixtures/v1` are the contract: these tests compare against
/// their decoded JSON, never against hand-written strings.
@MainActor
@Suite("Baby, sleep and diaper wire shape")
struct WireShapeTests {
    // MARK: Outbox payloads match the fixtures

    @Test func babyOutboxPayloadMatchesTheFixture() throws {
        let fixture = try WireFixture.mutation("baby.json")
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        try store.activateRealmForTesting(Fixture.sharedRealm)

        let baby = try store.addBaby(name: "Élie", birthDate: WireFixture.localNoon("2026-01-12"), referenceSex: .girls, expectedDueDate: nil)

        let mutation = try WireFixture.onlyMutation(in: store, for: baby.id)
        #expect(mutation.entityKind == fixture.entityKind)
        #expect(mutation.operation == fixture.operation)
        #expect(mutation.payloadVersion == fixture.payloadVersion)
        // A new baby now carries its assigned icon. `icon` is optional on the
        // wire and the shared fixture is the minimal create, so it is checked
        // here and the rest is held to the fixture.
        #expect(WireFixture.field("icon", of: mutation.payload) == .string("girl.1"))
        guard case var .object(fields) = mutation.payload else {
            Issue.record("payload is not an object: \(mutation.payload)")
            return
        }
        fields["icon"] = nil
        try WireFixture.expectSameShape(.object(fields), fixture.payload)
    }

    @Test func sleepOutboxPayloadMatchesTheFixture() throws {
        let fixture = try WireFixture.mutation("sleep.json")
        let store = try BabyStore.sharedFixture()

        let sleep = SleepEntry(
            id: fixture.entityID, babyID: Fixture.babyID,
            startedAt: try WireFixture.date("2026-09-20T13:00:00Z"), timeZoneID: "Europe/Zurich",
            realm: store.activeRealm
        )
        sleep.endedAt = try WireFixture.date("2026-09-20T14:00:00Z")
        sleep.kindOverride = .nap
        store.context.insert(sleep)
        try store.record(.create, on: sleep)

        let mutation = try WireFixture.onlyMutation(in: store, for: sleep.id)
        #expect(mutation.entityKind == fixture.entityKind)
        #expect(mutation.operation == fixture.operation)
        // The fixture already carries the session link at v2: the key is
        // present and null for a sleep with no session.
        #expect(mutation.payloadVersion == fixture.payloadVersion)
        try WireFixture.expectSameShape(mutation.payload, fixture.payload)
    }

    /// New client writes always carry the session link, even when there is
    /// none to report — the server requires the key at v2.
    @Test func aSleepAlwaysSendsItsSessionLinkAtVersionTwo() throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
        let store = try BabyStore.inMemory(dateProvider: clock)
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let sleep = try store.logSleep(from: clock.now.addingTimeInterval(-3_600), to: clock.now, for: baby.id)
        let fields = sleep.payloadFields()
        #expect(fields.keys.contains("sleep_session_id"))
        #expect(WireEntityKind.sleep.currentPayloadVersion == 2)
    }

    @Test func diaperOutboxPayloadMatchesTheFixture() throws {
        let fixture = try WireFixture.mutation("diaper.json")
        let store = try BabyStore.sharedFixture()

        let diaper = DiaperEntry(
            id: fixture.entityID, babyID: Fixture.babyID, contents: .mixed,
            startedAt: try WireFixture.date("2026-09-20T14:10:00Z"), timeZoneID: "Europe/Zurich",
            realm: store.activeRealm
        )
        diaper.consistency = "soft"
        diaper.color = "mustard"
        store.context.insert(diaper)
        try store.record(.create, on: diaper)

        let mutation = try WireFixture.onlyMutation(in: store, for: diaper.id)
        #expect(mutation.entityKind == fixture.entityKind)
        #expect(mutation.operation == fixture.operation)
        #expect(mutation.payloadVersion == fixture.payloadVersion)
        try WireFixture.expectSameShape(mutation.payload, fixture.payload)
    }

    /// The server's delete payload is `{}` for a baby and `{"baby_id"}` for a
    /// record — anything else is an unknown field and the delete is refused.
    @Test func deletesCarryOnlyWhatTheServerAccepts() throws {
        let store = try BabyStore.sharedFixture()
        let sleep = try store.logSleep(from: .now.addingTimeInterval(-600), to: .now, for: Fixture.babyID)
        let diaper = try store.logDiaper(.wet, for: Fixture.babyID)
        let baby = try #require(try store.baby(Fixture.babyID))
        try store.clearOutboxForTesting()

        try store.softDelete(sleep)
        try store.softDelete(diaper)
        try store.deleteBaby(baby)

        let babyID = JSONValue.string(Fixture.babyID.uuidString.lowercased())
        #expect(try WireFixture.onlyMutation(in: store, for: sleep.id).payload == .object(["baby_id": babyID]))
        #expect(try WireFixture.onlyMutation(in: store, for: diaper.id).payload == .object(["baby_id": babyID]))
        #expect(try WireFixture.onlyMutation(in: store, for: baby.id).payload == .object([:]))
    }

    /// A running-timer edit keeps its patch shape at v5.
    @Test func timerEditMatchesTheFixtureAndItsCanonicalVector() throws {
        let fixture = try #require(try WireFixture.loader.decode([String: WireMutation].self, named: "mutations.json")["timer_edit"])
        let vector = try WireFixture.loader.decode(CanonicalVectors.self, named: "canonical-vectors.json").timerEditMutation
        let clock = TestDateProvider(try WireFixture.date("2026-09-20T15:07:00Z"))
        let store = try BabyStore.sharedFixture(dateProvider: clock)
        let left = try #require(UUID(uuidString: "30000000-0000-4000-8000-000000000011"))
        let bottle = try #require(UUID(uuidString: "30000000-0000-4000-8000-000000000012"))
        try store.applySyncPage(SyncFixture.page(changes: [SyncFixture.change(fixture.entityID, kind: "timer", version: 2, payload: """
        {"baby_id":"\(Fixture.babyID.uuidString.lowercased())","timer_kind":"feed","started_at":"2026-09-20T14:50:00.000000Z",
         "time_zone_id":"Europe/Zurich","current_side":"right","current_side_started_at":"2026-09-20T15:05:00.000000Z",
         "completed_components":[{"id":"\(left.uuidString.lowercased())","kind":"breast","side":"left","seconds":400},
          {"id":"\(bottle.uuidString.lowercased())","kind":"bottle","volume_ml":60,"contents":"formula"}],
         "current_bottle_started_at":null,"session_id":null}
        """)]))

        // Named out of order: the payload lists parts in the timer's order.
        try store.editRunningTimer(
            id: fixture.entityID,
            startedAt: try WireFixture.date("2026-09-20T14:58:00Z"),
            components: [.bottle(id: bottle, volumeML: 70, contents: .formula), .breast(id: left, side: .left, seconds: 300)]
        )

        let mutation = try WireFixture.onlyMutation(in: store, for: fixture.entityID)
        #expect(mutation.entityKind == fixture.entityKind)
        #expect(mutation.operation == fixture.operation)
        // Every edit now carries the timer's session link; this timer has
        // none, so the key is present and null, matching the fixture at v5.
        #expect(mutation.payloadVersion == fixture.payloadVersion)
        #expect(mutation.observedEntityVersion == fixture.observedEntityVersion)
        try WireFixture.expectSameShape(mutation.payload, fixture.payload)
        #expect(String(decoding: try ProtocolCanonicalizer.canonicalMutationBytes(mutation), as: UTF8.self) == vector.canonicalJSON)
    }

    // MARK: Fixture payloads apply as remote changes

    /// Applied over a row this phone already has, so every column goes
    /// through the update path rather than the first-sight insert.
    @Test func babyFixtureAppliesAsARemoteChange() throws {
        let fixture = try WireFixture.mutation("baby.json")
        let store = try BabyStore.sharedFixture()
        let existing = try #require(try store.baby(fixture.entityID))
        existing.birthDate = try WireFixture.localNoon("2024-06-01")
        existing.sortOrder = 3
        try store.context.save()

        try store.applySyncPage(SyncFixture.page(changes: [try WireFixture.change(from: fixture)]))

        let baby = try #require(try store.baby(fixture.entityID))
        #expect(baby.name == "Élie")
        #expect(try LocalDate(displaying: baby.birthDate, in: .current).iso8601 == "2026-01-12")
        #expect(baby.birthDateISO == "2026-01-12")
        #expect(baby.sortOrder == 0)
    }

    @Test func sleepFixtureAppliesAsARemoteChange() throws {
        let fixture = try WireFixture.mutation("sleep.json")
        let store = try BabyStore.sharedFixture()

        try store.applySyncPage(SyncFixture.page(changes: [try WireFixture.change(from: fixture)]))

        let sleep = try #require(try store.sleepEntryForTesting(fixture.entityID))
        #expect(sleep.babyID == Fixture.babyID)
        #expect(sleep.startedAt == (try WireFixture.date("2026-09-20T13:00:00Z")))
        #expect(sleep.endedAt == (try WireFixture.date("2026-09-20T14:00:00Z")))
        #expect(sleep.timeZoneID == "Europe/Zurich")
        #expect(sleep.note == nil)
        #expect(sleep.kindOverride == .nap)
    }

    /// Also over an existing row: the fixture's nulls must clear what the
    /// row had, not leave it behind.
    @Test func diaperFixtureAppliesAsARemoteChange() throws {
        let fixture = try WireFixture.mutation("diaper.json")
        let store = try BabyStore.sharedFixture()
        let existing = DiaperEntry(
            id: fixture.entityID, babyID: Fixture.babyID, contents: .wet,
            startedAt: .now, timeZoneID: "UTC", realm: store.activeRealm
        )
        existing.endedAt = .now
        existing.note = "stale"
        existing.consistency = "firm"
        existing.color = "brown"
        store.context.insert(existing)
        try store.context.save()

        try store.applySyncPage(SyncFixture.page(changes: [try WireFixture.change(from: fixture)]))

        let diaper = try #require(try store.diaperEntryForTesting(fixture.entityID))
        #expect(diaper.babyID == Fixture.babyID)
        #expect(diaper.startedAt == (try WireFixture.date("2026-09-20T14:10:00Z")))
        #expect(diaper.endedAt == nil)
        #expect(diaper.timeZoneID == "Europe/Zurich")
        #expect(diaper.note == nil)
        #expect(diaper.contents == .mixed)
        #expect(diaper.consistency == "soft")
        #expect(diaper.color == "mustard")
    }

    /// A delete still waiting to be sent keeps the row hidden when an older
    /// live version arrives. The delete payload no longer says "tombstoned",
    /// so the overlay replay has to know it from the operation itself.
    @Test func pendingLocalDeleteKeepsTheRowHiddenUnderALiveBase() throws {
        let store = try BabyStore.sharedFixture()
        let sleep = try store.logSleep(from: .now.addingTimeInterval(-600), to: .now, for: Fixture.babyID)
        let create = try #require(try store.pendingMutationIDs(for: sleep.id).first)
        try store.softDelete(sleep)

        try store.applySyncPage(SyncFixture.page(
            outcomes: [SyncFixture.applied(create)],
            changes: [SyncFixture.change(sleep.id, kind: "sleep", version: 1, payload: """
            {"baby_id":"\(Fixture.babyID.uuidString.lowercased())","started_at":"2026-09-20T13:00:00Z",
             "ended_at":null,"time_zone_id":"UTC","note":null,"kind_override":null}
            """)]
        ))

        #expect(try store.sleepEntryForTesting(sleep.id)?.isTombstoned == true)
    }

    // MARK: Round trip between two phones

    @Test func babyRoundTripsBetweenTwoStores() throws {
        let a = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        try a.activateRealmForTesting(Fixture.sharedRealm)
        let b = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        try b.activateRealmForTesting(Fixture.sharedRealm)
        _ = try a.addBaby(name: "First", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let sent = try a.addBaby(name: "Noa", birthDate: WireFixture.localNoon("2025-12-31"), referenceSex: .girls, expectedDueDate: nil)

        try WireFixture.deliver(try WireFixture.onlyMutation(in: a, for: sent.id), to: b)

        let received = try #require(try b.baby(sent.id))
        #expect(received.name == sent.name)
        #expect(received.sortOrder == sent.sortOrder)
        #expect(try LocalDate(displaying: received.birthDate, in: .current) == LocalDate(displaying: sent.birthDate, in: .current))
    }

    @Test func sleepRoundTripsBetweenTwoStores() throws {
        let a = try BabyStore.sharedFixture()
        let b = try BabyStore.sharedFixture()
        let start = try WireFixture.date("2026-09-21T01:15:30Z")
        let sent = try a.logSleep(from: start, to: start.addingTimeInterval(5400), for: Fixture.babyID)
        sent.note = "Woke once"
        sent.kindOverride = .night
        try a.update(sent)

        try WireFixture.deliver(try #require(try a.liveOutboxRows().last { $0.entityID == sent.id }).decodedMutation(), to: b)

        let received = try #require(try b.sleepEntryForTesting(sent.id))
        #expect(received.babyID == sent.babyID)
        #expect(received.startedAt == sent.startedAt)
        #expect(received.endedAt == sent.endedAt)
        #expect(received.timeZoneID == sent.timeZoneID)
        #expect(received.note == sent.note)
        #expect(received.kindOverrideRaw == sent.kindOverrideRaw)
        #expect(received.isTombstoned == false)
    }

    @Test func diaperRoundTripsBetweenTwoStores() throws {
        let a = try BabyStore.sharedFixture()
        let b = try BabyStore.sharedFixture()
        let sent = try a.logDiaper(
            .dirty, for: Fixture.babyID, at: try WireFixture.date("2026-09-21T08:05:00Z"),
            consistency: "runny", color: "green"
        )

        try WireFixture.deliver(try WireFixture.onlyMutation(in: a, for: sent.id), to: b)

        let received = try #require(try b.diaperEntryForTesting(sent.id))
        #expect(received.babyID == sent.babyID)
        #expect(received.startedAt == sent.startedAt)
        #expect(received.endedAt == sent.endedAt)
        #expect(received.timeZoneID == sent.timeZoneID)
        #expect(received.contentsRaw == sent.contentsRaw)
        #expect(received.consistency == sent.consistency)
        #expect(received.color == sent.color)
        #expect(received.note == sent.note)
    }

    /// A feed payload is full state, like sleep's and diaper's: a note or an
    /// end time taken off on one phone has to come off on the other. Sent as
    /// an absent key and read as "leave it", a cleared note stayed on the
    /// other caregiver's phone for good.
    @Test func clearingAFeedNoteAndEndClearsThemOnTheOtherPhone() throws {
        let a = try BabyStore.sharedFixture()
        let b = try BabyStore.sharedFixture()
        let sent = try a.logBottle(volumeML: 90, contents: .formula, for: Fixture.babyID)
        sent.note = "Spat up a little"
        sent.endedAt = sent.startedAt.addingTimeInterval(600)
        try a.update(sent)
        func latest() throws -> WireMutation {
            try #require(try a.liveOutboxRows().last { $0.entityID == sent.id }).decodedMutation()
        }
        func deliver(_ mutation: WireMutation, version: Int64) throws {
            try b.applySyncPage(SyncFixture.page(changes: [SyncFixture.change(
                mutation.entityID, kind: "feed", version: version,
                payload: String(decoding: try JSONEncoder().encode(mutation.payload), as: UTF8.self)
            )]))
        }
        try deliver(try latest(), version: 1)
        #expect(try b.feedEntryForTesting(sent.id)?.note == "Spat up a little")
        #expect(try b.feedEntryForTesting(sent.id)?.endedAt != nil)

        sent.note = nil
        sent.endedAt = nil
        try a.update(sent)
        let cleared = try latest()
        #expect(WireFixture.field("note", of: cleared.payload) == .null)
        #expect(WireFixture.field("ended_at", of: cleared.payload) == .null)
        try deliver(cleared, version: 2)

        let received = try #require(try b.feedEntryForTesting(sent.id))
        #expect(received.note == nil)
        #expect(received.endedAt == nil)
    }

    // MARK: Import

    /// The owner's import. Babies are published by the server's resolver and
    /// an import-tagged baby create is refused (`invalid_import_operation`),
    /// so none is queued. Records go over in the fixture shape, pointing at
    /// the baby the resolver mapped them to.
    @Test func importQueuesWireShapedRecordsAndNoBabyCreates() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let sleep = try store.logSleep(from: .now.addingTimeInterval(-600), to: nil, for: baby.id)
        let diaper = try store.logDiaper(.wet, for: baby.id)
        let checkpoint = UUID()
        let canonical = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .matchShared)
        _ = try store.freezeImportManifest(checkpointID: checkpoint)
        try store.saveMapping([baby.id: canonical], checkpointID: checkpoint)

        try store.rewriteAndMoveWorkspace(checkpointID: checkpoint, to: ImportFixture.realm)

        let queued = try store.liveOutboxRows(in: ImportFixture.realm).map { try $0.decodedMutation() }
        #expect(queued.allSatisfy { $0.entityKind != .baby })
        let sleepMutation = try #require(queued.first { $0.entityID == sleep.id })
        let diaperMutation = try #require(queued.first { $0.entityID == diaper.id })
        // The fixture already carries the session link; a fresh sleep always
        // sends it, even with no session to report.
        try WireFixture.expectSameKeys(sleepMutation.payload, try WireFixture.mutation("sleep.json").payload)
        try WireFixture.expectSameKeys(diaperMutation.payload, try WireFixture.mutation("diaper.json").payload)
        let babyKey = JSONValue.string(canonical.uuidString.lowercased())
        #expect(WireFixture.field("baby_id", of: sleepMutation.payload) == babyKey)
        #expect(WireFixture.field("baby_id", of: diaperMutation.payload) == babyKey)
    }

    /// The resolver still reads the frozen wire-shaped bases for each baby's
    /// name, birth date and order, and each record's parent.
    @Test func manifestReadsBabiesAndParentsFromWireShapedBases() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        _ = try store.addBaby(name: "First", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let baby = try store.addBaby(name: "Mira", birthDate: WireFixture.localNoon("2025-03-04"), referenceSex: .girls, expectedDueDate: nil)
        let diaper = try store.logDiaper(.wet, for: baby.id)
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .preserveLocalIDs)

        let manifest = try store.freezeImportManifest(checkpointID: checkpoint)

        let importBaby = try #require(try manifest.babies().first { $0.localBabyID == baby.id })
        #expect(importBaby.name == "Mira")
        #expect(importBaby.birthDate == "2025-03-04")
        #expect(importBaby.sortOrder == 1)
        #expect(try manifest.records().first { $0.recordID == diaper.id }?.localBabyID == baby.id)
    }
}

/// Reads the shared protocol fixtures and compares payloads structurally.
@MainActor
enum WireFixture {
    static var loader: ProtocolFixtureLoader {
        ProtocolFixtureLoader(directory: repositoryRoot.appending(path: "Server/api/fixtures/v1"))
    }

    static func mutation(_ name: String) throws -> WireMutation {
        try loader.decode(WireMutation.self, named: name)
    }

    static func onlyMutation(in store: BabyStore, for entityID: UUID) throws -> WireMutation {
        let rows = try store.pendingOutbox().filter { $0.entityID == entityID }
        try #require(rows.count == 1)
        return try rows[0].decodedMutation()
    }

    /// Hands a mutation's payload to another store the way the server echoes
    /// it back: as the canonical payload of a journal change.
    static func deliver(_ mutation: WireMutation, to store: BabyStore) throws {
        try store.applySyncPage(SyncFixture.page(changes: [try change(from: mutation)]))
    }

    static func change(from mutation: WireMutation) throws -> JournalChange {
        SyncFixture.change(
            mutation.entityID, kind: mutation.entityKind.rawValue, version: 1,
            payload: String(decoding: try JSONEncoder().encode(mutation.payload), as: UTF8.self)
        )
    }

    static func field(_ key: String, of payload: JSONValue) -> JSONValue? {
        guard case let .object(fields) = payload else { return nil }
        return fields[key]
    }

    /// Same keys, same JSON types, same values — timestamps compared as
    /// instants, since `…:00Z` and `…:00.000Z` are the same moment.
    static func expectSameShape(_ actual: JSONValue, _ expected: JSONValue) throws {
        guard case let .object(actualFields) = actual, case let .object(expectedFields) = expected else {
            Issue.record("payloads are not objects: \(actual) vs \(expected)")
            return
        }
        #expect(Set(actualFields.keys) == Set(expectedFields.keys))
        for (key, expectedValue) in expectedFields {
            let actualValue = actualFields[key]
            if key.hasSuffix("_at"), case let .string(expectedTime) = expectedValue {
                guard case let .string(actualTime) = actualValue else {
                    Issue.record("\(key) is \(String(describing: actualValue)), expected a timestamp")
                    continue
                }
                #expect(try date(actualTime) == date(expectedTime), "\(key)")
            } else {
                #expect(actualValue == expectedValue, "\(key)")
            }
        }
    }

    static func expectSameKeys(_ actual: JSONValue, _ expected: JSONValue) throws {
        guard case let .object(actualFields) = actual, case let .object(expectedFields) = expected else {
            Issue.record("payloads are not objects")
            return
        }
        #expect(Set(actualFields.keys) == Set(expectedFields.keys))
    }

    static func date(_ value: String) throws -> Date {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let date = formatter.date(from: value) { return date }
        formatter.formatOptions = [.withInternetDateTime]
        return try #require(formatter.date(from: value))
    }

    /// Noon on a calendar day in the device's zone, the way the add-baby
    /// sheet stores a birth date.
    static func localNoon(_ iso: String) throws -> Date {
        let parts = iso.split(separator: "-").compactMap { Int($0) }
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = .current
        return try #require(calendar.date(from: DateComponents(year: parts[0], month: parts[1], day: parts[2], hour: 12)))
    }

    private static var repositoryRoot: URL {
        URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
    }
}
