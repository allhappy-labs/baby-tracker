import Foundation
import SwiftData

@Model
public final class RealmTransitionCheckpoint {
    @Attribute(.unique) public var stateKey: String = "active"
    public var activeRealmKey: String = RealmID.local.encodedKey
    public var targetRealmKey: String?
    public var checkpointID: UUID?
    public var generation: Int64 = 0
    public var phaseRaw: String = "idle"

    public init(activeRealm: RealmID = .local) { activeRealmKey = activeRealm.encodedKey }
}
