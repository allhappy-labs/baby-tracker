import Foundation

public struct RefreshAttempt: Codable, Sendable, Equatable {
    public let attemptID: UUID
    public let credentialVersion: UUID
    public let ownerGeneration: Int64
    public let startedAt: Date
    public let protocolMode: RefreshProtocolMode?
    public init(attemptID: UUID, credentialVersion: UUID, ownerGeneration: Int64, startedAt: Date, protocolMode: RefreshProtocolMode? = .legacy) {
        self.attemptID = attemptID; self.credentialVersion = credentialVersion
        self.ownerGeneration = ownerGeneration; self.startedAt = startedAt
        self.protocolMode = protocolMode
    }
    private enum CodingKeys: String, CodingKey { case attemptID, credentialVersion, ownerGeneration, startedAt, protocolMode }
    public init(from decoder: any Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        attemptID = try values.decode(UUID.self, forKey: .attemptID)
        credentialVersion = try values.decode(UUID.self, forKey: .credentialVersion)
        ownerGeneration = try values.decode(Int64.self, forKey: .ownerGeneration)
        startedAt = try values.decode(Date.self, forKey: .startedAt)
        protocolMode = try values.decodeIfPresent(RefreshProtocolMode.self, forKey: .protocolMode) ?? .legacy
    }

}

public enum RefreshAttemptState: Codable, Sendable, Equatable {
    case idle
    case inFlightUncertain(RefreshAttempt)
}
