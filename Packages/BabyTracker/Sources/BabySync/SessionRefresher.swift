import BabyStore
import Foundation

/// Rotates an expired access token, once, across every process.
///
/// The hard part is not the request; it is that the app, the widget and an App
/// Intent can all notice the same expiry at the same moment. A refresh token is
/// single-use, so two simultaneous refreshes mean one of them presents a token
/// the server has already consumed — which looks exactly like a stolen token
/// and gets the whole session revoked.
///
/// Serialised on the account-scoped fenced lease, and guarded by P3.1's
/// durable refresh-attempt marker so a crash mid-flight cannot be mistaken for
/// "never sent".
@MainActor
public final class SessionRefresher {
    public enum RefreshError: Error, Sendable, Equatable {
        case lockUnavailable
        case uncertainAttemptRequiresFreshProof
    }

    /// How long one refresh may hold the account lease. The identity
    /// client's request timeout is kept well inside it
    /// (`HTTPIdentityClient.requestTimeout`): a request that outlived the
    /// lease could succeed after another process took over, and the rotated
    /// token it brought back would be thrown away.
    nonisolated static let leaseDuration: TimeInterval = 30

    /// Failures that happen before a connection exists, so before any byte
    /// of the refresh token could have left the phone. `timedOut`,
    /// `networkConnectionLost` and the like are not here: by then the
    /// request may have arrived.
    nonisolated static let connectionNeverOpened: Set<URLError.Code> = [
        .notConnectedToInternet, .cannotConnectToHost, .cannotFindHost, .dnsLookupFailed,
    ]

    private let store: BabyStore
    private let keychain: any SecureSessionStore
    private let leases: any FencedLeaseStore
    private let exchange: (SessionCredentials) async throws -> SessionCredentials
    private var recovery: (endpoint: ConfirmedEndpoint, transport: any SessionRecoveryTransport)?
    private let ownerID = UUID()
    private let now: () -> Date
    private var diagnostics: (RefreshRecoveryEvent) -> Void = { _ in }

    public init(
        store: BabyStore,
        keychain: any SecureSessionStore,
        leases: any FencedLeaseStore,
        now: @escaping () -> Date = Date.init,
        exchange: @escaping (SessionCredentials) async throws -> SessionCredentials
    ) {
        self.store = store
        self.keychain = keychain
        self.leases = leases
        self.now = now
        self.exchange = exchange
    }

    public convenience init(store: BabyStore, keychain: any SecureSessionStore, leases: any FencedLeaseStore, endpoint: ConfirmedEndpoint, recovery: any SessionRecoveryTransport, now: @escaping () -> Date = Date.init, diagnostics: @escaping (RefreshRecoveryEvent) -> Void = { _ in }) {
        self.init(store: store, keychain: keychain, leases: leases, now: now) { current in
            let child = try await recovery.refresh(refreshToken: current.refreshToken, at: endpoint)
            return Self.preservingContinuity(child, parent: current)
        }
        self.recovery = (endpoint, recovery)
        self.diagnostics = diagnostics
    }

    /// Refreshes the family session for one account.
    ///
    /// Returns without doing anything if another process holds the lease — the
    /// point is that exactly one refresh happens, not that this one does it.
    public func refresh(accountKey: AccountSessionKey, familyKey: FamilySessionKey) async throws {
        guard familyKey.accountSessionKey == accountKey else { throw RefreshRecoveryError.invalidBinding }
        try await refresh(
            accountKey: accountKey,
            load: { try self.keychain.loadFamily(familyKey) },
            save: { try self.keychain.saveFamily($0, for: familyKey) }
        )
    }

    /// Refreshes the session an account holds before it is in a family.
    ///
    /// The same route, lease and durable attempt marker as the family's: an
    /// account that signed in and waited past the 10-minute access token
    /// before creating or joining would otherwise have no way back but a
    /// fresh Apple sign-in.
    public func refreshAccount(accountKey: AccountSessionKey) async throws {
        try await refresh(
            accountKey: accountKey,
            load: { try self.keychain.loadAccount(accountKey) },
            save: { try self.keychain.saveAccount($0, for: accountKey) }
        )
    }

