import Foundation
import SwiftData
import Testing
@testable import BabyCore
@testable import BabyProtocol
@testable import BabyStore

/// A reinstalled phone reads the family's whole history back.
///
/// The owner replays the server journal from sequence 0 and a joiner takes a
/// snapshot. Migration 000027 rewrote every stored row to its kind's current
/// shape (feed 4, timer 5, sleep 2, every other kind 1), and this client
/// reads only that shape. So each shape the server emits must apply,
/// including tombstones, a running bottle (`current_bottle_started_at`) and
/// a solids part with no foods. The payloads are the committed fixtures in
/// `Server/api/fixtures/v1`.
@MainActor
@Suite("Owner reinstall replays the journal")
struct OwnerReplayTests {
    static let timerFeedID = UUID(uuidString: "50000000-0000-4000-8000-000000000001")!
    static let timerSleepID = UUID(uuidString: "50000000-0000-4000-8000-000000000002")!
    static let noFoodsID = UUID(uuidString: "20000000-0000-4000-8000-0000000000f1")!
    static let diaperID = UUID(uuidString: "40000000-0000-4000-8000-0000000000f2")!
    static let runningBottleStart = "2026-09-20T15:06:00.000000Z"

    /// Every entity in the family, in the order the journal wrote it. The
    /// same entity appears again when a later change replaced it.
    static func history() throws -> [(entity: CanonicalEntity, operation: String)] {
        var history: [(CanonicalEntity, String)] = []
        var versions: [UUID: Int64] = [:]
        func add(_ id: UUID, _ kind: String, _ operation: String, _ payload: JSONValue?) throws {
            let version = (versions[id] ?? 0) + 1
            versions[id] = version
            history.append((try entity(id, kind: kind, version: version, payload: payload), operation))
        }
        func fixture(_ name: String) throws -> WireMutation { try WireFixture.mutation(name) }

        let baby = try fixture("baby.json")
        try add(baby.entityID, "baby", "create", baby.payload)
        for name in ["feed-bottle.json", "feed-breast.json", "feed-combination.json", "feed-solids.json",
                     "sleep.json", "diaper.json", "supplement.json", "growth-profile.json", "measurement.json",
                     "immunization.json", "pump.json"] {
            let record = try fixture(name)
            try add(record.entityID, record.entityKind.rawValue, record.operation.rawValue, record.payload)
        }
        // A solids part whose foods are empty: the server writes `foods: []`.
        let solids = try fixture("feed-solids.json")
        try add(noFoodsID, "feed", "create", try replacing(solids.payload, "components", with: .array([.object([
            "id": .string("31000000-0000-4000-8000-0000000000f1"), "kind": .string("solids"),
            "foods": .array([]), "amount": .string("taste"),
        ])])))

        // A running feed: a side, then a breast part with its step bounds
        // and a bottle running since `current_bottle_started_at`.
        let timerFeed = try fixture("timer-feed.json")
        try add(timerFeedID, "timer", "timer_start", try replacing(timerFeed.payload, "current_bottle_started_at", with: .null))
        var running = try replacing(timerFeed.payload, "current_side", with: .null)
        running = try replacing(running, "current_side_started_at", with: .null)
        running = try replacing(running, "current_bottle_started_at", with: .string(runningBottleStart))
        running = try replacing(running, "completed_components", with: .array([.object([
            "id": .string("30000000-0000-4000-8000-000000000020"), "kind": .string("breast"), "side": .string("left"),
            "seconds": .number(300), "started_at": .string("2026-09-20T15:00:00.000000Z"),
            "ended_at": .string("2026-09-20T15:05:00.000000Z"),
        ])]))
        try add(timerFeedID, "timer", "timer_finish_bottle", running)

        // A sleep timer that ran and stopped: the stop tombstones the timer.
        let timerSleep = try fixture("timer-sleep.json")
        try add(timerSleepID, "timer", "timer_start", try replacing(timerSleep.payload, "current_bottle_started_at", with: .null))
        try add(timerSleepID, "timer", "timer_stop", nil)

        // A finished timer's feed, exactly as the server returns it.
        let outcomes = try WireFixture.loader.decode(OutcomesFixture.self, named: "outcomes.json")
        for related in outcomes.outcomes.flatMap(\.relatedEntities) {
            try add(related.meta.id, related.entityKind, "create", related.payload)
        }

        // A deleted record: the journal keeps only its tombstone.
        let diaper = try fixture("diaper.json")
        try add(diaperID, "diaper", "create", diaper.payload)
        try add(diaperID, "diaper", "delete", nil)
        return history
    }

