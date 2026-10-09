import Foundation

/// Rotates a refresh token for a session that already exists.
///
/// Separated from `IdentityClient` so a renewer can be constructed against
/// exactly the one call it needs, in tests as well as in the app.
public protocol SessionRefreshing: Sendable {
    func refresh(refreshToken: String, at endpoint: ConfirmedEndpoint) async throws -> SessionCredentials
}

/// Carries the access token for authenticated calls.
/// Who the current session belongs to.
///
/// The server's family responses name the FAMILY and its members and nothing
/// about the device asking — because it already knows. The client needs all
/// four parts to key a realm, so the three the server does not repeat come
/// from the session it authenticated with.
public struct SessionIdentity: Sendable, Equatable {
    public let accountID: UUID
    public let deviceID: UUID
    public let recoveryEpoch: UInt64

    public init(accountID: UUID, deviceID: UUID, recoveryEpoch: UInt64) {
        self.accountID = accountID
        self.deviceID = deviceID
        self.recoveryEpoch = recoveryEpoch
    }
}

public protocol SessionTokenProviding: Sendable {
    func currentSessionCredentials() async -> SessionCredentials?
    func currentAccessToken() async -> String?
    func currentSessionIdentity() async -> SessionIdentity?
}

public extension SessionTokenProviding {
    func currentSessionCredentials() async -> SessionCredentials? { nil }
    /// Absent by default, so a provider that only carries a token — the sync
    /// transport's, the tests' — does not have to invent one. The family
    /// client refuses rather than guesses when it needs one and gets nil.
    func currentSessionIdentity() async -> SessionIdentity? { nil }
}

/// The P1 identity endpoints over HTTPS.
///
/// The session is configured to refuse redirects outright rather than follow
/// them: an origin that redirects is a different origin, and following one
/// would send a credential somewhere the user never confirmed.
public final class HTTPIdentityClient: NSObject, IdentityClient, SessionRecoveryTransport, URLSessionTaskDelegate, @unchecked Sendable {
    /// Decides a TLS server-trust challenge. Nil — the app's case — keeps the
    /// system's own evaluation.
    typealias ServerTrustEvaluator = @Sendable (URLAuthenticationChallenge) -> (URLSession.AuthChallengeDisposition, URLCredential?)

    /// The longest any identity request may take, start to finish. Well
    /// inside `SessionRefresher.leaseDuration`: URLSession's own default is a
    /// minute, twice the lease, so a slow but successful refresh could land
    /// after the lease lapsed, be refused by the fence, and leave the token it
    /// rotated looking reused the next time.
    static let requestTimeout: TimeInterval = 20

    private let configuration: URLSessionConfiguration
    var sessionConfiguration: URLSessionConfiguration { configuration }
    private let evaluateTrust: ServerTrustEvaluator?
    private lazy var session = makeRedirectRefusingSession()
    /// Guards the two fields below: `invalidate()` and the session's delegate
    /// callbacks run on different threads.
    private let lock = NSLock()
    private var invalidatedValue = false
    private var onSessionInvalidatedValue: (@Sendable () -> Void)?
    /// Whether `invalidate()` has run: no request may be made after it.
    var isInvalidated: Bool { lock.withLock { invalidatedValue } }
    /// Called once the session reports itself invalid. A seam for the suites
    /// that prove a short-lived graph closes this session. Set from the test's
    /// thread and read from URLSession's delegate queue, so it goes through
    /// the lock like the flag.
    var onSessionInvalidated: (@Sendable () -> Void)? {
        get { lock.withLock { onSessionInvalidatedValue } }
        set { lock.withLock { onSessionInvalidatedValue = newValue } }
    }

    /// The same delegate and configuration used for identity requests. An
    /// internal seam lets the transport tests exercise a real redirect before
    /// any confirmed HTTPS origin or credential is involved.
    func makeRedirectRefusingSession() -> URLSession {
        URLSession(configuration: configuration, delegate: self, delegateQueue: nil)
    }

    /// The app's client: the system's own TLS evaluation, always.
    public convenience init(configuration: URLSessionConfiguration = .ephemeral) {
        self.init(configuration: configuration, evaluateTrust: nil)
    }

