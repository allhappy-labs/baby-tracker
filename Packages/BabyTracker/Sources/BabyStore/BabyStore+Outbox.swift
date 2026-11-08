import BabyCore
import BabyProtocol
import Foundation
import SwiftData

/// What a dispatched mutation looks like on disk. Read *after* the `Void`
/// `markDispatched` command, so a test asserts against persisted state rather
/// than against something the command handed back.
public struct PersistedDispatch: Sendable, Equatable {
    public let mutationID: UUID
    public let frozenPayload: Data
    public let payloadHash: Data
    public let deliveryState: MutationDeliveryState
}

public extension BabyStore {
    /// True when writes in the active realm must also produce protocol
    /// mutations. Local-only tracking stays immediate and outbox-free.
    var realmIsShared: Bool {
        if case .shared = activeRealm { return true }
        return false
    }

    func outboxEntry(_ mutationID: UUID) throws -> SyncOutboxEntry {
        guard let row = try context.fetch(
            FetchDescriptor<SyncOutboxEntry>(predicate: #Predicate { $0.id == mutationID })
        ).first else { throw SyncPersistenceError.unknownMutation(mutationID) }
        return row
    }

    func persistedDispatch(mutationID: UUID) throws -> PersistedDispatch {
        let row = try outboxEntry(mutationID)
        return PersistedDispatch(
            mutationID: row.id,
            frozenPayload: row.payload,
            payloadHash: row.payloadHash,
            deliveryState: row.deliveryState
        )
    }

    /// Submission-ordered mutations that may go on the wire right now.
    ///
    /// In-flight rows are included: delivery is at-least-once, and a mutation
    /// whose reply was lost must be retried under its original ID. Rows whose
    /// dependency has not been applied are held back — that is what stops a
    /// stop command acting on a timer the server may never have started.
    ///
    /// A row this build can no longer decode — a timer command an older build
    /// queued (`timer_switch_side`, `timer_pause`, …), or a mutation in a
    /// payload shape the server no longer accepts — is quarantined with
    /// everything waiting on it, rather than failing every sync cycle.
    func nextDispatchBatch(limit: Int) throws -> [WireMutation] {
        var batch: [WireMutation] = []
        var undecodable: [SyncOutboxEntry] = []
        for row in try liveOutboxRows() where !row.deliveryState.isTerminal {
            if let dependency = row.dependencyMutationID {
                let parent = try? outboxEntry(dependency)
                guard parent == nil || parent?.deliveryState == .acknowledged else { continue }
            }
            do {
                batch.append(try row.dispatchableMutation())
            } catch {
                undecodable.append(row)
                continue
            }
            if batch.count == limit { break }
        }
        if !undecodable.isEmpty { quarantineUndecodable(undecodable) }
        return batch
    }

    /// Takes rows that will never decode out of circulation, with every row
    /// that depends on them, directly or not. A failed write is only logged:
    /// the rows stay put and the next cycle tries again, and the batch already
    /// built is still good.
    private func quarantineUndecodable(_ rows: [SyncOutboxEntry]) {
        do {
            let expected = try captureWriteContext()
            try withRealmWrite(expected: expected, allowsReadOnly: true) {
                let live = try self.liveOutboxRows()
                var quarantined = Set(rows.map(\.id))
                var frontier = quarantined
                while !frontier.isEmpty {
                    let next = live.filter {
                        guard let dependency = $0.dependencyMutationID else { return false }
                        return frontier.contains(dependency) && !quarantined.contains($0.id)
                    }.map(\.id)
                    quarantined.formUnion(next)
                    frontier = Set(next)
                }
                for row in live where quarantined.contains(row.id) && !row.deliveryState.isTerminal {
                    row.deliveryState = .quarantined
                }
            }
            AppLog.sync.notice("quarantined undecodable outbox rows: \(rows.map(\.operationRaw).joined(separator: ","), privacy: .public)")
        } catch {
            AppLog.sync.error("could not quarantine undecodable outbox rows: \(String(describing: error), privacy: .public)")
        }
    }

    /// Freezes each named row's bytes and hash and moves it to uncertain. A row
    /// already past `neverDispatched` is left exactly as it is.
    func markDispatched(mutationIDs: [UUID]) throws {
        let expected = try captureWriteContext()
        try withRealmWrite(expected: expected) {
            for id in mutationIDs {
                let entry = try self.outboxEntry(id)
                guard entry.deliveryState == .neverDispatched else { continue }
                entry.freezePayloadHashIfNeeded()
                entry.deliveryState = .inFlightOrUncertain
            }
        }
    }

    /// Appends one already-built mutation plus its overlay position.
    func enqueueIntent(_ mutation: WireMutation, overlay: EntityOverlay) throws {
        let expected = try captureWriteContext()
        try withRealmWrite(expected: expected) {
            _ = try self.appendOutboxRow(
                mutation,
                dependencyMutationID: overlay.orderedMutationIDs.last
            )
            self.syncWriteScope?.originatedCareWrite = true
        }
    }

    func liveOutboxRows(in realm: RealmID? = nil) throws -> [SyncOutboxEntry] {
        let realmKey = (realm ?? activeRealm).encodedKey
        var descriptor = FetchDescriptor<SyncOutboxEntry>(predicate: #Predicate { $0.realmKey == realmKey })
        descriptor.sortBy = [SortDescriptor(\.sequence, order: .forward)]
        return try context.fetch(descriptor)
    }
}

extension BabyStore {
    /// Inserts an outbox row. Must be called inside an open write boundary so
    /// the entity and its mutation land in one transaction.
    @discardableResult
    func appendOutboxRow(_ mutation: WireMutation, dependencyMutationID: UUID? = nil) throws -> SyncOutboxEntry {
        let row = SyncOutboxEntry(
            id: mutation.mutationID,
            realm: activeRealm,
            entityID: mutation.entityID,
            entityKind: mutation.entityKind,
            operation: mutation.operation,
            payload: try JSONEncoder().encode(mutation),
            sequence: try nextOutboxSequence(),
            createdAt: now,
            dependencyMutationID: dependencyMutationID
        )
        context.insert(row)
        return row
    }

    func nextOutboxSequence() throws -> Int64 {
        var descriptor = FetchDescriptor<SyncOutboxEntry>()
        descriptor.sortBy = [SortDescriptor(\.sequence, order: .reverse)]
        descriptor.fetchLimit = 1
        return (try context.fetch(descriptor).first?.sequence ?? 0) &+ 1
    }

    /// Records the protocol mutation for a change-logged entity write. Called
    /// from inside `record`'s transaction, so it shares its rollback.
    func appendEntityMutation(_ operation: ChangeOperation, on entity: some ChangeLoggable) throws {
        guard realmIsShared, let kind = MutationFactory.entityKind(forTypeName: entity.entityTypeName) else { return }
        // A feed with nothing the server would accept stays on the phone
        // rather than being sent, refused and quarantined.
        if operation != .delete, let feed = entity as? FeedEntry, !feed.hasWireContent {
            AppLog.sync.notice("kept a feed with no content out of the outbox")
            return
        }
        let mutation = try MutationFactory.mutation(
            entityID: entity.entityID,
            entityKind: kind,
            operation: MutationFactory.operation(for: operation),
            observedEntityVersion: try observedVersion(of: entity.entityID, kind: kind),
            at: now,
            payload: operation == .delete ? Self.deletePayload(for: entity) : entity.payloadFields()
        )
        try appendOutboxRow(mutation)
    }

    /// The server's delete payload: `{}` for a baby, `{"baby_id"}` for a
    /// record. It decodes with unknown fields disallowed, so sending the
    /// entity's full state with a delete would get the delete refused.
    static func deletePayload(for entity: some ChangeLoggable) -> [String: Any] {
        guard let record = entity as? any BabyOwnedRecord else { return [:] }
        return ["baby_id": record.babyID.uuidString.lowercased()]
    }

    /// The canonical version this client last saw, or 0 for an entity the
    /// server has never acknowledged.
    func observedVersion(of entityID: UUID, kind: WireEntityKind) throws -> Int64 {
        let storageKey = "\(activeRealm.encodedKey)|\(kind.rawValue)|\(entityID.uuidString)"
        return try context.fetch(
            FetchDescriptor<CanonicalEntityBase>(predicate: #Predicate { $0.storageKey == storageKey })
        ).first?.entityVersion ?? 0
    }
}

public extension BabyStore {
    /// Removes every row belonging to one shared realm: records, bases,
    /// outbox, cursor and attribution.
    ///
    /// D10 is explicit that this never recreates a local duplicate. The
    /// permanent local realm is a separate realm and is not touched here, so a
    /// user who disconnects keeps exactly what they tracked before they joined
    /// — and nothing the family shared afterwards. The confirmation copy is
    /// what points an owner at the server export flow.
    func purgeSharedRealm(_ realm: RealmID) throws {
        guard case .shared = realm else { return }
        let realmKey = realm.encodedKey
        try deleteRows(realmKey: realmKey)
        try context.save()
        if activeRealm == realm {
            // `setActiveRealm`, NOT a transition. A transition moves the source
            // realm's surviving rows into the target, so a single row that
            // escaped the delete above would be swept into the local realm and
            // reappear as the user's own — the local duplicate D10 forbids.
            try setActiveRealm(.local)
        }
        didWrite()
    }

    private func deleteRows(realmKey: String) throws {
        for row in try context.fetch(FetchDescriptor<Baby>(predicate: #Predicate { $0.realmKey == realmKey })) { context.delete(row) }
        for row in try context.fetch(FetchDescriptor<FeedEntry>(predicate: #Predicate { $0.realmKey == realmKey })) { context.delete(row) }
        for row in try context.fetch(FetchDescriptor<SleepEntry>(predicate: #Predicate { $0.realmKey == realmKey })) { context.delete(row) }
        for row in try context.fetch(FetchDescriptor<DiaperEntry>(predicate: #Predicate { $0.realmKey == realmKey })) { context.delete(row) }
        for row in try context.fetch(FetchDescriptor<SupplementEntry>(predicate: #Predicate { $0.realmKey == realmKey })) { context.delete(row) }
        for row in try context.fetch(FetchDescriptor<ImmunizationEntry>(predicate: #Predicate { $0.realmKey == realmKey })) { context.delete(row) }
        for row in try context.fetch(FetchDescriptor<PumpEntry>(predicate: #Predicate { $0.realmKey == realmKey })) { context.delete(row) }
        for row in try context.fetch(FetchDescriptor<GrowthProfile>(predicate: #Predicate { $0.realmKey == realmKey })) { context.delete(row) }
        for row in try context.fetch(FetchDescriptor<MeasurementCheckIn>(predicate: #Predicate { $0.realmKey == realmKey })) { context.delete(row) }
        for row in try context.fetch(FetchDescriptor<ActiveTimer>(predicate: #Predicate { $0.realmKey == realmKey })) { context.delete(row) }
        for row in try context.fetch(FetchDescriptor<ChangeLogEntry>(predicate: #Predicate { $0.realmKey == realmKey })) { context.delete(row) }
        for row in try context.fetch(FetchDescriptor<SyncOutboxEntry>(predicate: #Predicate { $0.realmKey == realmKey })) { context.delete(row) }
        for row in try context.fetch(FetchDescriptor<CanonicalEntityBase>(predicate: #Predicate { $0.realmKey == realmKey })) { context.delete(row) }
        for row in try context.fetch(FetchDescriptor<SyncCursorState>(predicate: #Predicate { $0.realmKey == realmKey })) { context.delete(row) }
        for row in try context.fetch(FetchDescriptor<CaregiverAttribution>(predicate: #Predicate { $0.realmKey == realmKey })) { context.delete(row) }
    }
}
