import Foundation
import SwiftData
import BabyCore

@Model
public final class DiaperEntry {
    public var id: UUID = UUID()
    public var realmKey: String = RealmID.local.encodedKey
    public var babyID: UUID = UUID()
    public var startedAt: Date = Date()
    public var endedAt: Date?
    public var timeZoneID: String = "UTC"
    public var note: String?
    public var createdBy: String = ""
    public var lastModifiedAt: Date = Date()
    public var isTombstoned: Bool = false

    public var contentsRaw: String = DiaperContents.wet.rawValue
    public var consistency: String?
    public var color: String?

    public init(
        id: UUID = UUID(),
        babyID: UUID,
        contents: DiaperContents,
        startedAt: Date,
        timeZoneID: String,
        createdBy: String = "",
        realm: RealmID = .local
    ) {
        self.id = id
        self.realmKey = realm.encodedKey
        self.babyID = babyID
        self.contentsRaw = contents.rawValue
        self.startedAt = startedAt
        self.timeZoneID = timeZoneID
        self.createdBy = createdBy
        self.lastModifiedAt = startedAt
        self.isTombstoned = false
    }

    public var realmID: RealmID { RealmID(encodedKey: realmKey) ?? .local }

    public var contents: DiaperContents {
        get { DiaperContents(rawValue: contentsRaw) ?? .wet }
        set { contentsRaw = newValue.rawValue }
    }
}
