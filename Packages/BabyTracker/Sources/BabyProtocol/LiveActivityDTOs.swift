import Foundation

public enum LiveActivityContract {
    public static let capability = "live-activity-push-v1"
    public static let payloadVersion = 1
    public static let maximumCounter: Int64 = 9_007_199_254_740_991

    static func counter(_ value: Int64, minimum: Int64 = 1) throws {
        guard (minimum...maximumCounter).contains(value) else { throw LiveActivityContractError.invalidCounter }
    }

    static func uuid(_ value: UUID) throws {
        guard value.uuidString != "00000000-0000-0000-0000-000000000000" else { throw LiveActivityContractError.invalidUUID }
    }

    static func token(_ value: String) throws {
        guard !value.isEmpty, value.utf8.count <= 2_048, value.utf8.count.isMultiple(of: 2),
              value.utf8.allSatisfy({ (48...57).contains($0) || (97...102).contains($0) }) else {
            throw LiveActivityContractError.invalidToken
        }
    }

    static func metadata(_ version: String, _ capabilities: [String]) throws {
        guard version == "v1" else { throw LiveActivityContractError.unsupportedProtocol }
        guard capabilities == [capability] else { throw LiveActivityContractError.unsupportedCapability }
    }
}

public enum LiveActivityContractError: Error, Equatable, Sendable {
    case invalidUUID, invalidCounter, invalidKind, invalidToken, invalidFeedBreak, invalidActivityID
    case unsupportedProtocol, unsupportedCapability, unsupportedPayloadVersion, invalidReceipt
}

/// Immutable realm and card target. Timer/session references belong to content.
/// Its Codable keys are ActivityKit's camelCase; registrations use a wire adapter.
public struct LiveActivityScope: Codable, Hashable, Sendable {
    public let serverInstanceID: UUID
    public let recoveryEpoch: Int64
    public let familyID: UUID
    public let babyID: UUID
    public let kindRaw: String

    public init(serverInstanceID: UUID, recoveryEpoch: Int64, familyID: UUID, babyID: UUID, kindRaw: String) throws {
        self.serverInstanceID = serverInstanceID; self.recoveryEpoch = recoveryEpoch
        self.familyID = familyID; self.babyID = babyID; self.kindRaw = kindRaw
        try validate()
    }

    public func validate() throws {
        try LiveActivityContract.uuid(serverInstanceID)
        try LiveActivityContract.uuid(familyID)
        try LiveActivityContract.uuid(babyID)
        try LiveActivityContract.counter(recoveryEpoch)
        guard kindRaw == "feed" || kindRaw == "sleep" else { throw LiveActivityContractError.invalidKind }
    }

    private enum CodingKeys: String, CodingKey { case serverInstanceID, recoveryEpoch, familyID, babyID, kindRaw }

    public init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        try self.init(serverInstanceID: container.canonicalUUID(.serverInstanceID), recoveryEpoch: container.exactInt64(.recoveryEpoch),
            familyID: container.canonicalUUID(.familyID), babyID: container.canonicalUUID(.babyID), kindRaw: container.decode(String.self, forKey: .kindRaw))
    }

    public func encode(to encoder: any Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(serverInstanceID.uuidString.lowercased(), forKey: .serverInstanceID)
        try container.encode(recoveryEpoch, forKey: .recoveryEpoch)
        try container.encode(familyID.uuidString.lowercased(), forKey: .familyID)
        try container.encode(babyID.uuidString.lowercased(), forKey: .babyID)
        try container.encode(kindRaw, forKey: .kindRaw)
    }
}

/// Ordering metadata for one card lifecycle. The APNs timestamp is Unix seconds,
/// independent of Foundation's reference-date seconds in ActivityKit content.
public struct LiveActivityRevision: Codable, Hashable, Sendable {
    public let scope: LiveActivityScope
    public let generationID: UUID
    public let timerID: UUID?
    public let sessionID: UUID?
    public let familySequence: Int64
    public let presentationVersion: Int64
    public let presentationTimestamp: Int64
    public let canWrite: Bool

