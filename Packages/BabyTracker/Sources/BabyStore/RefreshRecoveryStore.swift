import Foundation
import SwiftData

public extension BabyStore {
    func refreshRecoveryCheckpoint(for key: AccountLeaseKey) throws -> RefreshRecoveryCheckpoint? {
        try AppGroupBootstrapLock.withLock(at: coordinationLockURL) {
            let context = ModelContext(container)
            guard let row = try recoveryAccount(key, in: context) else { return nil }
            return try decodedCheckpoint(row, for: key)
        }
    }

    /// A completed Keychain binding remains meaningful after its ACK metadata
    /// is removed, only while the durable account still names that version.
    func verifyInstalledRefresh(_ binding: RefreshInstallBinding, for key: AccountLeaseKey, verifyCredentials: () throws -> Void) throws {
        try AppGroupBootstrapLock.withLock(at: coordinationLockURL) {
            let context = ModelContext(container)
            guard binding.isValid, binding.key == key,
                  let row = try recoveryAccount(key, in: context), row.allowsOrdinarySync,
                  row.credentialVersion == binding.expectedCredentialVersion else { throw RefreshRecoveryError.invalidBinding }
            if let realm = try context.fetch(FetchDescriptor<RealmTransitionCheckpoint>()).first {
                guard realm.phaseRaw == "idle", row.boundRealmKey == nil || row.boundRealmKey == realm.activeRealmKey else { throw FencedLeaseError.cancelled }
            } else if row.boundRealmKey != nil { throw FencedLeaseError.cancelled }
            let checkpoint = try decodedCheckpoint(row, for: key)
            if let checkpoint, checkpoint.phase == .installPrepared,
               checkpoint.installBinding == binding { throw RefreshRecoveryError.invalidBinding }
            try verifyCredentials()
        }
    }

    /// Snapshot for presentation guards across suspended recovery. Does not
    /// decode recovery data: even corrupt checkpoints need a bounded action.
    func accountCredentialVersion(for key: AccountLeaseKey) throws -> UUID? {
        try AppGroupBootstrapLock.withLock(at: coordinationLockURL) {
            try recoveryAccount(key, in: ModelContext(container))?.credentialVersion
        }
    }

    func accountCredentialIsCurrent(_ version: UUID, for key: AccountLeaseKey) throws -> Bool {
        try AppGroupBootstrapLock.withLock(at: coordinationLockURL) {
            let context = ModelContext(container)
            guard let row = try recoveryAccount(key, in: context) else { return false }
            return row.allowsOrdinarySync && row.credentialVersion == version
        }
    }

    func beginRecoverableRefresh(for key: AccountLeaseKey, recoveryEpoch: UInt64, credentialVersion: UUID, supersedingBinding: RefreshInstallBinding? = nil, using token: FencedLeaseToken, now: Date) throws -> RefreshRecoveryCheckpoint {
        try withRecoveryAccount(key, using: token, now: now) { row, context in
            let existing = try decodedCheckpoint(row, for: key)
            guard row.credentialVersion == credentialVersion else { throw FencedLeaseError.cancelled }
            var predecessor: AckRetry?
            if let existing, let supersedingBinding {
                guard existing.phase == .ackPending, existing.installBinding == supersedingBinding,
                      supersedingBinding.expectedCredentialVersion == credentialVersion,
                      supersedingBinding.recoveryEpoch == recoveryEpoch else { throw RefreshRecoveryError.invalidBinding }
                predecessor = existing.acknowledgement
            } else if let existing {
                guard existing.recoveryEpoch == recoveryEpoch,
                      existing.attempt.credentialVersion == credentialVersion else { throw RefreshRecoveryError.invalidBinding }
                return existing
            }
            guard supersedingBinding == nil || predecessor != nil else { throw RefreshRecoveryError.invalidBinding }
            guard try row.recoveryPayload().state == .idle else { throw FencedLeaseError.cancelled }
            let attempt = RefreshAttempt(attemptID: UUID(), credentialVersion: credentialVersion, ownerGeneration: token.generation, startedAt: now, protocolMode: .recoverableV1)
            let checkpoint = RefreshRecoveryCheckpoint(attempt: attempt, recoveryEpoch: recoveryEpoch, phase: .awaitingReply, predecessorAcknowledgement: predecessor)
            try persist(checkpoint, state: .inFlightUncertain(attempt), row: row, in: context)
            return checkpoint
        }
    }

