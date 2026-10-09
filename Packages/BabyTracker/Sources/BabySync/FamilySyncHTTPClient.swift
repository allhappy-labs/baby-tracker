import BabyProtocol
import Foundation

public struct CredentialBoundAuthFailure: Error, Sendable {
    public let underlying: HTTPClientError
    public let credentialVersion: UUID
    /// The refresh endpoint has already classified this refusal. Ordinary
    /// sync codes retain their own policy (notably epoch_changed snapshots).
    public let requiresFreshProof: Bool
    public init(underlying: HTTPClientError, credentialVersion: UUID, requiresFreshProof: Bool = false) {
        self.underlying = underlying; self.credentialVersion = credentialVersion
        self.requiresFreshProof = requiresFreshProof
    }
}

/// The P2 sync endpoints over HTTPS.
///
/// It translates the server's stable error envelope into `SyncErrorCode` and
/// throws that, so the coordinator's action table is the only place a code
/// turns into a decision.
public final class FamilySyncHTTPClient: FamilySyncTransport, @unchecked Sendable {
    private let endpoint: ConfirmedEndpoint
    private let tokens: any SessionTokenProviding
    private let session: URLSession
    private let retryCredentials: (@Sendable (UUID) async throws -> SessionCredentials?)?

    public init(endpoint: ConfirmedEndpoint, tokens: any SessionTokenProviding, session: URLSession = .shared, retryCredentials: (@Sendable (UUID) async throws -> SessionCredentials?)? = nil) {
        self.endpoint = endpoint
        self.tokens = tokens
        self.session = session
        self.retryCredentials = retryCredentials
    }

    public func sync(_ request: SyncRequest) async throws -> SyncResponse {
        try await send(SyncResponse.self, "POST", url("sync"), body: request, capabilities: request.capabilities)
    }

    public func snapshot(pageToken: String?) async throws -> SnapshotResponse {
        var components = URLComponents(url: url("sync/snapshot"), resolvingAgainstBaseURL: false)!
        if let pageToken {
            guard Data(pageToken.utf8).count <= 512 else { throw SyncErrorCode.invalidPageToken }
            components.queryItems = [URLQueryItem(name: "page_token", value: pageToken)]
        }
        return try await send(
            SnapshotResponse.self, "GET", components.url!, body: nil as Never?,
            capabilities: ["records-v1", "snapshot-v1"]
        )
    }

    private func url(_ path: String) -> URL {
        URL(string: endpoint.origin.canonicalString + "/v1/" + path)!
    }

    private func send<T: Decodable>(
        _ type: T.Type, _ method: String, _ url: URL, body: (some Encodable)?, capabilities: [String]
    ) async throws -> T {
        var request = URLRequest(url: url)
        request.httpMethod = method
        var credentials = await tokens.currentSessionCredentials()
        let accessToken = if let credentials { credentials.accessToken } else { await tokens.currentAccessToken() }
        if let accessToken { request.setValue("Bearer \(accessToken)", forHTTPHeaderField: "Authorization") }
        // The sync route negotiates on HEADERS, not only on the body. Neither
        // of these was being sent, so every request this transport made was a
        // 400 before the server looked at the mutations — and no test noticed,
        // because until now nothing ran it against a server.
        request.setValue("v1", forHTTPHeaderField: "X-Baby-Tracker-Protocol")
        request.setValue(capabilities.joined(separator: ","), forHTTPHeaderField: "X-Baby-Tracker-Capabilities")
        if let body {
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try JSONEncoder().encode(body)
        }

        // Encode exactly once above. Retry changes only Authorization, so
        // command IDs and request bytes cannot be regenerated between sends.
        for attempt in 0...1 {
            let (data, response) = try await session.data(for: request)
            guard let http = response as? HTTPURLResponse else { throw SyncErrorCode.serviceUnavailable }
            if (200..<300).contains(http.statusCode) { return try JSONDecoder().decode(type, from: data) }
            let code = Self.code(from: data)
            if http.statusCode == 401, let used = credentials {
                if attempt == 0, let retryCredentials,
                   let verified = try await retryCredentials(used.credentialVersion) {
                    credentials = verified
                    request.setValue("Bearer \(verified.accessToken)", forHTTPHeaderField: "Authorization")
                    continue
                }
                throw CredentialBoundAuthFailure(underlying: .status(http.statusCode, code), credentialVersion: used.credentialVersion)
            }
            throw SyncErrorCode(rawValue: code ?? "") ?? .serviceUnavailable
        }
        throw SyncErrorCode.serviceUnavailable
    }

    static func code(from data: Data) -> String? {
        ServerErrorEnvelope.decode(data)
    }
}
