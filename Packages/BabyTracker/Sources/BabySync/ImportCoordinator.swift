import BabyProtocol
import BabyStore
import Foundation

/// The P2 import endpoints, as the client needs them.
public protocol ImportResolverClient: Sendable {
    func resolve(_ request: ImportResolveRequest) async throws -> ImportResolveResponse
    func resolveConflict(
        conflictID: UUID,
        resolverRevision: Int64,
        resolution: ImportConflictResolution,
        commandID: UUID
    ) async throws -> ImportResolveResponse
    func conflictPage(importID: UUID, pageToken: String?) async throws -> ImportResolveResponse
}

public enum ImportCoordinatorError: Error, Sendable, Equatable {
    case normalizationVersionChanged(expected: String, actual: String)
    case resolverRevisionRegressed(known: Int64, received: Int64)
    case resolutionStale
    case mappingIncomplete
}

/// Drives one import from frozen manifest to exposed shared realm.
///
/// It owns the *ordering* rules, which is where the value is. The resolver is
/// allowed to answer in pages and to change its mind about conflicts; what it
/// is never allowed to do is make the client rebuild the frozen manifest or
/// rewire a reference before the mapping is durable.
@MainActor
public final class ImportCoordinator {
    /// The exact capability pair every resolver request advertises.
    public static let capabilities = ["records-v1", "import-resolver-v1"]
    /// P2's versioned normalizer. The client never does its own matching, so
    /// this is carried, compared and reported — not interpreted.
    public static let normalizationVersion = "nfc-trim-fold-nfc-v1"

    private let store: BabyStore
    private let client: any ImportResolverClient
    private let checkpointID: UUID

    public private(set) var conflicts: [ImportConflict] = []
    public private(set) var resolverRevision: Int64?

    public init(store: BabyStore, client: any ImportResolverClient, checkpointID: UUID) {
        self.store = store
        self.client = client
        self.checkpointID = checkpointID
    }

    /// Starts or resumes. Safe to call after a crash at any phase: the phase
    /// itself decides what still needs doing, so nothing is repeated and
    /// nothing is skipped.
    public func startOrResume(realm: RealmID, mode: ImportMode) async throws {
        let state = try store.beginImport(checkpointID: checkpointID, mode: mode)

        if state.phase < .manifestFrozen {
            _ = try store.freezeImportManifest(checkpointID: checkpointID)
        }
        if state.phase < .mappingDurable {
            try await resolveMapping(mode: mode)
        }
        if try store.resumeImport(checkpointID: checkpointID).phase < .sharedRealmExposed {
            try store.rewriteAndMoveWorkspace(checkpointID: checkpointID, to: realm)
        }
        try store.markImportDraining(checkpointID: checkpointID)
    }

    /// Sends every frozen chunk and persists the mapping once the resolver
    /// says it is complete.
    func resolveMapping(mode: ImportMode) async throws {
        guard let manifest = try store.frozenManifest(checkpointID: checkpointID) else {
            throw ImportStoreError.missingFrozenManifest(checkpointID)
        }

        var mapping: [UUID: UUID] = [:]
        var complete = false
        for chunk in manifest.chunks {
            let response = try await send(chunkOrdinal: chunk.ordinal, mode: mode, manifest: manifest)
            for entry in response.mappings { mapping[entry.localBabyID] = entry.sharedBabyID }
            conflicts = response.conflicts
            complete = response.mappingComplete
        }

        guard complete else { throw ImportCoordinatorError.mappingIncomplete }
        // Validated before persistence, and persistence before any rewiring.
        try store.saveMapping(mapping, checkpointID: checkpointID)
    }

    /// Resends one chunk using the frozen bytes.
    ///
    /// This is the whole reason the manifest is frozen: a lost response must be
    /// retried with the bytes the server may already have half-accepted, not
    /// with a rebuild from rows the user has edited since.
    @discardableResult
    public func retryManifestChunk(ordinal: Int, mode: ImportMode) async throws -> ImportResolveResponse {
        guard let manifest = try store.frozenManifest(checkpointID: checkpointID) else {
            throw ImportStoreError.missingFrozenManifest(checkpointID)
        }
        return try await send(chunkOrdinal: ordinal, mode: mode, manifest: manifest)
    }

