import Foundation
import BabyCore

public enum FeedComponentWireError: Error, Sendable, Equatable {
    case unknownKind(String)
    case unknownField(String)
    case invalidShape(String)
}

public struct WireFeedComponent: Codable, Sendable, Equatable, Identifiable {
    public enum Kind: String, Codable, Sendable { case breast, bottle, solids }

    public let id: UUID
    public let kind: Kind
    public let side: BreastSide?
    public let seconds: Int?
    public let volumeML: Decimal?
    public let contents: BottleContents?
    public let foods: [String]?
    public let amount: SolidsAmount?
    public let startedAt: String?
    public let endedAt: String?

    public static func breast(id: UUID, side: BreastSide, seconds: Int, startedAt: String? = nil, endedAt: String? = nil) -> Self {
        Self(id: id, kind: .breast, side: side, seconds: seconds, startedAt: startedAt, endedAt: endedAt)
    }

    public static func bottle(id: UUID, volumeML: Decimal, contents: BottleContents, seconds: Int? = nil, startedAt: String? = nil, endedAt: String? = nil) -> Self {
        Self(id: id, kind: .bottle, seconds: seconds, volumeML: volumeML, contents: contents, startedAt: startedAt, endedAt: endedAt)
    }

    public static func solids(id: UUID, foods: [String], amount: SolidsAmount) -> Self {
        Self(id: id, kind: .solids, foods: foods, amount: amount)
    }

    private init(
        id: UUID,
        kind: Kind,
        side: BreastSide? = nil,
        seconds: Int? = nil,
        volumeML: Decimal? = nil,
        contents: BottleContents? = nil,
        foods: [String]? = nil,
        amount: SolidsAmount? = nil,
        startedAt: String? = nil,
        endedAt: String? = nil
    ) {
        self.id = id; self.kind = kind; self.side = side; self.seconds = seconds
        self.volumeML = volumeML; self.contents = contents; self.foods = foods; self.amount = amount
        self.startedAt = startedAt; self.endedAt = endedAt
    }

    private struct Key: CodingKey {
        let stringValue: String
        let intValue: Int? = nil
        init?(stringValue: String) { self.stringValue = stringValue }
        init?(intValue: Int) { return nil }
    }

