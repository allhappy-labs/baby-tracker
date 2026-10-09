import Foundation

public struct RecoverableRefreshResult: Sendable, Equatable {
    public let credentials: SessionCredentials
    public let attemptID: UUID
    public let accountID: UUID
    public let reauthenticateAt: Date

    public init(credentials: SessionCredentials, attemptID: UUID, accountID: UUID, reauthenticateAt: Date) {
        self.credentials = credentials
        self.attemptID = attemptID
        self.accountID = accountID
        self.reauthenticateAt = reauthenticateAt
    }
}

public protocol SessionRecoveryTransport: SessionRefreshing {
    func serverInfo(_ origin: ServerOrigin) async throws -> ServerInfo
    func refreshRecoverable(parent: SessionCredentials, expectedAccountID: UUID, attemptID: UUID, at endpoint: ConfirmedEndpoint) async throws -> RecoverableRefreshResult
    func acknowledgeRefresh(attemptID: UUID, credentials: SessionCredentials, at endpoint: ConfirmedEndpoint) async throws
}

struct RecoverableRefreshBody: Encodable {
    let parent: SessionCredentials
    let attemptID: UUID
    enum CodingKeys: String, CodingKey {
        case protocolVersion = "protocol_version", capabilities, refreshToken = "refresh_token"
        case refreshAttemptID = "refresh_attempt_id", deviceContinuity = "device_continuity"
    }
    enum ContinuityKeys: String, CodingKey { case deviceID = "device_id", credential }
    func encode(to encoder: Encoder) throws {
        var body = encoder.container(keyedBy: CodingKeys.self)
        try body.encode("v1", forKey: .protocolVersion)
        try body.encode(["identity-v1", "refresh-recovery-v1"], forKey: .capabilities)
        try body.encode(parent.refreshToken, forKey: .refreshToken)
        try body.encode(attemptID.uuidString.lowercased(), forKey: .refreshAttemptID)
        var proof = body.nestedContainer(keyedBy: ContinuityKeys.self, forKey: .deviceContinuity)
        try proof.encode(parent.deviceContinuity.deviceID.uuidString.lowercased(), forKey: .deviceID)
        try proof.encode(parent.deviceContinuity.credential, forKey: .credential)
    }
}

struct RefreshAckBody: Encodable {
    let credentials: SessionCredentials
    let attemptID: UUID
    enum CodingKeys: String, CodingKey {
        case protocolVersion = "protocol_version", capabilities
        case refreshAttemptID = "refresh_attempt_id", deviceContinuity = "device_continuity"
    }
    enum ContinuityKeys: String, CodingKey { case deviceID = "device_id", credential }
    func encode(to encoder: Encoder) throws {
        var body = encoder.container(keyedBy: CodingKeys.self)
        try body.encode("v1", forKey: .protocolVersion)
        try body.encode(["identity-v1", "refresh-recovery-v1"], forKey: .capabilities)
        try body.encode(attemptID.uuidString.lowercased(), forKey: .refreshAttemptID)
        var proof = body.nestedContainer(keyedBy: ContinuityKeys.self, forKey: .deviceContinuity)
        try proof.encode(credentials.deviceContinuity.deviceID.uuidString.lowercased(), forKey: .deviceID)
        try proof.encode(credentials.deviceContinuity.credential, forKey: .credential)
    }
}

struct WireRecoverableRefreshResponse: Decodable {
    let session: WireRefreshResponse
    let attemptID: UUID
    let accountID: UUID
    let instanceID: UUID
    let reauthenticateAt: Date

    enum CodingKeys: String, CodingKey {
        case protocolVersion = "protocol_version", refreshAttemptID = "refresh_attempt_id"
        case account, serverInstanceID = "server_instance_id", reauthenticateAt = "reauthenticate_at"
    }
    enum AccountKeys: String, CodingKey { case id }

    init(from decoder: Decoder) throws {
        let body = try decoder.container(keyedBy: CodingKeys.self)
        guard try body.decode(String.self, forKey: .protocolVersion) == "v1" else { throw HTTPClientError.invalidRecoveryResponse }
        func canonical(_ value: String) throws -> UUID {
            guard let id = UUID(uuidString: value), value == id.uuidString.lowercased(), id != UUID(uuidString: "00000000-0000-0000-0000-000000000000") else { throw HTTPClientError.invalidRecoveryResponse }
            return id
        }
        attemptID = try canonical(body.decode(String.self, forKey: .refreshAttemptID))
        instanceID = try canonical(body.decode(String.self, forKey: .serverInstanceID))
        let account = try body.nestedContainer(keyedBy: AccountKeys.self, forKey: .account)
        accountID = try canonical(account.decode(String.self, forKey: .id))
        reauthenticateAt = try RFC3339.date(from: body.decode(String.self, forKey: .reauthenticateAt), key: .reauthenticateAt, in: body)
        session = try WireRefreshResponse(from: decoder)
    }
}