    /// `evaluateTrust` exists for the live-server gate, whose server presents
    /// a self-signed certificate the gate pins. This client owns its session
    /// (it has to, to refuse redirects), so the trust decision is the one
    /// thing a caller cannot supply by passing a `URLSession` in.
    ///
    /// Internal, not public: an override of certificate trust is a test
    /// seam, and nothing the app ships should be able to reach for it.
    init(configuration: URLSessionConfiguration = .ephemeral, evaluateTrust: ServerTrustEvaluator?) {
        let configuration = configuration.copy() as! URLSessionConfiguration
        configuration.timeoutIntervalForRequest = Self.requestTimeout
        configuration.timeoutIntervalForResource = Self.requestTimeout
        self.configuration = configuration
        self.evaluateTrust = evaluateTrust
        super.init()
    }

    public func urlSession(
        _ session: URLSession,
        didReceive challenge: URLAuthenticationChallenge
    ) async -> (URLSession.AuthChallengeDisposition, URLCredential?) {
        evaluateTrust?(challenge) ?? (.performDefaultHandling, nil)
    }

    public func urlSession(_ session: URLSession, didBecomeInvalidWithError error: (any Error)?) {
        onSessionInvalidated?()
    }

    /// Closes this client's session, cancelling anything still in flight.
    /// For a client built for one bounded job — a Lock Screen action's
    /// renewer — once every request through it has been awaited: a session
    /// that is never invalidated keeps its delegate (this client) and its
    /// connections alive. Nothing may be sent through the client afterwards.
    public func invalidate() {
        let first = lock.withLock { () -> Bool in
            guard !invalidatedValue else { return false }
            invalidatedValue = true
            return true
        }
        guard first else { return }
        session.invalidateAndCancel()
    }

    public func urlSession(
        _ session: URLSession,
        task: URLSessionTask,
        willPerformHTTPRedirection response: HTTPURLResponse,
        newRequest request: URLRequest
    ) async -> URLRequest? {
        nil
    }

    public func serverInfo(_ origin: ServerOrigin) async throws -> ServerInfo {
        try await get(ServerInfo.self, from: origin.serverInfoURL)
    }

    public func challenge(_ endpoint: ConfirmedEndpoint) async throws -> String {
        struct Response: Decodable { let challenge: String }
        let url = endpoint.origin.serverInfoURL
            .deletingLastPathComponent()
            .appending(path: "auth/challenge")
        return try await post(Response.self, to: url, body: IdentityRequestMeta()).challenge
    }

    public func exchangeApple(_ request: AppleExchangeRequest, at endpoint: ConfirmedEndpoint) async throws -> SessionBundle {
        let url = endpoint.origin.serverInfoURL
            .deletingLastPathComponent()
            .appending(path: "auth/apple")
        return try await post(WireSessionBundle.self, to: url, body: request).value
    }

    /// Exchanges a refresh token for a new session, single-use per P3.1: the
    /// server refuses the parent token the moment it has issued this one.
    public func refresh(refreshToken: String, at endpoint: ConfirmedEndpoint) async throws -> SessionCredentials {
        let url = endpoint.origin.serverInfoURL
            .deletingLastPathComponent()
            .appending(path: "auth/refresh")
        let body = RefreshRequestBody(refreshToken: refreshToken)
        return try await post(WireRefreshResponse.self, to: url, body: body).value
    }

    public func refreshRecoverable(parent: SessionCredentials, expectedAccountID: UUID, attemptID: UUID, at endpoint: ConfirmedEndpoint) async throws -> RecoverableRefreshResult {
        let url = endpoint.origin.serverInfoURL.deletingLastPathComponent().appending(path: "auth/refresh-recoverable")
        let wire = try await post(WireRecoverableRefreshResponse.self, to: url, body: RecoverableRefreshBody(parent: parent, attemptID: attemptID))
        guard wire.attemptID == attemptID, wire.accountID == expectedAccountID,
              wire.instanceID == endpoint.serverInstanceID,
              wire.session.deviceID == parent.deviceContinuity.deviceID,
              wire.session.recoveryEpoch == parent.recoveryEpoch else {
            throw HTTPClientError.invalidRecoveryResponse
        }
        let rotated = wire.session.value
        return RecoverableRefreshResult(credentials: SessionCredentials(
            accessToken: rotated.accessToken, refreshToken: rotated.refreshToken,
            expiresAt: rotated.expiresAt, recoveryEpoch: rotated.recoveryEpoch,
            deviceContinuity: parent.deviceContinuity
        ), attemptID: wire.attemptID, accountID: wire.accountID, reauthenticateAt: wire.reauthenticateAt)
    }

