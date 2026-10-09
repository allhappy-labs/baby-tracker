import BabyProtocol
import BabyStore
import Foundation

/// The P2 import-resolver endpoints over HTTPS.
///
/// Mirrors `FamilySyncHTTPClient`: the token is fetched per request, the
/// protocol headers ride alongside the body's own envelope, and a non-2xx
/// response decodes the server's `{"error": {"code": …}}` envelope into `SyncErrorCode`
/// — falling back to `.serviceUnavailable` for a code this build has never
/// heard of, same as every other sync transport.
public final class HTTPImportResolverClient: ImportResolverClient, @unchecked Sendable {
    private let endpoint: ConfirmedEndpoint
    private let tokens: any SessionTokenProviding
    private let session: URLSession
    /// `resolveConflict` names the CONFLICT, not the import — the server's
    /// route needs the import id in the path (`/family/import/{import_id}/
    /// resolve-conflict`) and `ImportResolverClient` has no parameter for it.
    /// Every real caller reaches `resolveConflict` only after `resolve` or
    /// `conflictPage` has already named the import on this same instance, so
    /// the id last seen from either is remembered here rather than invented.
    private let lastImportID = ImportIDBox()

    /// The exact pair `ImportCoordinator.capabilities` advertises, duplicated
    /// rather than referenced: `ImportCoordinator` is `@MainActor`, so its
    /// static members are actor-isolated, and this transport is not.
    static let capabilities = ["records-v1", "import-resolver-v1"]

    public init(endpoint: ConfirmedEndpoint, tokens: any SessionTokenProviding, session: URLSession = .shared) {
        self.endpoint = endpoint
        self.tokens = tokens
        self.session = session
    }

    public func resolve(_ request: ImportResolveRequest) async throws -> ImportResolveResponse {
        lastImportID.set(request.importID)
        return try await send(
            "POST", url("family/import/resolve"), body: request, capabilities: request.capabilities
        )
    }

    public func resolveConflict(
        conflictID: UUID,
        resolverRevision: Int64,
        resolution: ImportConflictResolution,
        commandID: UUID
    ) async throws -> ImportResolveResponse {
        guard let importID = lastImportID.get() else { throw SyncErrorCode.invalidRequest }
        let body = ImportConflictRequestBody(
            protocolVersion: "v1",
            capabilities: Self.capabilities,
            commandID: commandID,
            resolverRevision: resolverRevision,
            conflictID: conflictID,
            choice: resolution.choice.rawValue,
            canonicalBabyID: resolution.canonicalBabyID
        )
        return try await send(
            "POST",
            url("family/import/\(importID.uuidString.lowercased())/resolve-conflict"),
            body: body,
            capabilities: Self.capabilities
        )
    }

    public func conflictPage(importID: UUID, pageToken: String?) async throws -> ImportResolveResponse {
        lastImportID.set(importID)
        var components = URLComponents(
            url: url("family/import/\(importID.uuidString.lowercased())"), resolvingAgainstBaseURL: false
        )!
        if let pageToken {
            guard Data(pageToken.utf8).count <= 512 else { throw SyncErrorCode.invalidPageToken }
            components.queryItems = [URLQueryItem(name: "page_token", value: pageToken)]
        }
        return try await send(
            "GET", components.url!, body: nil as Never?, capabilities: Self.capabilities
        )
    }

    private func url(_ path: String) -> URL {
        URL(string: endpoint.origin.canonicalString + "/v1/" + path)!
    }

    private func send<T: Decodable>(
        _ method: String, _ url: URL, body: (some Encodable)?, capabilities: [String]
    ) async throws -> T {
        var request = URLRequest(url: url)
        request.httpMethod = method
        if let token = await tokens.currentAccessToken() {
            request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        }
        request.setValue("v1", forHTTPHeaderField: "X-Baby-Tracker-Protocol")
        request.setValue(capabilities.joined(separator: ","), forHTTPHeaderField: "X-Baby-Tracker-Capabilities")
        if let body {
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try Self.canonicalizedJSON(body)
        }

        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse else { throw SyncErrorCode.serviceUnavailable }
        guard (200..<300).contains(http.statusCode) else {
            throw SyncErrorCode(rawValue: Self.code(from: data) ?? "") ?? .serviceUnavailable
        }
        return try JSONDecoder().decode(T.self, from: data)
    }

