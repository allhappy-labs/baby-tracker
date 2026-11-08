import BabyCore
import BabyProtocol
import Foundation

/// Anything the change log can carry. Payloads are plain JSON dictionaries in
/// the server's wire shape (`Server/api/fixtures/v1`), because the same
/// fields become the protocol mutation: the server refuses any key it does
/// not know, so a local column name here is a record that never syncs.
public protocol ChangeLoggable {
    var entityID: UUID { get }
    var entityTypeName: String { get }
    func payloadFields() -> [String: Any]
}

/// Thrown by `payloadData()` when `payloadFields()` contains a value
/// `JSONSerialization` cannot represent (e.g. a raw `Date`, or a non-finite
/// `Double`). `JSONSerialization.data(withJSONObject:)` raises an uncatchable
/// Objective-C exception for such values instead of throwing, so callers must
/// pre-validate with `isValidJSONObject` rather than relying on `try`.
public struct InvalidChangeLogPayload: Error, Sendable {
    public let entityTypeName: String
}

public extension ChangeLoggable {
    func payloadData() throws -> Data {
        let fields = payloadFields()
        guard JSONSerialization.isValidJSONObject(fields) else {
            throw InvalidChangeLogPayload(entityTypeName: entityTypeName)
        }
        return try JSONSerialization.data(withJSONObject: fields, options: [.sortedKeys])
    }
}

extension Baby: ChangeLoggable {
    public var entityID: UUID { id }
    public var entityTypeName: String { "Baby" }
    /// The server's `BabyPayload`. The birth date is the calendar day the
    /// user sees in this device's zone — the same reading the import
    /// manifest sends the resolver — not the stored instant, which could
    /// land on the neighbouring day in UTC.
    public func payloadFields() -> [String: Any] {
        var fields: [String: Any] = [
            "name": name,
            "birth_date": (try? LocalDate(displaying: birthDate, in: .current))?.iso8601 ?? "",
            "sort_order": sortOrder,
        ]
        // Only when set: `baby.json`'s key set is the contract for a baby
        // without one, and the server keeps a stored icon an update leaves out.
        if let iconID, !iconID.isEmpty { fields["icon"] = iconID }
        return fields
    }
}

extension FeedEntry: ChangeLoggable {
    public var entityID: UUID { id }
    public var entityTypeName: String { "FeedEntry" }
    public func payloadFields() -> [String: Any] {
        var fields: [String: Any] = [
            "baby_id": babyID.uuidString.lowercased(),
            "started_at": MutationFactory.wireTime(startedAt),
            "time_zone_id": timeZoneID,
            "kind": kindRaw,
            "components": components.map(MutationFactory.componentPayload),
            "feeding_session_id": feedingSessionID?.uuidString.lowercased() as Any? ?? NSNull(),
        ]
        // Null, not absent, when cleared — see the sleep and diaper note
        // below. Absent, a cleared note never left the other phone.
        fields["ended_at"] = endedAt.map(MutationFactory.wireTime) ?? NSNull()
        fields["note"] = note ?? NSNull()
        return fields
    }

    /// False for a feed with no components. The server refuses a feed
    /// without any, so such a feed stays on the phone.
    var hasWireContent: Bool { !components.isEmpty }
}

/// Feed, sleep and diaper payloads spell out every optional column as JSON null
/// rather than omitting it, matching the protocol fixtures. The server reads a
/// missing key and a null the same way; the remote-apply side reads a null as
/// "clear this", which a full-state payload needs so an edit that removes a
/// note actually removes it on the other phone.
extension SleepEntry: ChangeLoggable {
    public var entityID: UUID { id }
    public var entityTypeName: String { "SleepEntry" }
    public func payloadFields() -> [String: Any] {
        [
            "baby_id": babyID.uuidString.lowercased(),
            "started_at": MutationFactory.wireTime(startedAt),
            "ended_at": endedAt.map(MutationFactory.wireTime) ?? NSNull(),
            "time_zone_id": timeZoneID,
            "note": note ?? NSNull(),
            "kind_override": kindOverrideRaw ?? NSNull(),
            "sleep_session_id": sleepSessionID?.uuidString.lowercased() as Any? ?? NSNull(),
        ]
    }
}