    public init(scope: LiveActivityScope, generationID: UUID, timerID: UUID? = nil, sessionID: UUID? = nil,
                familySequence: Int64, presentationVersion: Int64, presentationTimestamp: Int64, canWrite: Bool) throws {
        self.scope = scope; self.generationID = generationID; self.timerID = timerID; self.sessionID = sessionID
        self.familySequence = familySequence; self.presentationVersion = presentationVersion
        self.presentationTimestamp = presentationTimestamp; self.canWrite = canWrite
        try validate()
    }

    public func validate() throws {
        try scope.validate()
        for id in [generationID, timerID, sessionID].compactMap({ $0 }) { try LiveActivityContract.uuid(id) }
        try LiveActivityContract.counter(familySequence, minimum: 0)
        try LiveActivityContract.counter(presentationVersion)
        try LiveActivityContract.counter(presentationTimestamp, minimum: 0)
    }

    private enum CodingKeys: String, CodingKey {
        case scope, generationID, timerID, sessionID, familySequence, presentationVersion, presentationTimestamp, canWrite
    }

    public init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        try self.init(scope: container.decode(LiveActivityScope.self, forKey: .scope), generationID: container.canonicalUUID(.generationID),
            timerID: container.optionalCanonicalUUID(.timerID), sessionID: container.optionalCanonicalUUID(.sessionID),
            familySequence: container.exactInt64(.familySequence, minimum: 0), presentationVersion: container.exactInt64(.presentationVersion),
            presentationTimestamp: container.exactInt64(.presentationTimestamp, minimum: 0), canWrite: container.decode(Bool.self, forKey: .canWrite))
    }

    public func encode(to encoder: any Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(scope, forKey: .scope)
        try container.encode(generationID.uuidString.lowercased(), forKey: .generationID)
        try container.encodeIfPresent(timerID?.uuidString.lowercased(), forKey: .timerID)
        try container.encodeIfPresent(sessionID?.uuidString.lowercased(), forKey: .sessionID)
        try container.encode(familySequence, forKey: .familySequence)
        try container.encode(presentationVersion, forKey: .presentationVersion)
        try container.encode(presentationTimestamp, forKey: .presentationTimestamp)
        try container.encode(canWrite, forKey: .canWrite)
    }
}

public enum LiveActivityAPNsEnvironment: String, Codable, Sendable { case sandbox, production }
public enum LiveActivityRegistrationState: String, Codable, Sendable { case active, ended, dismissed }

/// A foreground Activity.activities inventory is the only absence proof. A
/// matching old APNs submission may still arrive later; this is not cancellation.
public struct LiveActivityAbsentGeneration: Codable, Equatable, Sendable {
    public let generationID: UUID
    public let observedAttemptID: UUID
    public init(generationID: UUID, observedAttemptID: UUID) throws {
        try LiveActivityContract.uuid(generationID); try LiveActivityContract.uuid(observedAttemptID)
        self.generationID = generationID; self.observedAttemptID = observedAttemptID
    }
    private enum CodingKeys: String, CodingKey { case generationID = "generation_id", observedAttemptID = "observed_attempt_id" }
    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        try self.init(generationID: c.canonicalUUID(.generationID), observedAttemptID: c.canonicalUUID(.observedAttemptID))
    }
    public func encode(to encoder: any Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(generationID.uuidString.lowercased(), forKey: .generationID)
        try c.encode(observedAttemptID.uuidString.lowercased(), forKey: .observedAttemptID)
    }
}
public struct LiveActivityForegroundInventory: Codable, Equatable, Sendable {
    public let inventoryVersion: Int64
    public let absentGenerations: [LiveActivityAbsentGeneration]
    public init(inventoryVersion: Int64, absentGenerations: [LiveActivityAbsentGeneration]) throws {
        try LiveActivityContract.counter(inventoryVersion)
        guard absentGenerations.count <= 16, Set(absentGenerations.map(\.generationID)).count == absentGenerations.count else { throw LiveActivityContractError.invalidReceipt }
        self.inventoryVersion = inventoryVersion; self.absentGenerations = absentGenerations
    }
    private enum CodingKeys: String, CodingKey { case inventoryVersion = "inventory_version", absentGenerations = "absent_generations" }
    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        try self.init(inventoryVersion: c.exactInt64(.inventoryVersion), absentGenerations: c.decode([LiveActivityAbsentGeneration].self, forKey: .absentGenerations))
    }
}
public struct LiveActivityUnconfirmedStart: Codable, Equatable, Sendable {
    public let scope: LiveActivityScope
    public let generationID: UUID
    public let attemptID: UUID
    public init(scope: LiveActivityScope, generationID: UUID, attemptID: UUID) throws {
        try LiveActivityContract.uuid(generationID); try LiveActivityContract.uuid(attemptID)
        self.scope = scope; self.generationID = generationID; self.attemptID = attemptID
    }
    private enum CodingKeys: String, CodingKey { case scope, generationID = "generation_id", attemptID = "attempt_id" }
    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        try self.init(scope: c.decode(RegistrationScope.self, forKey: .scope).value, generationID: c.canonicalUUID(.generationID), attemptID: c.canonicalUUID(.attemptID))
    }
    public func encode(to encoder: any Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(RegistrationScope(value: scope), forKey: .scope)
        try c.encode(generationID.uuidString.lowercased(), forKey: .generationID)
        try c.encode(attemptID.uuidString.lowercased(), forKey: .attemptID)
    }
}

