import BabyCore
import BabyProtocol
import BabyStore
import Foundation
import SwiftData
import Testing
@testable import BabySync

/// A scripted resolver. It records every request, which is how the tests prove
/// the immutable-mode and frozen-bytes rules rather than merely the happy path.
@MainActor
final class FakeResolverClient: ImportResolverClient, @unchecked Sendable {
    var response: ImportResolveResponse
    var conflictPages: [ImportResolveResponse] = []
    var failNextResolveWith: (any Error)?
    private(set) var requests: [ImportResolveRequest] = []
    private(set) var resolutions: [(UUID, ImportConflictResolution)] = []

    init(response: ImportResolveResponse) { self.response = response }

    func resolve(_ request: ImportResolveRequest) async throws -> ImportResolveResponse {
        requests.append(request)
        if let error = failNextResolveWith {
            failNextResolveWith = nil
            throw error
        }
        return response
    }

    func resolveConflict(
        conflictID: UUID, resolverRevision: Int64,
        resolution: ImportConflictResolution, commandID: UUID
    ) async throws -> ImportResolveResponse {
        resolutions.append((conflictID, resolution))
        return response
    }

    func conflictPage(importID: UUID, pageToken: String?) async throws -> ImportResolveResponse {
        guard !conflictPages.isEmpty else { return response }
        return conflictPages.removeFirst()
    }
}

enum ResolverFixture {
    static func response(
        importID: UUID,
        revision: Int64 = 1,
        mappings: [BabyMapping] = [],
        conflicts: [ImportConflict] = [],
        conflictsComplete: Bool = true,
        nextConflictPageToken: String? = nil,
        mappingComplete: Bool = true,
        normalization: String = "nfc-trim-fold-nfc-v1"
    ) -> ImportResolveResponse {
        let json = """
        {
          "import_id": "\(importID.uuidString.lowercased())",
          "resolver_revision": \(revision),
          "normalization_version": "\(normalization)",
          "mappings": [],
          "conflicts": [],
          "conflict_count": \(conflicts.count),
          "conflicts_complete": \(conflictsComplete),
          "next_conflict_page_token": \(nextConflictPageToken.map { "\"\($0)\"" } ?? "null"),
          "mapping_complete": \(mappingComplete),
          "next_chunk_index": 1,
          "manifest_complete": true
        }
        """
        var decoded = try! JSONDecoder().decode(ImportResolveResponse.self, from: Data(json.utf8))
        // Mappings and conflicts are injected after decoding so a test can
        // build them without hand-writing their JSON each time.
        decoded = ImportResolveResponse(
            importID: decoded.importID, resolverRevision: decoded.resolverRevision,
            normalizationVersion: decoded.normalizationVersion, mappings: mappings,
            conflicts: conflicts, conflictCount: Int64(conflicts.count),
            conflictsComplete: conflictsComplete, nextConflictPageToken: nextConflictPageToken,
            mappingComplete: mappingComplete, nextChunkIndex: decoded.nextChunkIndex,
            manifestComplete: decoded.manifestComplete
        )
        return decoded
    }

    static func conflict(_ id: UUID, localBabyID: UUID) -> ImportConflict {
        ImportConflict(
            conflictID: id, kind: "owner_choice", localBabyID: localBabyID,
            recordID: nil, candidateBabyIDs: [UUID()], candidateVersions: [1]
        )
    }
}

