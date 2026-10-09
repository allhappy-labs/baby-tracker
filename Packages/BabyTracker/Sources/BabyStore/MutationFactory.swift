import BabyCore
import BabyProtocol
import Foundation

/// Turns a local domain entity into the exact wire shape the server accepts.
///
/// The server owns these JSON names, so nothing here invents a field: the
/// payload is the entity's own stored columns, and the operation/entity-kind
/// pair is drawn from the closed `WireOperation`/`WireEntityKind` vocabulary.
enum MutationFactory {
    /// RFC 3339 in UTC with fractional seconds, matching the P2
    /// `client_mutation_time` vectors. Built per call: `ISO8601DateFormatter`
    /// is not `Sendable`, and a shared instance is exactly the kind of mutable
    /// global Swift 6 refuses.
    static func wireTime(_ date: Date) -> String {
        date.formatted(
            .iso8601
                .year().month().day()
                .dateTimeSeparator(.standard)
                .time(includingFractionalSeconds: true)
                .timeZone(separator: .omitted)
        )
    }

    static func mutation(
        id: UUID = UUID(),
        entityID: UUID,
        entityKind: WireEntityKind,
        operation: WireOperation,
        observedEntityVersion: Int64,
        at date: Date,
        importID: UUID? = nil,
        payload: [String: Any]
    ) throws -> WireMutation {
        WireMutation(
            mutationID: id,
            entityID: entityID,
            entityKind: entityKind,
            operation: operation,
            payloadVersion: entityKind.currentPayloadVersion,
            observedEntityVersion: observedEntityVersion,
            clientMutationTime: wireTime(date),
            importID: importID,
            payload: try jsonValue(payload)
        )
    }

    static func jsonValue(_ fields: [String: Any]) throws -> JSONValue {
        guard JSONSerialization.isValidJSONObject(fields) else {
            throw MutationFactoryError.unrepresentablePayload
        }
        let data = try JSONSerialization.data(withJSONObject: fields, options: [.sortedKeys])
        return try JSONDecoder().decode(JSONValue.self, from: data)
    }

    /// The wire kind for each change-loggable entity type.
    static func entityKind(forTypeName name: String) -> WireEntityKind? {
        switch name {
        case "Baby": .baby
        case "FeedEntry": .feed
        case "SleepEntry": .sleep
        case "DiaperEntry": .diaper
        case "SupplementEntry": .supplement
        case "GrowthProfile": .growthProfile
        case "MeasurementCheckIn": .measurement
        default: nil
        }
    }

    static func operation(for change: ChangeOperation) -> WireOperation {
        switch change {
        case .create: .create
        case .update: .update
        case .delete: .delete
        }
    }

    /// The timer payload the server reconstructs a running feed from.
    static func timerPayload(_ timer: ActiveTimer) -> [String: Any] {
        var fields: [String: Any] = [
            "baby_id": timer.babyID.uuidString.lowercased(),
            "timer_kind": timer.kindRaw,
            "started_at": wireTime(timer.startedAt),
            "time_zone_id": timer.timeZoneID,
            "completed_components": timer.completedComponents.map(componentPayload),
        ]
        fields["current_side"] = timer.currentSideRaw
        if timer.currentSide != nil {
            fields["current_side_started_at"] = wireTime(timer.currentSideStartedAt)
        }
        fields["current_bottle_started_at"] = timer.currentBottleStartedAt.map(wireTime) ?? NSNull()
        fields["session_id"] = timer.sessionID?.uuidString.lowercased() as Any? ?? NSNull()
        return fields
    }

    static func componentPayload(_ component: FeedComponent) -> [String: Any] {
        switch component {
        case let .breast(id, side, seconds, start, end):
            var payload: [String: Any] = ["id": id.uuidString.lowercased(), "kind": "breast", "side": side.rawValue, "seconds": seconds]
            if let start { payload["started_at"] = wireTime(start) }
            if let end { payload["ended_at"] = wireTime(end) }
            return payload
        case let .bottle(id, volumeML, contents, seconds, start, end):
            var payload: [String: Any] = ["id": id.uuidString.lowercased(), "kind": "bottle", "volume_ml": volumeML, "contents": WireFeedComponent.wireContents(contents)]
            if let seconds { payload["seconds"] = seconds }
            if let start { payload["started_at"] = wireTime(start) }
            if let end { payload["ended_at"] = wireTime(end) }
            return payload
        case let .solids(id, foods, amount):
            return ["id": id.uuidString.lowercased(), "kind": "solids", "foods": foods, "amount": amount.rawValue]
        }
    }
}

public enum MutationFactoryError: Error, Sendable, Equatable {
    case unrepresentablePayload
}
