import BabyCore
import BabyProtocol
import BabyStore
import BabySync
import Foundation
import Testing
@testable import SettingsFeature

/// The identity and sync server as one phone sees it: which access tokens it
/// still accepts, and which refresh tokens it still rotates.
///
/// Tokens are the whole point here. The bug this suite pins down was not a
/// wrong status or a missing call; it was a sign-in that filed its new tokens
/// where nothing read them, so every later request still carried the old one.
final class TokenCheckingServer: @unchecked Sendable {
    private let lock = NSLock()
    private var acceptedAccess: Set<String> = ["access"]
    private var rotations: [String: SessionCredentials] = [:]
    private var seen: [String?] = []

    // `FamilySettingsFixture`'s, spelled out: that fixture is main-actor
    // isolated and this is read from the transports' own contexts.
    let instanceID = UUID(uuidString: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    let familyID = UUID(uuidString: "b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!

    /// Every access token a sync or snapshot request carried, in order.
    var accessTokensSeen: [String?] { locked { seen } }

    /// The old session has been invalidated: nothing this
    /// session holds is accepted, and refreshing it needs fresh Apple proof.
    func ageOutEverySession() {
        locked {
            acceptedAccess = []
            rotations = [:]
        }
    }

    func accept(access: String) { locked { _ = acceptedAccess.insert(access) } }

    /// Ten minutes later: `access` has lapsed, and `refresh` rotates into
    /// `renewed`.
    func expire(access: String, rotating refresh: String, into renewed: SessionCredentials) {
        locked {
            acceptedAccess.remove(access)
            rotations[refresh] = renewed
        }
    }

    func authorize(_ token: String?) throws {
        try locked {
            seen.append(token)
            guard let token, acceptedAccess.contains(token) else { throw SyncErrorCode.accessExpired }
        }
    }

    func rotate(_ refreshToken: String) throws -> SessionCredentials {
        try locked {
            // `/v1/auth/refresh` on a session past its absolute age.
            guard let renewed = rotations.removeValue(forKey: refreshToken) else {
                throw HTTPClientError.status(401, "reauthentication_required")
            }
            acceptedAccess.insert(renewed.accessToken)
            return renewed
        }
    }

    var cursor: Cursor { Cursor(serverInstanceID: instanceID, recoveryEpoch: 0, familyID: familyID, sequence: 1) }

    private func locked<T>(_ body: () throws -> T) rethrows -> T {
        lock.lock(); defer { lock.unlock() }
        return try body()
    }
}

/// The sync routes, reading the token exactly the way the app's transports do.
struct TokenCheckingTransport: FamilySyncTransport {
    let server: TokenCheckingServer
    let tokens: KeychainTokenProvider

    func sync(_ request: SyncRequest) async throws -> SyncResponse {
        try server.authorize(await tokens.currentAccessToken())
        return SyncResponse(
            serverInstanceID: server.instanceID, recoveryEpoch: 0, outcomes: [], changes: [],
            highWater: server.cursor, nextPageToken: nil
        )
    }

    func snapshot(pageToken: String?) async throws -> SnapshotResponse {
        try server.authorize(await tokens.currentAccessToken())
        return SnapshotResponse(
            serverInstanceID: server.instanceID, recoveryEpoch: 0, snapshotID: UUID(),
            highWater: server.cursor, entities: [], nextPageToken: nil
        )
    }
}

struct TokenCheckingRefresher: SessionRefreshing {
    let server: TokenCheckingServer
    func refresh(refreshToken: String, at endpoint: ConfirmedEndpoint) async throws -> SessionCredentials {
        try server.rotate(refreshToken)
    }
}

/// A joiner never imports.
struct UnusedImportClient: ImportResolverClient {
    struct NotUsed: Error {}
    func resolve(_ request: ImportResolveRequest) async throws -> ImportResolveResponse { throw NotUsed() }
    func resolveConflict(
        conflictID: UUID, resolverRevision: Int64, resolution: ImportConflictResolution, commandID: UUID
    ) async throws -> ImportResolveResponse { throw NotUsed() }
    func conflictPage(importID: UUID, pageToken: String?) async throws -> ImportResolveResponse { throw NotUsed() }
}

@MainActor
@Suite("Signing in again after session invalidation")
struct ReauthenticationWiringTests {
    /// The self-hosted server refuses an invalidated session; then
    /// only a fresh Apple proof is accepted. Each phone must then offer the
    /// sign-in, and one sign-in must be enough to sync again — and keep
    /// syncing past the next 10-minute access token.
    ///
    /// It used to be a permanent stop: the sign-in filed its tokens under the
    /// account key while every family reader looked under the family key, the
    /// failed refresh's "uncertain" marker refused every later renewal, and
    /// the status line said "sign in with Apple again" without the button.
    @Test func signingInAgainAfterTheAbsoluteSessionAgeRestoresSync() async throws {
        let server = TokenCheckingServer()
        var engines: [FamilySyncEngine] = []
        var fixture: FamilySettingsFixture!
        let makeEngine: FamilySyncEngineFactory = { familyKey, endpoint in
            let sessionKey = CurrentSessionKey()
            sessionKey.follow(fixture.coordinator)
            let tokens = KeychainTokenProvider(keychain: fixture.keychain, key: { sessionKey.value })
            let renewer = FamilySessionRenewer(
                store: fixture.store, keychain: fixture.keychain,
                leases: AppGroupFencedLease(container: fixture.store.container, lockURL: fixture.store.coordinationLockURL),
                identity: TokenCheckingRefresher(server: server), endpoint: endpoint
            )
            let engine = FamilySyncEngine(
                store: fixture.store, familyKey: familyKey,
                transport: TokenCheckingTransport(server: server, tokens: tokens),
                importClient: UnusedImportClient(),
                renew: { try await renewer.renew(familyKey: familyKey) }
            )
            engines.append(engine)
            return engine
        }
        fixture = try FamilySettingsFixture.make(familyInBundle: false, makeEngine: makeEngine)
        let model = fixture.model

        // Joined, and syncing on the promoted session.
        try await model.signInWithApple()
        try await model.acceptInvitation(
            InvitationIntent(origin: FamilySettingsFixture.origin, token: "abcdefghijklmnop0123456789")
        )
        let engine = try #require(engines.last)
        #expect(engine.status.isUpToDate)

        // A day later.
        server.ageOutEverySession()
        await engine.syncNow(.foreground)
        let line = try #require(model.syncStatusLine)
        #expect(line.offersSignIn)
        #expect(line.text == FamilySyncStatusCopy.signInAgain)

        // The button, from `.shared` mode. The server answers with a session
        // for the same family.
        fixture.identity.bundle = SessionBundle(
            accountID: FamilySettingsFixture.accountID, deviceID: FamilySettingsFixture.deviceID, recoveryEpoch: 0,
            accessToken: "fresh-access", refreshToken: "fresh-refresh",
            expiresAt: Date(timeIntervalSince1970: 20_000),
            deviceCredential: "device-credential", familyID: FamilySettingsFixture.familyID
        )
        server.accept(access: "fresh-access")
        try await model.signInWithApple()

        let current = try #require(model.engine as? FamilySyncEngine)
        #expect(current.status.isUpToDate)
        #expect(server.accessTokensSeen.last == "fresh-access")

        // Ten minutes after that, the fresh session renews like any other.
        server.expire(access: "fresh-access", rotating: "fresh-refresh", into: SessionCredentials(
            accessToken: "renewed-access", refreshToken: "renewed-refresh",
            expiresAt: Date(timeIntervalSince1970: 30_000), recoveryEpoch: 0,
            deviceContinuity: DeviceContinuity(deviceID: FamilySettingsFixture.deviceID, credential: "")
        ))
        await current.syncNow(.foreground)

        #expect(current.status.isUpToDate)
        #expect(server.accessTokensSeen.last == "renewed-access")
    }
}

extension FamilySyncEngine.Status {
    var isUpToDate: Bool {
        if case .upToDate = self { return true }
        return false
    }
}
