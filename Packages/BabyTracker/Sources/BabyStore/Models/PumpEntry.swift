import Foundation
import SwiftData
import BabyCore

/// A pumping session. It belongs to the family, not a baby: there is no
/// `babyID`, every baby's screens show it, and deleting a baby leaves it.
/// Running while `endedAt` is nil; at most one runs per family.
@Model
public final class PumpEntry {
    public var id: UUID = UUID()
    public var realmKey: String = RealmID.local.encodedKey
    public var startedAt: Date = Date()
    public var endedAt: Date?
    public var timeZoneID: String = "UTC"
    public var sideRaw: String = PumpSide.both.rawValue
    /// Millilitres, two decimals at most. Nil until entered, and may stay nil.
    public var volumeML: Double?
    public var note: String?
    public var createdBy: String = ""
    public var lastModifiedAt: Date = Date()
    public var isTombstoned: Bool = false

    public init(id: UUID = UUID(), startedAt: Date, endedAt: Date? = nil, timeZoneID: String, side: PumpSide,
                createdBy: String = "", realm: RealmID = .local) {
        self.id = id
        self.startedAt = startedAt
        self.endedAt = endedAt
        self.timeZoneID = timeZoneID
        self.sideRaw = side.rawValue
        self.createdBy = createdBy
        self.lastModifiedAt = startedAt
        self.realmKey = realm.encodedKey
    }

    public var side: PumpSide {
        get { PumpSide(rawValue: sideRaw) ?? .both }
        set { sideRaw = newValue.rawValue }
    }

    public var isRunning: Bool { endedAt == nil && !isTombstoned }
    public var realmID: RealmID { RealmID(encodedKey: realmKey) ?? .local }
}