    private func refresh(
        accountKey: AccountSessionKey,
        load: () throws -> SessionCredentials?,
        save: (SessionCredentials) throws -> Void
    ) async throws {
        let correlation = UUID()
        let started = now()
        func emit(_ phase: RefreshRecoveryEvent.Phase, _ reason: RefreshRecoveryEvent.Reason? = nil) {
            let elapsed = max(0, min(now().timeIntervalSince(started) * 1000, Double(Int32.max)))
            diagnostics(RefreshRecoveryEvent(phase: phase, reason: reason, durationMilliseconds: Int(elapsed), correlation: correlation))
        }
        do {
            let scope = LeaseScope.account(accountKey.leaseKey)
            guard let token = try leases.acquire(scope: scope, ownerID: UUID(), now: now(), ttl: Self.leaseDuration) else {
                throw RefreshError.lockUnavailable
            }
            defer { try? leases.cancel(token) }

            if let recovery {
                if try await recover(accountKey: accountKey, load: load, save: save, token: token, endpoint: recovery.endpoint, transport: recovery.transport, emit: emit) { return }
            }

            // A marker left by a previous attempt means bytes may already have
            // gone. Re-sending the parent token would be reuse, so the only safe
            // way forward is fresh Apple proof.
            if case .inFlightUncertain = try store.refreshAttemptState(for: accountKey.leaseKey) {
                throw RefreshError.uncertainAttemptRequiresFreshProof
            }

            guard let current = try load() else {
                throw RefreshError.uncertainAttemptRequiresFreshProof
            }

            // Nothing is marked, and nothing sent, for a renewal already
            // cancelled — `engine.stop()` winding sync down.
            try Task.checkCancellation()

            // Durable BEFORE any byte leaves. A crash after this point stays
            // uncertain, which is the correct reading.
            let attempt = try store.beginRefreshAttempt(
                for: accountKey.leaseKey,
                credentialVersion: current.credentialVersion,
                using: token,
                now: now()
            )
            // The marker is only worth anything while it is true. Wherever it is
            // provable that no request went out, it comes down again — otherwise a
            // dropped connection would demand Apple proof over a token nobody
            // ever spent.
            func abandon() {
                try? store.clearRefreshAttemptAfterProvenNoSend(attempt.attemptID, for: accountKey.leaseKey, using: token)
            }
            if Task.isCancelled {
                abandon()
                throw CancellationError()
            }

            // Apart from proven unsent or quota-refused requests below, a failure
            // deliberately leaves the marker standing. A
            // timeout, a disconnect or an unknown send status all leave the token
            // possibly consumed, and clearing would let the next attempt reuse it.
            let refreshed: SessionCredentials
            do {
                refreshed = try await exchange(current)
            } catch let error as URLError where Self.connectionNeverOpened.contains(error.code) {
                abandon()
                throw error
            } catch HTTPClientError.status(429, "rate_limited") {
                // The refresh handler refuses quotas before rotating the token.
                // This definite refusal leaves the parent token unspent, unlike
                // a lost response or an unspecified server failure.
                abandon()
                throw HTTPClientError.status(429, "rate_limited")
            }

            try store.finishRefreshAttempt(
                attempt.attemptID,
                for: accountKey.leaseKey,
                using: token,
                installingCredentialVersion: refreshed.credentialVersion
            ) {
                try save(refreshed)
            }
        } catch {
            if let reason = Self.terminalReason(error) { emit(.terminal, reason) }
            else { emit(.deferred, .retryLater) }
            throw error
        }
    }

