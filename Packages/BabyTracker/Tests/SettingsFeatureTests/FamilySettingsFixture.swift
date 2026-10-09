import BabyCore
import BabyStore
import BabySync
import Foundation
import Testing
@testable import SettingsFeature

/// Scripted stand-ins for the server, Apple, and the Keychain.
///
/// They record what was asked of them, which is how the tests prove the
/// *negative* requirements — that no `/logout` was sent, that no request used
/// an erased session, that ordinary sync was refused.
@MainActor
final class FakeFamilyClient: FamilyClient, @unchecked Sendable {
    var membership: FamilyMembership
    var profileValue: CaregiverProfile
    var membersValue: [FamilyMember]
    var recoveredMembers: [FamilyMember] = []
    var invitation = FamilyInvitation(
        id: UUID(uuidString: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")!,
        shareURL: URL(string: "https://sync.example.test/invite?token=abcdefghijklmnop0123456789")!,
        role: .caregiver,
        expiresAt: Date(timeIntervalSince1970: 10_000)
    )
    /// What `pendingInvitations()` answers with. Mutable so a test can seed
    /// the list the way a scripted server response would.
    var pending: [PendingInvitation] = []
    var pendingInvitationsErrors: [any Error] = []
    /// Answers from `createInvitation`, in order — falls back to `invitation`
    /// once exhausted, so tests that mint several invitations can give each
    /// one a distinct id.
    var invitationsToReturn: [FamilyInvitation] = []
    var createInvitationErrors: [any Error] = []
    var revokeInvitationErrors: [any Error] = []
    var previewResult = InvitationPreview(familyName: "Family", expiresAt: Date(timeIntervalSince1970: 10_000))
    var previewErrors: [any Error] = []
    private(set) var previewedTokens: [String] = []

    private(set) var logoutCount = 0
    private(set) var performed: [FamilyLifecycleCommand] = []
    private(set) var scheduledDeletion = false
    private(set) var recoveryClaimConsumed = false
    private(set) var revokedInvitations: [UUID] = []

    init(membership: FamilyMembership, profile: CaregiverProfile, members: [FamilyMember]) {
        self.membership = membership
        profileValue = profile
        membersValue = members
    }

    /// Thrown, in order, by the next calls — a lapsed session, say.
    var createFamilyErrors: [any Error] = []
    var confirmJoinErrors: [any Error] = []
    private(set) var createFamilyCalls = 0

    func createFamily(name: String, displayName: String) async throws -> FamilyMembership {
        createFamilyCalls += 1
        if !createFamilyErrors.isEmpty { throw createFamilyErrors.removeFirst() }
        return membership
    }
    func confirmJoin(token: String, displayName: String) async throws -> FamilyMembership {
        if !confirmJoinErrors.isEmpty { throw confirmJoinErrors.removeFirst() }
        return membership
    }
    func members() async throws -> [FamilyMember] { membersValue }
    var profileError: (any Error)?
    func profile() async throws -> CaregiverProfile {
        if let profileError { throw profileError }
        return profileValue
    }

    func updateDisplayName(_ name: String) async throws -> CaregiverProfile {
        profileValue = CaregiverProfile(accountID: profileValue.accountID, displayName: name, role: profileValue.role)
        return profileValue
    }

    private(set) var invitedRoles: [FamilyRole] = []
    /// Mints an invitation and files it under `pending`, the way the real
    /// server would — so a test only has to override `pending` for the
    /// scenarios where the server's list disagrees with what this device
    /// just created (an invitation revoked elsewhere, one that fell off the
    /// list on reload).
    func createInvitation(role: FamilyRole) async throws -> FamilyInvitation {
        invitedRoles.append(role)
        if !createInvitationErrors.isEmpty { throw createInvitationErrors.removeFirst() }
        let created = invitationsToReturn.isEmpty ? invitation : invitationsToReturn.removeFirst()
        pending.append(PendingInvitation(id: created.id, role: created.role, expiresAt: created.expiresAt, createdAt: Date()))
        return created
    }
    func pendingInvitations() async throws -> [PendingInvitation] {
        if !pendingInvitationsErrors.isEmpty { throw pendingInvitationsErrors.removeFirst() }
        return pending
    }
    func revokeInvitation(_ invitationID: UUID) async throws {
        revokedInvitations.append(invitationID)
        if !revokeInvitationErrors.isEmpty { throw revokeInvitationErrors.removeFirst() }
        pending.removeAll { $0.id == invitationID }
    }
    func previewInvitation(token: String) async throws -> InvitationPreview {
        previewedTokens.append(token)
        if !previewErrors.isEmpty { throw previewErrors.removeFirst() }
        return previewResult
    }
    private(set) var leaveCount = 0
    func leaveFamily() async throws {
        leaveCount += 1
        membersValue = []
    }
    func perform(_ command: FamilyLifecycleCommand) async throws {
        performed.append(command)
        switch command {
        case .removeMember(let id):
            membersValue.removeAll { $0.id == id }
        case .transferOwnership(let id):
            // The server demotes the old owner and promotes the successor, so
            // a later `profile()` must report the caller as a caregiver.
            membersValue = membersValue.map {
                FamilyMember(id: $0.id, displayName: $0.displayName, role: $0.id == id ? "owner" : "caregiver")
            }
            profileValue = CaregiverProfile(
                accountID: profileValue.accountID, displayName: profileValue.displayName, role: "caregiver"
            )
        case .changeRole(let id, let newRole):
            membersValue = membersValue.map {
                $0.id == id
                    ? FamilyMember(id: $0.id, displayName: $0.displayName, role: newRole.rawValue)
                    : $0
            }
            if id == profileValue.accountID {
                profileValue = CaregiverProfile(
                    accountID: profileValue.accountID, displayName: profileValue.displayName, role: newRole.rawValue
                )
            }
        case .deleteAccount:
            membersValue = []
        }
    }

    func scheduleFamilyDeletion() async throws {
        scheduledDeletion = true
        membersValue = []
    }

    func recoverFamily() async throws -> [FamilyMember] {
        recoveryClaimConsumed = true
        membersValue = recoveredMembers
        return recoveredMembers
    }

    func forfeitFamilyRecovery() async throws {
        recoveryClaimConsumed = true
        membersValue = []
    }

    var logoutError: (any Error)?
    func logout() async throws {
        if let logoutError { throw logoutError }
        logoutCount += 1
    }
}

final class FakeIdentityClient: IdentityClient, @unchecked Sendable {
    var info: ServerInfo
    var bundle: SessionBundle
    private(set) var exchangeCount = 0
    private(set) var lastRequest: AppleExchangeRequest?
    var exchangeError: (any Error)?

