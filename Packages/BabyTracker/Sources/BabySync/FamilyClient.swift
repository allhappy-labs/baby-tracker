import BabyStore
import Foundation

public struct FamilyMembership: Sendable, Equatable {
    public let familyID: UUID
    public let accountID: UUID
    public let deviceID: UUID
    public let recoveryEpoch: UInt64
    public init(familyID: UUID, accountID: UUID, deviceID: UUID, recoveryEpoch: UInt64) {
        self.familyID = familyID; self.accountID = accountID
        self.deviceID = deviceID; self.recoveryEpoch = recoveryEpoch
    }
}

public struct CaregiverProfile: Sendable, Equatable {
    public let accountID: UUID
    public let displayName: String
    public let role: String
    public init(accountID: UUID, displayName: String, role: String) {
        self.accountID = accountID; self.displayName = displayName; self.role = role
    }

    /// Fails closed for the same reason `FamilyMember.familyRole` does.
    public var familyRole: FamilyRole { FamilyRole(rawValue: role) ?? .viewer }
}

public struct FamilyMember: Sendable, Equatable, Identifiable {
    public let id: UUID
    public let displayName: String
    public let role: String
    public init(id: UUID, displayName: String, role: String) {
        self.id = id; self.displayName = displayName; self.role = role
    }

    /// An unrecognised role from a newer server degrades to the least
    /// privileged one. Guessing upwards would hand a stranger write access.
    public var familyRole: FamilyRole { FamilyRole(rawValue: role) ?? .viewer }
}

public enum FamilyLifecycleCommand: Sendable, Equatable {
    case removeMember(UUID)
    case transferOwnership(UUID)
    case changeRole(UUID, FamilyRole)
    case deleteAccount
}

public enum AppleCredentialState: Sendable, Equatable {
    case authorized, revoked, notFound, transferred

    /// Revocation and a missing record both mean this Apple account no longer
    /// authorizes us. `transferred` does not: the credential moved between
    /// Apple teams and the session stays valid.
    public var endsAuthorization: Bool {
        switch self {
        case .revoked, .notFound: true
        case .authorized, .transferred: false
        }
    }
}

/// A caregiver invitation the family already minted, as it looks in the list
/// view. Never carries the token or link — those are only ever seen once,
/// at creation, which is why `InvitationCodeStore` exists.
public struct PendingInvitation: Sendable, Equatable, Identifiable {
    public let id: UUID
    public let role: FamilyRole
    public let expiresAt: Date
    public let createdAt: Date
    public init(id: UUID, role: FamilyRole, expiresAt: Date, createdAt: Date) {
        self.id = id
        self.role = role
        self.expiresAt = expiresAt
        self.createdAt = createdAt
    }
}

/// What an invitation link resolves to before it is redeemed, so a scanned or
/// pasted code can be shown to the user without joining anything.
public struct InvitationPreview: Sendable, Equatable {
    public let familyName: String
    public let expiresAt: Date
    public init(familyName: String, expiresAt: Date) {
        self.familyName = familyName
        self.expiresAt = expiresAt
    }
}

public enum FamilySyncError: Error, Sendable, Equatable {
    case ownerOnly
    case endpointConfirmationRequired
    case deviceContinuityInvalid
    case deviceContinuityRequired
    case displayNameConfirmationRequired
    case ownerTransferRequired
    case ownerCredentialStateRequired
}

/// The P1 family and lifecycle operations, as the client needs them.
public protocol FamilyClient: Sendable {
    func createFamily(name: String, displayName: String) async throws -> FamilyMembership
    func confirmJoin(token: String, displayName: String) async throws -> FamilyMembership
    func members() async throws -> [FamilyMember]
    func profile() async throws -> CaregiverProfile
    func updateDisplayName(_ name: String) async throws -> CaregiverProfile
    /// Mints an invitation that confers `role` when it is redeemed.
    ///
    /// The role travels with the command. It used to be a parameter that never
    /// left the device, which made a "view-only invitation" quietly produce an
    /// ordinary caregiver; the server now has a field for it, and refuses any
    /// role but caregiver or viewer.
    func createInvitation(role: FamilyRole) async throws -> FamilyInvitation
    /// Every invitation this family has minted that has not been redeemed or
    /// revoked yet — never with the token or link, which the server only ever
    /// answers with once.
    func pendingInvitations() async throws -> [PendingInvitation]
    func revokeInvitation(_ invitationID: UUID) async throws
    /// Resolves a token to the family it invites into, without redeeming it —
    /// so a scanned or pasted code can be shown before it is accepted.
    func previewInvitation(token: String) async throws -> InvitationPreview
    func leaveFamily() async throws
    func perform(_ command: FamilyLifecycleCommand) async throws
    func scheduleFamilyDeletion() async throws
    func recoverFamily() async throws -> [FamilyMember]
    func forfeitFamilyRecovery() async throws
    func logout() async throws
}
