import BabyCore
import BabyStore
import Foundation
import Testing
@testable import BabySync

/// Never actually called: `promoteAfterMembership` is the only path these
/// tests exercise, and it touches neither Apple nor the identity server.
private final class UnusedIdentityClient: IdentityClient, @unchecked Sendable {
    func serverInfo(_ origin: ServerOrigin) async throws -> ServerInfo { fatalError("not used") }
    func challenge(_ endpoint: ConfirmedEndpoint) async throws -> String { fatalError("not used") }
    func exchangeApple(_ request: AppleExchangeRequest, at endpoint: ConfirmedEndpoint) async throws -> SessionBundle {
        fatalError("not used")
    }
}

private final class UnusedAppleSignInClient: AppleSignInClient, @unchecked Sendable {
    func requestProof(nonce: String) async throws -> AppleProof { fatalError("not used") }
}

private final class UnusedAppleCredentialStateClient: AppleCredentialStateClient, @unchecked Sendable {
    func credentialState(forUserIdentifier userIdentifier: String) async throws -> AppleCredentialState {
        fatalError("not used")
    }
}

/// Scripted to return one fixed `SessionBundle` from `exchangeApple`, so a
/// test can drive `signInWithApple()` into whatever mode that bundle implies
/// without a server.
private final class StubIdentityClient: IdentityClient, @unchecked Sendable {
    let bundle: SessionBundle
    init(bundle: SessionBundle) { self.bundle = bundle }
    func serverInfo(_ origin: ServerOrigin) async throws -> ServerInfo {
        ServerInfo(
            serverInstanceID: SessionFixture.instanceID, canonicalOrigin: origin.canonicalString, recoveryEpoch: 0,
            capabilities: ["identity-v1"], appleAuthorizationMode: "identity_token_only",
            maxItemsPerPage: 0, maxEncodedBytes: 0, sourceVersion: "", sourceURL: ""
        )
    }
    func challenge(_ endpoint: ConfirmedEndpoint) async throws -> String {
        "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
    }
    func exchangeApple(_ request: AppleExchangeRequest, at endpoint: ConfirmedEndpoint) async throws -> SessionBundle {
        bundle
    }
}

private final class StubAppleSignInClient: AppleSignInClient, @unchecked Sendable {
    func requestProof(nonce: String) async throws -> AppleProof {
        AppleProof(identityToken: "identity-token", authorizationCode: "auth-code", userIdentifier: "apple-user-001", fullName: nil)
    }
}

/// A Keychain that refuses to read for now — what a background launch sees
/// before the first unlock, or `errSecInteractionNotAllowed` at any time.
private final class LockedKeychainBackend: KeychainBackend, @unchecked Sendable {
    let inner: InMemoryKeychainBackend
    init(inner: InMemoryKeychainBackend) { self.inner = inner }
    func read(service: String, account: String) throws -> Data? {
        throw SessionStoreError.keychain(errSecInteractionNotAllowed)
    }
    func write(_ data: Data, service: String, account: String) throws { try inner.write(data, service: service, account: account) }
    func delete(service: String, account: String) throws { try inner.delete(service: service, account: account) }
}

@MainActor
@Suite("Restoring the family session across launches")
struct FamilySessionRestoreTests {
    private struct Harness {
        let store: BabyStore
        let backend: InMemoryKeychainBackend
        let defaults: UserDefaults
        let suiteName: String
    }

    private func harness() throws -> Harness {
        let suiteName = "FamilySessionRestoreTests.\(UUID().uuidString)"
        let defaults = try #require(UserDefaults(suiteName: suiteName))
        return Harness(
            store: try BabyStore.inMemory(dateProvider: TestDateProvider(.now)),
            backend: InMemoryKeychainBackend(), defaults: defaults, suiteName: suiteName
        )
    }

    private func coordinator(
        _ harness: Harness, backend: InMemoryKeychainBackend? = nil,
        identity: (any IdentityClient)? = nil, apple: (any AppleSignInClient)? = nil
    ) -> AccountCoordinator {
        let coordinator = AccountCoordinator(
            store: harness.store, keychain: KeychainSessionStore(backend: backend ?? harness.backend),
            identity: identity ?? UnusedIdentityClient(), apple: apple ?? UnusedAppleSignInClient(),
            credentialState: UnusedAppleCredentialStateClient(), defaults: harness.defaults
        )
        coordinator.confirmEndpoint(SessionFixture.endpoint)
        return coordinator
    }

    /// A recovery-only bundle for the same family, as a fresh Apple sign-in
    /// may receive after fresh Apple proof when no
    /// device continuity proof was accepted.
    private func recoveryOnlyBundle() -> SessionBundle {
        SessionBundle(
            accountID: SessionFixture.accountID, deviceID: SessionFixture.deviceID, recoveryEpoch: 0,
            accessToken: "recovery-access", refreshToken: "recovery-refresh",
            expiresAt: Date(timeIntervalSince1970: 20_000),
            deviceCredential: "device-credential-recovery",
            familyID: SessionFixture.familyID, isRecoveryOnly: true
        )
    }

