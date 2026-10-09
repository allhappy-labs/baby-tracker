import BabyCore
import BabyProtocol
import BabyStore
import Foundation
import SwiftData
import Testing
@testable import BabySync

// MARK: - A tiny in-process family server

/// The smallest server that is still honest about the parts the engine leans
/// on: it assigns increasing cursors, versions each entity, de-duplicates by
/// mutation ID, answers every mutation with a receipt, and hands a client
/// every journal change past the cursor it sent — its own included, as the
/// real server does.
///
/// It is NOT a payload validator. The server rules it copies are the ones
/// the tests depend on: a feed sent as `payload_version` 1 has its components
/// rebuilt from the v1 fields, so the ordered components are lost
/// (`upgradeComponentPayload` in `Server/internal/protocol`); and timers
/// follow `Server/internal/records/timers.go` — see `acceptTimer`.
@MainActor
final class FakeFamilyServer {
    static let instanceID = UUID(uuidString: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let familyID = UUID(uuidString: "b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!

    struct Entity {
        let kind: String
        var version: Int64
        var payload: Any
        var tombstoned: Bool
        let creator: UUID
    }

    private(set) var sequence: Int64 = 0
    private(set) var journal: [[String: Any]] = []
    private(set) var entities: [UUID: Entity] = [:]
    /// Every mutation the server accepted, once each.
    private(set) var applied: [WireMutation] = []
    /// Every request body, encoded exactly as a transport would send it.
    private(set) var requestBodies: [Data] = []

    /// Entities the server refuses, as it would a payload it cannot accept.
    var rejectedEntityIDs: Set<UUID> = []
    /// The status each mutation first got, replayed as `original_status` when
    /// the same mutation ID comes back.
    private var receipts: [UUID: String] = [:]
    /// The record each stopped timer produced (`active_timers.completed_record_id`).
    private var completedRecordIDs: [UUID: UUID] = [:]

    func sync(_ request: SyncRequest, accountID: UUID, deviceID: UUID) throws -> SyncResponse {
        requestBodies.append(try JSONEncoder().encode(request))
        var outcomes: [[String: Any]] = []
        for mutation in request.mutations {
            if rejectedEntityIDs.contains(mutation.entityID) {
                var rejected = Self.outcome(mutation.mutationID, status: "rejected", original: nil)
                rejected["code"] = "invalid_request"
                outcomes.append(rejected)
                continue
            }
            // The server refuses a component feed without components
            // (`validateFeed`, `Server/internal/records/validation.go`).
            if mutation.entityKind == .feed, mutation.payloadVersion >= 2, mutation.operation != .delete,
               case let .object(fields) = mutation.payload, fields["components"] == .array([]) {
                var rejected = Self.outcome(mutation.mutationID, status: "rejected", original: nil)
                rejected["code"] = "invalid_collection"
                outcomes.append(rejected)
                continue
            }
            if let original = receipts[mutation.mutationID] {
                outcomes.append(Self.outcome(mutation.mutationID, status: "already_applied", original: original))
                continue
            }
            if mutation.entityKind == .timer {
                let outcome = try acceptTimer(mutation, accountID: accountID, deviceID: deviceID)
                receipts[mutation.mutationID] = outcome["status"] as? String
                if receipts[mutation.mutationID] == "applied" { applied.append(mutation) }
                outcomes.append(outcome)
                continue
            }
            receipts[mutation.mutationID] = "applied"
            applied.append(mutation)
            try accept(mutation, accountID: accountID, deviceID: deviceID)
            outcomes.append(Self.outcome(mutation.mutationID, status: "applied", original: nil))
        }
        let since = request.cursor?.sequence ?? 0
        let body: [String: Any] = [
            "server_instance_id": Self.instanceID.uuidString.lowercased(),
            "recovery_epoch": 1,
            "outcomes": outcomes,
            "changes": journal.filter { ($0["cursor"] as! [String: Any])["sequence"] as! Int64 > since },
            "high_water": Self.cursor(sequence),
            "next_page_token": NSNull(),
        ]
        return try JSONDecoder().decode(SyncResponse.self, from: JSONSerialization.data(withJSONObject: body))
    }

    func snapshot() throws -> SnapshotResponse {
        let body: [String: Any] = [
            "server_instance_id": Self.instanceID.uuidString.lowercased(),
            "recovery_epoch": 1,
            "snapshot_id": UUID().uuidString.lowercased(),
            "high_water": Self.cursor(sequence),
            "entities": entities.map { id, entity in Self.canonical(id, entity) },
            "next_page_token": NSNull(),
        ]
        return try JSONDecoder().decode(SnapshotResponse.self, from: JSONSerialization.data(withJSONObject: body))
    }

    /// Every entity ID the server has ever been sent.
    var receivedEntityIDs: Set<UUID> { Set(applied.map(\.entityID)) }

    /// What the real resolver does once an import's mapping is complete: it
    /// creates each mapped baby itself, as an ordinary journaled create
    /// (`Server/internal/importer/conflict.go`). The client never sends these
    /// babies, so without this the joiner would never see them.
    func publishImportedBabies(_ babies: [ImportBaby], mappings: [UUID: UUID], accountID: UUID, deviceID: UUID) throws {
        for baby in babies {
            let target = mappings[baby.localBabyID] ?? baby.localBabyID
            guard entities[target] == nil else { continue }
            try accept(WireMutation(
                mutationID: UUID(), entityID: target, entityKind: .baby, operation: .create,
                payloadVersion: 1, observedEntityVersion: 0, clientMutationTime: "2026-09-25T12:00:00Z",
                importID: nil,
                payload: .object([
                    "name": .string(baby.name),
                    "birth_date": .string(baby.birthDate),
                    "sort_order": .number(Decimal(baby.sortOrder)),
                ])
            ), accountID: accountID, deviceID: deviceID)
        }
    }

    private func accept(_ mutation: WireMutation, accountID: UUID, deviceID: UUID) throws {
        var payload = try JSONSerialization.jsonObject(with: JSONEncoder().encode(mutation.payload))
        if mutation.entityKind == .feed, mutation.payloadVersion == 1, var fields = payload as? [String: Any] {
            fields.removeValue(forKey: "components")
            payload = fields
        }
        var entity = entities[mutation.entityID] ?? Entity(
            kind: mutation.entityKind.rawValue, version: 0, payload: payload, tombstoned: false, creator: accountID
        )
        entity.version += 1
        if mutation.operation == .delete {
            entity.tombstoned = true
        } else {
            entity.payload = payload
        }
        store(mutation.entityID, entity, operation: mutation.operation, accountID: accountID, deviceID: deviceID)
    }

    /// Saves an entity and journals it, as every accepted change is.
    private func store(_ id: UUID, _ entity: Entity, operation: WireOperation, accountID: UUID, deviceID: UUID) {
        entities[id] = entity
        sequence += 1
        journal.append([
            "cursor": Self.cursor(sequence),
            "entity_kind": entity.kind,
            "entity_id": id.uuidString.lowercased(),
            "entity_version": entity.version,
            "actor_account_id": accountID.uuidString.lowercased(),
            "device_id": deviceID.uuidString.lowercased(),
            "operation": operation.rawValue,
            "accepted_at": "2026-09-25T12:00:00Z",
            "optional": false,
            "snapshot_or_tombstone": Self.canonical(id, entity),
        ])
    }

    /// The timer rules of `Server/internal/records/timers.go`, as far as the
    /// tests lean on them:
    ///
    /// - one live timer per baby and kind: a second start is `superseded`
    ///   with the live timer as its canonical entity (`startTimer`);
    /// - a stop tombstones the timer and creates the finished sleep or feed
    ///   under the client's `completed_record_id`, journalling both
    ///   (`stopTimer`, `completedTimerPayload`);
    /// - the finished record takes the timer's `session_id` as its
    ///   `feeding_session_id` or `sleep_session_id` (`completedTimerPayload`);
    /// - an edit (`update`) patches the start and the session link
    ///   (`editTimer`); component patches are not modelled;
    /// - any command against an ended timer is `superseded` with code
    ///   `timer_ended`, carrying the record its stop produced in
    ///   `related_entities` (`endedTimerOutcome`).
    private func acceptTimer(_ mutation: WireMutation, accountID: UUID, deviceID: UUID) throws -> [String: Any] {
        let id = mutation.entityID
        let fields = try #require(try JSONSerialization.jsonObject(with: JSONEncoder().encode(mutation.payload)) as? [String: Any])
        func refused(_ status: String, _ code: String?, _ canonical: [String: Any]? = nil,
                     related: [[String: Any]] = []) -> [String: Any] {
            var outcome = Self.outcome(mutation.mutationID, status: status, original: nil)
            outcome["code"] = code ?? NSNull()
            outcome["canonical_entity"] = canonical ?? NSNull()
            outcome["related_entities"] = related
            return outcome
        }
        let applied = Self.outcome(mutation.mutationID, status: "applied", original: nil)

        if mutation.operation == .timerStart {
            if let existing = entities[id] {
                return existing.tombstoned ? refused("rejected", "entity_exists") : refused("superseded", nil, Self.canonical(id, existing))
            }
            if let (liveID, live) = entities.first(where: { _, entity in
                guard entity.kind == "timer", !entity.tombstoned, let payload = entity.payload as? [String: Any] else { return false }
                return payload["baby_id"] as? String == fields["baby_id"] as? String
                    && payload["timer_kind"] as? String == fields["timer_kind"] as? String
            }) {
                return refused("superseded", nil, Self.canonical(liveID, live))
            }
            store(id, Entity(kind: "timer", version: 1, payload: fields, tombstoned: false, creator: accountID),
                  operation: mutation.operation, accountID: accountID, deviceID: deviceID)
            return applied
        }

        guard var timer = entities[id], let payload = timer.payload as? [String: Any] else {
            return refused("rejected", "entity_not_found")
        }
        guard !timer.tombstoned else {
            let completion = completedRecordIDs[id].flatMap { recordID in
                entities[recordID].map { Self.canonical(recordID, $0) }
            }
            return refused("superseded", "timer_ended", Self.canonical(id, timer), related: completion.map { [$0] } ?? [])
        }
        var components = payload["completed_components"] as? [[String: Any]] ?? []
        /// Closes the running side at `instant`, as the server does.
        func closeRunningSide(at instant: String, componentID: Any?) throws {
            guard let side = payload["current_side"] as? String,
                  let started = payload["current_side_started_at"] as? String else { return }
            let seconds = Int(try Self.date(instant).timeIntervalSince(try Self.date(started)))
            guard seconds > 0, let componentID = componentID as? String else { return }
            components.append(["id": componentID, "kind": "breast", "side": side, "seconds": seconds])
        }

        switch mutation.operation {
        case .timerStop:
            let stoppedAt = try #require(fields["stopped_at"] as? String)
            let recordID = try #require((fields["completed_record_id"] as? String).flatMap(UUID.init(uuidString:)))
            var record: [String: Any] = [
                "baby_id": payload["baby_id"] ?? NSNull(), "started_at": payload["started_at"] ?? NSNull(),
                "ended_at": stoppedAt, "time_zone_id": payload["time_zone_id"] ?? NSNull(), "note": NSNull(),
            ]
            let kind: String
            if payload["timer_kind"] as? String == "sleep" {
                kind = "sleep"
                record["kind_override"] = NSNull()
                record["sleep_session_id"] = payload["session_id"] ?? NSNull()
            } else {
                kind = "feed"
                try closeRunningSide(at: stoppedAt, componentID: fields["component_id"])
                record["kind"] = "breast"
                record["components"] = components
                record["feeding_session_id"] = payload["session_id"] ?? NSNull()
            }
            timer.tombstoned = true
            timer.version += 1
            completedRecordIDs[id] = recordID
            store(id, timer, operation: mutation.operation, accountID: accountID, deviceID: deviceID)
            store(recordID, Entity(kind: kind, version: 1, payload: record, tombstoned: false, creator: accountID),
                  operation: mutation.operation, accountID: accountID, deviceID: deviceID)
        case .update:
            var edited = payload
            if let started = fields["started_at"] as? String { edited["started_at"] = started }
            if let session = fields["session_id"] { edited["session_id"] = session }
            timer.payload = edited
            timer.version += 1
            store(id, timer, operation: mutation.operation, accountID: accountID, deviceID: deviceID)
        default:
            return refused("rejected", "invalid_mutation")
        }
        return applied
    }

    private static func date(_ value: String) throws -> Date {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return try #require(formatter.date(from: value))
    }

    /// Live timers the server holds.
    var liveTimerIDs: [UUID] {
        entities.filter { $0.value.kind == "timer" && !$0.value.tombstoned }.map(\.key)
    }

    private static func canonical(_ id: UUID, _ entity: Entity) -> [String: Any] {
        [
            "entity_kind": entity.kind,
            "meta": [
                "id": id.uuidString.lowercased(),
                "family_id": familyID.uuidString.lowercased(),
                "entity_version": entity.version,
                "creator_account_id": entity.creator.uuidString.lowercased(),
                "last_editor_account_id": entity.creator.uuidString.lowercased(),
                "client_mutation_time": "2026-09-25T12:00:00Z",
                "accepted_at": "2026-09-25T12:00:00Z",
                "tombstoned_at": entity.tombstoned ? "2026-09-25T12:00:00Z" : NSNull(),
            ] as [String: Any],
            // A tombstone carries no payload (`snapshot.go`, `Canonical()`).
            "payload": entity.tombstoned ? NSNull() : entity.payload,
        ]
    }

    private static func cursor(_ sequence: Int64) -> [String: Any] {
        [
            "server_instance_id": instanceID.uuidString.lowercased(),
            "recovery_epoch": 1,
            "family_id": familyID.uuidString.lowercased(),
            "sequence": sequence,
        ]
    }

    private static func outcome(_ id: UUID, status: String, original: String?) -> [String: Any] {
        [
            "mutation_id": id.uuidString.lowercased(), "status": status,
            "original_status": original ?? NSNull(), "canonical_entity": NSNull(),
            "related_entities": [], "cursor": NSNull(), "code": NSNull(), "overwrote_newer_version": false,
        ]
    }
}

/// One phone's connection to the fake server.
@MainActor
final class FakeServerTransport: FamilySyncTransport, @unchecked Sendable {
    let server: FakeFamilyServer
    let accountID: UUID
    let deviceID: UUID
    var offline = false
    var failNextWith: SyncErrorCode?
    /// Fails the sync call with this 1-based number, after any hold.
    var failCall: (number: Int, code: SyncErrorCode)?
    var failNextSnapshotWith: SyncErrorCode?
    /// Holds the next sync open until `release()`, to observe overlap.
    var holdNext = false
    var holdNextSnapshot = false
    private var held: CheckedContinuation<Void, Never>?
    private(set) var syncCalls = 0
    private(set) var snapshotCalls = 0
    private(set) var inFlight = 0
    private(set) var maxInFlight = 0

    init(server: FakeFamilyServer, accountID: UUID, deviceID: UUID) {
        self.server = server
        self.accountID = accountID
        self.deviceID = deviceID
    }

    func sync(_ request: SyncRequest) async throws -> SyncResponse {
        syncCalls += 1
        inFlight += 1
        maxInFlight = max(maxInFlight, inFlight)
        defer { inFlight -= 1 }
        if holdNext {
            holdNext = false
            await withCheckedContinuation { held = $0 }
        }
        if offline { throw URLError(.notConnectedToInternet) }
        if let code = failNextWith {
            failNextWith = nil
            throw code
        }
        if let failCall, failCall.number == syncCalls {
            self.failCall = nil
            throw failCall.code
        }
        return try server.sync(request, accountID: accountID, deviceID: deviceID)
    }

    func snapshot(pageToken: String?) async throws -> SnapshotResponse {
        snapshotCalls += 1
        if holdNextSnapshot {
            holdNextSnapshot = false
            await withCheckedContinuation { held = $0 }
        }
        if offline { throw URLError(.notConnectedToInternet) }
        if let code = failNextSnapshotWith {
            failNextSnapshotWith = nil
            throw code
        }
        return try server.snapshot()
    }

    var isHolding: Bool { held != nil }

    func release() {
        held?.resume()
        held = nil
    }
}

// MARK: - Fixtures

@MainActor
enum EngineFixture {
    static let origin = "https://sync.example.test"
    static let ownerAccount = UUID(uuidString: "d1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let ownerDevice = UUID(uuidString: "f1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let joinerAccount = UUID(uuidString: "e1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let joinerDevice = UUID(uuidString: "01b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!

    static let left = UUID(uuidString: "30000000-0000-4000-8000-000000000001")!
    static let right = UUID(uuidString: "30000000-0000-4000-8000-000000000002")!
    static let milk = UUID(uuidString: "30000000-0000-4000-8000-000000000003")!
    static let formula = UUID(uuidString: "30000000-0000-4000-8000-000000000004")!

    static var composite: [FeedComponent] {
        [
            .breast(id: left, side: .left, seconds: 480),
            .breast(id: right, side: .right, seconds: 360),
            .bottle(id: milk, volumeML: 50, contents: .breastMilk),
            .bottle(id: formula, volumeML: 30, contents: .formula),
        ]
    }

    static func familyKey(account: UUID, device: UUID) -> FamilySessionKey {
        FamilySessionKey(
            canonicalOrigin: origin, serverInstanceID: FakeFamilyServer.instanceID,
            familyID: FakeFamilyServer.familyID, accountID: account, deviceID: device, recoveryEpoch: 0
        )
    }

    /// A store whose account is allowed to sync — what a completed sign-in
    /// leaves behind.
    static func store(for key: FamilySessionKey, clock: TestDateProvider = TestDateProvider(.now)) throws -> BabyStore {
        let store = try BabyStore.inMemory(dateProvider: clock)
        try store.markAccountAuthenticated(key.leaseKey, credentialVersion: UUID())
        return store
    }

    static func resolver() -> FakeResolverClient {
        FakeResolverClient(response: ResolverFixture.response(importID: UUID()))
    }

    static func feeds(_ store: BabyStore) throws -> [FeedEntry] {
        try store.context.fetch(FetchDescriptor<FeedEntry>())
    }

    static func diapers(_ store: BabyStore) throws -> [DiaperEntry] {
        try store.context.fetch(FetchDescriptor<DiaperEntry>())
    }
}

/// The fake resolver joined to the fake server, so a completed mapping
/// publishes the imported babies the way the real resolver does.
@MainActor
final class PublishingResolverClient: ImportResolverClient, @unchecked Sendable {
    let inner: FakeResolverClient
    let server: FakeFamilyServer
    let accountID: UUID
    let deviceID: UUID

    init(inner: FakeResolverClient, server: FakeFamilyServer, accountID: UUID, deviceID: UUID) {
        self.inner = inner
        self.server = server
        self.accountID = accountID
        self.deviceID = deviceID
    }

    func resolve(_ request: ImportResolveRequest) async throws -> ImportResolveResponse {
        let response = try await inner.resolve(request)
        if response.mappingComplete {
            let mappings = Dictionary(uniqueKeysWithValues: response.mappings.map { ($0.localBabyID, $0.sharedBabyID) })
            try server.publishImportedBabies(request.babies, mappings: mappings, accountID: accountID, deviceID: deviceID)
        }
        return response
    }

    func resolveConflict(
        conflictID: UUID, resolverRevision: Int64,
        resolution: ImportConflictResolution, commandID: UUID
    ) async throws -> ImportResolveResponse {
        try await inner.resolveConflict(
            conflictID: conflictID, resolverRevision: resolverRevision, resolution: resolution, commandID: commandID
        )
    }

    func conflictPage(importID: UUID, pageToken: String?) async throws -> ImportResolveResponse {
        try await inner.conflictPage(importID: importID, pageToken: pageToken)
    }
}

/// The server's rule for import IDs (`importer/resolver.go`): every chunk it
/// accepted is kept, an identical resend gets the stored answer, and the same
/// ID and chunk with different contents is refused `import_chunk_reused` —
/// a code this client does not know, so its transport reads it as
/// `service_unavailable`.
@MainActor
final class ImportIDLedger: ImportResolverClient, @unchecked Sendable {
    let inner: any ImportResolverClient
    private var chunks: [String: ImportResolveRequest] = [:]

    init(inner: any ImportResolverClient) { self.inner = inner }

    func resolve(_ request: ImportResolveRequest) async throws -> ImportResolveResponse {
        let key = "\(request.importID)|\(request.chunkIndex)"
        if let kept = chunks[key], kept != request { throw SyncErrorCode.serviceUnavailable }
        chunks[key] = request
        return try await inner.resolve(request)
    }

    func resolveConflict(
        conflictID: UUID, resolverRevision: Int64,
        resolution: ImportConflictResolution, commandID: UUID
    ) async throws -> ImportResolveResponse {
        try await inner.resolveConflict(
            conflictID: conflictID, resolverRevision: resolverRevision, resolution: resolution, commandID: commandID
        )
    }

    func conflictPage(importID: UUID, pageToken: String?) async throws -> ImportResolveResponse {
        try await inner.conflictPage(importID: importID, pageToken: pageToken)
    }
}

/// Owner and joiner sharing one fake server.
@MainActor
struct Family {
    let server = FakeFamilyServer()
    let ownerKey = EngineFixture.familyKey(account: EngineFixture.ownerAccount, device: EngineFixture.ownerDevice)
    let joinerKey = EngineFixture.familyKey(account: EngineFixture.joinerAccount, device: EngineFixture.joinerDevice)
    let owner: BabyStore
    let joiner: BabyStore
    let ownerTransport: FakeServerTransport
    let joinerTransport: FakeServerTransport
    let resolver = EngineFixture.resolver()

    /// Both phones read the one clock, so a test can let time pass on both.
    init(clock: TestDateProvider = TestDateProvider(.now)) throws {
        owner = try EngineFixture.store(for: ownerKey, clock: clock)
        joiner = try EngineFixture.store(for: joinerKey, clock: clock)
        ownerTransport = FakeServerTransport(server: server, accountID: EngineFixture.ownerAccount, deviceID: EngineFixture.ownerDevice)
        joinerTransport = FakeServerTransport(server: server, accountID: EngineFixture.joinerAccount, deviceID: EngineFixture.joinerDevice)
    }

    func ownerEngine(renew: @escaping () async throws -> Void = {}) -> FamilySyncEngine {
        let importClient = PublishingResolverClient(
            inner: resolver, server: server,
            accountID: EngineFixture.ownerAccount, deviceID: EngineFixture.ownerDevice
        )
        return FamilySyncEngine(store: owner, familyKey: ownerKey, transport: ownerTransport, importClient: importClient, renew: renew)
    }

    func joinerEngine(
        renew: @escaping () async throws -> Void = {},
        debounce: Duration = .seconds(1),
        pollInterval: Duration = .seconds(20),
        livePollInterval: Duration = .seconds(300),
        announcer: any RemoteTimerAnnouncer = RecordingTimerAnnouncer(),
        live: LiveHintChannel? = nil,
        onRevoked: @escaping (RealmID) async -> Void = { _ in }
    ) -> FamilySyncEngine {
        FamilySyncEngine(
            store: joiner, familyKey: joinerKey, transport: joinerTransport, importClient: EngineFixture.resolver(),
            renew: renew, debounce: debounce, pollInterval: pollInterval, livePollInterval: livePollInterval,
            announcer: announcer, live: live, onRevoked: onRevoked
        )
    }

    /// The owner with the full schedule injectable, for the live-hint tests.
    func ownerEngine(live: LiveHintChannel?) -> FamilySyncEngine {
        let importClient = PublishingResolverClient(
            inner: resolver, server: server,
            accountID: EngineFixture.ownerAccount, deviceID: EngineFixture.ownerDevice
        )
        return FamilySyncEngine(
            store: owner, familyKey: ownerKey, transport: ownerTransport, importClient: importClient,
            renew: {}, debounce: .seconds(1), pollInterval: .seconds(20), livePollInterval: .seconds(300),
            announcer: RecordingTimerAnnouncer(), live: live
        )
    }

    /// A hint as the server would send it for this family.
    func hint(_ sequence: Int64) -> Cursor {
        Cursor(
            serverInstanceID: FakeFamilyServer.instanceID, recoveryEpoch: 1,
            familyID: FakeFamilyServer.familyID, sequence: sequence
        )
    }

    /// The owner's local history before the family exists: a baby and a
    /// composite feed.
    @discardableResult
    func seedOwner() throws -> (Baby, FeedEntry) {
        let baby = try owner.addBaby(name: "Mira", birthDate: Date(timeIntervalSince1970: 1_780_000_000), referenceSex: .girls, expectedDueDate: nil)
        let feed = try owner.logBottle(volumeML: 1, contents: .formula, for: baby.id)
        feed.components = EngineFixture.composite
        try owner.update(feed)
        return (baby, feed)
    }
}

// MARK: - Tests

@MainActor
@Suite("Family sync engine")
struct FamilySyncEngineTests {
    /// The whole point: the owner's pre-family history reaches the other
    /// phone, and a composite feed arrives with every component, in order,
    /// under the same IDs.
    @Test func ownerHistoryReachesJoinerWithIdenticalComponents() async throws {
        let family = try Family()
        let (baby, feed) = try family.seedOwner()

        let owner = family.ownerEngine()
        try await owner.activateAsOwner()
        #expect(owner.status.isUpToDate)
        #expect(try family.owner.nextDispatchBatch(limit: 10).isEmpty)

        let joiner = family.joinerEngine()
        try await joiner.activateAsJoiner()
        #expect(joiner.status.isUpToDate)

        #expect(family.joiner.activeRealm == family.joinerKey.realmID)
        let received = try #require(try EngineFixture.feeds(family.joiner).first { $0.id == feed.id })
        #expect(received.components == EngineFixture.composite)
        #expect(received.babyID == baby.id)
        #expect(received.realmKey == family.joinerKey.realmID.encodedKey)
        #expect(try family.joiner.babies().map(\.id) == [baby.id])
    }

    /// A membership revoked during ordinary sync — not through the settings
    /// screen at all — still runs `onRevoked` with the realm the server just
    /// purged. This is the seam `FamilySyncComposition` uses to clear this
    /// device's saved invitation codes on a sign-out that never touches
    /// `FamilySettingsViewModel`; if the engine stopped forwarding the
    /// coordinator's callback, that cleanup would silently stop happening.
    @Test func revokedMembershipRunsOnRevokedWithThePurgedRealm() async throws {
        let family = try Family()
        var revokedRealms: [RealmID] = []
        let joiner = family.joinerEngine(onRevoked: { realm in revokedRealms.append(realm) })
        family.joinerTransport.failNextWith = .membershipRevoked

        try await joiner.activateAsJoiner()

        #expect(revokedRealms == [family.joinerKey.realmID])
    }

    /// A joiner starts fresh. What it tracked before joining stays exactly
    /// where it was — in the local realm — and is never sent.
    @Test func joinerLocalHistoryStaysLocalAndIsNeverSent() async throws {
        let family = try Family()
        try family.seedOwner()
        let localBaby = try family.joiner.addBaby(name: "Solo", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let localDiaper = try family.joiner.logDiaper(.wet, for: localBaby.id)

        try await family.ownerEngine().activateAsOwner()
        let joiner = family.joinerEngine()
        try await joiner.activateAsJoiner()
        // A second pass, as a relaunch would make, still sends nothing local.
        await joiner.syncNow(.foreground)

        let diaper = try #require(try EngineFixture.diapers(family.joiner).first { $0.id == localDiaper.id })
        #expect(diaper.realmKey == RealmID.local.encodedKey)
        #expect(!diaper.isTombstoned)
        #expect(try family.joiner.babies(in: .local).map(\.id) == [localBaby.id])
        #expect(!family.server.receivedEntityIDs.contains(localDiaper.id))
        #expect(!family.server.receivedEntityIDs.contains(localBaby.id))
    }

    /// And what the joiner logs after joining does go to the family.
    @Test func joinerWritesAfterJoiningAreSent() async throws {
        let family = try Family()
        let (baby, _) = try family.seedOwner()
        try await family.ownerEngine().activateAsOwner()
        let joiner = family.joinerEngine()
        try await joiner.activateAsJoiner()

        let diaper = try family.joiner.logDiaper(.dirty, for: baby.id)
        await joiner.syncNow(.localWrite)

        #expect(family.server.receivedEntityIDs.contains(diaper.id))
    }

    /// An entry logged offline stays owed, goes on the next successful sync,
    /// and is applied by the server exactly once.
    @Test func offlineWriteIsDeliveredExactlyOnceOnTheNextSync() async throws {
        let family = try Family()
        let (baby, _) = try family.seedOwner()
        let owner = family.ownerEngine()
        try await owner.activateAsOwner()

        family.ownerTransport.offline = true
        let diaper = try family.owner.logDiaper(.wet, for: baby.id)
        await owner.syncNow(.localWrite)
        #expect(owner.status == .offline)
        #expect(!family.server.receivedEntityIDs.contains(diaper.id))

        family.ownerTransport.offline = false
        await owner.syncNow(.foreground)
        await owner.syncNow(.foreground)

        #expect(owner.status.isUpToDate)
        #expect(family.server.applied.filter { $0.entityID == diaper.id }.count == 1)
        #expect(try family.owner.nextDispatchBatch(limit: 10).isEmpty)
    }

    /// An expired access token: renew once, then the same sync succeeds
    /// without the parent seeing an error.
    @Test func expiredAccessRenewsOnceThenSucceeds() async throws {
        let family = try Family()
        let (baby, _) = try family.seedOwner()
        var renewals = 0
        let owner = family.ownerEngine { renewals += 1 }
        try await owner.activateAsOwner()

        let diaper = try family.owner.logDiaper(.wet, for: baby.id)
        family.ownerTransport.failNextWith = .accessExpired
        await owner.syncNow(.localWrite)

        #expect(renewals == 1)
        #expect(owner.status.isUpToDate)
        #expect(family.server.applied.filter { $0.entityID == diaper.id }.count == 1)
    }

    /// The owner's first activation right after creating the family. The
    /// session it holds was minted before the family existed, and the real
    /// server answers the import route with `access_expired` until it is
    /// renewed (`httpapi/import.go`). The engine renewed only on the sync
    /// route, which activation never reached — so on a real phone the owner's
    /// history never left it. Found by the two-caregiver live-server gate.
    @Test func ownerImportRenewsAPreFamilySessionThenImports() async throws {
        let family = try Family()
        let (_, feed) = try family.seedOwner()
        var renewals = 0
        let owner = family.ownerEngine { renewals += 1 }
        family.resolver.failNextResolveWith = SyncErrorCode.accessExpired

        try await owner.activateAsOwner()

        #expect(renewals == 1)
        #expect(owner.status.isUpToDate)
        #expect(family.server.receivedEntityIDs.contains(feed.id))
    }

    /// The joiner's equivalent: the snapshot route refuses a session minted
    /// before the invitation was redeemed.
    @Test func joinerSnapshotRenewsAPreFamilySessionThenSnapshots() async throws {
        let family = try Family()
        let (baby, _) = try family.seedOwner()
        try await family.ownerEngine().activateAsOwner()
        var renewals = 0
        let joiner = family.joinerEngine(renew: { renewals += 1 })
        family.joinerTransport.failNextSnapshotWith = .accessExpired

        try await joiner.activateAsJoiner()

        #expect(renewals == 1)
        #expect(joiner.status.isUpToDate)
        #expect(try family.joiner.babies().map(\.id) == [baby.id])
    }

    /// One renewal, not a loop: a session the server still refuses straight
    /// after renewing is not something another renewal will fix.
    @Test func activationRenewsAtMostOnce() async throws {
        let family = try Family()
        try family.seedOwner()
        try await family.ownerEngine().activateAsOwner()
        var renewals = 0
        let joiner = family.joinerEngine(renew: {
            renewals += 1
            family.joinerTransport.failNextSnapshotWith = .accessExpired
        })
        family.joinerTransport.failNextSnapshotWith = .accessExpired

        await #expect(throws: SyncErrorCode.accessExpired) { try await joiner.activateAsJoiner() }
        #expect(renewals == 1)
    }

    /// An activation whose renewal the server refuses for good — the
    /// revoked session — offers the sign-in; one that could not renew
    /// yet because another process holds the lease is only a retry.
    @Test func activationRenewalFailureAsksForSignInOnlyWhenOnlyProofHelps() async throws {
        let family = try Family()
        try family.seedOwner()
        try await family.ownerEngine().activateAsOwner()

        let refused = family.joinerEngine(renew: { throw HTTPClientError.status(401, "reauthentication_required") })
        family.joinerTransport.failNextSnapshotWith = .accessExpired
        _ = try? await refused.activateAsJoiner()
        #expect(refused.status == .needsSignIn)

        let busy = family.joinerEngine(renew: { throw SessionRefresher.RefreshError.lockUnavailable })
        family.joinerTransport.failNextSnapshotWith = .accessExpired
        _ = try? await busy.activateAsJoiner()
        #expect(busy.status == .failed(SyncIssueCopy.retrying))
    }

    @Test func freshProofRequiredAsksForSignIn() async throws {
        let family = try Family()
        try family.seedOwner()
        let owner = family.ownerEngine()
        try await owner.activateAsOwner()

        family.ownerTransport.failNextWith = .reauthenticationRequired
        await owner.syncNow(.foreground)

        #expect(owner.status == .needsSignIn)
    }

    @Test func freshProofRequiredStaysVisibleUntilSignIn() async throws {
        let family = try Family()
        let (baby, _) = try family.seedOwner()
        try await family.ownerEngine().activateAsOwner()
        let joiner = family.joinerEngine(debounce: .milliseconds(20))
        try await joiner.activateAsJoiner()
        _ = try family.joiner.logDiaper(.wet, for: baby.id)

        family.joinerTransport.failNextWith = .reauthenticationRequired
        await joiner.syncNow(.foreground)
        let refusedCalls = family.joinerTransport.syncCalls
        #expect(joiner.status == .needsSignIn)

        await joiner.syncNow(.foreground)
        joiner.localWriteHappened()
        await joiner.scheduledPush?.value

        #expect(joiner.status == .needsSignIn)
        #expect(family.joinerTransport.syncCalls == refusedCalls)
    }

    @Test func surfacedIssueIsReportedAsFailure() async throws {
        let family = try Family()
        try family.seedOwner()
        let owner = family.ownerEngine()
        try await owner.activateAsOwner()

        family.ownerTransport.failNextWith = .readOnlyCaregiver
        await owner.syncNow(.foreground)

        #expect(owner.status == .failed(SyncIssueCopy.readOnly))
    }

    /// Activation after a relaunch — or a second tap — must neither re-import
    /// nor duplicate anything.
    @Test func ownerReactivationIsIdempotent() async throws {
        let family = try Family()
        let (_, feed) = try family.seedOwner()
        try await family.ownerEngine().activateAsOwner()
        let resolves = family.resolver.requests.count
        let applied = family.server.applied.count

        // A new engine over the same store is what a relaunch builds.
        try await family.ownerEngine().activateAsOwner()

        #expect(family.resolver.requests.count == resolves)
        #expect(family.server.applied.count == applied)
        #expect(try EngineFixture.feeds(family.owner).filter { $0.id == feed.id }.count == 1)
        #expect(family.owner.activeRealm == family.ownerKey.realmID)
    }

    /// A crash — here, the resolver failing — part-way through the import.
    /// Activating again resumes the same import and sends each record once.
    @Test func ownerActivationResumesAfterAFailedImport() async throws {
        let family = try Family()
        let (_, feed) = try family.seedOwner()
        family.resolver.failNextResolveWith = URLError(.networkConnectionLost)

        let interrupted = family.ownerEngine()
        await #expect(throws: URLError.self) { try await interrupted.activateAsOwner() }
        #expect(interrupted.status == .offline)
        #expect(family.server.applied.isEmpty)

        let owner = family.ownerEngine()
        try await owner.activateAsOwner()

        #expect(owner.status.isUpToDate)
        #expect(family.server.applied.filter { $0.entityID == feed.id }.count == 1)
        // One import, one ID, resumed rather than restarted.
        #expect(Set(family.resolver.requests.map(\.importID)).count == 1)
    }

    @Test func joinerReactivationIsIdempotent() async throws {
        let family = try Family()
        let (_, feed) = try family.seedOwner()
        try await family.ownerEngine().activateAsOwner()
        try await family.joinerEngine().activateAsJoiner()

        try await family.joinerEngine().activateAsJoiner()

        #expect(try EngineFixture.feeds(family.joiner).filter { $0.id == feed.id }.count == 1)
        #expect(try family.joiner.babies().count == 1)
        #expect(family.joinerTransport.snapshotCalls == 1)
    }

    /// The import ID is derived from the family, the device and the store, so
    /// a relaunch finds the same checkpoint with nothing else persisted, and
    /// a fresh store names a new import.
    @Test func importCheckpointIsStablePerFamilyDeviceAndStore() throws {
        let owner = EngineFixture.familyKey(account: EngineFixture.ownerAccount, device: EngineFixture.ownerDevice)
        let again = EngineFixture.familyKey(account: EngineFixture.ownerAccount, device: EngineFixture.ownerDevice)
        let other = EngineFixture.familyKey(account: EngineFixture.joinerAccount, device: EngineFixture.joinerDevice)
        let store = try EngineFixture.store(for: owner)
        let storeID = try store.importStoreID()
        #expect(try store.importStoreID() == storeID)
        let freshStoreID = try EngineFixture.store(for: owner).importStoreID()
        #expect(freshStoreID != storeID)

        let id = FamilySyncEngine.importCheckpointID(for: owner, storeID: storeID)
        #expect(id == FamilySyncEngine.importCheckpointID(for: again, storeID: storeID))
        #expect(id != FamilySyncEngine.importCheckpointID(for: other, storeID: storeID))
        #expect(id != FamilySyncEngine.importCheckpointID(for: owner, storeID: freshStoreID))
    }

    /// Task 11, found by the owner-reinstall e2e. The same owner on the same
    /// device, but a fresh store: the old one was moved aside (an install
    /// over an older build) after its import finished, and the user signed in
    /// again. The server keeps every import ID and refuses a different chunk
    /// 0 under one it already holds, so an empty store that re-derived the
    /// device's import ID was refused on every activation and sat at
    /// "Syncing…" forever, never pulling the family. The new store imports
    /// under its own ID, pulls everything, and keeps that ID across relaunches.
    @Test func aFreshOwnerStoreDoesNotReuseTheImportAnEarlierStoreFinished() async throws {
        let family = try Family()
        let (baby, feed) = try family.seedOwner()
        let resolver = ImportIDLedger(inner: PublishingResolverClient(
            inner: family.resolver, server: family.server,
            accountID: EngineFixture.ownerAccount, deviceID: EngineFixture.ownerDevice
        ))
        let original = FamilySyncEngine(store: family.owner, familyKey: family.ownerKey, transport: family.ownerTransport,
                                        importClient: resolver, renew: {})
        try await original.activateAsOwner()
        #expect(original.status.isUpToDate)
        let finished = try #require(family.resolver.requests.last?.importID)

        let replacement = try EngineFixture.store(for: family.ownerKey)
        let transport = FakeServerTransport(server: family.server, accountID: EngineFixture.ownerAccount,
                                            deviceID: EngineFixture.ownerDevice)
        let engine = FamilySyncEngine(store: replacement, familyKey: family.ownerKey, transport: transport,
                                      importClient: resolver, renew: {})
        try await engine.activateAsOwner()

        #expect(engine.status.isUpToDate, "the fresh store is \(engine.status)")
        let fresh = try #require(family.resolver.requests.last?.importID)
        #expect(fresh != finished, "the fresh store reused the finished import \(finished)")
        #expect(replacement.activeRealm == family.ownerKey.realmID)
        #expect(try replacement.babies().map(\.id) == [baby.id])
        #expect(try EngineFixture.feeds(replacement).filter { $0.id == feed.id && !$0.isTombstoned }.count == 1)

        let requestsBefore = family.resolver.requests.count
        let relaunched = FamilySyncEngine(store: replacement, familyKey: family.ownerKey, transport: transport,
                                          importClient: resolver, renew: {})
        try await relaunched.activateAsOwner()
        #expect(relaunched.status.isUpToDate)
        #expect(family.resolver.requests.count == requestsBefore, "a relaunch started the fresh store's import again")
    }

    /// A burst of writes is one push, a second after the last one.
    @Test func localWritesAreDebouncedIntoOnePush() async throws {
        let family = try Family()
        let (baby, _) = try family.seedOwner()
        try await family.ownerEngine().activateAsOwner()
        let joiner = family.joinerEngine(debounce: .milliseconds(20))
        try await joiner.activateAsJoiner()
        let before = family.joinerTransport.syncCalls

        let diaper = try family.joiner.logDiaper(.wet, for: baby.id)
        joiner.localWriteHappened()
        joiner.localWriteHappened()
        joiner.localWriteHappened()
        await joiner.scheduledPush?.value

        #expect(family.joinerTransport.syncCalls == before + 1)
        #expect(family.server.receivedEntityIDs.contains(diaper.id))
    }

    /// Nothing owed, nothing sent: a write the outbox has already delivered
    /// — or a local-only write — does not wake the network.
    @Test func localWriteWithNothingOwedDoesNotSync() async throws {
        let family = try Family()
        try family.seedOwner()
        try await family.ownerEngine().activateAsOwner()
        let joiner = family.joinerEngine(debounce: .milliseconds(20))
        try await joiner.activateAsJoiner()
        let before = family.joinerTransport.syncCalls

        joiner.localWriteHappened()
        await joiner.scheduledPush?.value

        #expect(family.joinerTransport.syncCalls == before)
    }

    /// A sync already running is joined, never run alongside a second one.
    @Test func overlappingSyncsNeverRunTwice() async throws {
        let family = try Family()
        try family.seedOwner()
        try await family.ownerEngine().activateAsOwner()
        let joiner = family.joinerEngine()
        try await joiner.activateAsJoiner()

        family.joinerTransport.holdNext = true
        let first = Task { await joiner.syncNow(.foreground) }
        while !family.joinerTransport.isHolding { await Task.yield() }
        let second = Task { await joiner.syncNow(.liveHint) }
        for _ in 0..<20 { await Task.yield() }
        #expect(family.joinerTransport.maxInFlight == 1)

        family.joinerTransport.release()
        await first.value
        await second.value
        #expect(family.joinerTransport.maxInFlight == 1)
        #expect(joiner.status.isUpToDate)
    }

    @Test func foregroundPollingSyncsUntilStopped() async throws {
        let family = try Family()
        try family.seedOwner()
        try await family.ownerEngine().activateAsOwner()
        let joiner = family.joinerEngine(pollInterval: .milliseconds(10))
        try await joiner.activateAsJoiner()
        let before = family.joinerTransport.syncCalls

        joiner.startForegroundPolling()
        let deadline = ContinuousClock.now + .seconds(5)
        while family.joinerTransport.syncCalls < before + 2, ContinuousClock.now < deadline {
            try await Task.sleep(for: .milliseconds(5))
        }
        joiner.stopForegroundPolling()
        #expect(family.joinerTransport.syncCalls >= before + 2)

        let stopped = family.joinerTransport.syncCalls
        try await Task.sleep(for: .milliseconds(60))
        #expect(family.joinerTransport.syncCalls == stopped)
    }

    /// Task 5 fix round 1. Disconnecting purges the shared realm; a sync
    /// still in flight must finish (or be cancelled) before that, and nothing
    /// may start afterwards — a late page would write the family back.
    @Test func stopWaitsForTheRunningSyncAndStartsNothingAfter() async throws {
        let family = try Family()
        let (baby, _) = try family.seedOwner()
        try await family.ownerEngine().activateAsOwner()
        let joiner = family.joinerEngine(debounce: .milliseconds(10), pollInterval: .milliseconds(10))
        try await joiner.activateAsJoiner()

        family.joinerTransport.holdNext = true
        let inFlight = Task { await joiner.syncNow(.foreground) }
        while !family.joinerTransport.isHolding { await Task.yield() }
        final class Flag { var raised = false }
        let stopped = Flag()
        let stopping = Task { await joiner.stop(); stopped.raised = true }
        for _ in 0..<20 { await Task.yield() }
        #expect(!stopped.raised)

        family.joinerTransport.release()
        await stopping.value
        await inFlight.value
        let after = family.joinerTransport.syncCalls

        _ = try family.joiner.logDiaper(.wet, for: baby.id)
        joiner.localWriteHappened()
        joiner.startForegroundPolling()
        await joiner.syncNow(.foreground)
        try await joiner.activateAsJoiner()
        try await Task.sleep(for: .milliseconds(60))
        #expect(family.joinerTransport.syncCalls == after)
    }

    /// Task 5 fix round 2. An activation already waiting its turn when
    /// `stop()` is called must not start once the running sync finishes —
    /// that would be work against a realm the caller is about to purge.
    @Test func activationQueuedBehindARunningSyncNeverRunsAfterStop() async throws {
        let family = try Family()
        try family.seedOwner()
        try await family.ownerEngine().activateAsOwner()
        let joiner = family.joinerEngine()
        try await joiner.activateAsJoiner()

        family.joinerTransport.holdNext = true
        let inFlight = Task { await joiner.syncNow(.foreground) }
        while !family.joinerTransport.isHolding { await Task.yield() }
        let queued = Task { try await joiner.activateAsJoiner() }
        for _ in 0..<20 { await Task.yield() }
        // Observable: a joiner activation points the store back at the family.
        try family.joiner.setActiveRealm(.local)

        let stopping = Task { await joiner.stop() }
        for _ in 0..<20 { await Task.yield() }
        family.joinerTransport.release()
        await stopping.value
        await inFlight.value
        _ = try? await queued.value
        for _ in 0..<20 { await Task.yield() }

        #expect(family.joiner.activeRealm == .local)
    }

    @Test func stopCancelsAPendingPush() async throws {
        let family = try Family()
        let (baby, _) = try family.seedOwner()
        try await family.ownerEngine().activateAsOwner()
        let joiner = family.joinerEngine(debounce: .milliseconds(30))
        try await joiner.activateAsJoiner()
        let before = family.joinerTransport.syncCalls

        _ = try family.joiner.logDiaper(.wet, for: baby.id)
        joiner.localWriteHappened()
        await joiner.stop()
        try await Task.sleep(for: .milliseconds(80))

        #expect(family.joinerTransport.syncCalls == before)
    }

    /// Fix round 1. A sync that could not run — here, because the store is no
    /// longer pointed at the family — must not be reported as up to date just
    /// because the previous one was.
    @Test func syncThatCannotRunIsNeverReportedUpToDate() async throws {
        let family = try Family()
        try family.seedOwner()
        try await family.ownerEngine().activateAsOwner()
        let joiner = family.joinerEngine()
        try await joiner.activateAsJoiner()
        #expect(joiner.status.isUpToDate)

        try family.joiner.setActiveRealm(.local)
        await joiner.syncNow(.foreground)

        #expect(!joiner.status.isUpToDate)
    }

    /// Fix round 1. Nothing else syncs while activation is still taking the
    /// snapshot: a poll or push interleaved with it would apply pages against
    /// a half-built base.
    @Test func activationHoldsOffOtherSyncsUntilItFinishes() async throws {
        let family = try Family()
        try family.seedOwner()
        try await family.ownerEngine().activateAsOwner()
        let joiner = family.joinerEngine()

        family.joinerTransport.holdNextSnapshot = true
        let activation = Task { try await joiner.activateAsJoiner() }
        while !family.joinerTransport.isHolding { await Task.yield() }
        let poll = Task { await joiner.syncNow(.foreground) }
        for _ in 0..<20 { await Task.yield() }
        #expect(family.joinerTransport.syncCalls == 0)

        family.joinerTransport.release()
        try await activation.value
        await poll.value
        #expect(family.joinerTransport.maxInFlight == 1)
        #expect(joiner.status.isUpToDate)
    }

    /// Fix round 1. The server refusing part of the owner's history is not a
    /// finished import: the checkpoint stays open for the next activation and
    /// the owner is told.
    @Test func rejectedImportIsNotMarkedComplete() async throws {
        let family = try Family()
        let (_, feed) = try family.seedOwner()
        family.server.rejectedEntityIDs = [feed.id]
        let checkpoint = FamilySyncEngine.importCheckpointID(for: family.ownerKey, storeID: try family.owner.importStoreID())

        let owner = family.ownerEngine()
        try await owner.activateAsOwner()
        #expect(owner.status == .failed(SyncIssueCopy.importIncomplete))
        #expect(try family.owner.resumeImport(checkpointID: checkpoint).phase == .draining)

        // A routine sync afterwards does not paper over it.
        await owner.syncNow(.foreground)
        #expect(owner.status == .failed(SyncIssueCopy.importIncomplete))

        let relaunched = family.ownerEngine()
        try await relaunched.activateAsOwner()
        #expect(relaunched.status == .failed(SyncIssueCopy.importIncomplete))
        #expect(try family.owner.resumeImport(checkpointID: checkpoint).phase == .draining)
    }

    /// Fix round 1. A feed with nothing in it is left out of the
    /// import rather than sent and refused, so the rest of the owner's history
    /// still completes the import.
    @Test func feedWithNothingToSendDoesNotBlockTheImport() async throws {
        let family = try Family()
        let (baby, _) = try family.seedOwner()
        let empty = FeedEntry(babyID: baby.id, kind: .breast, startedAt: .now, timeZoneID: "UTC")
        family.owner.context.insert(empty)
        try family.owner.record(.create, on: empty)
        let checkpoint = FamilySyncEngine.importCheckpointID(for: family.ownerKey, storeID: try family.owner.importStoreID())

        let owner = family.ownerEngine()
        try await owner.activateAsOwner()

        #expect(owner.status.isUpToDate)
        #expect(try family.owner.resumeImport(checkpointID: checkpoint).phase == .complete)
        #expect(!family.server.receivedEntityIDs.contains(empty.id))
    }

    /// 300 ms: with the socket the push debounce is the largest single delay
    /// between a tap on one phone and the row on the other.
    @Test func defaultScheduleIsAQuickPushAndTwentySecondPoll() {
        #expect(FamilySyncEngine.defaultDebounce == .milliseconds(300))
        #expect(FamilySyncEngine.defaultPollInterval == .seconds(20))
        #expect(FamilySyncEngine.defaultLivePollInterval == .seconds(300))
    }

    /// The server refuses a non-canonical UUID. Every identifier a sync
    /// request carries outside a record payload — mutation, entity, import
    /// and cursor IDs — must go out in lower case.
    @Test func syncRequestEncodesIdentifiersInLowerCase() async throws {
        let family = try Family()
        try family.seedOwner()
        try await family.ownerEngine().activateAsOwner()

        let body = try #require(family.server.requestBodies.first { !$0.isEmpty })
        let object = try #require(try JSONSerialization.jsonObject(with: body) as? [String: Any])
        let cursorIDs = (object["cursor"] as? [String: Any]).map { [$0["server_instance_id"], $0["family_id"]] } ?? []
        let mutations = try #require(object["mutations"] as? [[String: Any]])
        #expect(!mutations.isEmpty)
        let ids = mutations.flatMap { [$0["mutation_id"], $0["entity_id"], $0["import_id"]] } + cursorIDs
        for case let id as String in ids {
            #expect(id == id.lowercased())
        }
    }
}

extension FamilySyncEngine.Status {
    var isUpToDate: Bool {
        if case .upToDate = self { return true }
        return false
    }
}

// MARK: - Live hints

/// The live channel wired to the socket fakes, with a server probe the test
/// controls.
final class LiveRig: @unchecked Sendable {
    let connector = FakeLiveSocketConnector()
    let tokens = FakeLiveTokens()
    let sleeper = FakeLiveSleeper(blocking: [30, LiveHarness.openTimeout])
    private let probe = Locked((calls: 0, failuresLeft: 0, waits: [TimeInterval]()))
    private let info: ServerInfo

    @MainActor
    init(instanceID: UUID? = nil, liveHints: Bool = true, failingProbes: Int = 0) {
        info = ServerInfo(
            serverInstanceID: instanceID ?? FakeFamilyServer.instanceID, canonicalOrigin: EngineFixture.origin, recoveryEpoch: 1,
            capabilities: liveHints ? ["records-v1", "live-hints-v1"] : ["records-v1"],
            appleAuthorizationMode: "hosted", maxItemsPerPage: 200, maxEncodedBytes: 1_000_000,
            sourceVersion: "1.0.0", sourceURL: ""
        )
        probe { $0.failuresLeft = failingProbes }
    }

    var probes: Int { probe { $0.calls } }
    var probeWaits: [TimeInterval] { probe { $0.waits } }

    var channel: LiveHintChannel {
        LiveHintChannel(
            serverInfo: {
                let fails = self.probe { state -> Bool in
                    state.calls += 1
                    guard state.failuresLeft > 0 else { return false }
                    state.failuresLeft -= 1
                    return true
                }
                if fails { throw URLError(.timedOut) }
                return self.info
            },
            probeRetry: SyncRetryPolicy(base: 1, ceiling: 60, maximumAttempts: .max),
            sleep: { delay in
                self.probe { $0.waits.append(delay) }
                await Task.yield()
                try Task.checkCancellation()
            },
            makeSocket: { callbacks in
                FamilySyncWebSocket(
                    connector: self.connector, tokens: self.tokens, openTimeout: LiveHarness.openTimeout,
                    sleep: { try await self.sleeper.sleep($0) },
                    jitter: { $0.upperBound },
                    onHint: callbacks.onHint, onConnected: callbacks.onConnected,
                    onUnauthorized: callbacks.onUnauthorized, onStateChange: callbacks.onStateChange
                )
            }
        )
    }
}

@MainActor
@Suite("Family sync engine with live hints")
struct FamilySyncEngineLiveTests {
    /// Both phones in the family; the joiner's engine is the one under test.
    private func joined(
        live: LiveHintChannel? = nil, announcer: RecordingTimerAnnouncer? = nil
    ) async throws -> (Family, Baby, FamilySyncEngine, FamilySyncEngine) {
        let family = try Family()
        let (baby, _) = try family.seedOwner()
        let owner = family.ownerEngine()
        try await owner.activateAsOwner()
        let joiner = family.joinerEngine(announcer: announcer ?? RecordingTimerAnnouncer(), live: live)
        try await joiner.activateAsJoiner()
        return (family, baby, owner, joiner)
    }

    /// Spec test 15. The socket up means the slow safety poll; the socket
    /// down means the 20 s poll again — and the poll never stops, because the
    /// Lock Screen reads "in the foreground" from it.
    @Test func pollSlowsWhileTheSocketIsUpAndNeverStops() async throws {
        let rig = LiveRig()
        let socket = FakeLiveSocket()
        rig.connector.script = [.socket(socket)]
        let (_, _, _, joiner) = try await joined(live: rig.channel)

        joiner.startForegroundPolling()
        #expect(joiner.foregroundPollInterval == .seconds(20))
        try await until { await joiner.liveState == .connected }
        #expect(joiner.foregroundPollInterval == .seconds(300))
        #expect(joiner.isInForeground)

        socket.fail(.closed(code: 1013, reason: "slow consumer"))
        try await until { await joiner.liveState != .connected }
        #expect(joiner.foregroundPollInterval == .seconds(20))
        #expect(joiner.isInForeground)

        await joiner.stop()
    }

    /// Background closes the socket with 1000, freeing the device's one slot;
    /// foreground dials again. The server is probed once per engine.
    @Test func backgroundClosesTheSocketAndForegroundReopensIt() async throws {
        let rig = LiveRig()
        let first = FakeLiveSocket()
        let second = FakeLiveSocket()
        rig.connector.script = [.socket(first), .socket(second)]
        let (_, _, _, joiner) = try await joined(live: rig.channel)

        joiner.startForegroundPolling()
        try await until { await joiner.liveState == .connected }
        joiner.stopForegroundPolling()
        #expect(!joiner.isInForeground)
        try await until { await joiner.liveState == .stopped }
        #expect(first.closeCodes.first == .normalClosure)

        joiner.startForegroundPolling()
        try await until { await joiner.liveState == .connected }
        #expect(rig.connector.dialledTokens.count == 2)
        #expect(rig.probes == 1)

        await joiner.stop()
        #expect(second.closeCodes.first == .normalClosure)
    }

    /// `stop()` ends the socket before anything else, and a hint after it
    /// starts nothing: the realm is about to be purged.
    @Test func noHintSyncsAfterStop() async throws {
        let rig = LiveRig()
        let socket = FakeLiveSocket()
        rig.connector.script = [.socket(socket)]
        let (family, _, _, joiner) = try await joined(live: rig.channel)
        joiner.startForegroundPolling()
        try await until { await joiner.liveState == .connected }
        try await until { await !joiner.isBusy }

        await joiner.stop()
        #expect(joiner.liveState == .stopped)
        #expect(socket.closeCodes.first == .normalClosure)
        let after = family.joinerTransport.syncCalls

        joiner.liveHintArrived(family.hint(family.server.sequence + 1))
        socket.deliver(LiveHint(familyID: FakeFamilyServer.familyID, cursor: family.hint(family.server.sequence + 2)))
        try await Task.sleep(for: .milliseconds(30))
        #expect(family.joinerTransport.syncCalls == after)
        #expect(!joiner.isBusy)
    }

    /// Spec test 16. A timer the other phone started, seen through a hint,
    /// still gets its Live Activity: the hint sync runs in the foreground.
    @Test func aRemoteTimerSeenThroughAHintGetsALiveActivity() async throws {
        let rig = LiveRig()
        let socket = FakeLiveSocket()
        rig.connector.script = [.socket(socket)]
        let announcer = RecordingTimerAnnouncer()
        let (family, baby, owner, joiner) = try await joined(live: rig.channel, announcer: announcer)
        joiner.startForegroundPolling()
        try await until { await joiner.liveState == .connected }
        try await until { await !joiner.isBusy }

        let timer = try family.owner.startSleep(for: baby.id)
        await owner.syncNow(.localWrite)
        socket.deliver(LiveHint(familyID: FakeFamilyServer.familyID, cursor: family.hint(family.server.sequence)))

        let timerID = timer.id
        try await until { await announcer.started == [timerID] }
        #expect(joiner.lastSyncReason == .liveHint)
        await joiner.stop()
    }

    /// Spec test 4. A hint the local cursor already covers costs nothing; one
    /// past it is one sync.
    @Test func aHintTheCursorCoversSyncsNothing() async throws {
        let (family, baby, owner, joiner) = try await joined()
        let before = family.joinerTransport.syncCalls

        joiner.liveHintArrived(family.hint(family.server.sequence))
        joiner.liveHintArrived(family.hint(family.server.sequence - 1))
        #expect(!joiner.isBusy)
        #expect(family.joinerTransport.syncCalls == before)

        let diaper = try family.owner.logDiaper(.wet, for: baby.id)
        await owner.syncNow(.localWrite)
        joiner.liveHintArrived(family.hint(family.server.sequence))
        try await until { await !joiner.isBusy }

        #expect(family.joinerTransport.syncCalls == before + 1)
        #expect(joiner.lastSyncReason == .liveHint)
        #expect(try EngineFixture.diapers(family.joiner).map(\.id) == [diaper.id])
    }

    /// Spec test 5. A burst of hints during a sync is one more pass, not five.
    @Test func hintsDuringARunningSyncCostAtMostOneMorePass() async throws {
        let (family, _, _, joiner) = try await joined()
        let before = family.joinerTransport.syncCalls

        family.joinerTransport.holdNext = true
        let inFlight = Task { await joiner.syncNow(.foreground) }
        while !family.joinerTransport.isHolding { await Task.yield() }
        let top = family.server.sequence
        for step in Int64(1)...5 { joiner.liveHintArrived(family.hint(top + step)) }
        family.joinerTransport.release()
        await inFlight.value

        #expect(family.joinerTransport.syncCalls == before + 2)
        #expect(joiner.lastSyncReason == .liveHint)
    }

    /// Spec test 6. A hint does not replace a local write queued behind the
    /// running sync; the write goes out as its own reason.
    @Test func aHintNeverReplacesAQueuedLocalWrite() async throws {
        let (family, baby, owner, joiner) = try await joined()
        _ = try family.owner.logDiaper(.dirty, for: baby.id)
        await owner.syncNow(.localWrite)

        family.joinerTransport.holdNext = true
        let inFlight = Task { await joiner.syncNow(.foreground) }
        while !family.joinerTransport.isHolding { await Task.yield() }
        let mine = try family.joiner.logDiaper(.wet, for: baby.id)
        let push = Task { await joiner.syncNow(.localWrite) }
        try await until { await joiner.queuedFollowUp == .localWrite }
        joiner.liveHintArrived(family.hint(family.server.sequence))
        #expect(joiner.queuedFollowUp == .localWrite)
        family.joinerTransport.release()
        await inFlight.value
        await push.value

        #expect(joiner.lastSyncReason == .localWrite)
        #expect(family.server.receivedEntityIDs.contains(mine.id))
    }

    /// Spec test 7. The writer hears its own hint while its push is still in
    /// flight; the push's own cursor covers it, so no second request.
    @Test func theWritersOwnHintCostsNoSecondRequest() async throws {
        let (family, baby, owner, _) = try await joined()
        _ = try family.owner.logDiaper(.wet, for: baby.id)
        let before = family.ownerTransport.syncCalls

        family.ownerTransport.holdNext = true
        let push = Task { await owner.syncNow(.localWrite) }
        while !family.ownerTransport.isHolding { await Task.yield() }
        // The commit's NOTIFY goes out before the response does.
        owner.liveHintArrived(family.hint(family.server.sequence + 1))
        family.ownerTransport.release()
        await push.value

        #expect(family.ownerTransport.syncCalls == before + 1)
    }

    /// Fix round 1. The refused socket's sync joins a running one whose hint
    /// follow-up the running pass will cover. That follow-up must still run,
    /// or nothing renews the token the socket is about to redial with.
    @Test func theRenewingSyncIsNeverSkippedAsCovered() async throws {
        let family = try Family()
        let (baby, _) = try family.seedOwner()
        let owner = family.ownerEngine()
        try await owner.activateAsOwner()
        var renewals = 0
        let joiner = family.joinerEngine(renew: { renewals += 1 })
        try await joiner.activateAsJoiner()
        _ = try family.owner.logDiaper(.wet, for: baby.id)
        await owner.syncNow(.localWrite)
        let before = family.joinerTransport.syncCalls

        family.joinerTransport.holdNext = true
        let inFlight = Task { await joiner.syncNow(.foreground) }
        while !family.joinerTransport.isHolding { await Task.yield() }
        // Covered once the held pass lands: on its own, a skipped follow-up.
        joiner.liveHintArrived(family.hint(family.server.sequence))
        #expect(joiner.queuedFollowUp == .liveHint)
        family.joinerTransport.failCall = (before + 2, .accessExpired)
        let refused = Task { await joiner.liveRefused() }
        try await until { await joiner.pendingHintSequence == .max }
        family.joinerTransport.release()
        await inFlight.value

        #expect(await refused.value)
        #expect(renewals == 1)
        // The held pass, the follow-up refused as expired, and its retry.
        #expect(family.joinerTransport.syncCalls == before + 3)
    }

    /// Fix round 1. A reconnect syncs even when the last hint is covered:
    /// whatever changed while the socket was down sent no hint at all.
    @Test func aReconnectSyncsEvenWhenTheCursorCoversTheLastHint() async throws {
        let (family, baby, owner, joiner) = try await joined()
        let before = family.joinerTransport.syncCalls

        joiner.liveConnected()
        try await until { await !joiner.isBusy }
        #expect(family.joinerTransport.syncCalls == before + 1)

        family.joinerTransport.holdNext = true
        let inFlight = Task { await joiner.syncNow(.foreground) }
        while !family.joinerTransport.isHolding { await Task.yield() }
        // The other phone writes while this pass is held; the pass will pull
        // it, so the hint alone would be covered and its follow-up skipped.
        _ = try family.owner.logDiaper(.wet, for: baby.id)
        await owner.syncNow(.localWrite)
        joiner.liveHintArrived(family.hint(family.server.sequence))
        joiner.liveConnected()
        #expect(joiner.pendingHintSequence == .max)
        family.joinerTransport.release()
        await inFlight.value

        #expect(family.joinerTransport.syncCalls == before + 3)
        #expect(joiner.lastSyncReason == .liveHint)
    }

    /// A refused socket token is one HTTP sync, which renews; then the socket
    /// dials again with whatever token the renewal left.
    @Test func aRefusedSocketRenewsThroughOneSyncAndRedials() async throws {
        let rig = LiveRig()
        let socket = FakeLiveSocket()
        rig.connector.script = [.fail(.unauthorized), .socket(socket)]
        let family = try Family()
        try family.seedOwner()
        try await family.ownerEngine().activateAsOwner()
        var renewals = 0
        let joiner = family.joinerEngine(renew: { renewals += 1; rig.tokens.token = "token-2" }, live: rig.channel)
        try await joiner.activateAsJoiner()

        family.joinerTransport.failNextWith = .accessExpired
        joiner.startForegroundPolling()
        try await until { await joiner.liveState == .connected }

        #expect(renewals == 1)
        #expect(rig.connector.dialledTokens == ["token-1", "token-2"])
        await joiner.stop()
    }

    /// When only a fresh sign-in helps, the socket stops dialling — it would
    /// only collect refusals — and the poll carries on at 20 s.
    @Test func aRefusedSocketStopsWhenOnlySignInHelps() async throws {
        let rig = LiveRig()
        rig.connector.script = [.fail(.unauthorized), .socket(FakeLiveSocket())]
        let (family, _, _, joiner) = try await joined(live: rig.channel)

        family.joinerTransport.failNextWith = .reauthenticationRequired
        joiner.startForegroundPolling()
        try await until { await MainActor.run { joiner.status == .needsSignIn && joiner.liveState == .stopped } }
        try await Task.sleep(for: .milliseconds(20))

        #expect(rig.connector.dialledTokens.count == 1)
        #expect(joiner.isInForeground)
        #expect(joiner.foregroundPollInterval == .seconds(20))
        await joiner.stop()
    }

    /// A probe that fails is a network problem, not a server without hints:
    /// it is backed off and retried.
    @Test func aFailedServerProbeIsRetriedNotTakenAsNoHints() async throws {
        let rig = LiveRig(failingProbes: 2)
        rig.connector.script = [.socket(FakeLiveSocket())]
        let (_, _, _, joiner) = try await joined(live: rig.channel)

        joiner.startForegroundPolling()
        try await until { await joiner.liveState == .connected }

        #expect(rig.probes == 3)
        // Jittered within the growing windows 0...1 and 0...2.
        #expect(rig.probeWaits.count == 2)
        #expect(zip(rig.probeWaits, [1.0, 2.0]).allSatisfy { $0 <= $1 })
        await joiner.stop()
    }

    /// No socket for a server without hints, nor for a different instance
    /// answering at the family's origin. Both stay on the 20 s poll.
    @Test func noSocketWithoutHintsOrForAnotherInstance() async throws {
        for rig in [LiveRig(liveHints: false), LiveRig(instanceID: UUID())] {
            let (_, _, _, joiner) = try await joined(live: rig.channel)
            joiner.startForegroundPolling()
            try await until { rig.probes == 1 }
            try await Task.sleep(for: .milliseconds(20))

            #expect(rig.connector.dialledTokens.isEmpty)
            #expect(joiner.liveState == .stopped)
            #expect(joiner.foregroundPollInterval == .seconds(20))
            await joiner.stop()
        }
    }
}

// The actual HTTP transport binds 401s to the sent credential version. A
// bare SyncErrorCode fake cannot detect activation's wrapper regression.
private struct ActivationTokens: SessionTokenProviding {
    let credentials: SessionCredentials
    func currentAccessToken() async -> String? { credentials.accessToken }
    func currentSessionCredentials() async -> SessionCredentials? { credentials }
}
private final class Activation401Protocol: URLProtocol, @unchecked Sendable {
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        let code = request.value(forHTTPHeaderField: "X-Activation-Code") ?? "access_expired"
        let data = try! JSONSerialization.data(withJSONObject: ["error": ["code": code]])
        client?.urlProtocol(self, didReceive: HTTPURLResponse(url: request.url!, statusCode: 401, httpVersion: nil, headerFields: nil)!, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: data)
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}
@MainActor
private final class ActivationBoundTransport: FamilySyncTransport, @unchecked Sendable {
    let http: FamilySyncHTTPClient
    let succeeding: FakeServerTransport
    var first = true
    var beforeResponse: () throws -> Void = {}
    var forceFreshProof = false
    init(http: FamilySyncHTTPClient, succeeding: FakeServerTransport) { self.http = http; self.succeeding = succeeding }
    func sync(_ request: SyncRequest) async throws -> SyncResponse { try await succeeding.sync(request) }
    func snapshot(pageToken: String?) async throws -> SnapshotResponse {
        if first {
            first = false
            try beforeResponse()
            do { return try await http.snapshot(pageToken: pageToken) }
            catch let failure as CredentialBoundAuthFailure {
                if forceFreshProof { throw CredentialBoundAuthFailure(underlying: failure.underlying, credentialVersion: failure.credentialVersion, requiresFreshProof: true) }
                throw failure
            }
        }
        return try await succeeding.snapshot(pageToken: pageToken)
    }
}

@MainActor
@Suite("Credential-bound family activation")
struct CredentialBoundActivationTests {
    @Test(arguments: ["current", "stale", "stale_expired", "closed", "terminal", "explicit"])
    func realHTTPSnapshotRefusalHonorsCurrentCredentialAndGate(_ scenario: String) async throws {
        let family = try Family()
        let (baby, _) = try family.seedOwner()
        let owner = family.ownerEngine()
        try await owner.activateAsOwner()
        let credentials = SessionCredentials(accessToken: "synthetic-activation-access", refreshToken: "synthetic-activation-refresh", expiresAt: .distantFuture, recoveryEpoch: 0, deviceContinuity: DeviceContinuity(deviceID: family.joinerKey.deviceID, credential: "synthetic-continuity"))
        try family.joiner.markAccountAuthenticated(family.joinerKey.leaseKey, credentialVersion: credentials.credentialVersion)
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [Activation401Protocol.self]
        config.httpAdditionalHeaders = ["X-Activation-Code": ["current", "stale_expired", "explicit"].contains(scenario) ? "access_expired" : "reauthentication_required"]
        let session = URLSession(configuration: config)
        defer { session.invalidateAndCancel() }
        let http = FamilySyncHTTPClient(endpoint: ConfirmedEndpoint(origin: try ServerOrigin(validating: family.joinerKey.canonicalOrigin), serverInstanceID: family.joinerKey.serverInstanceID), tokens: ActivationTokens(credentials: credentials), session: session)
        let transport = ActivationBoundTransport(http: http, succeeding: family.joinerTransport)
        transport.forceFreshProof = scenario == "explicit"
        let newer = UUID()
        transport.beforeResponse = {
            if ["stale", "stale_expired"].contains(scenario) { try family.joiner.markAccountSignedIn(family.joinerKey.leaseKey, credentialVersion: newer) }
            if scenario == "closed" { try family.joiner.markAccountSignedOut(family.joinerKey.leaseKey) }
        }
        var renewals = 0
        let engine = FamilySyncEngine(store: family.joiner, familyKey: family.joinerKey, transport: transport, importClient: EngineFixture.resolver(), renew: { renewals += 1 })
        if scenario == "current" {
            try await engine.activateAsJoiner()
            #expect(renewals == 1)
            #expect(engine.status.isUpToDate)
            #expect(try family.joiner.babies().map(\.id) == [baby.id])
        } else {
            do { try await engine.activateAsJoiner() } catch let failure as CredentialBoundAuthFailure {
                #expect(failure.credentialVersion == credentials.credentialVersion)
            }
            #expect(renewals == 0)
            if ["terminal", "explicit"].contains(scenario) { #expect(engine.status == .needsSignIn) }
            if ["stale", "stale_expired"].contains(scenario) {
                #expect(engine.status != .needsSignIn)
                #expect(try family.joiner.accountCredentialIsCurrent(newer, for: family.joinerKey.leaseKey))
            }
            if scenario == "closed" { #expect(try !family.joiner.accountAllowsOrdinarySync(family.joinerKey.leaseKey)) }
        }
    }
}
