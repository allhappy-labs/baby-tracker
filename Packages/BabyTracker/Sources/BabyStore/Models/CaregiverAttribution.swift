import Foundation
import SwiftData

@Model
public final class CaregiverAttribution {
    @Attribute(.unique) public var storageKey: String = ""
    public var realmKey: String = RealmID.local.encodedKey
    public var entityID: UUID = UUID()
    public var accountID: UUID = UUID()
    public var displayName: String = ""
    /// Who created the entry, and who last changed it. Both are carried
    /// because they answer different questions — "who logged this feed" and
    /// "who corrected the volume" — and collapsing them loses the second.
    public var creatorDisplayName: String = ""
    public var lastEditorDisplayName: String = ""

    public init(
        realm: RealmID,
        entityID: UUID,
        accountID: UUID,
        displayName: String,
        creatorDisplayName: String = "",
        lastEditorDisplayName: String = ""
    ) {
        realmKey = realm.encodedKey
        self.entityID = entityID
        self.accountID = accountID
        self.displayName = displayName
        self.creatorDisplayName = creatorDisplayName.isEmpty ? displayName : creatorDisplayName
        self.lastEditorDisplayName = lastEditorDisplayName.isEmpty ? displayName : lastEditorDisplayName
        storageKey = "\(realm.encodedKey)|\(entityID.uuidString)"
    }
}

/// Who touched an entry, as a screen should say it.
///
/// Deliberately a value type: the presentation must not depend on a live
/// SwiftData object, and an entry whose author has since left the family still
/// has to render.
public struct EntryAttribution: Sendable, Equatable {
    public let creator: String
    public let lastEditor: String

    public init(creator: String, lastEditor: String) {
        self.creator = creator
        self.lastEditor = lastEditor
    }

    /// What a caregiver who has left, or was never named, is called.
    ///
    /// A pseudonymized account keeps its records but loses its display name.
    /// Showing a blank, an account UUID, or "Unknown" would each be worse:
    /// the first looks broken, the second leaks an identifier, and the third
    /// implies the app lost track rather than that the person left.
    public static let formerCaregiver = String(localized: "Former caregiver", bundle: .module)

    public static func named(_ raw: String?) -> String {
        let trimmed = raw?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        return trimmed.isEmpty ? formerCaregiver : trimmed
    }

    /// The one line shown on an entry's detail.
    ///
    /// Only the last editor when the entry has been changed by someone else;
    /// an entry nobody has edited says who logged it. Two lines of provenance
    /// on every entry would bury the thing the parent came to read.
    public var displayText: String {
        creator == lastEditor
            ? String(format: String(localized: "Logged by %@", bundle: .module), creator)
            : String(format: String(localized: "Logged by %@ · edited by %@", bundle: .module), creator, lastEditor)
    }
}

public extension BabyStore {
    /// Attribution for one entry, or nil when there is none — a local-only
    /// entry has no caregiver to name.
    func attribution(for entityID: UUID, in realm: RealmID? = nil) throws -> EntryAttribution? {
        let key = "\((realm ?? activeRealm).encodedKey)|\(entityID.uuidString)"
        guard let row = try context.fetch(
            FetchDescriptor<CaregiverAttribution>(predicate: #Predicate { $0.storageKey == key })
        ).first else { return nil }
        return EntryAttribution(
            creator: EntryAttribution.named(row.creatorDisplayName),
            lastEditor: EntryAttribution.named(row.lastEditorDisplayName)
        )
    }

    /// Records who touched an entry. Called when a remote page names an actor.
    func recordAttribution(
        entityID: UUID,
        accountID: UUID,
        creatorDisplayName: String,
        lastEditorDisplayName: String,
        in realm: RealmID? = nil
    ) throws {
        let targetRealm = realm ?? activeRealm
        let key = "\(targetRealm.encodedKey)|\(entityID.uuidString)"
        if let existing = try context.fetch(
            FetchDescriptor<CaregiverAttribution>(predicate: #Predicate { $0.storageKey == key })
        ).first {
            existing.lastEditorDisplayName = lastEditorDisplayName
            // The creator never changes; only the editor does.
            if existing.creatorDisplayName.isEmpty { existing.creatorDisplayName = creatorDisplayName }
            try context.save()
            return
        }
        context.insert(CaregiverAttribution(
            realm: targetRealm, entityID: entityID, accountID: accountID,
            displayName: creatorDisplayName,
            creatorDisplayName: creatorDisplayName,
            lastEditorDisplayName: lastEditorDisplayName
        ))
        try context.save()
    }
}