    /// `promote(_:to:checkpointID:)` moves an existing account item onto the
    /// family key, so this account session must already exist before
    /// `promoteAfterMembership` runs.
    private func promote(_ coordinator: AccountCoordinator, _ harness: Harness) throws -> FamilySessionKey {
        let membership = FamilyMembership(
            familyID: SessionFixture.familyID, accountID: SessionFixture.accountID,
            deviceID: SessionFixture.deviceID, recoveryEpoch: 0
        )
        try harness.store.markAccountAuthenticated(
            SessionFixture.accountKey().leaseKey, credentialVersion: UUID()
        )
        try KeychainSessionStore(backend: harness.backend).saveAccount(
            SessionFixture.credentials(), for: SessionFixture.accountKey()
        )
        try coordinator.promoteAfterMembership(membership)
        return SessionFixture.familyKey()
    }

    @Test func restoresSharedModeAfterRelaunchOverTheSameKeychainAndDefaults() throws {
        let harness = try harness()
        defer { UserDefaults().removePersistentDomain(forName: harness.suiteName) }
        let first = coordinator(harness)
        let familyKey = try promote(first, harness)
        #expect(first.mode == .shared(familyKey))

        // Simulate a relaunch: a brand-new coordinator, same durable state.
        // Confirming the endpoint again is what a relaunched app does before
        // it can even ask the server anything, so the mode starts here at
        // `.endpointConfirmed`, not `.localOnly`.
        let relaunched = coordinator(harness)
        #expect(relaunched.mode == .endpointConfirmed(SessionFixture.endpoint))

        let restored = relaunched.restoreFamilySession()

        #expect(restored == familyKey)
        #expect(relaunched.mode == .shared(familyKey))
        #expect(relaunched.confirmedEndpoint == SessionFixture.endpoint)
        #expect(try harness.store.accountAllowsOrdinarySync(familyKey.accountSessionKey.leaseKey) == true)
    }

    @Test func returnsNilAndClearsTheRecordWhenTheKeychainItemIsGone() throws {
        let harness = try harness()
        defer { UserDefaults().removePersistentDomain(forName: harness.suiteName) }
        let first = coordinator(harness)
        let familyKey = try promote(first, harness)

        try KeychainSessionStore(backend: harness.backend).deleteFamily(familyKey)

        let relaunched = coordinator(harness)
        let restored = relaunched.restoreFamilySession()

        #expect(restored == nil)
        #expect(relaunched.mode == .endpointConfirmed(SessionFixture.endpoint))
        // The stale record must not resurrect the session on a later restore.
        #expect(relaunched.restoreFamilySession() == nil)
    }

    /// A Keychain that cannot be read right now is not a Keychain without
    /// the session. Treating it as one deleted the record, and a caregiver
    /// whose phone relaunched while locked silently stopped syncing for good.
    @Test func aKeychainErrorKeepsTheRecordAndStillRestores() throws {
        let harness = try harness()
        defer { UserDefaults().removePersistentDomain(forName: harness.suiteName) }
        let familyKey = try promote(coordinator(harness), harness)

        let locked = AccountCoordinator(
            store: harness.store, keychain: KeychainSessionStore(backend: LockedKeychainBackend(inner: harness.backend)),
            identity: UnusedIdentityClient(), apple: UnusedAppleSignInClient(),
            credentialState: UnusedAppleCredentialStateClient(), defaults: harness.defaults
        )
        #expect(locked.restoreFamilySession() == familyKey)
        #expect(locked.mode == .shared(familyKey))

        // And once the Keychain reads again, the next launch finds it too.
        let unlocked = coordinator(harness)
        #expect(unlocked.restoreFamilySession() == familyKey)
    }

    /// A `.shared` family moving into `.recovery` (session aged out, no
    /// device continuity accepted) must not leave behind a persisted record
    /// that a relaunch would use to silently jump back into `.shared`,
    /// bypassing the pending `completeRecovery`/`forfeitRecovery` decision.
    @Test func recoveryOnlySignInClearsAPersistedSharedRecord() async throws {
        let harness = try harness()
        defer { UserDefaults().removePersistentDomain(forName: harness.suiteName) }
        let promoter = coordinator(harness)
        _ = try promote(promoter, harness)

        let signingIn = coordinator(
            harness, identity: StubIdentityClient(bundle: recoveryOnlyBundle()), apple: StubAppleSignInClient()
        )
        try await signingIn.signInWithApple()
        #expect(signingIn.mode == .recovery(SessionFixture.familyKey()))

        let relaunched = coordinator(harness)
        let restored = relaunched.restoreFamilySession()

        #expect(restored == nil)
        #expect(relaunched.mode == .endpointConfirmed(SessionFixture.endpoint))
    }