    /// Returns false only when a new, still-unsent refresh selects legacy mode.
    private func recover(accountKey: AccountSessionKey, load: () throws -> SessionCredentials?, save: (SessionCredentials) throws -> Void, token: FencedLeaseToken, endpoint: ConfirmedEndpoint, transport: any SessionRecoveryTransport, emit: (RefreshRecoveryEvent.Phase, RefreshRecoveryEvent.Reason?) -> Void) async throws -> Bool {
        guard endpoint.origin.canonicalString == accountKey.canonicalOrigin,
              endpoint.serverInstanceID == accountKey.serverInstanceID else { throw RefreshRecoveryError.invalidBinding }
        guard let current = try load() else { throw RefreshError.uncertainAttemptRequiresFreshProof }
        guard current.recoveryEpoch == accountKey.recoveryEpoch else { throw RefreshRecoveryError.invalidBinding }
        var checkpoint = try store.refreshRecoveryCheckpoint(for: accountKey.leaseKey)
        if checkpoint == nil {
            if case .inFlightUncertain = try store.refreshAttemptState(for: accountKey.leaseKey) {
                throw RefreshError.uncertainAttemptRequiresFreshProof
            }
        }
        if let saved = checkpoint, saved.phase == .installPrepared, let binding = saved.installBinding,
           current.credentialVersion == binding.expectedCredentialVersion {
            try Self.verify(current, binding: binding, accountKey: accountKey)
            try store.completeRefreshInstall(binding, for: accountKey.leaseKey, using: token, now: now()) {
                guard let installed = try load() else { throw RefreshRecoveryError.invalidBinding }
                try Self.verify(installed, binding: binding, accountKey: accountKey)
            }
            emit(.installed, nil)
            emit(.ackPending, nil)
            return true
        }
        if checkpoint == nil || checkpoint?.phase == .ackPending {
            try Task.checkCancellation()
            emit(.probing, nil)
            let info = try await transport.serverInfo(endpoint.origin)
            _ = try EndpointIdentityProbe().confirm(origin: endpoint.origin, info: info, knownInstanceID: endpoint.serverInstanceID)
            guard info.recoveryEpoch == current.recoveryEpoch else { throw HTTPClientError.status(401, "epoch_changed") }
            if !info.advertisesRefreshRecovery || current.deviceContinuity.credential.isEmpty {
                guard checkpoint == nil else { throw RefreshRecoveryError.invalidBinding }
                emit(.legacy, current.deviceContinuity.credential.isEmpty ? .recoveryUnavailableMissingDeviceBinding : .legacyServer)
                return false
            }
            guard current.deviceContinuity.deviceID == accountKey.deviceID else { throw RefreshRecoveryError.invalidBinding }
            if let prior = checkpoint?.installBinding { try Self.verify(current, binding: prior, accountKey: accountKey) }
            checkpoint = try store.beginRecoverableRefresh(for: accountKey.leaseKey, recoveryEpoch: current.recoveryEpoch, credentialVersion: current.credentialVersion, supersedingBinding: checkpoint?.installBinding, using: token, now: now())
        }
        guard let checkpoint,
              checkpoint.recoveryEpoch == current.recoveryEpoch,
              checkpoint.attempt.credentialVersion == current.credentialVersion,
              current.deviceContinuity.deviceID == accountKey.deviceID,
              !current.deviceContinuity.credential.isEmpty else { throw RefreshRecoveryError.invalidBinding }
        try Task.checkCancellation()
        emit(.recovering, nil)
        let result = try await transport.refreshRecoverable(parent: current, expectedAccountID: accountKey.accountID, attemptID: checkpoint.attempt.attemptID, at: endpoint)
        guard result.accountID == accountKey.accountID, result.attemptID == checkpoint.attempt.attemptID,
              result.credentials.recoveryEpoch == checkpoint.recoveryEpoch else { throw RefreshRecoveryError.invalidBinding }
        let child = Self.preservingContinuity(result.credentials, parent: current)
        guard child.deviceContinuity.deviceID == accountKey.deviceID,
              !child.accessToken.isEmpty, !child.refreshToken.isEmpty else { throw RefreshRecoveryError.invalidBinding }
        let binding = checkpoint.installBinding ?? RefreshInstallBinding(key: accountKey.leaseKey, recoveryEpoch: checkpoint.recoveryEpoch, attemptID: checkpoint.attempt.attemptID, parentCredentialVersion: current.credentialVersion, expectedCredentialVersion: child.credentialVersion, accessFingerprint: RefreshInstallBinding.fingerprint(child.accessToken), refreshFingerprint: RefreshInstallBinding.fingerprint(child.refreshToken))
        let installed = SessionCredentials(accessToken: child.accessToken, refreshToken: child.refreshToken, expiresAt: child.expiresAt, recoveryEpoch: child.recoveryEpoch, deviceContinuity: child.deviceContinuity, credentialVersion: binding.expectedCredentialVersion, installBinding: binding)
        try Self.verify(installed, binding: binding, accountKey: accountKey)
        try store.prepareRefreshInstall(binding, for: accountKey.leaseKey, using: token, now: now())
        emit(.preparing, nil)
        try store.completeRefreshInstall(binding, for: accountKey.leaseKey, using: token, now: now()) {
            guard let before = try load(), before == current else { throw RefreshRecoveryError.invalidBinding }
            try save(installed)
            guard let after = try load() else { throw RefreshRecoveryError.invalidBinding }
            try Self.verify(after, binding: binding, accountKey: accountKey)
        }
        emit(.installed, nil)
        emit(.ackPending, nil)
        return true
    }

