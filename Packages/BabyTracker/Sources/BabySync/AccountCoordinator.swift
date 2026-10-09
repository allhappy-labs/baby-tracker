import BabyCore
import BabyStore
import Foundation

public enum FamilySyncMode: Sendable, Equatable {
    case localOnly
    case endpointConfirmed(ConfirmedEndpoint)
    case authenticated(AccountSessionKey)
    case shared(FamilySessionKey)
    case recovery(FamilySessionKey)
    case signedOut(AccountSessionKey)
}

/// The `BabySync`-side adapter that carries promotion state into `BabyStore`.
///
/// It accepts session keys here and passes only neutral origin/instance/
/// account/device/family/epoch fields and a `RealmID` onwards, so `BabyStore`
/// never sees a Keychain session type.
@MainActor
public final class RealmTransitionAdapter {
    public private(set) var promotionCheckpointID = UUID()
    public private(set) var membershipReadyKey: FamilySessionKey?
    private let store: BabyStore

    public init(store: BabyStore) { self.store = store }

    public var activeRealm: RealmID { store.activeRealm }

    /// `credentialVersion` is the version of the credentials being promoted
    /// — the Keychain item does not change, only the key it is filed under —
    /// and it must be exactly that one. The durable row's version is what
    /// `beginRefreshAttempt` checks the stored credentials against; a fresh
    /// UUID here made every later renewal of the promoted session refuse
    /// itself as `cancelled`, so no device ever got a family-scoped session.
    public func saveCredentialPromotionCheckpoint(
        accountKey: AccountSessionKey,
        familyKey: FamilySessionKey,
        credentialVersion: UUID
    ) throws {
        promotionCheckpointID = UUID()
        try store.markAccountAuthenticated(accountKey.leaseKey, credentialVersion: credentialVersion)
    }

    /// Records that membership exists. It deliberately does *not* activate the
    /// shared realm: P3.4 performs the single realm activation once mapping and
    /// canonical babies are established, so a half-onboarded family can never
    /// be the realm the app is writing into.
    public func markMembershipReady(familyKey: FamilySessionKey) {
        membershipReadyKey = familyKey
    }
}

/// Owns endpoint confirmation, Apple proof, session storage, and the
/// credential-state policy.
@MainActor
public final class AccountCoordinator {
    public private(set) var mode: FamilySyncMode = .localOnly {
        didSet { modeDidChange?(mode) }
    }
    /// Told of every mode change, on the main actor. What lets a reader that
    /// is *not* on the main actor — a transport fetching its token — keep a
    /// snapshot of the current session instead of reaching into this object
    /// from the wrong isolation.
    public var modeDidChange: (@MainActor (FamilySyncMode) -> Void)?
    public private(set) var confirmedEndpoint: ConfirmedEndpoint?
    /// Apple's opaque user ID for the signed-in account, held only in Keychain.
    public private(set) var appleUserIdentifier: String?
    /// Apple returns a name on the FIRST authorization only. A second device,
    /// or a re-authorization, gets nothing — so this is nil far more often
    /// than not, and the display name must then be confirmed by the user
    /// rather than derived from an email or a subject claim.
    public private(set) var pendingAppleFullName: String?

    private let store: BabyStore
    private let keychain: any SecureSessionStore
    private let identity: any IdentityClient
    private let apple: any AppleSignInClient
    private let credentialState: any AppleCredentialStateClient
    private let transitions: RealmTransitionAdapter
    private let defaults: UserDefaults

    public init(
        store: BabyStore,
        keychain: any SecureSessionStore,
        identity: any IdentityClient,
        apple: any AppleSignInClient,
        credentialState: any AppleCredentialStateClient,
        defaults: UserDefaults? = nil
    ) {
        self.store = store
        self.keychain = keychain
        self.identity = identity
        self.apple = apple
        self.credentialState = credentialState
        transitions = RealmTransitionAdapter(store: store)
        self.defaults = defaults
            ?? UserDefaults(suiteName: BabySchema.appGroupID)
            ?? .standard
    }

    // MARK: Endpoint

    public func confirmEndpoint(_ endpoint: ConfirmedEndpoint) {
        confirmedEndpoint = endpoint
        if case .localOnly = mode { mode = .endpointConfirmed(endpoint) }
    }

    // MARK: Apple sign-in

