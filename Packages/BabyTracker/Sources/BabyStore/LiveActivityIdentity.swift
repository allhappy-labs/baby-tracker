import Foundation
public import BabyProtocol

public typealias LiveActivityScope = BabyProtocol.LiveActivityScope
public typealias LiveActivityRevision = BabyProtocol.LiveActivityRevision

public extension LiveActivityScope {
    /// A card must pass the entire realm fence before it can be adopted or acted on.
    func matches(serverInstanceID: UUID, recoveryEpoch: Int64, familyID: UUID) -> Bool {
        self.serverInstanceID == serverInstanceID && self.recoveryEpoch == recoveryEpoch && self.familyID == familyID
    }

    func matchesTarget(babyID: UUID, kindRaw: String) -> Bool {
        self.babyID == babyID && self.kindRaw == kindRaw
    }
}