extension DiaperEntry: ChangeLoggable {
    public var entityID: UUID { id }
    public var entityTypeName: String { "DiaperEntry" }
    public func payloadFields() -> [String: Any] {
        [
            "baby_id": babyID.uuidString.lowercased(),
            "started_at": MutationFactory.wireTime(startedAt),
            "ended_at": endedAt.map(MutationFactory.wireTime) ?? NSNull(),
            "time_zone_id": timeZoneID,
            "note": note ?? NSNull(),
            "contents": contentsRaw,
            "consistency": consistency ?? NSNull(),
            "color": color ?? NSNull(),
        ]
    }
}

/// A record that belongs to one baby. The server's delete payload for such a
/// record names that baby and nothing else.
protocol BabyOwnedRecord {
    var babyID: UUID { get }
}

extension FeedEntry: BabyOwnedRecord {}
extension SleepEntry: BabyOwnedRecord {}
extension DiaperEntry: BabyOwnedRecord {}

extension SupplementEntry: ChangeLoggable, BabyOwnedRecord, Touchable, RealmScoped {
    public var entityID: UUID { id }
    public var entityTypeName: String { "SupplementEntry" }
    public func payloadFields() -> [String: Any] {
        ["baby_id": babyID.uuidString.lowercased(), "started_at": MutationFactory.wireTime(startedAt),
         "logged_at": SupplementTimestamp.format(loggedAt),
         "time_zone_id": timeZoneID, "name": name, "note": note ?? NSNull()]
    }
}

extension ImmunizationEntry: ChangeLoggable, BabyOwnedRecord, Touchable, RealmScoped {
    public var entityID: UUID { id }
    public var entityTypeName: String { "ImmunizationEntry" }
    public func payloadFields() -> [String: Any] {
        ["baby_id": babyID.uuidString.lowercased(), "given_at": MutationFactory.wireTime(givenAt),
         "time_zone_id": timeZoneID, "vaccine": vaccine,
         "dose": dose as Any? ?? NSNull(), "note": note ?? NSNull()]
    }
}

/// Not `BabyOwnedRecord`: a pump belongs to the family, so its delete payload
/// is `{}` and no hash, import or cascade ever looks for its baby.
extension PumpEntry: ChangeLoggable, Touchable, RealmScoped {
    public var entityID: UUID { id }
    public var entityTypeName: String { "PumpEntry" }
    public func payloadFields() -> [String: Any] {
        ["started_at": MutationFactory.wireTime(startedAt),
         "ended_at": endedAt.map(MutationFactory.wireTime) ?? NSNull(),
         "time_zone_id": timeZoneID, "side": sideRaw,
         "volume_ml": volumeML as Any? ?? NSNull(),
         "note": note ?? NSNull()]
    }
}


extension GrowthProfile: ChangeLoggable, BabyOwnedRecord, Touchable, RealmScoped {
    public var entityID: UUID { id }
    public var entityTypeName: String { "GrowthProfile" }
    public func payloadFields() -> [String: Any] {
        ["baby_id": babyID.uuidString.lowercased(), "reference_sex": referenceSexRaw,
         "expected_due_date": expectedDueDateISO ?? NSNull()]
    }
}

extension MeasurementCheckIn: ChangeLoggable, BabyOwnedRecord, Touchable, RealmScoped {
    public var entityID: UUID { id }
    public var entityTypeName: String { "MeasurementCheckIn" }
    public func payloadFields() -> [String: Any] {
        ["baby_id": babyID.uuidString.lowercased(), "measured_at": SupplementTimestamp.format(measuredAt),
         "time_zone_id": timeZoneID, "logged_at": SupplementTimestamp.format(loggedAt),
         "weight_kg": weightKG as Any? ?? NSNull(), "length_cm": lengthCM as Any? ?? NSNull(),
         "head_circumference_cm": headCircumferenceCM as Any? ?? NSNull(),
         "length_position": lengthPositionRaw ?? NSNull()]
    }
}
