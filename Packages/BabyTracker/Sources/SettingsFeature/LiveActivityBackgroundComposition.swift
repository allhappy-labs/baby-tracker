import BabyProtocol
import BabyStore
import BabySync
import Foundation
#if canImport(UIKit)
import UIKit
#endif

/// What one Lock Screen or Dynamic Island action needs to check the family's
/// current state, built in the process the App Intent runs in — the app's,
/// often launched in the background for this tap alone.
///
/// The same pieces as the app's own sync, bound to the store the intent
/// opened (`WidgetStore`), so the catch-up lands in exactly the store the
/// action then reads and writes:
///
/// - the shared Keychain and the saved family session — read, not restored;
/// - the renewal lease through the store's coordination lock, so a renewal
///   here and one by the app or another intent fence each other;
/// - the ordinary sync transport, over a URL session of its own.
///
/// It performs at most one bounded catch-up before the action — only for a
/// card ahead of the store — and one bounded sync after an authorized
/// mutation, which runs after the intent has returned, under a background
/// task (`runInBackground`). `finish()` stops it: nothing it started keeps
/// running past the publish. A session renewed here leaves its
/// acknowledgement to the app, from the durable record.
@MainActor
public final class LiveActivityBackgroundComposition: LiveActivityActionGate {
    private let preflight: LiveActivityActionPreflight
    private let session: URLSession
    /// The renewer's identity client, with a session of its own; nil when
    /// there is no saved family session to renew.
    let identity: HTTPIdentityClient?
    private var finished = false

    /// The production graph: the App Group Keychain, the session the app
    /// saved, and the push state this process's cards are reconciled against.
    public static func make(store: BabyStore) -> LiveActivityBackgroundComposition {
        let backend = SystemKeychainBackend(accessGroup: BabySchema.appGroupID)
        installProvisionalPushState(into: LiveCards.pushState, from: .appGroup(keychain: backend))
        // The app's own choices: the sync transport on a default session,
        // identity on an ephemeral one (`HTTPIdentityClient()`).
        return LiveActivityBackgroundComposition(store: store, keychainBackend: backend, record: FamilySessionRecord.saved(),
                                                 configuration: .default, identityConfiguration: .ephemeral)
    }

    #if canImport(UIKit)
    /// Runs a card action's publish after the intent has returned. The
    /// background-task assertion is taken before this returns — not inside
    /// the task, which the intent's return could beat to a suspension — and
    /// when iOS takes the time back the work is cancelled; the outbox keeps
    /// the mutation for the app.
    public static func runInBackground(_ work: @escaping @MainActor () async -> Void) {
        let assertion = BackgroundAssertion()
        assertion.identifier = UIApplication.shared.beginBackgroundTask(withName: "LiveActivityCardActionPublish") {
            MainActor.assumeIsolated { assertion.end() }
        }
        assertion.work = Task { @MainActor in
            await work()
            assertion.end()
        }
    }

    @MainActor
    private final class BackgroundAssertion {
        var identifier = UIBackgroundTaskIdentifier.invalid
        var work: Task<Void, Never>?

        func end() {
            work?.cancel()
            work = nil
            guard identifier != .invalid else { return }
            UIApplication.shared.endBackgroundTask(identifier)
            identifier = .invalid
        }
    }
    #endif

    /// A card this action ends — the phone's own stop — must be retired, not
    /// reported to the server as the parent dismissing it, and a pushed card
    /// must be judged as push-managed. The app's coordinator arranges both
    /// where it runs; where it does not yet, the realm the server last
    /// accepted this device for is installed from the ledger. A coordinator's
    /// own state is never replaced.
    static func installProvisionalPushState(into state: LiveActivityPushState, from ledger: LiveActivityRegistrationStore) {
        guard !state.hasRetireHandler else { return }
        LiveActivityPushCardState.installProvisional(from: ledger, into: state)
    }

    init(
        store: BabyStore,
        keychainBackend: any KeychainBackend,
        record: FamilySessionRecord?,
        configuration: URLSessionConfiguration,
        identityConfiguration: URLSessionConfiguration? = nil,
        sleep: @escaping @Sendable (Duration) async throws -> Void = { try await Task.sleep(for: $0) }
    ) {
        let session = URLSession(configuration: configuration)
        self.session = session
        let catchUp: any LiveActivityCatchUp
        let sessionIsCurrent: @MainActor () -> Bool
        if let record, record.familyKey.realmID == store.activeRealm {
            let familyKey = record.familyKey
            let keychain = KeychainSessionStore(backend: keychainBackend)
            let identity = HTTPIdentityClient(configuration: identityConfiguration ?? configuration)
            self.identity = identity
            let renewer = FamilySessionRenewer(
                store: store, keychain: keychain,
                leases: AppGroupFencedLease(container: store.container, lockURL: store.coordinationLockURL),
                identity: identity,
                endpoint: record.endpoint, schedulesAcknowledgements: false
            )
            let transport = FamilySyncHTTPClient(
                endpoint: record.endpoint, tokens: KeychainTokenProvider(keychain: keychain, key: { .family(familyKey) }),
                session: session,
                retryCredentials: { version in try await renewer.credentialsForRetry(after: version, familyKey: familyKey) }
            )
            catchUp = SyncCoordinatorCatchUp(
                store: store, transport: transport, accountKey: familyKey.accountSessionKey,
                renew: { try await renewer.renew(familyKey: familyKey) },
                // As the app's engine does: a membership revoked during this
                // sync has its realm purged by the coordinator; this
                // device's saved invitation codes for it go too.
                onRevoked: { realm in try? InvitationCodeStore(backend: keychainBackend, realm: realm).removeAll() }
            )
            sessionIsCurrent = { [weak store] in
                guard let store else { return false }
                return store.activeRealm == familyKey.realmID
                    && (try? store.accountAllowsOrdinarySync(familyKey.leaseKey)) == true
            }
        } else {
            // No family session saved for the store's realm: nothing here can
            // vouch for a pushed card. The catch-up never succeeds; there is
            // no session to have changed.
            catchUp = UnavailableCatchUp()
            sessionIsCurrent = { true }
            identity = nil
        }
        preflight = LiveActivityActionPreflight(store: store, catchUp: catchUp, sessionIsCurrent: sessionIsCurrent, sleep: sleep)
    }

    public func prepare(target: LiveActivityActionTarget, timeout: Duration = LiveActivityActionPreflight.defaultTimeout) async -> LiveActivityActionDecision {
        guard !finished else { return .openApp(reason: .refreshFailed) }
        return await preflight.prepare(target: target, timeout: timeout)
    }

    public func publish(timeout: Duration = LiveActivityActionPreflight.defaultTimeout) async {
        guard !finished else { return }
        await preflight.publish(timeout: timeout)
    }

    /// Every catch-up and publish has been awaited, or cancelled and awaited,
    /// by now; this refuses any later one and closes both transports — the
    /// sync session and the renewer's identity session. Nothing unstructured
    /// could still be using the latter: the renewer schedules no
    /// acknowledgement here, and every renewal ran inside an awaited sync.
    public func finish() {
        guard !finished else { return }
        finished = true
        preflight.finish()
        session.invalidateAndCancel()
        identity?.invalidate()
    }
}

/// No family session to sync with: never a successful catch-up.
@MainActor
private final class UnavailableCatchUp: LiveActivityCatchUp {
    func catchUp() async -> Bool { false }
}