    public init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: Key.self)
        let allowed = Set(["id", "kind", "side", "seconds", "volume_ml", "contents", "foods", "amount", "started_at", "ended_at"])
        if let unknown = container.allKeys.map(\.stringValue).first(where: { !allowed.contains($0) }) {
            throw FeedComponentWireError.unknownField(unknown)
        }
        func key(_ value: String) -> Key { Key(stringValue: value)! }
        id = try container.decode(UUID.self, forKey: key("id"))
        let rawKind = try container.decode(String.self, forKey: key("kind"))
        guard let decodedKind = Kind(rawValue: rawKind) else {
            throw FeedComponentWireError.unknownKind(rawKind)
        }
        kind = decodedKind
        side = try container.decodeIfPresent(BreastSide.self, forKey: key("side"))
        seconds = try container.decodeIfPresent(Int.self, forKey: key("seconds"))
        volumeML = try container.decodeIfPresent(Decimal.self, forKey: key("volume_ml"))
        if let raw = try container.decodeIfPresent(String.self, forKey: key("contents")) {
            contents = Self.contents(fromWire: raw)
            if contents == nil { throw FeedComponentWireError.invalidShape("contents") }
        } else { contents = nil }
        foods = try container.decodeIfPresent([String].self, forKey: key("foods"))
        amount = try container.decodeIfPresent(SolidsAmount.self, forKey: key("amount"))
        startedAt = try container.decodeIfPresent(String.self, forKey: key("started_at"))
        endedAt = try container.decodeIfPresent(String.self, forKey: key("ended_at"))

        let valid = switch kind {
        case .breast:
            side != nil && seconds.map { $0 > 0 } == true
                && volumeML == nil && contents == nil && foods == nil && amount == nil
        case .bottle:
            volumeML.map { $0 >= 0 } == true && contents != nil
                && side == nil && seconds.map { $0 > 0 } != false && foods == nil && amount == nil
        case .solids:
            foods != nil && amount != nil && startedAt == nil && endedAt == nil
                && side == nil && seconds == nil && volumeML == nil && contents == nil
        }
        guard valid else { throw FeedComponentWireError.invalidShape(kind.rawValue) }
    }

    public func encode(to encoder: any Encoder) throws {
        var container = encoder.container(keyedBy: Key.self)
        func key(_ value: String) -> Key { Key(stringValue: value)! }
        try container.encode(id.uuidString.lowercased(), forKey: key("id"))
        try container.encode(kind.rawValue, forKey: key("kind"))
        switch kind {
        case .breast:
            try container.encode(side, forKey: key("side"))
            try container.encode(seconds, forKey: key("seconds"))
        case .bottle:
            try container.encode(volumeML, forKey: key("volume_ml"))
            try container.encode(contents.map(Self.wireContents), forKey: key("contents"))
            if let seconds { try container.encode(seconds, forKey: key("seconds")) }
        case .solids:
            try container.encode(foods, forKey: key("foods"))
            try container.encode(amount, forKey: key("amount"))
        }
        if let startedAt { try container.encode(startedAt, forKey: key("started_at")) }
        if let endedAt { try container.encode(endedAt, forKey: key("ended_at")) }
    }

    /// The parts of a payload in its current shape: a feed's `components`
    /// or a running timer's `completed_components`, each part with the
    /// server's own ID. The scalar shape before parts (`sides`, `volume_ml`,
    /// `contents`, `foods`, `amount` on the record) is not read: the server
    /// stores and sends only the current shape.
    public static func decodeComponents(_ key: String, of payload: JSONValue) throws -> [WireFeedComponent] {
        guard case .object(let fields) = payload, case .array(let values) = fields[key] else {
            throw FeedComponentWireError.invalidShape(key)
        }
        return try values.map { value in
            try JSONDecoder().decode(Self.self, from: JSONEncoder().encode(value))
        }
    }

    public static func wireContents(_ value: BottleContents) -> String {
        value == .breastMilk ? "breast_milk" : value.rawValue
    }

    public static func contents(fromWire value: String) -> BottleContents? {
        if value == "breast_milk" { return .breastMilk }
        return BottleContents(rawValue: value)
    }
}

public enum JSONValue: Codable, Sendable, Equatable {
    case object([String: JSONValue])
    case array([JSONValue])
    case string(String)
    case number(Decimal)
    case bool(Bool)
    case null

    public init(from decoder: any Decoder) throws {
        let container = try decoder.singleValueContainer()
        if container.decodeNil() { self = .null }
        else if let value = try? container.decode(Bool.self) { self = .bool(value) }
        else if let value = try? container.decode(Decimal.self) { self = .number(value) }
        else if let value = try? container.decode(String.self) { self = .string(value) }
        else if let value = try? container.decode([JSONValue].self) { self = .array(value) }
        else { self = .object(try container.decode([String: JSONValue].self)) }
    }

    public func encode(to encoder: any Encoder) throws {
        var container = encoder.singleValueContainer()
        switch self {
        case .object(let value): try container.encode(value)
        case .array(let value): try container.encode(value)
        case .string(let value): try container.encode(value)
        case .number(let value): try container.encode(value)
        case .bool(let value): try container.encode(value)
        case .null: try container.encodeNil()
        }
    }
}

public struct WireMutation: Codable, Sendable, Equatable {
    public let mutationID: UUID
    public let entityID: UUID
    public let entityKind: WireEntityKind
    public let operation: WireOperation
    public let payloadVersion: Int64
    public let observedEntityVersion: Int64
    public let clientMutationTime: String
    public let importID: UUID?
    public let payload: JSONValue

    public init(
        mutationID: UUID,
        entityID: UUID,
        entityKind: WireEntityKind,
        operation: WireOperation,
        payloadVersion: Int64,
        observedEntityVersion: Int64,
        clientMutationTime: String,
        importID: UUID?,
        payload: JSONValue
    ) {
        self.mutationID = mutationID
        self.entityID = entityID
        self.entityKind = entityKind
        self.operation = operation
        self.payloadVersion = payloadVersion
        self.observedEntityVersion = observedEntityVersion
        self.clientMutationTime = clientMutationTime
        self.importID = importID
        self.payload = payload
    }