    /// Signs in with a fresh Apple assertion bound to the confirmed endpoint's
    /// origin and instance.
    ///
    /// `displayName` is the name the user confirmed *before* signing in: the
    /// server refuses an account's first sign-in without one
    /// (`display_name_required`), and asking afterwards would mean a second
    /// Apple prompt. It is sent on every sign-in, which the server accepts.
    ///
    /// The server's `apple_authorization_mode` is read first: a self-hosted
    /// server (`identity_token_only`) refuses Apple's authorization code, a
    /// hosted one requires it.
    ///
    /// The exchange offers the device-continuity proof only when this device
    /// already holds one for *this* account at *this* origin and instance. A
    /// server that refuses it returns `device_continuity_invalid`, which is
    /// surfaced rather than worked around: silently allocating a new device
    /// would strand the old binding and its frozen mutations.
    @discardableResult
    public func signInWithApple(displayName: String? = nil) async throws -> AccountSessionKey {
        guard let endpoint = confirmedEndpoint else { throw FamilySyncError.endpointConfirmationRequired }
        let info = try await identity.serverInfo(endpoint.origin)
        guard info.serverInstanceID == endpoint.serverInstanceID else {
            throw EndpointIdentityError.instanceChanged(known: endpoint.serverInstanceID, reported: info.serverInstanceID)
        }
        let challenge = try await identity.challenge(endpoint)
        let nonce = appleNonce(
            canonicalOrigin: endpoint.origin.canonicalString,
            serverInstanceID: endpoint.serverInstanceID,
            challenge: challenge
        )
        let proof = try await apple.requestProof(nonce: nonce)
        appleUserIdentifier = proof.userIdentifier
        pendingAppleFullName = proof.fullName

        let request = AppleExchangeRequest(
            challenge: challenge,
            identityToken: proof.identityToken,
            authorizationCode: AppleExchangeRequest.authorizationCode(for: info.appleAuthorizationMode, proof: proof),
            installationID: try keychain.installationID(),
            displayName: displayName,
            deviceContinuity: try existingContinuity(at: endpoint, accountID: previousAccountID)?.continuity
        )
        let bundle: SessionBundle
        do {
            bundle = try await identity.exchangeApple(request, at: endpoint)
        } catch HTTPClientError.status(_, "display_name_required") {
            // Only reachable without a name; the screen asks for one.
            throw FamilySyncError.displayNameConfirmationRequired
        }
        return try adoptExchangedSession(bundle, at: endpoint)
    }

    /// Everything a sign-in does once the server has answered: file the
    /// credentials, open the durable gate, and move to the mode the bundle
    /// describes.
    ///
    /// The credentials go where the mode they lead to reads them. A bundle
    /// that names a family — including a re-authentication after revocation
    /// or recovery — is filed under
    /// the family key, because in `.shared` and `.recovery` the token provider
    /// and the renewer look nowhere else. Filed under the account key, as they
    /// once were, the new tokens were never read: every request still carried
    /// the aged-out ones, and the device never synced again.
    ///
    /// The durable gate records the very credential version just filed, and
    /// forgets any refresh attempt the aged-out session left uncertain — see
    /// `markAccountSignedIn`. Either one left stale refuses every later
    /// renewal of the new session.
    ///
    /// Separate from `signInWithApple` so the live-server gate can hand it a
    /// session the server's identity-test adapter minted — it cannot ask
    /// Apple — and still install it exactly the way a real sign-in does.
    @discardableResult
    func adoptExchangedSession(_ bundle: SessionBundle, at endpoint: ConfirmedEndpoint) throws -> AccountSessionKey {
        let accountKey = AccountSessionKey(
            canonicalOrigin: endpoint.origin.canonicalString,
            serverInstanceID: endpoint.serverInstanceID,
            accountID: bundle.accountID,
            deviceID: bundle.deviceID,
            recoveryEpoch: bundle.recoveryEpoch
        )
        let continuity = try resolveContinuity(bundle: bundle, accountKey: accountKey)
        let credentials = SessionCredentials(
            accessToken: bundle.accessToken,
            refreshToken: bundle.refreshToken,
            expiresAt: bundle.expiresAt,
            recoveryEpoch: bundle.recoveryEpoch,
            deviceContinuity: continuity
        )
        let familyKey = bundle.familyID.map { familyID in
            FamilySessionKey(
                canonicalOrigin: accountKey.canonicalOrigin,
                serverInstanceID: accountKey.serverInstanceID,
                familyID: familyID,
                accountID: accountKey.accountID,
                deviceID: accountKey.deviceID,
                recoveryEpoch: accountKey.recoveryEpoch
            )
        }
        try store.markAccountSignedIn(accountKey.leaseKey, credentialVersion: credentials.credentialVersion) {
            if let familyKey {
                try keychain.saveFamily(credentials, for: familyKey)
                // `saveAccount` files the continuity proof as a side effect;
                // the family item does not, and the next sign-in offers it.
                try keychain.saveDeviceContinuity(
                    DeviceContinuityBinding(accountID: accountKey.accountID, continuity: continuity),
                    for: accountKey.continuityKey
                )
            } else {
                try keychain.saveAccount(credentials, for: accountKey)
            }
        }

        if let familyKey {
            if bundle.isRecoveryOnly {
                mode = .recovery(familyKey)
                // A recovery-only sign-in means the previous `.shared`
                // session's record (if any) is no longer trustworthy: the
                // pending recovery decision must not be silently bypassed by
                // a relaunch finding a stale "just resume .shared" record.
                FamilySessionRecordStore.clear(from: defaults)
            } else {
                mode = .shared(familyKey)
                // A returning family session (e.g. after fresh Apple proof)
                // must be restorable after the next relaunch, not just the first
                // `promoteAfterMembership`.
                FamilySessionRecordStore.save(FamilySessionRecord(endpoint: endpoint, familyKey: familyKey), to: defaults)
            }
        } else {
            mode = .authenticated(accountKey)
        }
        return accountKey
    }

