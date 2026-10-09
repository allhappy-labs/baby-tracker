import BabyProtocol
import BabyStore
import BabySync
import Foundation
#if canImport(UIKit)
import UIKit
#endif

/// Builds the production family-sync object graph.
///
/// Kept in one function so the app target does not have to know the wiring
/// order — and so the one decision that matters, that the Keychain is scoped
/// to the App Group access group, is visible in a single place rather than
/// spread across the app delegate.
@MainActor
public enum FamilySyncComposition {
    /// The private Tailnet endpoint and immutable server identity allocated by
    /// the reviewed Unraid deployment. The selection screen still probes both
    /// values before confirmation; this constant is a pin, not automatic trust.
    public static let hostedEndpoint: ConfirmedEndpoint? = try? ConfirmedEndpoint(
        origin: ServerOrigin(validating: "https://baby-sync.example-tailnet.ts.net"),
        serverInstanceID: UUID(uuidString: "11111111-2222-4333-8444-555555555555")!
    )

    public static func make(store: BabyStore, keychainAccessGroup: String? = nil, recoveryDiagnostics: @escaping (RefreshRecoveryEvent) -> Void = { _ in }, syncDiagnostics: (any SyncDiagnosticSink)? = nil) -> FamilySettingsViewModel {
        assemble(
            store: store,
            keychainBackend: SystemKeychainBackend(accessGroup: keychainAccessGroup),
            identity: HTTPIdentityClient(),
            session: .shared,
            defaults: nil,
            recoveryDiagnostics: recoveryDiagnostics,
            syncDiagnostics: syncDiagnostics
        ).model
    }

    /// What the app process owns: the settings model and, at most once per
    /// process, the Live Activity push driver.
    public struct AppGraph {
        public let model: FamilySettingsViewModel
        /// Nil when another graph in this process already owns ActivityKit
        /// observation: two owners would race each other's versions.
        public let liveActivityPush: LiveActivityPushDriver?
    }

    /// The app's graph: `make`'s, plus the automatic push registration on the
    /// built-in server. Nothing about it is a setting.
    public static func makeApp(store: BabyStore, keychainAccessGroup: String? = nil, recoveryDiagnostics: @escaping (RefreshRecoveryEvent) -> Void = { _ in }, syncDiagnostics: (any SyncDiagnosticSink)? = nil) -> AppGraph {
        let keychainBackend = SystemKeychainBackend(accessGroup: keychainAccessGroup)
        let claimed = liveActivityPushClaimed
        liveActivityPushClaimed = true
        let graph = assemble(
            store: store,
            keychainBackend: keychainBackend,
            identity: HTTPIdentityClient(),
            session: .shared,
            defaults: nil,
            recoveryDiagnostics: recoveryDiagnostics,
            syncDiagnostics: syncDiagnostics,
            liveActivityPush: claimed ? nil : .system(keychainBackend: keychainBackend)
        )
        #if canImport(UIKit)
        graph.liveActivityPush?.observe(.default, becameActive: UIApplication.didBecomeActiveNotification, preferencesChanged: UserDefaults.didChangeNotification)
        #else
        graph.liveActivityPush?.observe(.default, becameActive: nil, preferencesChanged: UserDefaults.didChangeNotification)
        #endif
        return AppGraph(model: graph.model, liveActivityPush: graph.liveActivityPush)
    }

    /// One ActivityKit observer per process.
    private static var liveActivityPushClaimed = false

    /// Where the push pieces come from. Production reads ActivityKit, the App
    /// Group ledger, the signed provisioning profile and the user's feed break
    /// preference; tests substitute any of them.
    struct LiveActivityPushWiring {
        var events: any LiveActivityPushEventSource
        var store: LiveActivityRegistrationStore
        var builtIn: ConfirmedEndpoint?
        var environment: any LiveActivityAPNsEnvironmentResolving
        var probe: (@Sendable (ServerOrigin) async throws -> ServerInfo)?
        var makeClient: LiveActivityPushCoordinator.ClientFactory?
        var background: any LiveActivityPushBackgroundExecution
        var feedBreakMinutes: @MainActor () -> Int
        /// Read when a foreground inventory is about to be taken.
        var isForeground: @MainActor () -> Bool = { false }
        /// What the card controller reads. The app's is the process-wide one;
        /// a test graph gets its own.
        var pushState = LiveActivityPushState()