public struct LiveActivityDeviceRegistration: Codable, Equatable, Sendable {
    public let protocolVersion: String
    public let capabilities: [String]
    public let installationID: UUID
    public let registrationVersion: Int64
    public let apnsEnvironment: LiveActivityAPNsEnvironment
    public let payloadVersion: Int
    public let pushToStartToken: String
    public let feedBreakMinutes: Int
    public let foregroundInventory: LiveActivityForegroundInventory?

    public init(installationID: UUID, registrationVersion: Int64, apnsEnvironment: LiveActivityAPNsEnvironment,
                payloadVersion: Int = 1, pushToStartToken: String, feedBreakMinutes: Int = 30, foregroundInventory: LiveActivityForegroundInventory? = nil) throws {
        self.protocolVersion = "v1"; self.capabilities = [LiveActivityContract.capability]
        self.installationID = installationID; self.registrationVersion = registrationVersion; self.apnsEnvironment = apnsEnvironment
        self.foregroundInventory = foregroundInventory
        self.payloadVersion = payloadVersion; self.pushToStartToken = pushToStartToken; self.feedBreakMinutes = feedBreakMinutes
        try validate()
    }

    public func validate() throws {
        try LiveActivityContract.metadata(protocolVersion, capabilities)
        try LiveActivityContract.uuid(installationID)
        try LiveActivityContract.counter(registrationVersion)
        guard payloadVersion == 1 else { throw LiveActivityContractError.unsupportedPayloadVersion }
        try LiveActivityContract.token(pushToStartToken)
        guard (5...120).contains(feedBreakMinutes), feedBreakMinutes.isMultiple(of: 5) else { throw LiveActivityContractError.invalidFeedBreak }
    }

    private enum CodingKeys: String, CodingKey {
        case protocolVersion = "protocol_version", capabilities, installationID = "installation_id", registrationVersion = "registration_version"
        case foregroundInventory = "foreground_inventory"
        case apnsEnvironment = "apns_environment", payloadVersion = "payload_version", pushToStartToken = "push_to_start_token", feedBreakMinutes = "feed_break_minutes"
    }

