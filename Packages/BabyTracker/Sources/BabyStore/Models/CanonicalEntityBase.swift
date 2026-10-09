import Foundation
import SwiftData

@Model
public final class CanonicalEntityBase {
    @Attribute(.unique) public var storageKey: String = ""
    public var realmKey: String = RealmID.local.encodedKey
    public var entityID: UUID = UUID()
    public var entityKind: String = ""
    public var entityVersion: Int64 = 0
    public var payload: Data = Data()
    public init(realm: RealmID, entityID: UUID, entityKind: String, entityVersion: Int64, payload: Data) {
        realmKey = realm.encodedKey; self.entityID = entityID; self.entityKind = entityKind
        self.entityVersion = entityVersion; self.payload = payload; storageKey = "\(realm.encodedKey)|\(entityKind)|\(entityID.uuidString)"
    }
}