    func prepareRefreshInstall(_ binding: RefreshInstallBinding, for key: AccountLeaseKey, using token: FencedLeaseToken, now: Date) throws {
        try withRecoveryAccount(key, using: token, now: now) { row, context in
            guard var checkpoint = try decodedCheckpoint(row, for: key) else { throw RefreshRecoveryError.invalidBinding }
            try validate(binding, checkpoint: checkpoint, key: key)
            guard row.credentialVersion == binding.parentCredentialVersion else { throw FencedLeaseError.cancelled }
            if let prepared = checkpoint.installBinding {
                guard prepared == binding else { throw RefreshRecoveryError.invalidBinding }
                return
            }
            guard checkpoint.phase == .awaitingReply else { throw RefreshRecoveryError.invalidBinding }
            checkpoint.phase = .installPrepared
            checkpoint.installBinding = binding
            try persist(checkpoint, state: .inFlightUncertain(checkpoint.attempt), row: row, in: context)
        }
    }

    /// The callback must save, or verify an already-saved, atomic credential
    /// value carrying this exact binding. It runs under the coordination lock
    /// only after the durable prepared record and current gates are checked.
    /// A callback or SQLite save failure retains the prepared record so the
    /// same logical installation can resume under a newly acquired fence.
    func completeRefreshInstall(_ binding: RefreshInstallBinding, for key: AccountLeaseKey, using token: FencedLeaseToken, now: Date, installCredentials: () throws -> Void) throws {
        try withRecoveryAccount(key, using: token, now: now) { row, context in
            guard var checkpoint = try decodedCheckpoint(row, for: key) else { throw RefreshRecoveryError.invalidBinding }
            try validate(binding, checkpoint: checkpoint, key: key)
            guard checkpoint.installBinding == binding else { throw RefreshRecoveryError.invalidBinding }
            guard row.credentialVersion == binding.parentCredentialVersion || row.credentialVersion == binding.expectedCredentialVersion else { throw FencedLeaseError.cancelled }
            if checkpoint.phase == .ackPending {
                guard row.credentialVersion == binding.expectedCredentialVersion else { throw FencedLeaseError.cancelled }
                return
            }
            guard checkpoint.phase == .installPrepared else { throw RefreshRecoveryError.invalidBinding }
            try installCredentials()
            row.credentialVersion = binding.expectedCredentialVersion
            checkpoint.predecessorAcknowledgement = nil
            checkpoint.phase = .ackPending
            checkpoint.acknowledgement = AckRetry(attempt: checkpoint.attempt, binding: binding, nextRetryAt: now, retryCount: 0)
            try persist(checkpoint, state: .idle, row: row, in: context)
        }
    }

    /// The observation is mandatory: a late result must not clear a successor.
    func updateRefreshAcknowledgement(_ retry: AckRetry?, observing binding: RefreshInstallBinding, for key: AccountLeaseKey, using token: FencedLeaseToken, now: Date) throws {
        try withRecoveryAccount(key, using: token, now: now) { row, context in
            guard var checkpoint = try decodedCheckpoint(row, for: key) else { return }
            if let retry {
                guard retry.binding == binding, (0...10).contains(retry.retryCount),
                      retry.nextRetryAt.timeIntervalSince1970.isFinite else { throw RefreshRecoveryError.invalidBinding }
            }
            if checkpoint.predecessorAcknowledgement?.binding == binding {
                guard retry == nil || retry?.attempt == checkpoint.predecessorAcknowledgement?.attempt else { throw RefreshRecoveryError.invalidBinding }
                checkpoint.predecessorAcknowledgement = retry
                try persist(checkpoint, state: .inFlightUncertain(checkpoint.attempt), row: row, in: context)
                return
            }
            guard checkpoint.installBinding == binding else { return }
            guard checkpoint.phase == .ackPending else { throw RefreshRecoveryError.invalidBinding }
            guard row.credentialVersion == binding.expectedCredentialVersion,
                  retry == nil || retry?.attempt == checkpoint.attempt else { throw RefreshRecoveryError.invalidBinding }
            if let retry {
                checkpoint.acknowledgement = retry
                try persist(checkpoint, state: .idle, row: row, in: context)
            } else {
                try persist(nil, state: .idle, row: row, in: context)
            }
        }
    }
}

