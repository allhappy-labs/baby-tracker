import BabyCore
import BabyProtocol
import Foundation
import SwiftData

/// The durable import state machine.
///
/// Every phase boundary is a place the process can die, so each one is written
/// before the work it describes and the whole rewrite happens in one
/// transaction under the realm's fenced lease. The two failure modes this
/// exists to prevent are losing the user's local data and duplicating it into
/// the family, and both come from a half-finished move.
@MainActor
public protocol ImportStore {
    func freezeImportManifest(checkpointID: UUID) throws -> FrozenImportManifest
    func appendPostImportIntent(_ mutation: WireMutation, checkpointID: UUID) throws
    func saveMapping(_ mapping: [UUID: UUID], checkpointID: UUID) throws
    func rewriteAndMoveWorkspace(checkpointID: UUID, to realm: RealmID) throws
    func resumeImport(checkpointID: UUID) throws -> ImportCheckpointState
}

public extension BabyStore {
    /// Starts an import, fixing its mode. Re-starting an existing one returns
    /// what is already there rather than restarting it.
    @discardableResult
    func beginImport(checkpointID: UUID, mode: ImportMode) throws -> ImportCheckpointState {
        if let existing = try? resumeImport(checkpointID: checkpointID) {
            try ImportStoreValidator.validate(mode: mode, against: existing.importMode)
            return existing
        }
        let state = ImportCheckpointState(checkpointID: checkpointID, phase: .membershipReady, importMode: mode)
        try writeCheckpoint(state)
        return state
    }

    func resumeImport(checkpointID: UUID) throws -> ImportCheckpointState {
        guard let row = try checkpointRow(Self.stateKey(checkpointID)) else {
            throw ImportStoreError.missingCheckpoint(checkpointID)
        }
        return try JSONDecoder().decode(ImportCheckpointState.self, from: row.payload)
    }

    /// Captures every live local entity exactly as it is now.
    ///
    /// One transaction, so a create, delete or timer switch made a moment
    /// before is either wholly in the manifest or wholly outside it. After
    /// this returns, the bytes never change: a retried chunk resends these,
    /// not a rebuild from rows the user may since have edited or deleted.
    @discardableResult
    func freezeImportManifest(checkpointID: UUID) throws -> FrozenImportManifest {
        var state = try resumeImport(checkpointID: checkpointID)
        if state.phase >= .manifestFrozen, let existing = try frozenManifest(checkpointID: checkpointID) {
            return existing
        }

        var bases: [EntityKey: Data] = [:]
        var entries: [ManifestEntry] = []
        var ordinal = 0

        func capture(_ kind: WireEntityKind, _ entityID: UUID, _ fields: [String: Any]) throws {
            let payload = try JSONSerialization.data(withJSONObject: fields, options: [.sortedKeys])
            bases[EntityKey(kind: kind.rawValue, id: entityID)] = payload
            entries.append(ManifestEntry(ordinal: ordinal, kind: kind.rawValue, entityID: entityID, payload: payload))
            ordinal += 1
        }

        // Tombstoned rows are excluded. A record the user deleted before
        // joining is not something to carry into the family, and importing it
        // so the server can immediately be told to delete it again would show
        // the other caregivers a record that briefly existed and then vanished.
        for baby in try babies(in: .local) { try capture(.baby, baby.id, baby.payloadFields()) }
        // A feed with nothing the server would accept is left out: the
        // resolver would be promised a record the server then refuses, and
        // the import could never complete. It still moves into the family
        // realm with everything else; it is just never sent.
        var withoutContent = 0
        for feed in try localRows(FeedEntry.self) where !feed.isTombstoned {
            guard feed.hasWireContent else { withoutContent += 1; continue }
            try capture(.feed, feed.id, feed.payloadFields())
        }
        if withoutContent > 0 {
            AppLog.sync.notice("left \(withoutContent, privacy: .public) feeds with no content out of the import")
        }
        for sleep in try localRows(SleepEntry.self) where !sleep.isTombstoned {
            try capture(.sleep, sleep.id, sleep.payloadFields())
        }
        for supplement in try localRows(SupplementEntry.self) where !supplement.isTombstoned {
            try capture(.supplement, supplement.id, supplement.payloadFields())
        }
        for shot in try localRows(ImmunizationEntry.self) where !shot.isTombstoned {
            try capture(.immunization, shot.id, shot.payloadFields())
        }
        let liveBabyIDs = Set(try babies(in: .local).map(\.id))
        for profile in try localRows(GrowthProfile.self) where !profile.isTombstoned && liveBabyIDs.contains(profile.babyID) {
            try capture(.growthProfile, profile.id, profile.payloadFields())
        }
        for measurement in try localRows(MeasurementCheckIn.self) where !measurement.isTombstoned && liveBabyIDs.contains(measurement.babyID) {
            try capture(.measurement, measurement.id, measurement.payloadFields())
        }
        for diaper in try localRows(DiaperEntry.self) where !diaper.isTombstoned {
            try capture(.diaper, diaper.id, diaper.payloadFields())
        }
        // A running timer enters as its start state, carrying whatever side and
        // bottle it has accumulated. A completed timer is already a feed or
        // sleep record and is captured as one above.
        for timer in try localRows(ActiveTimer.self) {
            try capture(.timer, timer.id, MutationFactory.timerPayload(timer))
        }

        let chunks = try Self.chunked(entries)
        let manifest = FrozenImportManifest(
            sourceRevision: Int64(revision),
            chunks: chunks,
            sourceBases: bases
        )
        try writeCheckpoint(manifest, key: Self.manifestKey(checkpointID))
        state.phase = .manifestFrozen
        try writeCheckpoint(state)
        return manifest
    }