    /// Builds the request for one chunk. `import_mode` is immutable from chunk
    /// 0, so it is read from the durable checkpoint rather than the caller.
    public func makeResolveRequest(chunkOrdinal: Int, mode: ImportMode) throws -> ImportResolveRequest {
        let state = try store.resumeImport(checkpointID: checkpointID)
        try ImportStoreValidator.validate(mode: mode, against: state.importMode)
        guard let manifest = try store.frozenManifest(checkpointID: checkpointID) else {
            throw ImportStoreError.missingFrozenManifest(checkpointID)
        }
        return try Self.request(
            importID: checkpointID,
            mode: state.importMode,
            manifest: manifest,
            chunkOrdinal: chunkOrdinal
        )
    }

    /// Drains conflict pages in the resolver's order before anything is shown.
    ///
    /// Completeness comes from `conflicts_complete`, never inferred from
    /// `mapping_complete`: a mapping can be settled while conflicts are still
    /// being paged, and presenting a partial list would have the owner resolve
    /// against a set that is about to grow.
    public func drainConflicts(importID: UUID) async throws {
        var token: String? = nil
        var collected: [ImportConflict] = []
        repeat {
            try ImportStoreValidator.validatePageToken(token)
            let page = try await client.conflictPage(importID: importID, pageToken: token)
            try check(page)
            collected.append(contentsOf: page.conflicts)
            token = page.nextConflictPageToken
            if page.conflictsComplete { break }
        } while token != nil
        conflicts = collected.sorted { $0.conflictID.uuidString < $1.conflictID.uuidString }
    }

    /// Applies one owner decision.
    ///
    /// `import_resolution_stale` discards only the paged conflict projection
    /// and restarts at page one. It never discards the frozen manifest and
    /// never refreezes live edits — the import the server is holding is still
    /// the one that was frozen.
    public func resolveConflict(
        _ conflictID: UUID,
        resolution: ImportConflictResolution,
        commandID: UUID = UUID()
    ) async throws {
        try ImportStoreValidator.validate(resolution)
        guard let revision = resolverRevision else { throw ImportCoordinatorError.resolutionStale }
        do {
            let response = try await client.resolveConflict(
                conflictID: conflictID,
                resolverRevision: revision,
                resolution: resolution,
                commandID: commandID
            )
            try check(response)
            conflicts = response.conflicts
        } catch ImportCoordinatorError.resolutionStale {
            conflicts = []
            try await drainConflicts(importID: checkpointID)
            throw ImportCoordinatorError.resolutionStale
        }
    }

    // MARK: Internals

    private func send(chunkOrdinal: Int, mode: ImportMode, manifest: FrozenImportManifest) async throws -> ImportResolveResponse {
        let request = try Self.request(
            importID: checkpointID, mode: mode, manifest: manifest, chunkOrdinal: chunkOrdinal
        )
        let response = try await client.resolve(request)
        try check(response)
        return response
    }

    /// A resolver that changes its normalizer or walks its revision backwards
    /// is not one this import can continue against.
    private func check(_ response: ImportResolveResponse) throws {
        guard response.normalizationVersion == Self.normalizationVersion else {
            throw ImportCoordinatorError.normalizationVersionChanged(
                expected: Self.normalizationVersion, actual: response.normalizationVersion
            )
        }
        if let known = resolverRevision, response.resolverRevision < known {
            throw ImportCoordinatorError.resolverRevisionRegressed(known: known, received: response.resolverRevision)
        }
        resolverRevision = response.resolverRevision
    }

    static func request(
        importID: UUID,
        mode: ImportMode,
        manifest: FrozenImportManifest,
        chunkOrdinal: Int
    ) throws -> ImportResolveRequest {
        let babies = try manifest.babies()
        let records = try manifest.records()
        return ImportResolveRequest(
            protocolVersion: "v1",
            capabilities: capabilities,
            importID: importID,
            importMode: mode.rawValue,
            normalizationVersion: normalizationVersion,
            babies: babies,
            records: records,
            totalRecordCount: Int64(records.count),
            chunkIndex: Int64(chunkOrdinal),
            finalize: chunkOrdinal == manifest.chunks.count - 1
        )
    }
}
