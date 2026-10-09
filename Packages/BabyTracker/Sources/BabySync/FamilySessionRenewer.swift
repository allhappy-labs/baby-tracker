import BabyStore
import Foundation

/// Renews one family's session over HTTPS, on demand.
///
/// This is the one place that turns `SessionRefresher`'s abstract `exchange`
/// closure into an actual `POST /v1/auth/refresh`. Everything about *when* to
/// refresh, and making sure only one refresh happens at a time, stays
/// `SessionRefresher`'s job; this type only knows how to ask the server and
/// where the family's credentials live.
///
/// Every call obtains its own account fence. Recovery and ordinary sync share
/// the same store and Keychain paths used by widgets and App Intents.
@MainActor
public final class FamilySessionRenewer {
    private let store: BabyStore
    private let keychain: any SecureSessionStore
    private let leases: any FencedLeaseStore
    private let identity: any SessionRefreshing
    private let endpoint: ConfirmedEndpoint
    private let diagnostics: (RefreshRecoveryEvent) -> Void
    private let acknowledgements: RefreshAcknowledgementCoordinator?
    private let schedulesAcknowledgements: Bool

    /// - Parameter schedulesAcknowledgements: false for a short-lived graph —
    ///   an App Intent's — that must leave no task running once it returns.
    ///   A refresh it completes still records its acknowledgement durably;
    ///   the app sends it from that record (`prepare`, or its next renewal).
    public init(
        store: BabyStore,
        keychain: any SecureSessionStore,
        leases: any FencedLeaseStore,
        identity: any SessionRefreshing,
        endpoint: ConfirmedEndpoint,
        diagnostics: @escaping (RefreshRecoveryEvent) -> Void = { _ in },
        schedulesAcknowledgements: Bool = true
    ) {
        self.store = store
        self.keychain = keychain
        self.leases = leases
        self.identity = identity
        self.endpoint = endpoint
        self.diagnostics = diagnostics
        self.schedulesAcknowledgements = schedulesAcknowledgements
        acknowledgements = (identity as? any SessionRecoveryTransport).map {
            RefreshAcknowledgementCoordinator(store: store, keychain: keychain, leases: leases, endpoint: endpoint, recovery: $0, diagnostics: diagnostics)
        }
    }

    /// Loads the family's credentials, refreshes them, and saves the result
    /// back under the same family key.
    public func renew(familyKey: FamilySessionKey) async throws {
        try await renewFamily(familyKey: familyKey, bindFailures: false)
    }

    private func renewFamily(familyKey: FamilySessionKey, bindFailures: Bool) async throws {
        func refresh() async throws {
            let version = bindFailures ? try store.accountCredentialVersion(for: familyKey.leaseKey) : nil
            do { try await refresher().refresh(accountKey: familyKey.accountSessionKey, familyKey: familyKey) }
            catch let error as HTTPClientError where SessionRefresher.requiresFreshProof(error) {
                guard let version else { throw error }
                throw CredentialBoundAuthFailure(underlying: error, credentialVersion: version, requiresFreshProof: true)
            }
        }
        try await refresh()
        if acknowledgements != nil, let current = try keychain.loadFamily(familyKey), current.expiresAt <= Date() {
            try await refresh()
        }
        scheduleAcknowledgement(accountKey: familyKey.accountSessionKey, familyKey: familyKey)
    }

    /// The same for the session an account holds before it has a family.
    public func renew(accountKey: AccountSessionKey) async throws {
        try await refresher().refreshAccount(accountKey: accountKey)
        if acknowledgements != nil, let current = try keychain.loadAccount(accountKey), current.expiresAt <= Date() {
            try await refresher().refreshAccount(accountKey: accountKey)
        }
        scheduleAcknowledgement(accountKey: accountKey, familyKey: nil)
    }

    /// Restore resumes a durable incomplete install before any ordinary use.
    public func prepare(familyKey: FamilySessionKey) async throws {
        let checkpoint: RefreshRecoveryCheckpoint?
        do { checkpoint = try store.refreshRecoveryCheckpoint(for: familyKey.accountSessionKey.leaseKey) }
        catch {
            let reason = SessionRefresher.terminalReason(error)
            diagnostics(RefreshRecoveryEvent(phase: reason == nil ? .deferred : .terminal, reason: reason ?? .retryLater, durationMilliseconds: 0, correlation: UUID()))
            throw error
        }
        if let checkpoint, checkpoint.phase != .ackPending {
            try await renewFamily(familyKey: familyKey, bindFailures: true)
        } else if let current = try keychain.loadFamily(familyKey), current.expiresAt <= Date() {
            try await renewFamily(familyKey: familyKey, bindFailures: true)
        }
        scheduleAcknowledgement(accountKey: familyKey.accountSessionKey, familyKey: familyKey)
    }

    /// Returns the exact verified snapshot to send; callers must not reload a
    /// token after this check because a concurrent sign-in can replace it.
    public func credentialsForRetry(after usedVersion: UUID, familyKey: FamilySessionKey) async throws -> SessionCredentials? {
        let key = familyKey.accountSessionKey
        if let pending = try store.refreshRecoveryCheckpoint(for: key.leaseKey), pending.phase != .ackPending {
            do {
                try await refresher().refresh(accountKey: key, familyKey: familyKey)
            } catch let error as HTTPClientError where SessionRefresher.requiresFreshProof(error) {
                // Reconciliation may be renewing a newer child than the
                // ordinary request used. Bind the refusal to that attempt's
                // actual parent, so a later sign-in/logout still supersedes it.
                throw CredentialBoundAuthFailure(underlying: error, credentialVersion: pending.attempt.credentialVersion, requiresFreshProof: true)
            }
        }
        guard let current = try keychain.loadFamily(familyKey), current.credentialVersion != usedVersion,
              let binding = current.installBinding else { return nil }
        try store.verifyInstalledRefresh(binding, for: key.leaseKey) {
            guard let reread = try keychain.loadFamily(familyKey), reread == current else { throw RefreshRecoveryError.invalidBinding }
            try SessionRefresher.verify(reread, binding: binding, accountKey: key)
        }
        return current
    }

    private func scheduleAcknowledgement(accountKey: AccountSessionKey, familyKey: FamilySessionKey?) {
        guard schedulesAcknowledgements, let acknowledgements else { return }
        Task { await acknowledgements.retryPending(accountKey: accountKey, familyKey: familyKey) }
    }

    private func refresher() -> SessionRefresher {
        let identity = identity
        let endpoint = endpoint
        if let recovery = identity as? any SessionRecoveryTransport {
            return SessionRefresher(store: store, keychain: keychain, leases: leases, endpoint: endpoint, recovery: recovery, diagnostics: diagnostics)
        }
        return SessionRefresher(store: store, keychain: keychain, leases: leases) { current in
            let refreshed = try await identity.refresh(refreshToken: current.refreshToken, at: endpoint)
            // A refresh response only carries `device_credential` on the rare
            // exchange that rotates it. Everywhere else, the device's own
            // continuity proof outlives the token rotation and must not be
            // overwritten with an empty one.
            guard refreshed.deviceContinuity.credential.isEmpty else { return refreshed }
            return SessionCredentials(
                accessToken: refreshed.accessToken,
                refreshToken: refreshed.refreshToken,
                expiresAt: refreshed.expiresAt,
                recoveryEpoch: refreshed.recoveryEpoch,
                deviceContinuity: current.deviceContinuity,
                credentialVersion: refreshed.credentialVersion
            )
        }
    }
}
