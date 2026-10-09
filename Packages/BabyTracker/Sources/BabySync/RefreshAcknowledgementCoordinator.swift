import BabyStore
import Foundation

/// ACK failure never rolls back an installed session. The durable due date also
/// lets a new process continue after this process's timer disappears.
@MainActor
public final class RefreshAcknowledgementCoordinator {
    private let store: BabyStore
    private let keychain: any SecureSessionStore
    private let leases: any FencedLeaseStore
    private let endpoint: ConfirmedEndpoint
    private let recovery: any SessionRecoveryTransport
    private let diagnostics: (RefreshRecoveryEvent) -> Void
    private let now: () -> Date
    private let sleep: @Sendable (Duration) async throws -> Void
    private var inFlight: Set<AccountSessionKey> = []
    private var scheduled: [AccountSessionKey: Task<Void, Never>] = [:]

    public init(store: BabyStore, keychain: any SecureSessionStore, leases: any FencedLeaseStore, endpoint: ConfirmedEndpoint, recovery: any SessionRecoveryTransport, now: @escaping () -> Date = Date.init, diagnostics: @escaping (RefreshRecoveryEvent) -> Void = { _ in }, sleep: @escaping @Sendable (Duration) async throws -> Void = { try await Task.sleep(for: $0) }) {
        self.store = store; self.keychain = keychain; self.leases = leases
        self.endpoint = endpoint; self.recovery = recovery; self.now = now; self.sleep = sleep; self.diagnostics = diagnostics
    }

    public func retryPending(accountKey: AccountSessionKey, familyKey: FamilySessionKey?) async {
        guard familyKey == nil || familyKey?.accountSessionKey == accountKey,
              endpoint.origin.canonicalString == accountKey.canonicalOrigin,
              endpoint.serverInstanceID == accountKey.serverInstanceID else { return }
        guard inFlight.insert(accountKey).inserted else { return }
        defer { inFlight.remove(accountKey) }
        scheduled.removeValue(forKey: accountKey)?.cancel()
        let started = now()
        let correlation = UUID()
        var retryCount = 0
        func emit(_ phase: RefreshRecoveryEvent.Phase, _ reason: RefreshRecoveryEvent.Reason? = nil) {
            let elapsed = max(0, min(now().timeIntervalSince(started) * 1000, Double(Int32.max)))
            diagnostics(RefreshRecoveryEvent(phase: phase, reason: reason, durationMilliseconds: Int(elapsed), retryCount: retryCount, correlation: correlation))
        }
        do {
            guard let checkpoint = try store.refreshRecoveryCheckpoint(for: accountKey.leaseKey),
                  let pending = checkpoint.acknowledgement ?? checkpoint.predecessorAcknowledgement else { return }
            retryCount = pending.retryCount
            emit(.ackPending)
            if pending.nextRetryAt > now() {
                emit(.deferred, .retryLater)
                schedule(accountKey, familyKey, pending.nextRetryAt); return
            }
            guard let token = try leases.acquire(scope: .account(accountKey.leaseKey), ownerID: UUID(), now: now(), ttl: SessionRefresher.leaseDuration) else {
                emit(.deferred, .retryLater)
                schedule(accountKey, familyKey, now().addingTimeInterval(1)); return
            }
            defer { try? leases.cancel(token) }
            let credentials = try familyKey.map { try keychain.loadFamily($0) } ?? keychain.loadAccount(accountKey)
            guard let credentials else { emit(.deferred, .retryLater); return }
            try store.verifyInstalledRefresh(pending.binding, for: accountKey.leaseKey) {
                let reread = try familyKey.map { try keychain.loadFamily($0) } ?? keychain.loadAccount(accountKey)
                guard reread == credentials else { throw RefreshRecoveryError.invalidBinding }
                try SessionRefresher.verify(credentials, binding: pending.binding, accountKey: accountKey)
            }
            guard credentials.expiresAt > now() else {
                // Ordinary renewal consumes this installed child and carries
                // the observed obligation until the successor is installed.
                emit(.deferred, .retryLater)
                schedule(accountKey, familyKey, now().addingTimeInterval(300)); return
            }
            do {
                try await recovery.acknowledgeRefresh(attemptID: pending.attempt.attemptID, credentials: credentials, at: endpoint)
                try store.updateRefreshAcknowledgement(nil, observing: pending.binding, for: accountKey.leaseKey, using: token, now: now())
                emit(.acknowledged)
            } catch {
                let delay = min(pow(2, Double(pending.retryCount)), 300)
                let retry = AckRetry(attempt: pending.attempt, binding: pending.binding, nextRetryAt: now().addingTimeInterval(delay), retryCount: min(pending.retryCount + 1, 10))
                try store.updateRefreshAcknowledgement(retry, observing: pending.binding, for: accountKey.leaseKey, using: token, now: now())
                retryCount = retry.retryCount
                emit(.deferred, .retryLater)
                schedule(accountKey, familyKey, retry.nextRetryAt)
            }
        } catch FencedLeaseError.expired {
            emit(.deferred, .retryLater)
            schedule(accountKey, familyKey, now().addingTimeInterval(1))
        } catch FencedLeaseError.staleOwner {
            emit(.deferred, .retryLater)
            schedule(accountKey, familyKey, now().addingTimeInterval(1))
        } catch is SessionStoreError {
            emit(.deferred, .retryLater)
            schedule(accountKey, familyKey, now().addingTimeInterval(300))
        } catch {
            emit(.deferred, .retryLater)
            // An invalid binding or closed account gate cannot authorize ACK.
        }
    }

    private func schedule(_ account: AccountSessionKey, _ family: FamilySessionKey?, _ date: Date) {
        let delay = max(0, min(date.timeIntervalSince(now()), 300))
        let sleep = sleep
        scheduled[account] = Task { [self] in
            do { try await sleep(.seconds(delay)); try Task.checkCancellation() }
            catch { return }
            self.scheduled.removeValue(forKey: account)
            await self.retryPending(accountKey: account, familyKey: family)
        }
    }
}