    /// Dispatch batches are keyed by mutation ID.
    public var id: UUID { mutationID }

    /// The server's inbound version rule (`AcceptedPayloadVersion`): a
    /// create, an update or a timer command must declare its kind's current
    /// version, so it is in the current shape. A delete names only its baby,
    /// so it has no versioned shape and is accepted at versions 1 through
    /// the current one.
    public var declaresAcceptedPayloadVersion: Bool {
        let current = entityKind.currentPayloadVersion
        if operation == .delete { return (1...current).contains(payloadVersion) }
        return payloadVersion == current
    }

    enum CodingKeys: String, CodingKey {
        case mutationID = "mutation_id", entityID = "entity_id", entityKind = "entity_kind"
        case operation, payloadVersion = "payload_version", observedEntityVersion = "observed_entity_version"
        case clientMutationTime = "client_mutation_time", importID = "import_id", payload
    }

    /// Identifiers go out in canonical lower case.
    ///
    /// `Foundation` encodes a `UUID` in UPPER case and the server requires the
    /// canonical form — it compares `identifier::uuid::text` against the
    /// string it was sent and refuses anything that is not already canonical.
    /// Every mutation this app has ever sent was rejected as
    /// `invalid_request` before the server looked at its contents, and
    /// nothing saw it because nothing ran this transport against a server.
    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(mutationID.uuidString.lowercased(), forKey: .mutationID)
        try container.encode(entityID.uuidString.lowercased(), forKey: .entityID)
        try container.encode(entityKind, forKey: .entityKind)
        try container.encode(operation, forKey: .operation)
        try container.encode(payloadVersion, forKey: .payloadVersion)
        try container.encode(observedEntityVersion, forKey: .observedEntityVersion)
        try container.encode(clientMutationTime, forKey: .clientMutationTime)
        try container.encodeIfPresent(importID?.uuidString.lowercased(), forKey: .importID)
        try container.encode(payload, forKey: .payload)
    }
}

public struct Cursor: Codable, Sendable, Equatable, Hashable {
    public let serverInstanceID: UUID
    public let recoveryEpoch: Int64
    public let familyID: UUID
    public let sequence: Int64

    public init(serverInstanceID: UUID, recoveryEpoch: Int64, familyID: UUID, sequence: Int64) {
        self.serverInstanceID = serverInstanceID
        self.recoveryEpoch = recoveryEpoch
        self.familyID = familyID
        self.sequence = sequence
    }

    enum CodingKeys: String, CodingKey {
        case serverInstanceID = "server_instance_id", recoveryEpoch = "recovery_epoch"
        case familyID = "family_id", sequence
    }

    /// Same canonical-case rule as `WireMutation`: a cursor travels back to
    /// the server on every pull.
    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(serverInstanceID.uuidString.lowercased(), forKey: .serverInstanceID)
        try container.encode(recoveryEpoch, forKey: .recoveryEpoch)
        try container.encode(familyID.uuidString.lowercased(), forKey: .familyID)
        try container.encode(sequence, forKey: .sequence)
    }
}

public struct CanonicalEntity: Codable, Sendable, Equatable {
    public let entityKind: String
    public let meta: EntityMetadata
    public let payload: JSONValue?

    enum CodingKeys: String, CodingKey { case entityKind = "entity_kind", meta, payload }
}

public struct EntityMetadata: Codable, Sendable, Equatable {
    public let id: UUID
    public let familyID: UUID
    public let entityVersion: Int64
    public let creatorAccountID: UUID
    public let lastEditorAccountID: UUID
    public let clientMutationTime: String
    public let acceptedAt: String
    public let tombstonedAt: String?

    enum CodingKeys: String, CodingKey {
        case id, familyID = "family_id", entityVersion = "entity_version"
        case creatorAccountID = "creator_account_id", lastEditorAccountID = "last_editor_account_id"
        case clientMutationTime = "client_mutation_time", acceptedAt = "accepted_at", tombstonedAt = "tombstoned_at"
    }
}