    static func verify(_ credentials: SessionCredentials, binding: RefreshInstallBinding, accountKey: AccountSessionKey) throws {
        guard binding.key == accountKey.leaseKey, binding.recoveryEpoch == accountKey.recoveryEpoch,
              credentials.recoveryEpoch == accountKey.recoveryEpoch,
              credentials.deviceContinuity.deviceID == accountKey.deviceID,
              !credentials.deviceContinuity.credential.isEmpty,
              credentials.credentialVersion == binding.expectedCredentialVersion,
              credentials.installBinding == binding,
              RefreshInstallBinding.fingerprint(credentials.accessToken) == binding.accessFingerprint,
              RefreshInstallBinding.fingerprint(credentials.refreshToken) == binding.refreshFingerprint else { throw RefreshRecoveryError.invalidBinding }
    }

    private static func preservingContinuity(_ child: SessionCredentials, parent: SessionCredentials) -> SessionCredentials {
        SessionCredentials(accessToken: child.accessToken, refreshToken: child.refreshToken, expiresAt: child.expiresAt, recoveryEpoch: child.recoveryEpoch, deviceContinuity: child.deviceContinuity.credential.isEmpty ? parent.deviceContinuity : child.deviceContinuity, credentialVersion: child.credentialVersion, installBinding: child.installBinding)
    }

    /// Whether a failed renewal can only be put right by a fresh Apple proof,
    /// as opposed to trying again later.
    ///
    /// The two must not be confused either way. Read as transient, a session
    /// whose renewal was terminally refused retries forever and the parent
    /// is never offered the sign-in that would fix it. Read as needing
    /// proof, a lease another process holds or a dropped connection asks the
    /// parent to sign in over something that clears by itself.
    ///
    /// Fresh proof: an attempt left uncertain (its token may be consumed), the
    /// server refusing the refresh token outright (`refresh.go` answers 401
    /// `reauthentication_required` past the absolute age, `session_revoked` or
    /// `refresh_reused` for a dead token, `epoch_changed` after a recovery),
    /// and the durable gate refusing the attempt (`cancelled`: signed out, or
    /// holding credentials it no longer recognises). Everything else backs off.
    nonisolated public static func requiresFreshProof(_ error: any Error) -> Bool {
        switch error {
        case RefreshError.uncertainAttemptRequiresFreshProof, FencedLeaseError.cancelled:
            return true
        case HTTPClientError.status(401, let code?):
            return ["reauthentication_required", "session_revoked", "refresh_reused", "epoch_changed", "refresh_receipt_unavailable"].contains(code)
        case HTTPClientError.status(409, "refresh_attempt_conflict"):
            return true
        case let code as SyncErrorCode:
            return code.action == .freshProofRequired
        default:
            return false
        }
    }

    nonisolated static func terminalReason(_ error: any Error) -> RefreshRecoveryEvent.Reason? {
        switch error {
        case HTTPClientError.status(401, "reauthentication_required"): .reauthenticationRequired
        case HTTPClientError.status(401, "session_revoked"): .sessionRevoked
        case HTTPClientError.status(401, "refresh_reused"): .refreshReused
        case HTTPClientError.status(401, "epoch_changed"): .epochChanged
        case HTTPClientError.status(401, "refresh_receipt_unavailable"): .receiptUnavailable
        case HTTPClientError.status(409, "refresh_attempt_conflict"): .attemptConflict
        case is RefreshRecoveryError: .irreconcilableCheckpoint
        case RefreshError.uncertainAttemptRequiresFreshProof: .uncertainLegacyAttempt
        case FencedLeaseError.cancelled: .accountGateClosed
        default: nil
        }
    }

    /// Clears the marker only when the caller can prove nothing was sent.
    public func abandonProvenUnsentAttempt(
        _ attemptID: UUID,
        accountKey: AccountSessionKey
    ) throws {
        let scope = LeaseScope.account(accountKey.leaseKey)
        guard let token = try leases.acquire(scope: scope, ownerID: ownerID, now: now(), ttl: Self.leaseDuration) else {
            throw RefreshError.lockUnavailable
        }
        defer { try? leases.cancel(token) }
        try store.clearRefreshAttemptAfterProvenNoSend(attemptID, for: accountKey.leaseKey, using: token)
    }
}