@MainActor
@Suite("Import coordinator")
struct ImportCoordinatorTests {
    static let realm = RealmID.shared(
        canonicalOrigin: "https://sync.example",
        serverInstanceID: UUID(uuidString: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")!,
        familyID: UUID(uuidString: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")!
    )

    private func fixture(
        mappings: @escaping (UUID) -> [BabyMapping] = { _ in [] }
    ) throws -> (BabyStore, FakeResolverClient, ImportCoordinator, UUID, Baby) {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let baby = try store.addBaby(name: " Élie ", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        _ = try store.logBottle(volumeML: 120, contents: .formula, for: baby.id)
        let checkpoint = UUID()
        let client = FakeResolverClient(
            response: ResolverFixture.response(importID: checkpoint, mappings: mappings(baby.id))
        )
        let coordinator = ImportCoordinator(store: store, client: client, checkpointID: checkpoint)
        return (store, client, coordinator, checkpoint, baby)
    }

    @Test func everyRequestAdvertisesTheExactCapabilityPair() async throws {
        let (_, client, coordinator, checkpoint, baby) = try fixture {
            [BabyMapping(localBabyID: $0, sharedBabyID: $0)]
        }
        _ = baby
        try await coordinator.startOrResume(realm: Self.realm, mode: .preserveLocalIDs)

        #expect(client.requests.allSatisfy { $0.capabilities == ["records-v1", "import-resolver-v1"] })
        #expect(client.requests.allSatisfy { $0.protocolVersion == "v1" })
        _ = checkpoint
    }

    @Test func createFamilyEmitsPreserveLocalIDsAndJoinEmitsMatchShared() async throws {
        for mode in [ImportMode.preserveLocalIDs, .matchShared] {
            let (_, client, coordinator, _, baby) = try fixture {
                [BabyMapping(localBabyID: $0, sharedBabyID: $0)]
            }
            _ = baby
            try await coordinator.startOrResume(realm: Self.realm, mode: mode)
            #expect(client.requests.allSatisfy { $0.importMode == mode.rawValue })
        }
    }

    /// The mode is fixed at chunk 0. A resume that disagrees is refused rather
    /// than quietly switching the semantics of the import mid-flight.
    @Test func importModeIsImmutableAcrossResume() async throws {
        let (store, _, coordinator, checkpoint, _) = try fixture()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .matchShared)

        await #expect(throws: ImportStoreError.importModeChanged) {
            try await coordinator.startOrResume(realm: Self.realm, mode: .preserveLocalIDs)
        }
    }

    /// The babies go over with their stored name untouched. The client does no
    /// normalization: P2 owns the normalizer, and a client that pre-folded
    /// names would disagree with the server about which two babies are one
    /// child.
    @Test func babyNamesAreSentUnnormalized() async throws {
        let (store, client, coordinator, checkpoint, baby) = try fixture {
            [BabyMapping(localBabyID: $0, sharedBabyID: $0)]
        }
        _ = try store.beginImport(checkpointID: checkpoint, mode: .matchShared)
        _ = try store.freezeImportManifest(checkpointID: checkpoint)
        _ = try await coordinator.retryManifestChunk(ordinal: 0, mode: .matchShared)

        let sent = try #require(client.requests.first)
        #expect(sent.normalizationVersion == "nfc-trim-fold-nfc-v1")
        #expect(sent.babies.first?.name == " Élie ")
        #expect(sent.babies.first?.localBabyID == baby.id)
    }

    /// The record manifest carries a content hash so the server can treat an
    /// identical record as a confirmed no-op rather than a conflict.
    @Test func recordsCarryContentHashes() async throws {
        let (store, client, coordinator, checkpoint, _) = try fixture()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .matchShared)
        _ = try store.freezeImportManifest(checkpointID: checkpoint)
        _ = try await coordinator.retryManifestChunk(ordinal: 0, mode: .matchShared)

        let sent = try #require(client.requests.first)
        #expect(sent.records.count == 2)
        #expect(sent.records.first?.entityKind == "feed")
        #expect(sent.records.first?.contentSHA256.count == 64)
        #expect(sent.totalRecordCount == 2)
    }

    /// A retried chunk resends the frozen bytes. This is the whole reason the
    /// manifest is frozen.
    @Test func retryAfterLocalDeleteStillSendsTheFrozenRecord() async throws {
        let (store, client, coordinator, checkpoint, _) = try fixture()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .matchShared)
        _ = try store.freezeImportManifest(checkpointID: checkpoint)
        _ = try await coordinator.retryManifestChunk(ordinal: 0, mode: .matchShared)
        let before = try #require(client.requests.first)

        let feed = try #require(try store.context.fetch(FetchDescriptorBox.feeds()).first)
        try store.softDelete(feed)
        _ = try await coordinator.retryManifestChunk(ordinal: 0, mode: .matchShared)

