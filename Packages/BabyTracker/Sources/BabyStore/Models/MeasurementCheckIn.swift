import Foundation
import SwiftData
import BabyCore

/// One dated check-in. Only original values and their measurement method are stored;
/// percentile and reference adjustments are always derived by the client.
@Model
public final class MeasurementCheckIn {
    public var id: UUID = UUID()
    public var realmKey: String = RealmID.local.encodedKey
    public var babyID: UUID = UUID()
    public var measuredAt: Date = Date()
    public var timeZoneID: String = "UTC"
    /// Logging action time survives corrections to the measured time and values.
    public internal(set) var loggedAt: Date = Date()
    public var weightKG: Double?
    public var lengthCM: Double?
    public var headCircumferenceCM: Double?
    public var lengthPositionRaw: String?
    public var createdBy: String = ""
    public var lastModifiedAt: Date = Date()
    public var isTombstoned: Bool = false

    public init(id: UUID = UUID(), babyID: UUID, measuredAt: Date, timeZoneID: String,
                loggedAt: Date, values: MeasurementValues, createdBy: String = "", realm: RealmID = .local) {
        self.id = id
        self.babyID = babyID
        self.realmKey = realm.encodedKey
        self.measuredAt = measuredAt
        self.timeZoneID = timeZoneID
        self.loggedAt = loggedAt
        self.weightKG = values.weightKG
        self.lengthCM = values.lengthCM
        self.headCircumferenceCM = values.headCircumferenceCM
        self.lengthPositionRaw = values.lengthPosition?.rawValue
        self.createdBy = createdBy
        self.lastModifiedAt = loggedAt
    }

    public var realmID: RealmID { RealmID(encodedKey: realmKey) ?? .local }
    public var lengthPosition: LengthPosition? { lengthPositionRaw.flatMap(LengthPosition.init(rawValue:)) }
}