    @Test func theOwnerReplaysEveryCurrentShapeFromTheStart() throws {
        let store = try Self.emptySharedStore()
        let history = try Self.history()
        let changes = try history.enumerated().map { index, item in
            try Self.change(item.entity, operation: item.operation, sequence: Int64(index + 1))
        }
        let highWater = SyncFixture.cursor(sequence: Int64(changes.count))

        try store.applySyncPage(SyncFixture.page(changes: changes, highWater: highWater))

        try Self.expectTheFamily(in: store)
        #expect(try store.syncCursor() == highWater)
    }

    /// A joiner reads the same history as a snapshot: each entity's last
    /// version only.
    @Test func aJoinerReadsEveryCurrentShapeFromASnapshot() throws {
        let store = try Self.emptySharedStore()
        var latest: [UUID: CanonicalEntity] = [:]
        var order: [UUID] = []
        for item in try Self.history() {
            if latest[item.entity.meta.id] == nil { order.append(item.entity.meta.id) }
            latest[item.entity.meta.id] = item.entity
        }
        let snapshotID = UUID()
        let highWater = SyncFixture.cursor(sequence: 40)

        store.beginSnapshot(snapshotID: snapshotID, highWater: highWater)
        try store.applySnapshotPage(SnapshotFixture.page(
            snapshotID: snapshotID, highWater: highWater, entities: order.compactMap { latest[$0] }
        ))
        try store.finishSnapshot(snapshotID: snapshotID)

        try Self.expectTheFamily(in: store)
        #expect(try store.syncCursor() == highWater)
    }

