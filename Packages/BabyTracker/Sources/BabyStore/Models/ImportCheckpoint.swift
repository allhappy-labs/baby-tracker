import Foundation
import SwiftData

@Model
public final class ImportCheckpoint {
    @Attribute(.unique) public var key: String = ""
    public var realmKey: String = RealmID.local.encodedKey
    public var payload: Data = Data()
    public init(key: String, realm: RealmID, payload: Data) { self.key = key; realmKey = realm.encodedKey; self.payload = payload }
}

public struct LegacyImportSummary: Codable, Sendable, Equatable {
    public let convertedBirthDates: [String]
    public init(convertedBirthDates: [String]) { self.convertedBirthDates = convertedBirthDates }
}

public enum MigratedEntityKind: String, Codable, Sendable, Equatable, CaseIterable {
    case baby, feed, sleep, diaper, supplement, growthProfile, measurement, immunization, pump, activeTimer, changeLog
}
