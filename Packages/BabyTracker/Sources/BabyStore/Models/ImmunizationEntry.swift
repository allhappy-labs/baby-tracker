import Foundation
import SwiftData

/// One vaccination. `vaccine` is free text the family reuses; `dose` is the
/// number on the booklet, when the caregiver gave one.
@Model
public final class ImmunizationEntry {
    public var id: UUID = UUID()
    public var realmKey: String = RealmID.local.encodedKey
    public var babyID: UUID = UUID()
    public var givenAt: Date = Date()
    public var timeZoneID: String = "UTC"
    public var vaccine: String = ""
    public var dose: Int?
    public var note: String?
    public var createdBy: String = ""
    public var lastModifiedAt: Date = Date()
    public var isTombstoned: Bool = false

    public init(id: UUID = UUID(), babyID: UUID, vaccine: String, dose: Int?, givenAt: Date,
                timeZoneID: String, modifiedAt: Date? = nil, createdBy: String = "", realm: RealmID = .local) {
        self.id = id; self.babyID = babyID; self.vaccine = vaccine; self.dose = dose
        self.givenAt = givenAt; self.timeZoneID = timeZoneID
        self.lastModifiedAt = modifiedAt ?? givenAt
        self.createdBy = createdBy; self.realmKey = realm.encodedKey
    }
    public var realmID: RealmID { RealmID(encodedKey: realmKey) ?? .local }
}