    init(info: ServerInfo, bundle: SessionBundle) {
        self.info = info
        self.bundle = bundle
    }

    func serverInfo(_ origin: ServerOrigin) async throws -> ServerInfo { info }
    func challenge(_ endpoint: ConfirmedEndpoint) async throws -> String {
        "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
    }

    func exchangeApple(_ request: AppleExchangeRequest, at endpoint: ConfirmedEndpoint) async throws -> SessionBundle {
        exchangeCount += 1
        lastRequest = request
        if let exchangeError { throw exchangeError }
        return bundle
    }
}

final class FakeAppleSignInClient: AppleSignInClient, @unchecked Sendable {
    var fullName: String?
    var userIdentifier = "apple-user-001"
    private(set) var freshProofRequestCount = 0

    init(fullName: String?) { self.fullName = fullName }

    func requestProof(nonce: String) async throws -> AppleProof {
        freshProofRequestCount += 1
        return AppleProof(
            identityToken: "identity-token",
            authorizationCode: "auth-code",
            userIdentifier: userIdentifier,
            fullName: fullName
        )
    }
}

final class FakeAppleCredentialStateClient: AppleCredentialStateClient, @unchecked Sendable {
    var nextState: AppleCredentialState = .authorized
    func credentialState(forUserIdentifier userIdentifier: String) async throws -> AppleCredentialState { nextState }
}

/// Builds a view model wired to the fakes, in a named starting state.
@MainActor
struct FamilySettingsFixture {
    let model: FamilySettingsViewModel
    let coordinator: AccountCoordinator
    let client: FakeFamilyClient
    let identity: FakeIdentityClient
    let appleClient: FakeAppleSignInClient
    let credentialStateClient: FakeAppleCredentialStateClient
    let keychain: KeychainSessionStore
    let backend: InMemoryKeychainBackend
    let store: BabyStore
    /// Per fixture, so neither the session record nor the entry mark leaks
    /// between tests — or into the real App Group.
    let defaults: UserDefaults