    func frozenManifest(checkpointID: UUID) throws -> FrozenImportManifest? {
        guard let row = try checkpointRow(Self.manifestKey(checkpointID)) else { return nil }
        return try JSONDecoder().decode(FrozenImportManifest.self, from: row.payload)
    }

    /// A write made after the freeze. It is immediately visible locally and is
    /// queued to be sent *after* the imported baseline, so the family sees the
    /// import and then the edit, in that order.
    ///
    /// It never touches the frozen payload or hash.
    func appendPostImportIntent(_ mutation: WireMutation, checkpointID: UUID) throws {
        var intents = try postImportIntents(checkpointID: checkpointID)
        guard !intents.contains(where: { $0.mutationID == mutation.mutationID }) else { return }
        intents.append(mutation)
        try writeCheckpoint(intents, key: Self.intentsKey(checkpointID))
    }

    func postImportIntents(checkpointID: UUID) throws -> [WireMutation] {
        guard let row = try checkpointRow(Self.intentsKey(checkpointID)) else { return [] }
        return try JSONDecoder().decode([WireMutation].self, from: row.payload)
    }

    /// Persists the complete baby mapping before any reference changes.
    ///
    /// Rejected unless injective: two local babies collapsing onto one shared
    /// baby would merge two children's records, and the rewrite is a single
    /// irreversible transaction.
    func saveMapping(_ mapping: [UUID: UUID], checkpointID: UUID) throws {
        try ImportStoreValidator.validate(mapping)
        var state = try resumeImport(checkpointID: checkpointID)
        state.completeBabyIDMap = mapping
        state.phase = .mappingDurable
        try writeCheckpoint(state)
    }

