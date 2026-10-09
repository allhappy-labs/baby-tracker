import Foundation
import SwiftData

public extension BabyStore {
    func accountAllowsOrdinarySync(_ key: AccountLeaseKey) throws -> Bool {
        try AppGroupBootstrapLock.withLock(at: coordinationLockURL) {
            try accountState(for: key, in: ModelContext(container))?.allowsOrdinarySync == true
        }
    }

    func markAccountSignedOut(_ key: AccountLeaseKey) throws {
        try AppGroupBootstrapLock.withLock(at: coordinationLockURL) {
            let state = try accountState(for: key) ?? AccountAccessState(key: key)
            if state.modelContext == nil { context.insert(state) }
            state.allowsOrdinarySync = false
            invalidateLease(scopeKey: LeaseScope.account(key).encodedKey)
            if let realmKey = state.boundRealmKey, let realm = RealmID(encodedKey: realmKey) {
                invalidateLease(scopeKey: LeaseScope.realm(realm).encodedKey)
            }
            let realmState = try writeBoundary.state(); realmState.generation &+= 1
            try context.save()
        }
    }

    func markAccountAuthenticated(_ key: AccountLeaseKey, credentialVersion: UUID) throws {
        try AppGroupBootstrapLock.withLock(at: coordinationLockURL) {
            let state = try accountState(for: key) ?? AccountAccessState(key: key)
            if state.modelContext == nil { context.insert(state) }
            state.allowsOrdinarySync = true
            state.credentialVersion = credentialVersion
            try context.save()
        }
    }

    /// What a fresh Apple proof installs: the gate opened on exactly these
    /// credentials, *and* any refresh attempt left uncertain forgotten.
    ///
    /// Separate from `markAccountAuthenticated` because only a new sign-in may
    /// clear that marker: it stands because the old refresh token may already
    /// be consumed, and a sign-in replaces that token outright, so nothing can
    /// reuse it any more. A promotion moves the *same* token and must leave the
    /// marker alone. Without this the marker a failed 24 h refresh leaves
    /// behind refused every renewal of the new session as well.
    /// The credential callback runs under the non-reentrant coordination lock:
    /// it may only save/verify Keychain values, never call locking store APIs.
    /// Failures leave the durable gate closed and preserve recovery metadata.
    func markAccountSignedIn(_ key: AccountLeaseKey, credentialVersion: UUID, installCredentials: () throws -> Void = {}) throws {
        try AppGroupBootstrapLock.withLock(at: coordinationLockURL) {
            let context = ModelContext(container)
            context.autosaveEnabled = false
            let encoded = key.encodedKey
            let state = try context.fetch(FetchDescriptor<AccountAccessState>(predicate: #Predicate { $0.key == encoded })).first ?? AccountAccessState(key: key)
            let isNew = state.modelContext == nil
            if isNew { context.insert(state) }
            // Commit a closed gate before changing the external credential
            // item. Any callback or subsequent SQLite failure stays closed.
            if state.allowsOrdinarySync || isNew {
                state.allowsOrdinarySync = false
                try context.save()
            }
            try installCredentials()
            state.allowsOrdinarySync = true
            state.credentialVersion = credentialVersion
            try state.setRecoveryPayload(state: .idle, recovery: nil)
            try context.save()
        }
    }

    func refreshAttemptState(for key: AccountLeaseKey) throws -> RefreshAttemptState {
        try AppGroupBootstrapLock.withLock(at: coordinationLockURL) { try accountState(for: key, in: ModelContext(container))?.refreshState ?? .idle }
    }

    func beginRefreshAttempt(
        for key: AccountLeaseKey,
        credentialVersion: UUID,
        using token: FencedLeaseToken,
        now: Date
    ) throws -> RefreshAttempt {
        try AppGroupBootstrapLock.withLock(at: coordinationLockURL) {
            let context = ModelContext(container)
            context.autosaveEnabled = false
            try validateAccountFence(key, token: token, now: now, in: context)
            guard let state = try accountState(for: key, in: context), state.allowsOrdinarySync,
                  state.credentialVersion == credentialVersion,
                  try state.refreshRecoveryData == nil,
                  try state.recoveryPayload().state == .idle else {
                throw FencedLeaseError.cancelled
            }
            let attempt = RefreshAttempt(attemptID: UUID(), credentialVersion: credentialVersion, ownerGeneration: token.generation, startedAt: now)
            state.refreshState = .inFlightUncertain(attempt)
            try context.save()
            return attempt
        }
    }

    func clearRefreshAttemptAfterProvenNoSend(
        _ attemptID: UUID,
        for key: AccountLeaseKey,
        using token: FencedLeaseToken
    ) throws {
        try AppGroupBootstrapLock.withLock(at: coordinationLockURL) {
            let context = ModelContext(container)
            context.autosaveEnabled = false
            try validateAccountFence(key, token: token, now: Date(), in: context)
            guard let state = try accountState(for: key, in: context),
                  try state.refreshRecoveryData == nil,
                  case .inFlightUncertain(let attempt) = try state.recoveryPayload().state,
                  attempt.attemptID == attemptID else { throw FencedLeaseError.staleOwner }
            state.refreshState = .idle; try context.save()
        }
    }

    func finishRefreshAttempt(
        _ attemptID: UUID,
        for key: AccountLeaseKey,
        using token: FencedLeaseToken,
        installingCredentialVersion: UUID,
        installCredentials: () throws -> Void
    ) throws {
        try AppGroupBootstrapLock.withLock(at: coordinationLockURL) {
            let context = ModelContext(container)
            context.autosaveEnabled = false
            try validateAccountFence(key, token: token, now: Date(), in: context)
            guard let state = try accountState(for: key, in: context),
                  try state.refreshRecoveryData == nil,
                  case .inFlightUncertain(let attempt) = try state.recoveryPayload().state,
                  attempt.attemptID == attemptID else { throw FencedLeaseError.staleOwner }
            try installCredentials()
            state.credentialVersion = installingCredentialVersion
            state.refreshState = .idle
            try context.save()
        }
    }

    private func accountState(for key: AccountLeaseKey, in readingContext: ModelContext? = nil) throws -> AccountAccessState? {
        let context = readingContext ?? context
        let encoded = key.encodedKey
        return try context.fetch(FetchDescriptor<AccountAccessState>(predicate: #Predicate { $0.key == encoded })).first
    }

    private func validateAccountFence(_ key: AccountLeaseKey, token: FencedLeaseToken, now: Date, in context: ModelContext) throws {
        guard token.scope == .account(key) else { throw FencedLeaseError.staleOwner }
        let scopeKey = token.scope.encodedKey
        guard let row = try context.fetch(FetchDescriptor<FencedLease>(predicate: #Predicate { $0.scopeKey == scopeKey })).first,
              row.ownerID == token.ownerID, row.generation == token.generation else { throw FencedLeaseError.staleOwner }
        guard row.expiresAt > now else { throw FencedLeaseError.expired }
    }

    private func invalidateLease(scopeKey: String) {
        if let row = try? context.fetch(FetchDescriptor<FencedLease>(predicate: #Predicate { $0.scopeKey == scopeKey })).first {
            row.generation &+= 1; row.expiresAt = .distantPast
        }
    }
}