    public func acknowledgeRefresh(attemptID: UUID, credentials: SessionCredentials, at endpoint: ConfirmedEndpoint) async throws {
        let url = endpoint.origin.serverInfoURL.deletingLastPathComponent().appending(path: "auth/refresh-ack")
        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.setValue("Bearer \(credentials.accessToken)", forHTTPHeaderField: "Authorization")
        request.httpBody = try JSONEncoder().encode(RefreshAckBody(credentials: credentials, attemptID: attemptID))
        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse else { throw HTTPClientError.notHTTP }
        guard http.statusCode == 204 else { throw HTTPClientError.status(http.statusCode, ServerErrorEnvelope.decode(data)) }
        guard data.isEmpty else { throw HTTPClientError.invalidRecoveryResponse }
    }

    // MARK: Plumbing

    /// `ChallengeRequest`: the metadata and nothing else. An empty object was
    /// refused as `unsupported_protocol`, so no sign-in ever got a challenge.
    struct IdentityRequestMeta: Encodable {
        let protocolVersion = "v1"
        let capabilities = ["identity-v1"]
        enum CodingKeys: String, CodingKey { case protocolVersion = "protocol_version", capabilities }
    }

    private func get<T: Decodable>(_ type: T.Type, from url: URL) async throws -> T {
        var request = URLRequest(url: url)
        request.httpMethod = "GET"
        return try await send(type, request)
    }

    private func post<T: Decodable>(_ type: T.Type, to url: URL, body: some Encodable) async throws -> T {
        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONEncoder().encode(body)
        return try await send(type, request)
    }

    private func send<T: Decodable>(_ type: T.Type, _ request: URLRequest) async throws -> T {
        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse else { throw HTTPClientError.notHTTP }
        guard (200..<300).contains(http.statusCode) else {
            throw HTTPClientError.status(http.statusCode, ServerErrorEnvelope.decode(data))
        }
        return try JSONDecoder().decode(type, from: data)
    }
}

public enum HTTPClientError: Error, Sendable, Equatable {
    case notHTTP
    case status(Int, String?)
    /// A command that produces a membership was sent by a provider that cannot
    /// say which device sent it. Refused rather than filled in with zeroes: a
    /// realm keyed on a wrong device is worse than a failed join.
    case sessionIdentityUnavailable
    case invalidRecoveryResponse
}

/// P1's stable error envelope, so a failure can say what the server said.
///
/// The code is NESTED: `{"error": {"code": …, "message": …}, …}`
/// (`protocol.ErrorResponse`, pinned by `Server/api/fixtures/v1/errors.json`).
/// Every transport used to read a top-level `code` the server never sends, so
/// every refusal decoded as nil and fell back to `.serviceUnavailable` — an
/// expired session was retried as an outage instead of being renewed, and a
/// revoked membership was never purged. One decoder, shared by every
/// transport, so they cannot disagree again.
enum ServerErrorEnvelope {
    static func decode(_ data: Data) -> String? {
        struct Envelope: Decodable {
            struct Detail: Decodable { let code: String? }
            let error: Detail?
        }
        return (try? JSONDecoder().decode(Envelope.self, from: data))?.error?.code
    }
}

/// `RefreshRequest` on the server: exactly `protocol_version`, `capabilities`
/// and `refresh_token` — no `command_id`. The server's `DisallowUnknownFields`
/// rejects anything else, and refresh is not itself a lifecycle command.
struct RefreshRequestBody: Encodable {
    let refreshToken: String

    enum CodingKeys: String, CodingKey {
        case protocolVersion = "protocol_version", capabilities, refreshToken = "refresh_token"
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode("v1", forKey: .protocolVersion)
        try container.encode(["identity-v1"], forKey: .capabilities)
        try container.encode(refreshToken, forKey: .refreshToken)
    }
}

/// `SessionResponse`'s refresh-relevant fields. `expires_at` arrives as an
/// RFC3339 string, which `Date`'s default `Decodable` conformance cannot read
/// (it expects a number), so it is decoded as a string and parsed explicitly.
struct WireRefreshResponse: Decodable {
    let accessToken: String
    let refreshToken: String
    let expiresAt: Date
    let recoveryEpoch: UInt64
    let deviceID: UUID
    let deviceCredential: String?

