import BabyProtocol
import Foundation
import SwiftData

/// The live, ordered local intents sitting on top of an entity's server base.
///
/// A base is *replaced* by what the server returns, never accumulated; the
/// overlay is only the mutations this device still owes. An acknowledged,
/// quarantined or superseded mutation leaves the overlay, which is what stops
/// an old intent being re-applied on top of fresh canonical state.
public struct EntityOverlay: Codable, Sendable, Equatable {
    public let entityID: UUID
    public let entityKind: WireEntityKind
    public let orderedMutationIDs: [UUID]

    public init(entityID: UUID, entityKind: WireEntityKind, orderedMutationIDs: [UUID]) {
        self.entityID = entityID
        self.entityKind = entityKind
        self.orderedMutationIDs = orderedMutationIDs
    }
}

public extension BabyStore {
    func overlay(for entityID: UUID, kind: WireEntityKind, in realm: RealmID? = nil) throws -> EntityOverlay {
        let rows = try liveOutboxRows(in: realm).filter {
            $0.entityID == entityID && $0.entityKind == kind && !$0.deliveryState.isTerminal
        }
        return EntityOverlay(entityID: entityID, entityKind: kind, orderedMutationIDs: rows.map(\.id))
    }

    /// Replaces an entity's canonical base with what the server just sent.
    func replaceBase(entityID: UUID, kind: WireEntityKind, version: Int64, payload: Data, in realm: RealmID? = nil) throws {
        let targetRealm = realm ?? activeRealm
        let storageKey = "\(targetRealm.encodedKey)|\(kind.rawValue)|\(entityID.uuidString)"
        if let existing = try context.fetch(
            FetchDescriptor<CanonicalEntityBase>(predicate: #Predicate { $0.storageKey == storageKey })
        ).first {
            existing.entityVersion = version
            existing.payload = payload
            return
        }
        context.insert(CanonicalEntityBase(
            realm: targetRealm, entityID: entityID, entityKind: kind.rawValue, entityVersion: version, payload: payload
        ))
    }
}