    // MARK: Credential state

    /// Applies Apple's verdict on the stored credential.
    ///
    /// On `.revoked`/`.notFound` this account is denied ordinary sync *first*,
    /// durably, so an in-flight completion or an extension cannot commit after
    /// the decision. No `/logout` is sent and no request uses the erased
    /// session: `/logout` revokes the device binding, which is for explicit
    /// disconnect and account deletion, not for a credential the user revoked
    /// in Apple's own settings. Hosted deployments get server-side revocation
    /// from Apple's server notifications; self-host stops locally and explains
    /// the manual step.
    @discardableResult
    public func refreshAppleCredentialState() async throws -> AppleCredentialState {
        guard let userIdentifier = appleUserIdentifier else { return .authorized }
        let state = try await credentialState.credentialState(forUserIdentifier: userIdentifier)
        guard state.endsAuthorization else { return state }
        guard let accountKey = currentAccountKey else { return state }

        try store.markAccountSignedOut(accountKey.leaseKey)
        try keychain.invalidateSessionsRetainingContinuity(for: accountKey)
        try purgeSharedRealm(boundFamilyRealm)
        FamilySessionRecordStore.clear(from: defaults)
        mode = .signedOut(accountKey)
        return state
    }

    /// Whether the app or an extension may make an ordinary sync request now.
    public func allowsOrdinarySync() throws -> Bool {
        guard let accountKey = currentAccountKey else { return false }
        if case .signedOut = mode { return false }
        return try store.accountAllowsOrdinarySync(accountKey.leaseKey)
    }

    // MARK: Membership

    /// Moves the pre-family session onto its family key and records that
    /// membership is ready. Realm activation is P3.4's, not this.
    public func promoteAfterMembership(_ membership: FamilyMembership) throws {
        guard let endpoint = confirmedEndpoint else { throw FamilySyncError.endpointConfirmationRequired }
        let accountKey = AccountSessionKey(
            canonicalOrigin: endpoint.origin.canonicalString,
            serverInstanceID: endpoint.serverInstanceID,
            accountID: membership.accountID,
            deviceID: membership.deviceID,
            recoveryEpoch: membership.recoveryEpoch
        )
        let familyKey = FamilySessionKey(
            canonicalOrigin: accountKey.canonicalOrigin,
            serverInstanceID: accountKey.serverInstanceID,
            familyID: membership.familyID,
            accountID: accountKey.accountID,
            deviceID: accountKey.deviceID,
            recoveryEpoch: membership.recoveryEpoch
        )
        // The item being moved; on a resumed promotion it may already be
        // filed under the family key. Neither existing is `promote`'s error
        // to report (`missingSource`), so any version will do until then.
        let promoted = try keychain.loadAccount(accountKey) ?? keychain.loadFamily(familyKey)
        try transitions.saveCredentialPromotionCheckpoint(
            accountKey: accountKey, familyKey: familyKey,
            credentialVersion: promoted?.credentialVersion ?? UUID()
        )
        try keychain.promote(accountKey, to: familyKey, checkpointID: transitions.promotionCheckpointID)
        transitions.markMembershipReady(familyKey: familyKey)
        mode = .shared(familyKey)
        FamilySessionRecordStore.save(FamilySessionRecord(endpoint: endpoint, familyKey: familyKey), to: defaults)
    }

    /// What a relaunch calls before anything else: without this, every launch
    /// starts `.localOnly` even though the Keychain still holds the family's
    /// credentials, so a joined caregiver would silently stop syncing after
    /// force-quitting the app.
    ///
    /// The App Group record alone is not trusted to resurrect the session —
    /// it is non-secret and could be stale (say, the family credentials were
    /// revoked elsewhere) — so it only points at a Keychain lookup that must
    /// itself confirm the credentials still exist.
    ///
    /// Only an item that is actually *missing* clears the record. A Keychain
    /// that cannot answer right now — a launch before first unlock,
    /// `errSecInteractionNotAllowed` — says nothing about the session, and
    /// clearing on it made a caregiver whose phone relaunched while locked
    /// stop syncing for good. The session is restored as recorded; if its
    /// credentials really are unreadable, the requests that need them fail
    /// and retry like any other transient failure.
    @discardableResult
    public func restoreFamilySession() -> FamilySessionKey? {
        guard let record = FamilySessionRecordStore.load(from: defaults) else { return nil }
        do {
            guard try keychain.loadFamily(record.familyKey) != nil else {
                FamilySessionRecordStore.clear(from: defaults)
                return nil
            }
        } catch {
            AppLog.sync.error("family session restored without reading its credentials: \(String(describing: error), privacy: .public)")
        }
        confirmedEndpoint = record.endpoint
        mode = .shared(record.familyKey)
        return record.familyKey
    }