    enum CodingKeys: String, CodingKey {
        case accessToken = "access_token", refreshToken = "refresh_token", expiresAt = "expires_at"
        case recoveryEpoch = "recovery_epoch", deviceID = "device_id", deviceCredential = "device_credential"
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        accessToken = try container.decode(String.self, forKey: .accessToken)
        refreshToken = try container.decode(String.self, forKey: .refreshToken)
        recoveryEpoch = try container.decode(UInt64.self, forKey: .recoveryEpoch)
        deviceID = try container.decode(UUID.self, forKey: .deviceID)
        deviceCredential = try container.decodeIfPresent(String.self, forKey: .deviceCredential)
        let raw = try container.decode(String.self, forKey: .expiresAt)
        expiresAt = try RFC3339.date(from: raw, key: .expiresAt, in: container)
    }

    /// A best-effort device continuity: the server only sends
    /// `device_credential` on the exchange that rotates it, so most refreshes
    /// carry none. `FamilySessionRenewer` preserves the caller's own
    /// continuity whenever this one is empty.
    var value: SessionCredentials {
        SessionCredentials(
            accessToken: accessToken,
            refreshToken: refreshToken,
            expiresAt: expiresAt,
            recoveryEpoch: recoveryEpoch,
            deviceContinuity: DeviceContinuity(deviceID: deviceID, credential: deviceCredential ?? "")
        )
    }
}

/// Parses the server's RFC3339 timestamps, with or without fractional seconds.
///
/// A fresh formatter per call, not a cached static one: `ISO8601DateFormatter`
/// is not `Sendable`, and this runs off `HTTPIdentityClient`'s own actor.
enum RFC3339 {
    static func date<Key: CodingKey>(from raw: String, key: Key, in container: KeyedDecodingContainer<Key>) throws -> Date {
        let withFraction = ISO8601DateFormatter()
        withFraction.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let withoutFraction = ISO8601DateFormatter()
        withoutFraction.formatOptions = [.withInternetDateTime]
        if let date = withFraction.date(from: raw) ?? withoutFraction.date(from: raw) { return date }
        throw DecodingError.dataCorruptedError(forKey: key, in: container, debugDescription: "not an RFC3339 timestamp: \(raw)")
    }
}

/// The wire form of a session bundle, kept separate from the domain type so
/// the JSON names stay the server's.
///
/// `SessionResponse` nests the account under `account.id` and the family under
/// `family_slot.family_id`/`family_slot.state` — there is no top-level
/// `account_id`, `family_id` or `recovery_only`. Decoding those flat names, as
/// this used to, threw `keyNotFound` on every real server response before
/// `expires_at` was even reached; nothing caught it because no test ever fed
/// this decoder a body shaped like the server's actual `SessionResponse`.
struct WireSessionBundle: Decodable {
    let accountID: UUID
    let deviceID: UUID
    let recoveryEpoch: UInt64
    let accessToken: String
    let refreshToken: String
    let expiresAt: Date
    let deviceCredential: String?
    let familyID: UUID?
    let recoveryOnly: Bool

    private enum CodingKeys: String, CodingKey {
        case deviceID = "device_id", recoveryEpoch = "recovery_epoch"
        case accessToken = "access_token", refreshToken = "refresh_token", expiresAt = "expires_at"
        case deviceCredential = "device_credential", account, familySlot = "family_slot"
    }

    private struct WireSessionAccount: Decodable { let id: UUID }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        deviceID = try container.decode(UUID.self, forKey: .deviceID)
        recoveryEpoch = try container.decode(UInt64.self, forKey: .recoveryEpoch)
        accessToken = try container.decode(String.self, forKey: .accessToken)
        refreshToken = try container.decode(String.self, forKey: .refreshToken)
        deviceCredential = try container.decodeIfPresent(String.self, forKey: .deviceCredential)
        accountID = try container.decode(WireSessionAccount.self, forKey: .account).id
        let familySlot = try container.decodeIfPresent(WireFamilySlot.self, forKey: .familySlot)
        familyID = familySlot?.familyID
        recoveryOnly = familySlot?.state == "family_recovery"
        // `expires_at` arrives as an RFC3339 string, which `Date`'s default
        // `Decodable` conformance cannot read (it expects a number), so it is
        // decoded as a string and parsed explicitly.
        let rawExpiresAt = try container.decode(String.self, forKey: .expiresAt)
        expiresAt = try RFC3339.date(from: rawExpiresAt, key: .expiresAt, in: container)
    }

    var value: SessionBundle {
        SessionBundle(
            accountID: accountID, deviceID: deviceID, recoveryEpoch: recoveryEpoch,
            accessToken: accessToken, refreshToken: refreshToken, expiresAt: expiresAt,
            deviceCredential: deviceCredential, familyID: familyID, isRecoveryOnly: recoveryOnly
        )
    }
}