        @MainActor static func system(keychainBackend: any KeychainBackend) -> LiveActivityPushWiring {
            #if canImport(UIKit)
            let background: any LiveActivityPushBackgroundExecution = UIKitLiveActivityPushExecution()
            let isForeground: @MainActor () -> Bool = { UIApplication.shared.applicationState == .active }
            #else
            let background: any LiveActivityPushBackgroundExecution = InlineLiveActivityPushExecution()
            let isForeground: @MainActor () -> Bool = { false }
            #endif
            return LiveActivityPushWiring(
                events: LiveActivityPushEvents.system,
                // The same shared Keychain backend as the sessions: tokens
                // live there and nowhere else.
                store: .appGroup(keychain: keychainBackend),
                builtIn: hostedEndpoint,
                environment: ProvisioningProfileEnvironmentResolver(),
                probe: nil,
                makeClient: nil,
                background: background,
                feedBreakMinutes: { TrackingPreferences().feedMinutes },
                isForeground: isForeground,
                pushState: .shared
            )
        }
    }

    /// The pieces of one assembled graph a caller may still need to reach.
    ///
    /// Only `make` is public: the app gets the model and nothing else. The
    /// coordinator is exposed internally for the live-server gate, which has
    /// no Apple to sign in with and hands the coordinator the session the
    /// server's identity-test adapter minted instead.
    struct Graph {
        let model: FamilySettingsViewModel
        let coordinator: AccountCoordinator
        var liveActivityPush: LiveActivityPushDriver? = nil
    }

    /// The whole wiring, with the things that differ between the app and the
    /// live-server gate — where secrets live, how TLS is trusted, which
    /// defaults hold the non-secret records, who vouches for the Apple
    /// sign-in — passed in.
    ///
    /// One function rather than a copy in the test: the gate is only worth
    /// anything if it runs the order and the sharing this builds — one token
    /// provider for every transport, one renewer per engine, one resolver
    /// client per import — and a hand-written duplicate would drift from it.
    ///
    /// `foregroundPollInterval` is the gate's alone: it slows every foreground
    /// poll so a change can only have arrived through the live socket.
    /// `liveHintsEnabled` exercises the existing polling-only path in the
    /// live-server negative control; public `make` always uses real hints.
    static func assemble(
        store: BabyStore,
        keychainBackend: any KeychainBackend,
        identity: HTTPIdentityClient,
        session: URLSession,
        defaults: UserDefaults?,
        apple: (any AppleSignInClient)? = nil,
        foregroundPollInterval: Duration? = nil,
        liveHintsEnabled: Bool = true,
        recoveryDiagnostics: @escaping (RefreshRecoveryEvent) -> Void = { _ in },
        syncDiagnostics: (any SyncDiagnosticSink)? = nil,
        liveActivityPush: LiveActivityPushWiring? = nil
    ) -> Graph {
        let keychain = KeychainSessionStore(backend: keychainBackend)

        // A box, so the token provider can read the *current* session key
        // without the coordinator and the client having to know about each
        // other.
        let sessionKey = CurrentSessionKey()
        let tokens = KeychainTokenProvider(keychain: keychain, key: { sessionKey.value })

        let client = DeferredFamilyClient { endpoint in
            HTTPFamilyClient(endpoint: endpoint, tokens: tokens, session: session)
        }

        #if canImport(AuthenticationServices) && canImport(UIKit)
        let coordinator = AccountCoordinator(
            store: store, keychain: keychain, identity: identity,
            apple: apple ?? SystemAppleSignInClient(), credentialState: SystemAppleCredentialStateClient(),
            defaults: defaults
        )
        #else
        let coordinator = AccountCoordinator(
            store: store, keychain: keychain, identity: identity,
            apple: apple ?? UnavailableAppleSignInClient(), credentialState: UnavailableAppleCredentialStateClient(),
            defaults: defaults
        )
        #endif

        // Leases through the store's own coordination lock, so a renewal
        // here and one started by the widget or an App Intent fence each
        // other rather than both rotating the refresh token.
        let leases = AppGroupFencedLease(container: store.container, lockURL: store.coordinationLockURL)
        let makeEngine: FamilySyncEngineFactory = { familyKey, endpoint in
            let renewer = FamilySessionRenewer(
                store: store, keychain: keychain, leases: leases, identity: identity, endpoint: endpoint, diagnostics: recoveryDiagnostics
            )
            let familyTokens = KeychainTokenProvider(keychain: keychain, key: { .family(familyKey) })
            return FamilySyncEngine(
                store: store,
                familyKey: familyKey,
                transport: FamilySyncHTTPClient(endpoint: endpoint, tokens: familyTokens, session: session, retryCredentials: { version in
                    try await renewer.credentialsForRetry(after: version, familyKey: familyKey)
                }),
                // One per engine: the resolver client remembers the import it
                // is resolving, and a conflict answer must name that import.
                importClient: HTTPImportResolverClient(endpoint: endpoint, tokens: tokens, session: session),
                renew: { try await renewer.renew(familyKey: familyKey) },
                prepareSession: { try await renewer.prepare(familyKey: familyKey) },
                // The same session and token provider as the sync transport:
                // one TLS trust decision, and the token a renewal just stored.
                live: liveHintsEnabled ? LiveHintChannel(
                    serverInfo: { try await identity.serverInfo(endpoint.origin) },
                    makeSocket: { callbacks in
                        FamilySyncWebSocket(
                            connector: URLSessionLiveSocketConnector(endpoint: endpoint, session: session),
                            tokens: tokens,
                            onHint: callbacks.onHint,
                            onConnected: callbacks.onConnected,
                            onUnauthorized: callbacks.onUnauthorized,
                            onStateChange: callbacks.onStateChange
                        )
                    }
                ) : nil,
                foregroundPollInterval: foregroundPollInterval,
                diagnostics: syncDiagnostics,
                // Ordinary sync can find this device's membership revoked
                // without the settings screen ever running (a background
                // poll, launch sync) — the shared realm gets purged either
                // way, and this device's saved invitation codes for that
                // family are exactly as stale as its records are. Ruling B
                // covers this path too, not only the ones the settings view
                // model itself drives.
                onRevoked: { realm in
                    try? InvitationCodeStore(backend: keychainBackend, realm: realm).removeAll()
                }
            )
        }

        let renewAccountSession: AccountSessionRenewal = { accountKey, endpoint in
            try await FamilySessionRenewer(
                store: store, keychain: keychain, leases: leases, identity: identity, endpoint: endpoint, diagnostics: recoveryDiagnostics
            ).renew(accountKey: accountKey)
        }

        let model = FamilySettingsViewModel(
            store: store, coordinator: coordinator, client: client, defaults: defaults,
            makeEngine: makeEngine, renewAccountSession: renewAccountSession,
            exportClientFactory: { endpoint in
                try? ExportClient(endpoint: endpoint, tokens: tokens, session: session)
            },
            exportCapabilityProbe: { endpoint in
                let info = try await identity.serverInfo(endpoint.origin)
                guard info.serverInstanceID == endpoint.serverInstanceID else {
                    throw EndpointIdentityError.instanceChanged(
                        known: endpoint.serverInstanceID, reported: info.serverInstanceID
                    )
                }
                return info.capabilities.contains("export-v1")
            },
            // Same backend as the session Keychain: this is a small, private
            // record, not something that needs its own access-group story.
            codeStoreBackend: keychainBackend
        )
        sessionKey.follow(coordinator)
        let push = liveActivityPush.map { wiring in
            makeLiveActivityPush(
                wiring, coordinator: coordinator, store: store, keychain: keychain, leases: leases,
                identity: identity, session: session, recoveryDiagnostics: recoveryDiagnostics
            )
        }
        return Graph(model: model, coordinator: coordinator, liveActivityPush: push)
    }

