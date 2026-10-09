import Foundation
import SwiftData

/// What a family membership is allowed to do.
///
/// Defined in `BabyStore` rather than `BabySync` because the store's write
/// boundary is what enforces it, and the widget and App Intents reach the
/// store without ever linking the transport.
public enum FamilyRole: String, Codable, Sendable, CaseIterable {
    case owner, caregiver, viewer

    /// The single authority on whether this role may originate a mutation.
    /// Read by the write boundary; no view decides this for itself.
    public var canWrite: Bool { self != .viewer }
    public var canManageMembers: Bool { self == .owner }
    public var canInvite: Bool { self == .owner }

    /// What the role is called in front of a user.
    public var displayName: String {
        switch self {
        case .owner: String(localized: "Owner", bundle: .module)
        case .caregiver: String(localized: "Caregiver", bundle: .module)
        case .viewer: String(localized: "View only", bundle: .module)
        }
    }
}

public enum WritePermissionError: Error, Sendable, Equatable {
    case readOnlyCaregiver
}

public extension BabyStore {
    /// Records the role this device holds in a shared realm.
    ///
    /// Durable and per-realm, so a widget or App Intent process that never saw
    /// the demotion still refuses to write. A per-process flag would leave
    /// exactly those writers to carry on until they were next relaunched.
    func setWritePermission(_ role: FamilyRole, for realm: RealmID) throws {
        guard case .shared = realm else { return }
        let key = realm.encodedKey
        if let existing = try context.fetch(
            FetchDescriptor<RealmWritePermission>(predicate: #Predicate { $0.realmKey == key })
        ).first {
            existing.roleRaw = role.rawValue
        } else {
            context.insert(RealmWritePermission(realm: realm, role: role))
        }
        try context.save()
        didWrite()
    }

    /// The local realm is never gated: it is the user's own tracking and
    /// predates any family. An unknown shared realm defaults to `caregiver`,
    /// which is what every membership was before this role existed.
    func writePermission(in realm: RealmID? = nil) throws -> FamilyRole {
        let target = realm ?? activeRealm
        guard case .shared = target else { return .owner }
        let key = target.encodedKey
        let stored = try context.fetch(
            FetchDescriptor<RealmWritePermission>(predicate: #Predicate { $0.realmKey == key })
        ).first
        return stored.flatMap { FamilyRole(rawValue: $0.roleRaw) } ?? .caregiver
    }

    /// True when this device may originate mutations in the active realm.
    var canWriteInActiveRealm: Bool {
        (try? writePermission()).map(\.canWrite) ?? true
    }
}

@Model
public final class RealmWritePermission {
    @Attribute(.unique) public var realmKey: String = RealmID.local.encodedKey
    public var roleRaw: String = FamilyRole.caregiver.rawValue

    public init(realm: RealmID, role: FamilyRole) {
        realmKey = realm.encodedKey
        roleRaw = role.rawValue
    }
}
