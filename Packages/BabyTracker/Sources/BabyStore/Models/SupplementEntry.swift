import Foundation
import SwiftData

@Model
public final class SupplementEntry {
    public var id: UUID = UUID()
    public var realmKey: String = RealmID.local.encodedKey
    public var babyID: UUID = UUID()
    public var startedAt: Date = Date()
    public var timeZoneID: String = "UTC"
    public var name: String = ""
    public var note: String?
    public var createdBy: String = ""
    public var lastModifiedAt: Date = Date()
    public var isTombstoned: Bool = false
    /// Logging action time, independent of the corrected event time.
    public var loggedAt: Date = Date()

    public init(id: UUID = UUID(), babyID: UUID, name: String, startedAt: Date,
                timeZoneID: String, loggedAt: Date, createdBy: String = "", realm: RealmID = .local) {
        self.id = id; self.babyID = babyID; self.name = name
        self.startedAt = startedAt; self.timeZoneID = timeZoneID
        self.loggedAt = loggedAt; self.lastModifiedAt = loggedAt
        self.createdBy = createdBy; self.realmKey = realm.encodedKey
    }
    public var realmID: RealmID { RealmID(encodedKey: realmKey) ?? .local }
}