    /// The push coordinator and its launch driver, on the graph's own
    /// Keychain, renewal lease and URL session.
    ///
    /// Each registration client is bound to one family session: its token
    /// provider reads that family's Keychain item rather than "whichever
    /// session is current", and its renewer is the same fenced renewer the
    /// sync engine uses, so a renewal here and one by sync, the widget or an
    /// App Intent serialise on the account lease.
    private static func makeLiveActivityPush(
        _ wiring: LiveActivityPushWiring,
        coordinator account: AccountCoordinator,
        store: BabyStore,
        keychain: KeychainSessionStore,
        leases: AppGroupFencedLease,
        identity: HTTPIdentityClient,
        session: URLSession,
        recoveryDiagnostics: @escaping (RefreshRecoveryEvent) -> Void
    ) -> LiveActivityPushDriver {
        let makeClient: LiveActivityPushCoordinator.ClientFactory = wiring.makeClient ?? { context in
            let familyKey = context.familyKey
            let renewer = FamilySessionRenewer(
                store: store, keychain: keychain, leases: leases, identity: identity, endpoint: context.endpoint, diagnostics: recoveryDiagnostics
            )
            return LiveActivityPushClient(
                endpoint: context.endpoint,
                tokens: KeychainTokenProvider(keychain: keychain, key: { .family(familyKey) }),
                session: session,
                retryCredentials: { version in try await renewer.credentialsForRetry(after: version, familyKey: familyKey) },
                renew: { try await renewer.renew(familyKey: familyKey) }
            )
        }
        let push = LiveActivityPushCoordinator(
            events: wiring.events, store: wiring.store, makeClient: makeClient,
            background: wiring.background, isForeground: wiring.isForeground,
            // The process's one push state: `LiveActivityController` reads it
            // through `LiveCards.pushState`.
            pushState: wiring.pushState
        )
        let events = wiring.events
        let driver = LiveActivityPushDriver(
            coordinator: push,
            builtIn: wiring.builtIn,
            mode: { [weak account] in account?.mode ?? .localOnly },
            endpoint: { [weak account] in account?.confirmedEndpoint },
            probe: wiring.probe ?? { origin in try await identity.serverInfo(origin) },
            activitiesAllowed: { events.activitiesEnabled() },
            environment: wiring.environment,
            feedBreakMinutes: wiring.feedBreakMinutes,
            background: wiring.background
        )
        let previous = account.modeDidChange
        account.modeDidChange = { [weak driver] mode in
            previous?(mode)
            driver?.modeDidChange()
        }
        return driver
    }
}