    static let origin = try! ServerOrigin(validating: "https://sync.example.test")
    static let instanceID = UUID(uuidString: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let accountID = UUID(uuidString: "d1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let deviceID = UUID(uuidString: "f1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let familyID = UUID(uuidString: "b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let removableMemberID = UUID(uuidString: "11111111-2222-4333-8444-555555555555")!
    static let successorMemberID = UUID(uuidString: "66666666-7777-4888-8999-aaaaaaaaaaaa")!
    static let localBabyID = UUID(uuidString: "10000000-0000-4000-8000-000000000001")!

    static var endpoint: ConfirmedEndpoint { ConfirmedEndpoint(origin: origin, serverInstanceID: instanceID) }

    static var sharedRealm: RealmID {
        .shared(canonicalOrigin: origin.canonicalString, serverInstanceID: instanceID, familyID: familyID)
    }

    static func make(
        role: String = "owner",
        displayName: String = "Alex",
        members: [FamilyMember] = [],
        appleFullName: String? = "Alex",
        confirmedName: String? = "Alex",
        familyInBundle: Bool = true,
        recoveryOnly: Bool = false,
        makeEngine: FamilySyncEngineFactory? = nil,
        renewAccountSession: AccountSessionRenewal? = nil,
        startLiveActivity: @escaping @MainActor (ActiveTimer, BabyStore) -> Void = { _, _ in }
    ) throws -> FamilySettingsFixture {
        let defaults = UserDefaults(suiteName: "FamilySettingsFixture-\(UUID().uuidString)")!
        // The name the user confirmed on the Caregivers screen before signing
        // in, which is where the model reads it from at launch.
        if let confirmedName { DisplayNameRecord.save(confirmedName, to: defaults) }
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let backend = InMemoryKeychainBackend()
        let keychain = KeychainSessionStore(backend: backend)

        let info = ServerInfo(
            serverInstanceID: instanceID, canonicalOrigin: origin.canonicalString, recoveryEpoch: 0,
            capabilities: ["records-v1", "live-hints-v1"], appleAuthorizationMode: "hosted",
            maxItemsPerPage: 200, maxEncodedBytes: 1_000_000,
            sourceVersion: "1.0.0", sourceURL: "https://example.test"
        )
        let bundle = SessionBundle(
            accountID: accountID, deviceID: deviceID, recoveryEpoch: 0,
            accessToken: "access", refreshToken: "refresh",
            expiresAt: Date(timeIntervalSince1970: 10_000),
            deviceCredential: "device-credential",
            familyID: familyInBundle ? familyID : nil,
            isRecoveryOnly: recoveryOnly
        )
        let identity = FakeIdentityClient(info: info, bundle: bundle)
        let apple = FakeAppleSignInClient(fullName: appleFullName)
        let credentialState = FakeAppleCredentialStateClient()

        let coordinator = AccountCoordinator(
            store: store, keychain: keychain, identity: identity,
            apple: apple, credentialState: credentialState, defaults: defaults
        )
        coordinator.confirmEndpoint(endpoint)

        let client = FakeFamilyClient(
            membership: FamilyMembership(familyID: familyID, accountID: accountID, deviceID: deviceID, recoveryEpoch: 0),
            profile: CaregiverProfile(accountID: accountID, displayName: displayName, role: role),
            members: members
        )
        let model = FamilySettingsViewModel(
            store: store, coordinator: coordinator, client: client, defaults: defaults,
            makeEngine: makeEngine, renewAccountSession: renewAccountSession,
            codeStoreBackend: backend, startLiveActivity: startLiveActivity
        )

        return FamilySettingsFixture(
            model: model, coordinator: coordinator, client: client, identity: identity, appleClient: apple,
            credentialStateClient: credentialState, keychain: keychain, backend: backend, store: store,
            defaults: defaults
        )
    }

    /// An owner already signed in and holding a family.
    static func ownerModel(members: [FamilyMember] = []) async throws -> FamilySettingsFixture {
        let fixture = try make(members: members)
        try await fixture.model.signInWithApple()
        return fixture
    }

    static func ownerModelWithTwoMembers() async throws -> FamilySettingsFixture {
        let members = [
            FamilyMember(id: accountID, displayName: "Alex", role: "owner"),
            FamilyMember(id: removableMemberID, displayName: "Sam", role: "caregiver"),
            FamilyMember(id: successorMemberID, displayName: "Jo", role: "caregiver"),
        ]
        let fixture = try await ownerModel(members: members)
        fixture.client.recoveredMembers = [FamilyMember(id: accountID, displayName: "Alex", role: "owner")]
        return fixture
    }

    /// A fresh install at the sign-in step: an endpoint, no session, and no
    /// name confirmed yet.
    static func unnamedModel() throws -> FamilySettingsFixture {
        try make(displayName: "", appleFullName: nil, confirmedName: nil, familyInBundle: false)
    }

    static func modelWithLocalAndSharedRows() async throws -> FamilySettingsFixture {
        let fixture = try await ownerModel()
        let local = Baby(id: localBabyID, name: "Local", birthDate: .now, realm: .local)
        fixture.store.context.insert(local)
        let shared = Baby(id: UUID(), name: "Shared", birthDate: .now, realm: sharedRealm)
        fixture.store.context.insert(shared)
        try fixture.store.context.save()
        return fixture
    }

    /// Moves the store's active realm to the shared family realm, the way a
    /// real sync engine's activation does. This fixture never runs one — no
    /// `makeEngine` — so without this, `store.activeRealm` stays `.local` for
    /// the whole test, which is fine for most of them but would hide a bug
    /// in anything scoped by realm: a save and a later clear could agree on
    /// the wrong realm and still look right.
    func enterSharedRealm() throws {
        let checkpointID = UUID()
        try store.beginTransition(to: Self.sharedRealm, checkpointID: checkpointID)
        try store.commitTransition(checkpointID: checkpointID)
    }
}