    /// `completeRecovery()` moves back into `.shared`, so it must re-save the
    /// record — otherwise a relaunch right after recovering would drop back
    /// to `.endpointConfirmed` instead of resuming the recovered family.
    @Test func completingRecoverySavesTheRecordSoARelaunchRestoresShared() async throws {
        let harness = try harness()
        defer { UserDefaults().removePersistentDomain(forName: harness.suiteName) }
        let promoter = coordinator(harness)
        let familyKey = try promote(promoter, harness)

        let signingIn = coordinator(
            harness, identity: StubIdentityClient(bundle: recoveryOnlyBundle()), apple: StubAppleSignInClient()
        )
        try await signingIn.signInWithApple()
        #expect(signingIn.mode == .recovery(familyKey))

        signingIn.completeRecovery()
        #expect(signingIn.mode == .shared(familyKey))

        let relaunched = coordinator(harness)
        let restored = relaunched.restoreFamilySession()

        #expect(restored == familyKey)
        #expect(relaunched.mode == .shared(familyKey))
    }

    /// Sign in, create or join a family, then renew — the first thing the
    /// engine does with the new family. The promotion re-marked the durable
    /// account row with a random credential version instead of the stored
    /// credentials' own, so `beginRefreshAttempt` saw a mismatch and refused
    /// every renewal as `cancelled`: no promoted session could ever become
    /// family-scoped. Found by the two-caregiver live-server gate.
    @Test func aSessionPromotedAfterSignInCanBeRenewed() async throws {
        let harness = try harness()
        defer { UserDefaults().removePersistentDomain(forName: harness.suiteName) }
        let bundle = SessionBundle(
            accountID: SessionFixture.accountID, deviceID: SessionFixture.deviceID, recoveryEpoch: 0,
            accessToken: "account-access", refreshToken: "account-refresh",
            expiresAt: Date(timeIntervalSince1970: 20_000),
            deviceCredential: "device-credential-first", familyID: nil
        )
        let coordinator = coordinator(
            harness, identity: StubIdentityClient(bundle: bundle), apple: StubAppleSignInClient()
        )
        try await coordinator.signInWithApple()
        try coordinator.promoteAfterMembership(FamilyMembership(
            familyID: SessionFixture.familyID, accountID: SessionFixture.accountID,
            deviceID: SessionFixture.deviceID, recoveryEpoch: 0
        ))
        let familyKey = SessionFixture.familyKey()
        let keychain = KeychainSessionStore(backend: harness.backend)

        let refresher = SessionRefresher(
            store: harness.store, keychain: keychain,
            leases: AppGroupFencedLease(container: harness.store.container, lockURL: harness.store.coordinationLockURL)
        ) { current in
            #expect(current.refreshToken == "account-refresh")
            return SessionCredentials(
                accessToken: "family-access", refreshToken: "family-refresh",
                expiresAt: Date(timeIntervalSince1970: 30_000), recoveryEpoch: 0,
                deviceContinuity: current.deviceContinuity
            )
        }
        try await refresher.refresh(accountKey: familyKey.accountSessionKey, familyKey: familyKey)

        #expect(try keychain.loadFamily(familyKey)?.refreshToken == "family-refresh")
        #expect(try harness.store.accountAllowsOrdinarySync(familyKey.leaseKey))
    }

    /// A signed-in account with no family yet renews its own session through
    /// the same route, lease and attempt marker — so waiting past the
    /// 10-minute access token before creating or joining is not a dead end.
    @Test func aPreFamilySessionCanBeRenewed() async throws {
        let harness = try harness()
        defer { UserDefaults().removePersistentDomain(forName: harness.suiteName) }
        let bundle = SessionBundle(
            accountID: SessionFixture.accountID, deviceID: SessionFixture.deviceID, recoveryEpoch: 0,
            accessToken: "account-access", refreshToken: "account-refresh",
            expiresAt: Date(timeIntervalSince1970: 20_000),
            deviceCredential: "device-credential-first", familyID: nil
        )
        let coordinator = coordinator(
            harness, identity: StubIdentityClient(bundle: bundle), apple: StubAppleSignInClient()
        )
        let accountKey = try await coordinator.signInWithApple(displayName: "Alex")
        let keychain = KeychainSessionStore(backend: harness.backend)

        let refresher = SessionRefresher(
            store: harness.store, keychain: keychain,
            leases: AppGroupFencedLease(container: harness.store.container, lockURL: harness.store.coordinationLockURL)
        ) { current in
            #expect(current.refreshToken == "account-refresh")
            return SessionCredentials(
                accessToken: "account-access-2", refreshToken: "account-refresh-2",
                expiresAt: Date(timeIntervalSince1970: 30_000), recoveryEpoch: 0,
                deviceContinuity: current.deviceContinuity
            )
        }
        try await refresher.refreshAccount(accountKey: accountKey)

        #expect(try keychain.loadAccount(accountKey)?.refreshToken == "account-refresh-2")
        // And the renewed session still promotes and renews as a family's.
        try coordinator.promoteAfterMembership(FamilyMembership(
            familyID: SessionFixture.familyID, accountID: SessionFixture.accountID,
            deviceID: SessionFixture.deviceID, recoveryEpoch: 0
        ))
        #expect(try keychain.loadFamily(SessionFixture.familyKey())?.refreshToken == "account-refresh-2")
    }
}