#if canImport(UIKit)
/// Asks iOS for time to finish registration work begun in the background — a
/// push-to-start wake, a rotated token — and cancels the work if the time runs
/// out, so nothing keeps running into a suspension. The next launch, foreground
/// or token event picks it up again.
struct UIKitLiveActivityPushExecution: LiveActivityPushBackgroundExecution {
    @MainActor
    private final class Assertion {
        var identifier = UIBackgroundTaskIdentifier.invalid
        var work: Task<Void, Never>?
        func end() {
            work?.cancel()
            guard identifier != .invalid else { return }
            UIApplication.shared.endBackgroundTask(identifier)
            identifier = .invalid
        }
    }

    @MainActor
    func perform(_ work: @escaping @MainActor () async -> Void) async {
        let assertion = Assertion()
        assertion.identifier = UIApplication.shared.beginBackgroundTask(withName: "LiveActivityPushRegistration") {
            MainActor.assumeIsolated { assertion.end() }
        }
        let task = Task { await work() }
        assertion.work = task
        await withTaskCancellationHandler {
            await task.value
        } onCancel: {
            task.cancel()
        }
        assertion.work = nil
        assertion.end()
    }
}
#endif

/// What the settings model needs from the sync engine.
///
/// A seam rather than the concrete engine so the model's decisions — which
/// activation, when to poll, when to sync — are testable without a server.
@MainActor
public protocol FamilySyncDriving: AnyObject {
    var status: FamilySyncEngine.Status { get }
    var operationalStatus: SyncOperationalStatus { get }
    func activateAsOwner() async throws
    func activateAsJoiner() async throws
    func syncNow(_ reason: SyncReason) async
    func localWriteHappened()
    func startForegroundPolling()
    func stopForegroundPolling()
    /// Waits until nothing is in flight and refuses all later work. Called
    /// before anything that purges the family's realm.
    func stop() async
}

extension FamilySyncEngine: FamilySyncDriving {}

public extension FamilySyncDriving {
    var operationalStatus: SyncOperationalStatus { .inactive }
}

public extension FamilySettingsViewModel {
    var syncOperationalStatus: SyncOperationalStatus {
        guard case .shared = mode else { return .inactive }
        return engine?.operationalStatus ?? .inactive
    }
}

/// Builds the engine for one family session at one endpoint. Called again
/// whenever the session key changes, because the engine is bound to a key.
public typealias FamilySyncEngineFactory = @MainActor (FamilySessionKey, ConfirmedEndpoint) -> any FamilySyncDriving

/// Holds whichever session key is current, so the token provider reads it
/// fresh on every request.
///
/// A snapshot the coordinator pushes on every mode change, behind a lock,
/// rather than a read of the coordinator itself: the token is asked for from
/// the transports' nonisolated async code, and reaching into a main-actor
/// object from there traps.
final class CurrentSessionKey: @unchecked Sendable {
    private let lock = NSLock()
    private var current: KeychainTokenProvider.SessionLookup?

    var value: KeychainTokenProvider.SessionLookup? {
        lock.lock(); defer { lock.unlock() }
        return current
    }

    @MainActor
    func follow(_ coordinator: AccountCoordinator) {
        store(Self.lookup(for: coordinator.mode))
        let previous = coordinator.modeDidChange
        coordinator.modeDidChange = { [weak self] mode in
            self?.store(Self.lookup(for: mode))
            previous?(mode)
        }
    }

    private func store(_ lookup: KeychainTokenProvider.SessionLookup?) {
        lock.lock(); defer { lock.unlock() }
        current = lookup
    }

    private static func lookup(for mode: FamilySyncMode) -> KeychainTokenProvider.SessionLookup? {
        switch mode {
        case .shared(let key), .recovery(let key): .family(key)
        case .authenticated(let key): .account(key)
        // A signed-out account has no usable session, and must not have one
        // invented for it: that is the whole point of the credential-state
        // policy.
        case .signedOut, .localOnly, .endpointConfirmed: nil
        }
    }
}

/// Platforms without Sign in with Apple get a client that says so rather than
/// one that silently does nothing.
struct UnavailableAppleSignInClient: AppleSignInClient {
    enum Unavailable: Error { case appleSignInUnavailable }
    func requestProof(nonce: String) async throws -> AppleProof { throw Unavailable.appleSignInUnavailable }
}

struct UnavailableAppleCredentialStateClient: AppleCredentialStateClient {
    func credentialState(forUserIdentifier userIdentifier: String) async throws -> AppleCredentialState { .authorized }
}
