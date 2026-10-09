import Foundation
import SwiftData

@Model
public final class FencedLease {
    @Attribute(.unique) public var scopeKey: String = ""
    public var ownerID: UUID = UUID()
    public var generation: Int64 = 0
    public var expiresAt: Date = Date.distantPast

    public init(scopeKey: String, ownerID: UUID, generation: Int64, expiresAt: Date) {
        self.scopeKey = scopeKey; self.ownerID = ownerID; self.generation = generation; self.expiresAt = expiresAt
    }
}
