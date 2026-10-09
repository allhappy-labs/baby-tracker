import BabyStore
import Foundation

/// A family client that does not exist until an endpoint has been confirmed.
///
/// The Caregivers screen is reachable from the moment the app launches, long
/// before any server has been chosen. Rather than make the whole screen
/// optional, every family call routes through here and fails with
/// `endpointConfirmationRequired` until there is somewhere legitimate to send
/// it. That keeps "no endpoint yet" a normal, answerable state instead of a
/// nil the UI has to special-case everywhere.
public final class DeferredFamilyClient: FamilyClient, @unchecked Sendable {
    private let lock = NSLock()
    private var resolved: (any FamilyClient)?
    private let make: @Sendable (ConfirmedEndpoint) -> any FamilyClient

    public init(make: @escaping @Sendable (ConfirmedEndpoint) -> any FamilyClient) {
        self.make = make
    }

    public func endpointConfirmed(_ endpoint: ConfirmedEndpoint) {
        lock.lock(); defer { lock.unlock() }
        resolved = make(endpoint)
    }

    private func client() throws -> any FamilyClient {
        lock.lock(); defer { lock.unlock() }
        guard let resolved else { throw FamilySyncError.endpointConfirmationRequired }
        return resolved
    }

    public func createFamily(name: String, displayName: String) async throws -> FamilyMembership {
        try await client().createFamily(name: name, displayName: displayName)
    }
    public func confirmJoin(token: String, displayName: String) async throws -> FamilyMembership {
        try await client().confirmJoin(token: token, displayName: displayName)
    }
    public func members() async throws -> [FamilyMember] { try await client().members() }
    public func profile() async throws -> CaregiverProfile { try await client().profile() }
    public func updateDisplayName(_ name: String) async throws -> CaregiverProfile {
        try await client().updateDisplayName(name)
    }
    public func createInvitation(role: FamilyRole) async throws -> FamilyInvitation {
        try await client().createInvitation(role: role)
    }
    public func pendingInvitations() async throws -> [PendingInvitation] { try await client().pendingInvitations() }
    public func revokeInvitation(_ invitationID: UUID) async throws { try await client().revokeInvitation(invitationID) }
    public func previewInvitation(token: String) async throws -> InvitationPreview {
        try await client().previewInvitation(token: token)
    }
    public func leaveFamily() async throws { try await client().leaveFamily() }
    public func perform(_ command: FamilyLifecycleCommand) async throws { try await client().perform(command) }
    public func scheduleFamilyDeletion() async throws { try await client().scheduleFamilyDeletion() }
    public func recoverFamily() async throws -> [FamilyMember] { try await client().recoverFamily() }
    public func forfeitFamilyRecovery() async throws { try await client().forfeitFamilyRecovery() }
    public func logout() async throws { try await client().logout() }
}

/// Reads the current access token out of the Keychain on every request.
///
/// Per-call rather than captured: a rotated epoch or an erased session has to
/// take effect on the very next request, not whenever this object is rebuilt.
public struct KeychainTokenProvider: SessionTokenProviding {
    private let keychain: any SecureSessionStore
    private let key: @Sendable () -> SessionLookup?

    public enum SessionLookup: Sendable {
        case account(AccountSessionKey)
        case family(FamilySessionKey)
    }

    public init(keychain: any SecureSessionStore, key: @escaping @Sendable () -> SessionLookup?) {
        self.keychain = keychain
        self.key = key
    }

    public func currentSessionCredentials() async -> SessionCredentials? {
        // The lookup is captured once: account switching cannot pair a token
        // with another item's credential version.
        switch key() {
        case .account(let account): try? keychain.loadAccount(account)
        case .family(let family): try? keychain.loadFamily(family)
        case nil: nil
        }
    }

    public func currentAccessToken() async -> String? {
        switch key() {
        case .account(let accountKey): try? keychain.loadAccount(accountKey)?.accessToken
        case .family(let familyKey): try? keychain.loadFamily(familyKey)?.accessToken
        case nil: nil
        }
    }

    /// Read off the lookup key rather than out of the stored session: the key
    /// is what the Keychain entry is filed under, so these three are the same
    /// three by construction.
    public func currentSessionIdentity() async -> SessionIdentity? {
        switch key() {
        case .account(let accountKey):
            SessionIdentity(accountID: accountKey.accountID, deviceID: accountKey.deviceID, recoveryEpoch: accountKey.recoveryEpoch)
        case .family(let familyKey):
            SessionIdentity(accountID: familyKey.accountID, deviceID: familyKey.deviceID, recoveryEpoch: familyKey.recoveryEpoch)
        case nil:
            nil
        }
    }
}