private extension BabyStore {
    func recoveryAccount(_ key: AccountLeaseKey, in context: ModelContext) throws -> AccountAccessState? {
        let encoded = key.encodedKey
        return try context.fetch(FetchDescriptor<AccountAccessState>(predicate: #Predicate { $0.key == encoded })).first
    }

    func withRecoveryAccount<T>(_ key: AccountLeaseKey, using token: FencedLeaseToken, now: Date, _ body: (AccountAccessState, ModelContext) throws -> T) throws -> T {
        try AppGroupBootstrapLock.withLock(at: coordinationLockURL) {
            // Independent contexts prevent a cached pre-sign-out account or
            // expired lease from authorizing a write by a resumed process.
            let context = ModelContext(container)
            context.autosaveEnabled = false
            guard token.scope == .account(key) else { throw FencedLeaseError.staleOwner }
            let scopeKey = token.scope.encodedKey
            guard let lease = try context.fetch(FetchDescriptor<FencedLease>(predicate: #Predicate { $0.scopeKey == scopeKey })).first,
                  lease.ownerID == token.ownerID, lease.generation == token.generation else { throw FencedLeaseError.staleOwner }
            guard lease.expiresAt > now else { throw FencedLeaseError.expired }
            guard let row = try recoveryAccount(key, in: context), row.allowsOrdinarySync else { throw FencedLeaseError.cancelled }
            if let realm = try context.fetch(FetchDescriptor<RealmTransitionCheckpoint>()).first {
                guard realm.phaseRaw == "idle", row.boundRealmKey == nil || row.boundRealmKey == realm.activeRealmKey else { throw FencedLeaseError.cancelled }
            } else if row.boundRealmKey != nil { throw FencedLeaseError.cancelled }
            do { return try body(row, context) }
            catch { context.rollback(); throw error }
        }
    }

    func decodedCheckpoint(_ row: AccountAccessState, for key: AccountLeaseKey) throws -> RefreshRecoveryCheckpoint? {
        let payload = try row.recoveryPayload()
        guard let bytes = payload.recovery else {
            if case .inFlightUncertain(let attempt) = payload.state, attempt.protocolMode == .recoverableV1 { throw RefreshRecoveryError.corruptCheckpoint }
            return nil
        }
        do {
            let value = try JSONDecoder().decode(RefreshRecoveryCheckpoint.self, from: bytes)
            guard value.attempt.protocolMode == .recoverableV1 else { throw RefreshRecoveryError.corruptCheckpoint }
            if let predecessor = value.predecessorAcknowledgement {
                let prior = predecessor.binding
                guard value.phase != .ackPending, prior.isValid, prior.key == key,
                      prior.recoveryEpoch == value.recoveryEpoch,
                      prior.attemptID == predecessor.attempt.attemptID,
                      prior.parentCredentialVersion == predecessor.attempt.credentialVersion,
                      predecessor.attempt.protocolMode == .recoverableV1,
                      prior.expectedCredentialVersion == value.attempt.credentialVersion,
                      prior.attemptID != value.attempt.attemptID,
                      (0...10).contains(predecessor.retryCount),
                      predecessor.nextRetryAt.timeIntervalSince1970.isFinite else { throw RefreshRecoveryError.corruptCheckpoint }
            }
            switch value.phase {
            case .awaitingReply:
                guard value.installBinding == nil, value.acknowledgement == nil,
                      payload.state == .inFlightUncertain(value.attempt) else { throw RefreshRecoveryError.corruptCheckpoint }
            case .installPrepared, .ackPending:
                guard let binding = value.installBinding else { throw RefreshRecoveryError.corruptCheckpoint }
                try validate(binding, checkpoint: value, key: key)
                guard payload.state == (value.phase == .ackPending ? .idle : .inFlightUncertain(value.attempt)) else { throw RefreshRecoveryError.corruptCheckpoint }
                if value.phase == .installPrepared {
                    guard value.acknowledgement == nil else { throw RefreshRecoveryError.corruptCheckpoint }
                } else {
                    guard let retry = value.acknowledgement, retry.attempt == value.attempt, retry.binding == binding,
                          (0...10).contains(retry.retryCount), retry.nextRetryAt.timeIntervalSince1970.isFinite else { throw RefreshRecoveryError.corruptCheckpoint }
                }
            }
            return value
        } catch { throw RefreshRecoveryError.corruptCheckpoint }
    }

    func validate(_ binding: RefreshInstallBinding, checkpoint: RefreshRecoveryCheckpoint, key: AccountLeaseKey) throws {
        guard binding.isValid, binding.key == key, binding.recoveryEpoch == checkpoint.recoveryEpoch,
              binding.attemptID == checkpoint.attempt.attemptID,
              binding.parentCredentialVersion == checkpoint.attempt.credentialVersion else { throw RefreshRecoveryError.invalidBinding }
    }

    func persist(_ checkpoint: RefreshRecoveryCheckpoint?, state: RefreshAttemptState, row: AccountAccessState, in context: ModelContext) throws {
        try row.setRecoveryPayload(state: state, recovery: try checkpoint.map { try JSONEncoder().encode($0) })
        try context.save()
    }
}