    /// Explicit disconnect or account deletion: the one path that does revoke
    /// the device binding.
    public func revokeDeviceBindingAndSessions(_ familyKey: FamilySessionKey, client: any FamilyClient) async throws {
        try await client.logout()
        try finishFamilyCleanup(familyKey)
    }

    /// A successful server leave already invalidates family access. Logging
    /// out with that former membership can return 401, so finish the device's
    /// cleanup without making another authenticated request.
    public func finishFamilyCleanup(_ familyKey: FamilySessionKey) throws {
        try keychain.deleteFamily(familyKey)
        try keychain.deleteDeviceContinuity(for: familyKey.accountSessionKey.continuityKey)
        try purgeSharedRealm(familyKey.realmID)
        FamilySessionRecordStore.clear(from: defaults)
        mode = confirmedEndpoint.map { .endpointConfirmed($0) } ?? .localOnly
    }

    /// Recovery consumes the claim and restores the owner's own membership.
    public func completeRecovery() {
        guard case .recovery(let familyKey) = mode else { return }
        mode = .shared(familyKey)
        // Re-entering `.shared` must be restorable after the next relaunch,
        // exactly like `promoteAfterMembership`'s and `signInWithApple`'s own
        // `.shared` transitions.
        if let endpoint = confirmedEndpoint {
            FamilySessionRecordStore.save(FamilySessionRecord(endpoint: endpoint, familyKey: familyKey), to: defaults)
        }
    }

    /// Forfeiture consumes the claim and the family slot without recovering
    /// anything: deletion stays scheduled and the account keeps no family.
    public func forfeitRecovery() {
        guard case .recovery(let familyKey) = mode else { return }
        mode = .authenticated(familyKey.accountSessionKey)
        // The family is gone for this account: no record should survive to
        // resurrect it on a later relaunch.
        FamilySessionRecordStore.clear(from: defaults)
    }

    // MARK: Internals

    public var currentAccountKey: AccountSessionKey? {
        switch mode {
        case .authenticated(let key), .signedOut(let key): key
        case .shared(let key), .recovery(let key): key.accountSessionKey
        case .localOnly, .endpointConfirmed: nil
        }
    }

    private var previousAccountID: UUID? { currentAccountKey?.accountID }

    private func existingContinuity(at endpoint: ConfirmedEndpoint, accountID: UUID?) throws -> DeviceContinuityBinding? {
        guard let accountID, let deviceID = currentAccountKey?.deviceID else { return nil }
        let key = ContinuityKey(
            canonicalOrigin: endpoint.origin.canonicalString,
            serverInstanceID: endpoint.serverInstanceID,
            accountID: accountID,
            deviceID: deviceID
        )
        return try keychain.loadDeviceContinuity(for: key)
    }

    private func resolveContinuity(bundle: SessionBundle, accountKey: AccountSessionKey) throws -> DeviceContinuity {
        if let credential = bundle.deviceCredential {
            return DeviceContinuity(deviceID: bundle.deviceID, credential: credential)
        }
        guard let existing = try keychain.loadDeviceContinuity(for: accountKey.continuityKey),
              existing.accountID == accountKey.accountID else {
            throw FamilySyncError.deviceContinuityRequired
        }
        return existing.continuity
    }

    /// The realm this session's family owns, if it has one.
    ///
    /// Deliberately not `store.activeRealm`: P3.4 owns the single realm
    /// activation, so a family can exist — with rows already imported — while
    /// the active realm is still `.local`. Purging the active realm here would
    /// silently miss exactly that case.
    private var boundFamilyRealm: RealmID? {
        switch mode {
        case .shared(let key), .recovery(let key): key.realmID
        case .localOnly, .endpointConfirmed, .authenticated, .signedOut: nil
        }
    }

    /// D10: the shared realm's rows go and are never silently recreated as a
    /// local duplicate. The permanent local realm itself stays exactly as it
    /// was — it is the user's own tracking, and it was never the family's.
    private func purgeSharedRealm(_ realm: RealmID?) throws {
        guard let realm, case .shared = realm else { return }
        try store.purgeSharedRealm(realm)
    }
}
