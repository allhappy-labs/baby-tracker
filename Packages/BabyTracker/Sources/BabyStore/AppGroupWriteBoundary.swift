import Foundation
import SwiftData

@MainActor
public final class AppGroupWriteBoundary {
    private unowned let store: BabyStore
    public let lockURL: URL

    init(store: BabyStore, lockURL: URL) { self.store = store; self.lockURL = lockURL }

    /// The single durable row describing the active realm and transition phase.
    ///
    /// Created through a *separate* `ModelContext` when it is missing. Saving
    /// the store's own context here would commit whatever the caller already
    /// has pending — and since this runs before every guarded write, that would
    /// make the boundary's rollback unable to undo the caller's insert.
    func state() throws -> RealmTransitionCheckpoint {
        if let existing = try store.context.fetch(FetchDescriptor<RealmTransitionCheckpoint>()).first { return existing }
        let bootstrap = ModelContext(store.container)
        if try bootstrap.fetch(FetchDescriptor<RealmTransitionCheckpoint>()).first == nil {
            bootstrap.insert(RealmTransitionCheckpoint(activeRealm: .local))
            try bootstrap.save()
        }
        guard let created = try store.context.fetch(FetchDescriptor<RealmTransitionCheckpoint>()).first else {
            throw RealmWriteError.retryAfterTransition
        }
        return created
    }

    public func capture() throws -> RealmWriteContext {
        try AppGroupBootstrapLock.withLock(at: lockURL) {
            let value = try state()
            return RealmWriteContext(realm: RealmID(encodedKey: value.activeRealmKey) ?? .local, transitionGeneration: value.generation)
        }
    }

    /// Guarded writes, including the read-only role check.
    ///
    /// The role is rechecked here, under the same lock that already guards the
    /// realm and generation, rather than at the call site. A check in a view or
    /// view model races a demotion landing mid-write, and is invisible to the
    /// widget and App Intent processes that reach the store directly.
    ///
    /// `allowsReadOnly` is for the writes that are not the user originating
    /// family data — presentation-only state goes through this boundary too,
    /// and gating that would lock a viewer out of their own device.
    public func commit<T>(
        expected: RealmWriteContext,
        allowsReadOnly: Bool = false,
        _ body: () throws -> T
    ) throws -> T {
        let (result, phase, committedAt, publishedAt) = try AppGroupBootstrapLock.withLock(at: lockURL) {
            let state = try state()
            guard state.phaseRaw == "idle",
                  state.generation == expected.transitionGeneration,
                  state.activeRealmKey == expected.realm.encodedKey else { throw RealmWriteError.retryAfterTransition }
            if !allowsReadOnly, try !store.writePermission(in: expected.realm).canWrite {
                throw WritePermissionError.readOnlyCaregiver
            }
            let scope = StoreSyncWriteScope()
            let previousScope = store.syncWriteScope
            store.syncWriteScope = scope
            defer { store.syncWriteScope = previousScope }
            do {
                let result = try body()
                let phase: StoreSyncMilestone.Phase? = scope.appliedRemoteData
                    ? .remoteCommit : scope.originatedCareWrite ? .localCommit : nil
                try store.context.save()
                let committedAt = DispatchTime.now().uptimeNanoseconds
                store.didWrite()
                let publishedAt = DispatchTime.now().uptimeNanoseconds
                return (result, phase, committedAt, publishedAt)
            } catch {
                store.context.rollback()
                throw error
            }
        }
        store.publishSyncMilestone(phase, committedAt: committedAt, publishedAt: publishedAt)
        return result
    }
}

public extension BabyStore {
    func captureWriteContext() throws -> RealmWriteContext { try writeBoundary.capture() }

    func withRealmWrite<T>(
        expected: RealmWriteContext,
        allowsReadOnly: Bool = false,
        _ body: () throws -> T
    ) throws -> T {
        try writeBoundary.commit(expected: expected, allowsReadOnly: allowsReadOnly, body)
    }