    public init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        protocolVersion = try container.decode(String.self, forKey: .protocolVersion)
        capabilities = try container.decode([String].self, forKey: .capabilities)
        installationID = try container.canonicalUUID(.installationID)
        registrationVersion = try container.exactInt64(.registrationVersion)
        apnsEnvironment = try container.decode(LiveActivityAPNsEnvironment.self, forKey: .apnsEnvironment)
        payloadVersion = Int(try container.exactInt64(.payloadVersion, minimum: 0))
        pushToStartToken = try container.decode(String.self, forKey: .pushToStartToken)
        foregroundInventory = try container.decodeIfPresent(LiveActivityForegroundInventory.self, forKey: .foregroundInventory)
        feedBreakMinutes = try container.optionalExactInt64(.feedBreakMinutes, minimum: 0).map(Int.init) ?? 30
        try validate()
    }

    public func encode(to encoder: any Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(protocolVersion, forKey: .protocolVersion)
        try container.encode(capabilities, forKey: .capabilities)
        try container.encode(installationID.uuidString.lowercased(), forKey: .installationID)
        try container.encode(registrationVersion, forKey: .registrationVersion)
        try container.encode(apnsEnvironment, forKey: .apnsEnvironment)
        try container.encode(payloadVersion, forKey: .payloadVersion)
        try container.encode(pushToStartToken, forKey: .pushToStartToken)
        try container.encode(feedBreakMinutes, forKey: .feedBreakMinutes)
        try container.encodeIfPresent(foregroundInventory, forKey: .foregroundInventory)
    }
}

public struct LiveActivityTokenRegistration: Codable, Equatable, Sendable {
    public let protocolVersion: String
    public let capabilities: [String]
    public let installationID: UUID
    public let activityID: String
    public let token: String
    public let tokenVersion: Int64
    public let scope: LiveActivityScope
    public let knownGenerationID: UUID?
    public let timerID: UUID?
    public let sessionID: UUID?
    public let state: LiveActivityRegistrationState

    public init(installationID: UUID, activityID: String, token: String, tokenVersion: Int64, scope: LiveActivityScope,
                knownGenerationID: UUID? = nil, timerID: UUID? = nil, sessionID: UUID? = nil, state: LiveActivityRegistrationState) throws {
        protocolVersion = "v1"; capabilities = [LiveActivityContract.capability]
        self.installationID = installationID; self.activityID = activityID; self.token = token; self.tokenVersion = tokenVersion
        self.scope = scope; self.knownGenerationID = knownGenerationID; self.timerID = timerID; self.sessionID = sessionID; self.state = state
        try validate()
    }

    public func validate() throws {
        try LiveActivityContract.metadata(protocolVersion, capabilities)
        for id in [installationID, knownGenerationID, timerID, sessionID].compactMap({ $0 }) { try LiveActivityContract.uuid(id) }
        guard !activityID.isEmpty, activityID.utf8.count <= 256,
              activityID.unicodeScalars.allSatisfy({ !CharacterSet.controlCharacters.contains($0) }) else { throw LiveActivityContractError.invalidActivityID }
        try LiveActivityContract.token(token)
        try LiveActivityContract.counter(tokenVersion)
        try scope.validate()
    }

    private enum CodingKeys: String, CodingKey {
        case protocolVersion = "protocol_version", capabilities, installationID = "installation_id", activityID = "activity_id", token
        case tokenVersion = "token_version", scope, knownGenerationID = "known_generation_id", timerID = "timer_id", sessionID = "session_id", state
    }

    public init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        protocolVersion = try container.decode(String.self, forKey: .protocolVersion)
        capabilities = try container.decode([String].self, forKey: .capabilities)
        installationID = try container.canonicalUUID(.installationID)
        activityID = try container.decode(String.self, forKey: .activityID)
        token = try container.decode(String.self, forKey: .token)
        tokenVersion = try container.exactInt64(.tokenVersion)
        scope = try container.decode(RegistrationScope.self, forKey: .scope).value
        knownGenerationID = try container.optionalCanonicalUUID(.knownGenerationID)
        timerID = try container.optionalCanonicalUUID(.timerID)
        sessionID = try container.optionalCanonicalUUID(.sessionID)
        state = try container.decode(LiveActivityRegistrationState.self, forKey: .state)
        try validate()
    }

    public func encode(to encoder: any Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(protocolVersion, forKey: .protocolVersion)
        try container.encode(capabilities, forKey: .capabilities)
        try container.encode(installationID.uuidString.lowercased(), forKey: .installationID)
        try container.encode(activityID, forKey: .activityID)
        try container.encode(token, forKey: .token)
        try container.encode(tokenVersion, forKey: .tokenVersion)
        try container.encode(RegistrationScope(value: scope), forKey: .scope)
        try container.encodeIfPresent(knownGenerationID?.uuidString.lowercased(), forKey: .knownGenerationID)
        try container.encodeIfPresent(timerID?.uuidString.lowercased(), forKey: .timerID)
        try container.encodeIfPresent(sessionID?.uuidString.lowercased(), forKey: .sessionID)
        try container.encode(state, forKey: .state)
    }
}

