import Foundation
import SwiftData

@Model
public final class SyncCursorState {
    @Attribute(.unique) public var realmKey: String = RealmID.local.encodedKey
    public var sequence: Int64 = 0
    public var recoveryEpoch: Int64 = 0
    /// Carried so a cursor cannot be replayed against a different server
    /// instance or family. A sequence number means nothing on its own.
    public var serverInstanceID: UUID = UUID()
    public var familyID: UUID = UUID()

    public init(
        realm: RealmID,
        sequence: Int64 = 0,
        recoveryEpoch: Int64 = 0,
        serverInstanceID: UUID = UUID(),
        familyID: UUID = UUID()
    ) {
        realmKey = realm.encodedKey
        self.sequence = sequence
        self.recoveryEpoch = recoveryEpoch
        self.serverInstanceID = serverInstanceID
        self.familyID = familyID
    }
}