    static func code(from data: Data) -> String? {
        ServerErrorEnvelope.decode(data)
    }

    /// `Foundation`'s `UUID` encodes upper case; the server's `ValidUUID`
    /// accepts only canonical lower case and rejects anything else outright —
    /// `import_id`, every `babies[].local_baby_id` and every
    /// `records[].record_id` / `records[].local_baby_id` in a resolve request
    /// would each fail that check. Rather than have `ImportResolveRequest`
    /// (shared with fixtures and other consumers in `BabyProtocol`) hand-roll
    /// lower-cased `String` fields, every request this client sends is
    /// normalized once after encoding: any string shaped like a UUID,
    /// anywhere in the JSON tree, is lower-cased before it leaves the device.
    static func canonicalizedJSON(_ value: some Encodable) throws -> Data {
        let data = try JSONEncoder().encode(value)
        let object = try JSONSerialization.jsonObject(with: data)
        let canonical = lowercasingUUIDs(in: object)
        return try JSONSerialization.data(withJSONObject: canonical)
    }

    private static let uuidPattern = try! NSRegularExpression(
        pattern: "^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$"
    )

    private static func lowercasingUUIDs(in value: Any) -> Any {
        if let string = value as? String {
            let range = NSRange(string.startIndex..<string.endIndex, in: string)
            return uuidPattern.firstMatch(in: string, range: range) != nil ? string.lowercased() : string
        }
        if let array = value as? [Any] { return array.map(lowercasingUUIDs(in:)) }
        if let dict = value as? [String: Any] { return dict.mapValues(lowercasingUUIDs(in:)) }
        return value
    }
}

/// `ImportConflictRequest`. Not shared with `BabyProtocol`'s `ImportDTOs`,
/// because `ImportConflictResolution` — the domain type it is built from — is
/// declared in `BabyStore`, which `BabyProtocol` does not depend on.
private struct ImportConflictRequestBody: Encodable {
    let protocolVersion: String
    let capabilities: [String]
    let commandID: UUID
    let resolverRevision: Int64
    let conflictID: UUID
    let choice: String
    let canonicalBabyID: UUID?

    enum CodingKeys: String, CodingKey {
        case protocolVersion = "protocol_version", capabilities
        case commandID = "command_id", resolverRevision = "resolver_revision"
        case conflictID = "conflict_id", choice, canonicalBabyID = "canonical_baby_id"
    }

    /// Hand-written rather than synthesized: the compiler's synthesized
    /// `Encodable` treats an `Optional` stored property specially and calls
    /// `encodeIfPresent`, which OMITS the key entirely when nil. The server's
    /// `requireImportFields` requires `canonical_baby_id` to be present — as
    /// `null` for `keep_local` is fine, but missing is a decode failure.
    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(protocolVersion, forKey: .protocolVersion)
        try container.encode(capabilities, forKey: .capabilities)
        try container.encode(commandID, forKey: .commandID)
        try container.encode(resolverRevision, forKey: .resolverRevision)
        try container.encode(conflictID, forKey: .conflictID)
        try container.encode(choice, forKey: .choice)
        try container.encode(canonicalBabyID, forKey: .canonicalBabyID)
    }
}

/// A single mutable slot, locked: `HTTPImportResolverClient` is declared
/// `@unchecked Sendable` like its sibling transports, and its methods can be
/// awaited concurrently even though real callers serialize them through the
/// `@MainActor` `ImportCoordinator`.
private final class ImportIDBox: @unchecked Sendable {
    private let lock = NSLock()
    private var value: UUID?

    func set(_ id: UUID) {
        lock.lock()
        value = id
        lock.unlock()
    }

    func get() -> UUID? {
        lock.lock()
        defer { lock.unlock() }
        return value
    }
}