    static func expectTheFamily(in store: BabyStore) throws {
        let babyID = Fixture.babyID
        #expect(try store.baby(babyID)?.name == "Élie")

        let feeds = try store.context.fetch(FetchDescriptor<FeedEntry>()).filter { !$0.isTombstoned }
        #expect(feeds.count == 6)
        func parts(_ id: String) throws -> [FeedComponent] {
            try #require(feeds.first { $0.id == UUID(uuidString: id) }).components
        }
        #expect(try parts("20000000-0000-4000-8000-000000000001") == [
            .bottle(id: UUID(uuidString: "31000000-0000-4000-8000-000000000001")!, volumeML: 1, contents: .formula),
        ])
        func id(_ last: String) -> UUID { UUID(uuidString: "31000000-0000-4000-8000-0000000000" + last)! }
        #expect(try parts("20000000-0000-4000-8000-000000000002") == [
            .breast(id: id("02"), side: .left, seconds: 300), .breast(id: id("03"), side: .right, seconds: 300),
        ])
        #expect(try parts("20000000-0000-4000-8000-000000000004") == [
            .breast(id: id("04"), side: .left, seconds: 300), .bottle(id: id("05"), volumeML: 60, contents: .breastMilk),
        ])
        #expect(try parts("20000000-0000-4000-8000-000000000003") == [
            .solids(id: UUID(uuidString: "31000000-0000-4000-8000-000000000006")!, foods: ["banana", "yogurt"], amount: .some),
        ])
        #expect(try parts(noFoodsID.uuidString) == [
            .solids(id: UUID(uuidString: "31000000-0000-4000-8000-0000000000f1")!, foods: [], amount: .taste),
        ])
        #expect(try parts("20000000-0000-4000-8000-000000000005") == [
            .breast(id: id("07"), side: .left, seconds: 300), .breast(id: id("08"), side: .right, seconds: 300),
            .bottle(id: id("09"), volumeML: 60, contents: .formula),
        ])

        let sleep = try #require(try store.sleepEntryForTesting(UUID(uuidString: "30000000-0000-4000-8000-000000000001")!))
        #expect(sleep.kindOverride == .nap)
        #expect(sleep.sleepSessionID == nil)
        #expect(try store.diaperEntryForTesting(UUID(uuidString: "40000000-0000-4000-8000-000000000001")!)?.contents == .mixed)
        #expect(try store.diaperEntryForTesting(diaperID)?.isTombstoned != false)
        #expect(try store.context.fetch(FetchDescriptor<SupplementEntry>()).map(\.name) == ["Vitamin D"])
        #expect(try store.context.fetch(FetchDescriptor<ImmunizationEntry>()).map(\.vaccine) == ["Prevenar 13"])
        #expect(try store.context.fetch(FetchDescriptor<PumpEntry>()).map(\.side) == [.both])
        #expect(try store.growthProfile(for: babyID)?.referenceSex == .girls)
        #expect(try store.measurements(for: babyID).first?.lengthCM == 65.5)

        let timer = try #require(store.activeTimerForTesting(timerFeedID))
        #expect(timer.currentSide == nil)
        #expect(timer.currentBottleStartedAt == (try WireFixture.date(runningBottleStart)))
        #expect(timer.completedComponents == [.breast(
            id: UUID(uuidString: "30000000-0000-4000-8000-000000000020")!, side: .left, seconds: 300,
            startedAt: try WireFixture.date("2026-09-20T15:00:00Z"), endedAt: try WireFixture.date("2026-09-20T15:05:00Z")
        )])
        #expect(store.activeTimerForTesting(timerSleepID) == nil)
        #expect(try store.pendingOutbox().isEmpty)
    }

    static func emptySharedStore() throws -> BabyStore {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        try store.activateRealmForTesting(Fixture.sharedRealm)
        return store
    }

    static func replacing(_ payload: JSONValue, _ key: String, with value: JSONValue) throws -> JSONValue {
        guard case var .object(fields) = payload else { throw ProtocolCanonicalizer.Error.invalidJSON }
        fields[key] = value
        return .object(fields)
    }

    /// A canonical entity as the server writes it; a nil payload is a
    /// tombstone.
    static func entity(_ id: UUID, kind: String, version: Int64, payload: JSONValue?) throws -> CanonicalEntity {
        let stamp = "2026-09-21T12:00:00.000000Z"
        let family = SyncFixture.familyID.uuidString.lowercased()
        let body = try payload.map { String(decoding: try JSONEncoder().encode($0), as: UTF8.self) } ?? "null"
        return try JSONDecoder().decode(CanonicalEntity.self, from: Data("""
        {"entity_kind":"\(kind)",
         "meta":{"id":"\(id.uuidString.lowercased())","family_id":"\(family)","entity_version":\(version),
                 "creator_account_id":"\(family)","last_editor_account_id":"\(family)",
                 "client_mutation_time":"\(stamp)","accepted_at":"\(stamp)",
                 "tombstoned_at":\(payload == nil ? "\"\(stamp)\"" : "null")},
         "payload":\(body)}
        """.utf8))
    }

    static func change(_ entity: CanonicalEntity, operation: String, sequence: Int64) throws -> JournalChange {
        let family = SyncFixture.familyID.uuidString.lowercased()
        let snapshot = String(decoding: try JSONEncoder().encode(entity), as: UTF8.self)
        return try JSONDecoder().decode(JournalChange.self, from: Data("""
        {"cursor":{"server_instance_id":"\(SyncFixture.instanceID.uuidString.lowercased())","recovery_epoch":1,
                   "family_id":"\(family)","sequence":\(sequence)},
         "entity_kind":"\(entity.entityKind)","entity_id":"\(entity.meta.id.uuidString.lowercased())",
         "entity_version":\(entity.meta.entityVersion),"actor_account_id":"\(family)","device_id":"\(family)",
         "operation":"\(operation)","accepted_at":"2026-09-21T12:00:00.000000Z","optional":false,
         "snapshot_or_tombstone":\(snapshot)}
        """.utf8))
    }

    struct OutcomesFixture: Decodable { let outcomes: [Outcome] }
}