/// The server maps observed ActivityKit IDs to its authoritative generation.
public struct LiveActivityGenerationMapping: Codable, Equatable, Sendable {
    public let activityID: String
    public let scope: LiveActivityScope
    public let generationID: UUID

    public init(activityID: String, scope: LiveActivityScope, generationID: UUID) throws {
        self.activityID = activityID; self.scope = scope; self.generationID = generationID
        try validate()
    }

    public func validate() throws {
        guard !activityID.isEmpty, activityID.utf8.count <= 256,
              activityID.unicodeScalars.allSatisfy({ !CharacterSet.controlCharacters.contains($0) }) else { throw LiveActivityContractError.invalidActivityID }
        try scope.validate()
        try LiveActivityContract.uuid(generationID)
    }

    private enum CodingKeys: String, CodingKey { case activityID = "activity_id", scope, generationID = "generation_id" }
    public init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        try self.init(activityID: container.decode(String.self, forKey: .activityID), scope: container.decode(RegistrationScope.self, forKey: .scope).value,
            generationID: container.canonicalUUID(.generationID))
    }
    public func encode(to encoder: any Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(activityID, forKey: .activityID)
        try container.encode(RegistrationScope(value: scope), forKey: .scope)
        try container.encode(generationID.uuidString.lowercased(), forKey: .generationID)
    }
}

public struct LiveActivityRegistrationReceipt: Codable, Equatable, Sendable {
    public let protocolVersion: String
    public let serverInstanceID: UUID
    public let recoveryEpoch: Int64
    public let acceptedRegistrationVersion: Int64
    public let acceptedTokenVersion: Int64?
    public let generations: [LiveActivityGenerationMapping]
    public let unconfirmedStarts: [LiveActivityUnconfirmedStart]

    public init(serverInstanceID: UUID, recoveryEpoch: Int64, acceptedRegistrationVersion: Int64,
                acceptedTokenVersion: Int64? = nil, generations: [LiveActivityGenerationMapping] = [], unconfirmedStarts: [LiveActivityUnconfirmedStart] = []) throws {
        protocolVersion = "v1"; self.serverInstanceID = serverInstanceID; self.recoveryEpoch = recoveryEpoch
        self.acceptedRegistrationVersion = acceptedRegistrationVersion; self.acceptedTokenVersion = acceptedTokenVersion; self.generations = generations; self.unconfirmedStarts = unconfirmedStarts
        try validate()
    }

    public func validate() throws {
        guard protocolVersion == "v1" else { throw LiveActivityContractError.unsupportedProtocol }
        try LiveActivityContract.uuid(serverInstanceID)
        try LiveActivityContract.counter(recoveryEpoch)
        try LiveActivityContract.counter(acceptedRegistrationVersion)
        if let acceptedTokenVersion { try LiveActivityContract.counter(acceptedTokenVersion) }
        for start in unconfirmedStarts {
            guard start.scope.serverInstanceID == serverInstanceID, start.scope.recoveryEpoch == recoveryEpoch else { throw LiveActivityContractError.invalidReceipt }
        }
        var activities = Set<String>()
        for mapping in generations {
            try mapping.validate()
            guard mapping.scope.serverInstanceID == serverInstanceID, mapping.scope.recoveryEpoch == recoveryEpoch,
                  activities.insert(mapping.activityID).inserted else { throw LiveActivityContractError.invalidReceipt }
        }
    }

