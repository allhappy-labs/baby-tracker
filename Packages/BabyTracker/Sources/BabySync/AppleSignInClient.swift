import Foundation

/// What a completed Sign in with Apple gives us.
public struct AppleProof: Sendable, Equatable {
    public let identityToken: String
    public let authorizationCode: String?
    /// Apple's opaque per-team user identifier. Stored only in Keychain.
    public let userIdentifier: String
    /// Apple returns a name on the *first* authorization only.
    public let fullName: String?

    public init(identityToken: String, authorizationCode: String?, userIdentifier: String, fullName: String?) {
        self.identityToken = identityToken
        self.authorizationCode = authorizationCode
        self.userIdentifier = userIdentifier
        self.fullName = fullName
    }
}

/// Requests a fresh Apple assertion bound to one server's nonce.
public protocol AppleSignInClient: Sendable {
    func requestProof(nonce: String) async throws -> AppleProof
}

/// Reads Apple's opinion of a stored credential.
public protocol AppleCredentialStateClient: Sendable {
    func credentialState(forUserIdentifier userIdentifier: String) async throws -> AppleCredentialState
}

/// `POST /v1/auth/apple`, exactly as the server's strict decoder takes it
/// (`protocol.AppleExchangeRequest`, `Server/internal/protocol/auth.go`;
/// pinned by `Server/api/fixtures/auth-reauth-continuity.json`).
///
/// It used to carry a `nonce` the server does not accept and none of
/// `protocol_version`, `capabilities`, `command_id`, `challenge` or
/// `installation_id`, so every sign-in was refused before the Apple proof
/// was even looked at. The server derives the nonce itself from `challenge`;
/// the client computes the same one only to hand it to Apple.
public struct AppleExchangeRequest: Encodable, Sendable, Equatable {
    /// Fresh per attempt: the server treats a repeated command ID as a replay.
    public let commandID: UUID
    /// The challenge exactly as `POST /v1/auth/challenge` returned it.
    public let challenge: String
    public let identityToken: String
    /// Empty unless the server's `apple_authorization_mode` is
    /// `code_exchange_required` (hosted). A self-hosted server runs
    /// `identity_token_only` and refuses any non-empty code as
    /// `hosted_only_field`.
    public let authorizationCode: String
    /// One per install, kept in the Keychain; the server binds the device to it.
    public let installationID: String
    /// The name other caregivers see. Required on an account's first sign-in
    /// (`display_name_required` otherwise) and accepted on every later one.
    public let displayName: String?
    /// Present only when this device already holds an account-qualified proof
    /// for this exact origin and instance.
    public let deviceContinuity: DeviceContinuity?

    public init(
        commandID: UUID = UUID(), challenge: String, identityToken: String, authorizationCode: String,
        installationID: String, displayName: String?, deviceContinuity: DeviceContinuity?
    ) {
        self.commandID = commandID
        self.challenge = challenge
        self.identityToken = identityToken
        self.authorizationCode = authorizationCode
        self.installationID = installationID
        self.displayName = displayName
        self.deviceContinuity = deviceContinuity
    }

    /// The `authorization_code` a server in `mode` accepts: Apple's code for
    /// a hosted server, nothing at all for a self-hosted one.
    public static func authorizationCode(for mode: String, proof: AppleProof) -> String {
        mode == "code_exchange_required" ? (proof.authorizationCode ?? "") : ""
    }

    enum CodingKeys: String, CodingKey {
        case protocolVersion = "protocol_version", capabilities, commandID = "command_id", challenge
        case identityToken = "identity_token", authorizationCode = "authorization_code"
        case installationID = "installation_id", profile, deviceContinuity = "device_continuity"
    }

    private struct Profile: Encodable {
        let displayName: String
        let confirmed = true
        enum CodingKeys: String, CodingKey { case displayName = "display_name", confirmed }
    }

    private struct Continuity: Encodable {
        let deviceID: String
        let credential: String
        enum CodingKeys: String, CodingKey { case deviceID = "device_id", credential }
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode("v1", forKey: .protocolVersion)
        try container.encode(["identity-v1"], forKey: .capabilities)
        try container.encode(commandID.uuidString.lowercased(), forKey: .commandID)
        try container.encode(challenge, forKey: .challenge)
        try container.encode(identityToken, forKey: .identityToken)
        try container.encode(authorizationCode, forKey: .authorizationCode)
        try container.encode(installationID, forKey: .installationID)
        if let displayName {
            try container.encode(Profile(displayName: displayName), forKey: .profile)
        } else {
            try container.encodeNil(forKey: .profile)
        }
        if let deviceContinuity {
            try container.encode(
                Continuity(deviceID: deviceContinuity.deviceID.uuidString.lowercased(), credential: deviceContinuity.credential),
                forKey: .deviceContinuity
            )
        } else {
            try container.encodeNil(forKey: .deviceContinuity)
        }
    }
}

public struct SessionBundle: Sendable, Equatable {
    public let accountID: UUID
    public let deviceID: UUID
    public let recoveryEpoch: UInt64
    public let accessToken: String
    public let refreshToken: String
    public let expiresAt: Date
    /// Returned exactly once, on the exchange that allocates the device.
    public let deviceCredential: String?
    public let familyID: UUID?
    public let isRecoveryOnly: Bool

    public init(
        accountID: UUID, deviceID: UUID, recoveryEpoch: UInt64,
        accessToken: String, refreshToken: String, expiresAt: Date,
        deviceCredential: String?, familyID: UUID?, isRecoveryOnly: Bool = false
    ) {
        self.accountID = accountID; self.deviceID = deviceID; self.recoveryEpoch = recoveryEpoch
        self.accessToken = accessToken; self.refreshToken = refreshToken; self.expiresAt = expiresAt
        self.deviceCredential = deviceCredential; self.familyID = familyID; self.isRecoveryOnly = isRecoveryOnly
    }
}

/// The P1 identity endpoints.
public protocol IdentityClient: Sendable {
    func serverInfo(_ origin: ServerOrigin) async throws -> ServerInfo
    func challenge(_ endpoint: ConfirmedEndpoint) async throws -> String
    func exchangeApple(_ request: AppleExchangeRequest, at endpoint: ConfirmedEndpoint) async throws -> SessionBundle
}