    /// Moves the local workspace into the shared realm.
    ///
    /// Held under the source realm's fenced lease and committed in one
    /// transaction, so no other process — app, widget or App Intent — can
    /// observe a half-moved workspace. A writer that captured its context
    /// before this began is rejected with `retryAfterTransition` and retries
    /// against the new realm.
    func rewriteAndMoveWorkspace(checkpointID: UUID, to realm: RealmID) throws {
        var state = try resumeImport(checkpointID: checkpointID)
        guard state.phase >= .mappingDurable else {
            throw ImportStoreError.wrongPhase(expected: .mappingDurable, actual: state.phase)
        }
        guard let manifest = try frozenManifest(checkpointID: checkpointID) else {
            throw ImportStoreError.missingFrozenManifest(checkpointID)
        }
        let intents = try postImportIntents(checkpointID: checkpointID)

        let owner = UUID()
        guard let token = try leaseStore.acquire(scope: .realm(activeRealm), ownerID: owner, now: now, ttl: 30) else {
            throw RealmWriteError.retryAfterTransition
        }
        defer { try? leaseStore.cancel(token) }

        let transitionCheckpoint = UUID()
        try beginTransition(to: realm, checkpointID: transitionCheckpoint)
        do {
            try leaseStore.withFencedCommit(token, now: now) {
                try self.remapBabiesAndDependents(state.completeBabyIDMap)
                try self.enqueueImportMutations(from: manifest, realm: realm, importID: checkpointID, babyIDMap: state.completeBabyIDMap)
                try self.enqueueLocalPumps(realm: realm)
                try self.enqueuePostImportIntents(intents, realm: realm, babyIDMap: state.completeBabyIDMap)
                try self.context.save()
            }
        } catch {
            context.rollback()
            try? recoverIncompleteTransition()
            throw error
        }
        // The row move and the realm switch land together: the same commit
        // that exposes the shared realm is the one that puts the rows in it.
        try commitTransition(checkpointID: transitionCheckpoint)

        state.phase = .sharedRealmExposed
        state.targetRealmKey = realm.encodedKey
        try writeCheckpoint(state)
    }

    func markImportDraining(checkpointID: UUID) throws {
        var state = try resumeImport(checkpointID: checkpointID)
        state.phase = .draining
        try writeCheckpoint(state)
    }

    func markImportComplete(checkpointID: UUID) throws {
        var state = try resumeImport(checkpointID: checkpointID)
        state.phase = .complete
        try writeCheckpoint(state)
    }

    /// This store's own part of an import's name: made on first use and kept
    /// beside the import checkpoints, so it lives and dies with them.
    ///
    /// A store moved aside, or lost to a reinstall, takes it along, and the
    /// fresh store that replaces it makes a new one. Its import is then its
    /// own: the server keeps every import ID it has seen and refuses a
    /// different first chunk under one an earlier store already finished.
    func importStoreID() throws -> UUID {
        if let row = try checkpointRow(Self.importStoreIDKey),
           let id = try? JSONDecoder().decode([String: UUID].self, from: row.payload)["id"] {
            return id
        }
        let id = UUID()
        try writeCheckpoint(["id": id], key: Self.importStoreIDKey)
        return id
    }
}

extension BabyStore {
    static func stateKey(_ id: UUID) -> String { "import-state|\(id.uuidString.lowercased())" }
    static func manifestKey(_ id: UUID) -> String { "import-manifest|\(id.uuidString.lowercased())" }
    static func intentsKey(_ id: UUID) -> String { "import-intents|\(id.uuidString.lowercased())" }
    static let importStoreIDKey = "import-store-id"

    /// The lease this store uses for its own exclusive phases.
    var leaseStore: AppGroupFencedLease {
        AppGroupFencedLease(container: container, lockURL: coordinationLockURL)
    }