    private enum CodingKeys: String, CodingKey {
        case protocolVersion = "protocol_version", serverInstanceID = "server_instance_id", recoveryEpoch = "recovery_epoch"
        case acceptedRegistrationVersion = "accepted_registration_version", acceptedTokenVersion = "accepted_token_version", generations, unconfirmedStarts = "unconfirmed_starts"
    }
    public init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        protocolVersion = try container.decode(String.self, forKey: .protocolVersion)
        serverInstanceID = try container.canonicalUUID(.serverInstanceID)
        recoveryEpoch = try container.exactInt64(.recoveryEpoch)
        acceptedRegistrationVersion = try container.exactInt64(.acceptedRegistrationVersion)
        acceptedTokenVersion = try container.optionalExactInt64(.acceptedTokenVersion)
        generations = try container.decode([LiveActivityGenerationMapping].self, forKey: .generations)
        unconfirmedStarts = try container.decodeIfPresent([LiveActivityUnconfirmedStart].self, forKey: .unconfirmedStarts) ?? []
        try validate()
    }
    public func encode(to encoder: any Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(protocolVersion, forKey: .protocolVersion)
        try container.encode(serverInstanceID.uuidString.lowercased(), forKey: .serverInstanceID)
        try container.encode(recoveryEpoch, forKey: .recoveryEpoch)
        try container.encode(acceptedRegistrationVersion, forKey: .acceptedRegistrationVersion)
        try container.encodeIfPresent(acceptedTokenVersion, forKey: .acceptedTokenVersion)
        try container.encode(generations, forKey: .generations)
        if !unconfirmedStarts.isEmpty { try container.encode(unconfirmedStarts, forKey: .unconfirmedStarts) }
    }
}

/// Snake-case adapter; the same identity has camelCase keys inside ActivityKit.
private struct RegistrationScope: Codable {
    let value: LiveActivityScope
    private enum CodingKeys: String, CodingKey {
        case serverInstanceID = "server_instance_id", recoveryEpoch = "recovery_epoch", familyID = "family_id", babyID = "baby_id", kindRaw = "kind_raw"
    }
    init(value: LiveActivityScope) { self.value = value }
    init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        value = try LiveActivityScope(serverInstanceID: container.canonicalUUID(.serverInstanceID), recoveryEpoch: container.exactInt64(.recoveryEpoch),
            familyID: container.canonicalUUID(.familyID), babyID: container.canonicalUUID(.babyID), kindRaw: container.decode(String.self, forKey: .kindRaw))
    }
    func encode(to encoder: any Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(value.serverInstanceID.uuidString.lowercased(), forKey: .serverInstanceID)
        try container.encode(value.recoveryEpoch, forKey: .recoveryEpoch)
        try container.encode(value.familyID.uuidString.lowercased(), forKey: .familyID)
        try container.encode(value.babyID.uuidString.lowercased(), forKey: .babyID)
        try container.encode(value.kindRaw, forKey: .kindRaw)
    }
}

private extension KeyedDecodingContainer {
    /// Decimal preserves fractional JSON values that Foundation's Int64 decoder can round away.
    /// JSONDecoder does not expose the original numeric lexeme, so precision beyond Decimal's
    /// representation cannot be checked here; incoming server requests use lexical validation.
    func exactInt64(_ key: Key, minimum: Int64 = 1) throws -> Int64 {
        let value = try decode(Decimal.self, forKey: key)
        guard value >= Decimal(minimum), value <= Decimal(LiveActivityContract.maximumCounter) else {
            throw LiveActivityContractError.invalidCounter
        }
        let integer = NSDecimalNumber(decimal: value).int64Value
        guard value == Decimal(integer) else { throw LiveActivityContractError.invalidCounter }
        return integer
    }
    func optionalExactInt64(_ key: Key, minimum: Int64 = 1) throws -> Int64? {
        guard contains(key), try !decodeNil(forKey: key) else { return nil }
        return try exactInt64(key, minimum: minimum)
    }
    func canonicalUUID(_ key: Key) throws -> UUID {
        let raw = try decode(String.self, forKey: key)
        guard let value = UUID(uuidString: raw), value.uuidString.lowercased() == raw else { throw LiveActivityContractError.invalidUUID }
        try LiveActivityContract.uuid(value)
        return value
    }
    func optionalCanonicalUUID(_ key: Key) throws -> UUID? {
        guard contains(key), try !decodeNil(forKey: key) else { return nil }
        return try canonicalUUID(key)
    }
}