    func beginTransition(to realm: RealmID, checkpointID: UUID) throws {
        try AppGroupBootstrapLock.withLock(at: coordinationLockURL) {
            let state = try writeBoundary.state()
            guard state.phaseRaw == "idle" else { throw RealmWriteError.retryAfterTransition }
            state.generation &+= 1
            state.targetRealmKey = realm.encodedKey
            state.checkpointID = checkpointID
            state.phaseRaw = "transition"
            try context.save()
        }
    }

    func commitTransition(checkpointID: UUID) throws {
        try AppGroupBootstrapLock.withLock(at: coordinationLockURL) {
            let state = try writeBoundary.state()
            guard state.phaseRaw == "transition", state.checkpointID == checkpointID,
                  let targetKey = state.targetRealmKey else { throw RealmWriteError.retryAfterTransition }
            let sourceKey = state.activeRealmKey
            for row in try context.fetch(FetchDescriptor<Baby>(predicate: #Predicate { $0.realmKey == sourceKey })) { row.realmKey = targetKey }
            for row in try context.fetch(FetchDescriptor<FeedEntry>(predicate: #Predicate { $0.realmKey == sourceKey })) { row.realmKey = targetKey }
            for row in try context.fetch(FetchDescriptor<SleepEntry>(predicate: #Predicate { $0.realmKey == sourceKey })) { row.realmKey = targetKey }
            for row in try context.fetch(FetchDescriptor<DiaperEntry>(predicate: #Predicate { $0.realmKey == sourceKey })) { row.realmKey = targetKey }
            for row in try context.fetch(FetchDescriptor<SupplementEntry>(predicate: #Predicate { $0.realmKey == sourceKey })) { row.realmKey = targetKey }
            for row in try context.fetch(FetchDescriptor<ImmunizationEntry>(predicate: #Predicate { $0.realmKey == sourceKey })) { row.realmKey = targetKey }
            for row in try context.fetch(FetchDescriptor<PumpEntry>(predicate: #Predicate { $0.realmKey == sourceKey })) { row.realmKey = targetKey }
            for row in try context.fetch(FetchDescriptor<GrowthProfile>(predicate: #Predicate { $0.realmKey == sourceKey })) { row.realmKey = targetKey }
            for row in try context.fetch(FetchDescriptor<MeasurementCheckIn>(predicate: #Predicate { $0.realmKey == sourceKey })) { row.realmKey = targetKey }
            for row in try context.fetch(FetchDescriptor<ActiveTimer>(predicate: #Predicate { $0.realmKey == sourceKey })) { row.realmKey = targetKey }
            for row in try context.fetch(FetchDescriptor<ChangeLogEntry>(predicate: #Predicate { $0.realmKey == sourceKey })) { row.realmKey = targetKey }
            state.activeRealmKey = targetKey
            state.targetRealmKey = nil; state.checkpointID = nil; state.phaseRaw = "idle"; state.generation &+= 1
            try context.save(); didWrite()
        }
    }

    /// Points the active realm at `realm` without moving a single row.
    ///
    /// Distinct from `beginTransition`/`commitTransition` on purpose. A
    /// transition MOVES the source realm's rows into the target, which is
    /// exactly right for an import and exactly wrong for everything else: a
    /// device that already holds shared rows from sync simply needs to point
    /// at them, and a purge must not sweep leftovers into the local realm as a
    /// duplicate.
    func setActiveRealm(_ realm: RealmID) throws {
        try AppGroupBootstrapLock.withLock(at: coordinationLockURL) {
            let state = try writeBoundary.state()
            guard state.phaseRaw == "idle" else { throw RealmWriteError.retryAfterTransition }
            state.activeRealmKey = realm.encodedKey
            state.generation &+= 1
            try context.save()
            didWrite()
        }
    }

    func recoverIncompleteTransition() throws {
        try AppGroupBootstrapLock.withLock(at: coordinationLockURL) {
            let state = try writeBoundary.state()
            guard state.phaseRaw != "idle" else { return }
            state.targetRealmKey = nil; state.checkpointID = nil; state.phaseRaw = "idle"; state.generation &+= 1
            try context.save()
        }
    }
}

extension BabyStore: RealmStore {}