        #expect(client.requests.last?.records == before.records)
    }

    /// An incomplete mapping must not be persisted: the rewrite would rewire
    /// against a map that is still being decided.
    @Test func incompleteMappingIsNeverPersisted() async throws {
        let (store, client, coordinator, checkpoint, baby) = try fixture()
        client.response = ResolverFixture.response(
            importID: checkpoint,
            mappings: [BabyMapping(localBabyID: baby.id, sharedBabyID: baby.id)],
            mappingComplete: false
        )

        await #expect(throws: ImportCoordinatorError.mappingIncomplete) {
            try await coordinator.startOrResume(realm: Self.realm, mode: .matchShared)
        }
        #expect(try store.resumeImport(checkpointID: checkpoint).phase == .manifestFrozen)
        #expect(store.activeRealm == .local)
    }

    /// A resolver that changes its normalizer mid-import is not one this
    /// import can continue against — its earlier answers were computed under
    /// different rules.
    @Test func changedNormalizationVersionStopsTheImport() async throws {
        let (store, client, coordinator, checkpoint, baby) = try fixture()
        client.response = ResolverFixture.response(
            importID: checkpoint,
            mappings: [BabyMapping(localBabyID: baby.id, sharedBabyID: baby.id)],
            normalization: "something-else-v2"
        )

        await #expect(throws: ImportCoordinatorError.normalizationVersionChanged(
            expected: "nfc-trim-fold-nfc-v1", actual: "something-else-v2"
        )) {
            try await coordinator.startOrResume(realm: Self.realm, mode: .matchShared)
        }
        #expect(store.activeRealm == .local)
    }

    /// Conflict completeness comes from `conflicts_complete`, never from
    /// `mapping_complete`: the mapping can be settled while conflicts are
    /// still paging, and showing a partial list has the owner resolve against
    /// a set about to grow.
    @Test func conflictsDrainAcrossPagesInOrderBeforeBeingPresented() async throws {
        let (_, client, coordinator, checkpoint, baby) = try fixture()
        let second = UUID(uuidString: "22222222-2222-4222-8222-222222222222")!
        let first = UUID(uuidString: "11111111-1111-4111-8111-111111111111")!
        client.conflictPages = [
            ResolverFixture.response(
                importID: checkpoint,
                conflicts: [ResolverFixture.conflict(second, localBabyID: baby.id)],
                conflictsComplete: false, nextConflictPageToken: "page-2", mappingComplete: true
            ),
            ResolverFixture.response(
                importID: checkpoint,
                conflicts: [ResolverFixture.conflict(first, localBabyID: baby.id)],
                conflictsComplete: true
            ),
        ]

        try await coordinator.drainConflicts(importID: checkpoint)

        #expect(coordinator.conflicts.map(\.conflictID) == [first, second])
    }

    @Test func onlyValidOwnerResolutionsAreSent() async throws {
        let (_, client, coordinator, checkpoint, baby) = try fixture()
        try await coordinator.drainConflicts(importID: checkpoint)
        let conflictID = UUID()

        await #expect(throws: ImportStoreError.invalidResolution) {
            try await coordinator.resolveConflict(
                conflictID, resolution: ImportConflictResolution(choice: .useExisting, canonicalBabyID: nil)
            )
        }
        try await coordinator.resolveConflict(
            conflictID, resolution: ImportConflictResolution(choice: .keepLocal, canonicalBabyID: nil)
        )
        #expect(client.resolutions.map(\.0) == [conflictID])
        _ = baby
    }

    /// Resuming after the realm is exposed must not run the rewrite twice.
    @Test func resumeAfterExposureIsIdempotent() async throws {
        let (store, _, coordinator, checkpoint, baby) = try fixture {
            [BabyMapping(localBabyID: $0, sharedBabyID: $0)]
        }
        _ = baby
        try await coordinator.startOrResume(realm: Self.realm, mode: .matchShared)
        let queuedFirst = try store.liveOutboxRows(in: Self.realm).count

        try await coordinator.startOrResume(realm: Self.realm, mode: .matchShared)

        #expect(try store.liveOutboxRows(in: Self.realm).count == queuedFirst)
        #expect(try store.resumeImport(checkpointID: checkpoint).phase == .draining)
    }
}

/// Keeps the SwiftData descriptor out of the test bodies.
enum FetchDescriptorBox {
    static func feeds() -> FetchDescriptor<FeedEntry> { FetchDescriptor<FeedEntry>() }
}