/// The entity kinds supported by this client. The server owns these spellings;
/// the raw values are the wire contract.
public enum WireEntityKind: String, Codable, Sendable, Hashable, CaseIterable {
    case baby, feed, sleep, diaper, supplement, measurement, timer
    case growthProfile = "growth_profile"

    /// The payload version of the kind's one wire shape: feed 4, timer 5,
    /// sleep 2, every other kind 1 (`CurrentPayloadVersion` in
    /// `Server/internal/protocol/mutations.go`). The server stores only this
    /// shape, and this client declares this version on every operation:
    /// creates, updates, deletes, timer commands and import records alike.
    public var currentPayloadVersion: Int64 {
        switch self {
        case .feed: 4
        case .timer: 5
        case .sleep: 2
        case .baby, .diaper, .supplement, .growthProfile, .measurement: 1
        }
    }
}

public enum WireOperation: String, Codable, Sendable, Hashable, CaseIterable {
    case create, update, delete
    case timerStart = "timer_start"
    case timerFinishBottle = "timer_finish_bottle"
    case timerStop = "timer_stop"

    /// The timer commands are the operations whose delivery state another
    /// command can depend on.
    public var isTimerCommand: Bool {
        switch self {
        case .timerStart, .timerFinishBottle, .timerStop: true
        case .create, .update, .delete: false
        }
    }
}

public enum OutcomeStatus: String, Codable, Sendable, Hashable {
    case applied
    case alreadyApplied = "already_applied"
    case superseded
    case rejected
}


/// Complete v1 growth payloads. Optional columns encode as null so corrections
/// clear the same values on every device.
public struct GrowthProfilePayloadV1: Codable, Sendable, Equatable {
    public let babyID: UUID
    public let referenceSex: GrowthReferenceSex
    public let expectedDueDate: String?

    enum CodingKeys: String, CodingKey {
        case babyID = "baby_id", referenceSex = "reference_sex", expectedDueDate = "expected_due_date"
    }

    public func validate(entityID: UUID) throws {
        guard entityID == GrowthProfileID.forBaby(babyID) else { throw GrowthWireError.invalidProfileID }
        if let expectedDueDate { _ = try LocalDate(expectedDueDate) }
    }

    public func encode(to encoder: any Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(babyID.uuidString.lowercased(), forKey: .babyID)
        try c.encode(referenceSex, forKey: .referenceSex)
        try c.encode(expectedDueDate, forKey: .expectedDueDate)
    }
}

public struct MeasurementPayloadV1: Codable, Sendable, Equatable {
    public let babyID: UUID
    public let measuredAt: String
    public let loggedAt: String
    public let timeZoneID: String
    public let weightKG: Double?
    public let lengthCM: Double?
    public let headCircumferenceCM: Double?
    public let lengthPosition: LengthPosition?

    enum CodingKeys: String, CodingKey {
        case babyID = "baby_id", measuredAt = "measured_at", loggedAt = "logged_at", timeZoneID = "time_zone_id"
        case weightKG = "weight_kg", lengthCM = "length_cm", headCircumferenceCM = "head_circumference_cm", lengthPosition = "length_position"
    }

    public func validatedValues() throws -> MeasurementValues {
        _ = try SupplementTimestamp.parse(measuredAt)
        _ = try SupplementTimestamp.parse(loggedAt)
        guard TimeZone(identifier: timeZoneID) != nil else { throw GrowthWireError.invalidTimeZone }
        return try MeasurementValues(weightKG: weightKG, lengthCM: lengthCM,
            headCircumferenceCM: headCircumferenceCM, lengthPosition: lengthPosition)
    }

    public func encode(to encoder: any Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(babyID.uuidString.lowercased(), forKey: .babyID)
        try c.encode(measuredAt, forKey: .measuredAt)
        try c.encode(loggedAt, forKey: .loggedAt)
        try c.encode(timeZoneID, forKey: .timeZoneID)
        try c.encode(weightKG, forKey: .weightKG)
        try c.encode(lengthCM, forKey: .lengthCM)
        try c.encode(headCircumferenceCM, forKey: .headCircumferenceCM)
        try c.encode(lengthPosition, forKey: .lengthPosition)
    }
}

public enum GrowthWireError: Error, Sendable {
    case invalidProfileID, invalidTimeZone
}