    func checkpointRow(_ key: String) throws -> ImportCheckpoint? {
        try context.fetch(FetchDescriptor<ImportCheckpoint>(predicate: #Predicate { $0.key == key })).first
    }

    func writeCheckpoint(_ state: ImportCheckpointState) throws {
        try writeCheckpoint(state, key: Self.stateKey(state.checkpointID))
    }

    func writeCheckpoint(_ value: some Encodable, key: String) throws {
        let payload = try JSONEncoder().encode(value)
        if let existing = try checkpointRow(key) {
            existing.payload = payload
        } else {
            context.insert(ImportCheckpoint(key: key, realm: .local, payload: payload))
        }
        try context.save()
    }

    func localRows<T: PersistentModel>(_ type: T.Type) throws -> [T] {
        let key = RealmID.local.encodedKey
        return try context.fetch(FetchDescriptor<T>()).filter { model in
            (model as? any RealmScoped)?.realmKey == key
        }
    }

    /// Matched babies adopt the server's canonical UUID; everything else keeps
    /// the ID it already had, including every record.
    func remapBabiesAndDependents(_ mapping: [UUID: UUID]) throws {
        guard !mapping.isEmpty else { return }
        for baby in try babies(in: .local) {
            guard let canonical = mapping[baby.id], canonical != baby.id else { continue }
            baby.id = canonical
        }
        for row in try localRows(FeedEntry.self) {
            if let canonical = mapping[row.babyID] { row.babyID = canonical }
        }
        for row in try localRows(SleepEntry.self) {
            if let canonical = mapping[row.babyID] { row.babyID = canonical }
        }
        for row in try localRows(SupplementEntry.self) {
            if let canonical = mapping[row.babyID] { row.babyID = canonical }
        }
        for row in try localRows(ImmunizationEntry.self) {
            if let canonical = mapping[row.babyID] { row.babyID = canonical }
        }
        for row in try localRows(GrowthProfile.self) {
            if let canonical = mapping[row.babyID] {
                row.babyID = canonical
                row.id = GrowthProfileID.forBaby(canonical)
            }
        }
        for row in try localRows(MeasurementCheckIn.self) {
            if let canonical = mapping[row.babyID] { row.babyID = canonical }
        }
        for row in try localRows(DiaperEntry.self) {
            if let canonical = mapping[row.babyID] { row.babyID = canonical }
        }
        for row in try localRows(ActiveTimer.self) {
            if let canonical = mapping[row.babyID] { row.babyID = canonical }
        }
    }

    /// The imported baseline goes on the wire first, tagged with the import ID
    /// so the server can treat an identical existing record as a no-op rather
    /// than a conflict.
    ///
    /// Babies are not queued. The server's resolver publishes every mapped
    /// baby itself when the mapping completes, and refuses an import-tagged
    /// baby create outright (`invalid_import_operation` in the importer's
    /// `AdmitRecord`) — sending one would put a mutation in the outbox that
    /// can only ever be rejected.
    ///
    /// Each record's `baby_id` is rewritten to the baby the resolver mapped it
    /// to. The frozen bytes still name the local baby, and the server admits
    /// an imported record only when its parent is the mapped shared baby. The
    /// server's content hash leaves `baby_id` out, so the rewrite does not
    /// change what the manifest promised.
    func enqueueImportMutations(
        from manifest: FrozenImportManifest,
        realm: RealmID,
        importID: UUID,
        babyIDMap: [UUID: UUID]
    ) throws {
        for chunk in manifest.chunks {
            for entry in try JSONDecoder().decode([ManifestEntry].self, from: chunk.manifestPayload) {
                guard let kind = WireEntityKind(rawValue: entry.kind), kind != .baby else { continue }
                var payload = try JSONDecoder().decode(JSONValue.self, from: entry.payload)
                var entityID = entry.entityID
                if case var .object(fields) = payload,
                   case let .string(local) = fields["baby_id"],
                   let localID = UUID(uuidString: local),
                   let shared = babyIDMap[localID] {
                    fields["baby_id"] = .string(shared.uuidString.lowercased())
                    payload = .object(fields)
                    if kind == .growthProfile { entityID = GrowthProfileID.forBaby(shared) }
                }
                let mutation = WireMutation(
                    mutationID: UUID(),
                    entityID: entityID,
                    entityKind: kind,
                    operation: kind == .timer ? .timerStart : .create,
                    // The kind's current version: every frozen base is in
                    // its kind's current shape.
                    payloadVersion: kind.currentPayloadVersion,
                    observedEntityVersion: 0,
                    clientMutationTime: MutationFactory.wireTime(now),
                    importID: importID,
                    payload: payload
                )
                try appendOutboxRow(mutation, in: realm)
            }
        }
    }

    /// Then the edits made while the import was in flight, in the order the
    /// user made them.
    func enqueuePostImportIntents(_ intents: [WireMutation], realm: RealmID, babyIDMap: [UUID: UUID] = [:]) throws {
        for mutation in intents {
            var payload = mutation.payload
            var entityID = mutation.entityID
            if case var .object(fields) = payload,
               case let .string(parent) = fields["baby_id"], let local = UUID(uuidString: parent),
               let canonical = babyIDMap[local] {
                fields["baby_id"] = .string(canonical.uuidString.lowercased())
                payload = .object(fields)
                if mutation.entityKind == .growthProfile { entityID = GrowthProfileID.forBaby(canonical) }
            }
            if mutation.entityKind == .baby { entityID = babyIDMap[entityID] ?? entityID }
            let mapped = WireMutation(mutationID: mutation.mutationID, entityID: entityID, entityKind: mutation.entityKind,
                operation: mutation.operation, payloadVersion: mutation.payloadVersion,
                observedEntityVersion: mutation.observedEntityVersion, clientMutationTime: mutation.clientMutationTime,
                importID: mutation.importID, payload: payload)
            try appendOutboxRow(mapped, in: realm)
        }
    }

    /// Pumps belong to the family, not a baby, so the import resolver, which
    /// maps every record to a baby, never sees them: they are not in the
    /// frozen manifest. Each live one, as it stands at the move, goes as a
    /// plain create once the workspace is in the family realm.
    func enqueueLocalPumps(realm: RealmID) throws {
        for pump in try localRows(PumpEntry.self) where !pump.isTombstoned {
            let mutation = try MutationFactory.mutation(entityID: pump.id, entityKind: .pump, operation: .create,
                                                        observedEntityVersion: 0, at: now, payload: pump.payloadFields())
            try appendOutboxRow(mutation, in: realm)
        }
    }

    @discardableResult
    func appendOutboxRow(_ mutation: WireMutation, in realm: RealmID) throws -> SyncOutboxEntry {
        let row = SyncOutboxEntry(
            id: mutation.mutationID,
            realm: realm,
            entityID: mutation.entityID,
            entityKind: mutation.entityKind,
            operation: mutation.operation,
            payload: try JSONEncoder().encode(mutation),
            sequence: try nextOutboxSequence(),
            createdAt: now
        )
        context.insert(row)
        return row
    }

    /// Chunked so a single oversized request cannot be the reason an import
    /// never completes.
    static func chunked(_ entries: [ManifestEntry], perChunk: Int = 100) throws -> [FrozenImportManifestChunk] {
        guard !entries.isEmpty else {
            let payload = try JSONEncoder().encode([ManifestEntry]())
            return [FrozenImportManifestChunk(ordinal: 0, manifestPayload: payload, manifestHash: ManifestHash.of(payload))]
        }
        var chunks: [FrozenImportManifestChunk] = []
        for (ordinal, slice) in stride(from: 0, to: entries.count, by: perChunk).enumerated() {
            let page = Array(entries[slice..<min(slice + perChunk, entries.count)])
            let payload = try JSONEncoder().encode(page)
            chunks.append(FrozenImportManifestChunk(ordinal: ordinal, manifestPayload: payload, manifestHash: ManifestHash.of(payload)))
        }
        return chunks
    }
}

/// One entity as it stood at freeze.
struct ManifestEntry: Codable, Sendable, Equatable {
    let ordinal: Int
    let kind: String
    let entityID: UUID
    let payload: Data
}

/// The realm-scoped models, so a generic local fetch can filter them.
protocol RealmScoped { var realmKey: String { get } }
extension Baby: RealmScoped {}
extension FeedEntry: RealmScoped {}
extension SleepEntry: RealmScoped {}
extension DiaperEntry: RealmScoped {}
extension ActiveTimer: RealmScoped {}

extension BabyStore: ImportStore {}
