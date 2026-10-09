import BabyCore
import BabyProtocol
import BabyStore
import Foundation
import Testing
@testable import BabySync

/// Captures the URL, method, headers and body of every request without a
/// server. A near-duplicate of `RecordingProtocol` in `HTTPRouteTests`: that
/// one keeps its captures in shared static state guarded by `.serialized`,
/// and this suite needs to run its own assertions on bodies keyed by path
/// without racing that suite's captures.
final class ImportRecordingProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var recorded: [(method: String, path: String, query: String?, headers: [String: String])] = []
    nonisolated(unsafe) static var bodies: [String: Data] = [:]
    nonisolated(unsafe) static var status = 200
    nonisolated(unsafe) static var body = Data("{}".utf8)

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        Self.recorded.append((
            request.httpMethod ?? "",
            request.url?.path ?? "",
            request.url?.query,
            request.allHTTPHeaderFields ?? [:]
        ))
        if let path = request.url?.path, let sent = Self.sentBody(of: request) {
            Self.bodies[path] = sent
        }
        let response = HTTPURLResponse(
            url: request.url!, statusCode: Self.status, httpVersion: nil, headerFields: nil
        )!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Self.body)
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}

    private static func sentBody(of request: URLRequest) -> Data? {
        if let body = request.httpBody { return body }
        guard let stream = request.httpBodyStream else { return nil }
        stream.open()
        defer { stream.close() }
        var data = Data()
        var buffer = [UInt8](repeating: 0, count: 4096)
        while stream.hasBytesAvailable {
            let read = stream.read(&buffer, maxLength: buffer.count)
            if read <= 0 { break }
            data.append(contentsOf: buffer[0..<read])
        }
        return data.isEmpty ? nil : data
    }

    static func session() -> URLSession {
        recorded = []
        bodies = [:]
        status = 200
        body = Data("{}".utf8)
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [ImportRecordingProtocol.self]
        return URLSession(configuration: configuration)
    }
}

private struct StubTokens: SessionTokenProviding {
    func currentAccessToken() async -> String? { "a-token" }
}

@MainActor
@Suite("HTTP import resolver client", .serialized)
struct HTTPImportResolverClientTests {
    private var endpoint: ConfirmedEndpoint {
        get throws {
            ConfirmedEndpoint(
                origin: try ServerOrigin(validating: "https://sync.example"),
                serverInstanceID: UUID()
            )
        }
    }

    private func client() throws -> (HTTPImportResolverClient, URLSession) {
        let session = ImportRecordingProtocol.session()
        return (HTTPImportResolverClient(endpoint: try endpoint, tokens: StubTokens(), session: session), session)
    }

    /// The exact response shape the resolver answers with — reused across
    /// every test that only needs *a* well-formed reply.
    private func responseJSON(importID: UUID) -> Data {
        Data("""
        {
          "import_id": "\(importID.uuidString.lowercased())",
          "resolver_revision": 1,
          "normalization_version": "nfc-trim-fold-nfc-v1",
          "mappings": [],
          "conflicts": [],
          "conflict_count": 0,
          "conflicts_complete": true,
          "next_conflict_page_token": null,
          "mapping_complete": true,
          "next_chunk_index": 1,
          "manifest_complete": true
        }
        """.utf8)
    }

    private func resolveRequest(importID: UUID) -> ImportResolveRequest {
        let baby = UUID()
        return ImportResolveRequest(
            protocolVersion: "v1", capabilities: HTTPImportResolverClient.capabilities, importID: importID,
            importMode: ImportMode.preserveLocalIDs.rawValue, normalizationVersion: "nfc-trim-fold-nfc-v1",
            babies: [ImportBaby(localBabyID: baby, name: "Alex", birthDate: "2024-01-01", sortOrder: 0)],
            records: [RecordManifest(recordID: UUID(), entityKind: "feed", localBabyID: baby, contentSHA256: String(repeating: "a", count: 64))],
            totalRecordCount: 1, chunkIndex: 0, finalize: true
        )
    }

    @Test func resolveRoutesMethodAndHeaders() async throws {
        let (client, _) = try client()
        let importID = UUID()
        ImportRecordingProtocol.body = responseJSON(importID: importID)

        _ = try await client.resolve(resolveRequest(importID: importID))

        let recorded = try #require(ImportRecordingProtocol.recorded.first)
        #expect(recorded.method == "POST")
        #expect(recorded.path == "/v1/family/import/resolve")
        #expect(recorded.headers["X-Baby-Tracker-Protocol"] == "v1")
        #expect(recorded.headers["X-Baby-Tracker-Capabilities"] == "records-v1,import-resolver-v1")
        #expect(recorded.headers["Authorization"] == "Bearer a-token")
    }

    @Test func resolveDecodesTheResponse() async throws {
        let (client, _) = try client()
        let importID = UUID()
        ImportRecordingProtocol.body = responseJSON(importID: importID)

        let response = try await client.resolve(resolveRequest(importID: importID))
        #expect(response.importID == importID)
        #expect(response.resolverRevision == 1)
        #expect(response.mappingComplete)
    }

    /// `Foundation`'s `UUID` encodes upper case; the server's `ValidUUID`
    /// requires canonical lower case and refuses anything else. Every UUID in
    /// the request built by `ImportCoordinator.makeResolveRequest` — the
    /// import id and the ids nested inside `babies` and `records` — must
    /// leave the device already lower-cased.
    @Test func resolveRequestUUIDsAreLowercasedOnTheWire() async throws {
        let (client, _) = try client()
        let importID = UUID()
        ImportRecordingProtocol.body = responseJSON(importID: importID)
        let request = resolveRequest(importID: importID)

        _ = try await client.resolve(request)

        let raw = try #require(ImportRecordingProtocol.bodies["/v1/family/import/resolve"])
        let object = try #require(JSONSerialization.jsonObject(with: raw) as? [String: Any])
        #expect(object["import_id"] as? String == importID.uuidString.lowercased())
        let babies = try #require(object["babies"] as? [[String: Any]])
        #expect(babies.first?["local_baby_id"] as? String == request.babies.first?.localBabyID.uuidString.lowercased())
        let records = try #require(object["records"] as? [[String: Any]])
        #expect(records.first?["record_id"] as? String == request.records.first?.recordID.uuidString.lowercased())
        #expect(records.first?["local_baby_id"] as? String == request.records.first?.localBabyID.uuidString.lowercased())
    }

    /// A request produced by the real coordinator — not hand-built here —
    /// round-trips through this client's encoding.
    @Test func aCoordinatorBuiltRequestRoundTrips() async throws {
        let (client, _) = try client()
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let baby = try store.addBaby(name: "Alex", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        _ = try store.logBottle(volumeML: 100, contents: .formula, for: baby.id)
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .matchShared)
        _ = try store.freezeImportManifest(checkpointID: checkpoint)
        let coordinator = ImportCoordinator(store: store, client: client, checkpointID: checkpoint)
        let request = try coordinator.makeResolveRequest(chunkOrdinal: 0, mode: .matchShared)
        ImportRecordingProtocol.body = responseJSON(importID: checkpoint)

        let response = try await client.resolve(request)
        #expect(response.importID == checkpoint)

        let raw = try #require(ImportRecordingProtocol.bodies["/v1/family/import/resolve"])
        let object = try #require(JSONSerialization.jsonObject(with: raw) as? [String: Any])
        #expect(object["import_id"] as? String == checkpoint.uuidString.lowercased())
        #expect(object["import_mode"] as? String == "match_shared")
        #expect(object["finalize"] as? Bool == true)
    }

    @Test func resolveConflictRoutesMethodHeadersAndBody() async throws {
        let (client, _) = try client()
        let importID = UUID()
        ImportRecordingProtocol.body = responseJSON(importID: importID)
        _ = try await client.resolve(resolveRequest(importID: importID))

        let conflictID = UUID()
        let canonical = UUID()
        let commandID = UUID()
        ImportRecordingProtocol.body = responseJSON(importID: importID)
        _ = try await client.resolveConflict(
            conflictID: conflictID, resolverRevision: 3,
            resolution: ImportConflictResolution(choice: .useExisting, canonicalBabyID: canonical),
            commandID: commandID
        )

        let path = "/v1/family/import/\(importID.uuidString.lowercased())/resolve-conflict"
        let recorded = try #require(ImportRecordingProtocol.recorded.last)
        #expect(recorded.method == "POST")
        #expect(recorded.path == path)
        #expect(recorded.headers["X-Baby-Tracker-Capabilities"] == "records-v1,import-resolver-v1")

        let raw = try #require(ImportRecordingProtocol.bodies[path])
        let object = try #require(JSONSerialization.jsonObject(with: raw) as? [String: Any])
        #expect(object["protocol_version"] as? String == "v1")
        #expect(object["capabilities"] as? [String] == ["records-v1", "import-resolver-v1"])
        #expect(object["command_id"] as? String == commandID.uuidString.lowercased())
        #expect(object["resolver_revision"] as? Int == 3)
        #expect(object["conflict_id"] as? String == conflictID.uuidString.lowercased())
        #expect(object["choice"] as? String == "use_existing")
        #expect(object["canonical_baby_id"] as? String == canonical.uuidString.lowercased())
    }

    /// `keep_local` names no canonical baby. The key is still sent — the
    /// server requires its presence even when the value is `null` — but
    /// null, not omitted.
    @Test func keepLocalSendsANullCanonicalBabyIDKeyRatherThanOmittingIt() async throws {
        let (client, _) = try client()
        let importID = UUID()
        ImportRecordingProtocol.body = responseJSON(importID: importID)
        _ = try await client.resolve(resolveRequest(importID: importID))

        ImportRecordingProtocol.body = responseJSON(importID: importID)
        _ = try await client.resolveConflict(
            conflictID: UUID(), resolverRevision: 1,
            resolution: ImportConflictResolution(choice: .keepLocal, canonicalBabyID: nil),
            commandID: UUID()
        )

        let path = "/v1/family/import/\(importID.uuidString.lowercased())/resolve-conflict"
        let raw = try #require(ImportRecordingProtocol.bodies[path])
        let object = try #require(JSONSerialization.jsonObject(with: raw) as? [String: Any])
        #expect(object.keys.contains("canonical_baby_id"))
        #expect(object["canonical_baby_id"] is NSNull)
    }

    /// Without a prior `resolve` or `conflictPage` on this instance, there is
    /// no import id to put in the path — refused before the network rather
    /// than sent to a nonsense URL.
    @Test func resolveConflictWithNoKnownImportThrows() async throws {
        let (client, _) = try client()

        await #expect(throws: SyncErrorCode.invalidRequest) {
            _ = try await client.resolveConflict(
                conflictID: UUID(), resolverRevision: 1,
                resolution: ImportConflictResolution(choice: .keepLocal, canonicalBabyID: nil),
                commandID: UUID()
            )
        }
        #expect(ImportRecordingProtocol.recorded.isEmpty)
    }

    @Test func conflictPageRoutesMethodAndPageToken() async throws {
        let (client, _) = try client()
        let importID = UUID()
        ImportRecordingProtocol.body = responseJSON(importID: importID)

        _ = try await client.conflictPage(importID: importID, pageToken: "next-page")

        let recorded = try #require(ImportRecordingProtocol.recorded.first)
        #expect(recorded.method == "GET")
        #expect(recorded.path == "/v1/family/import/\(importID.uuidString.lowercased())")
        #expect(recorded.query == "page_token=next-page")
        #expect(recorded.headers["X-Baby-Tracker-Capabilities"] == "records-v1,import-resolver-v1")
    }

    @Test func conflictPageWithNoTokenSendsNoQuery() async throws {
        let (client, _) = try client()
        let importID = UUID()
        ImportRecordingProtocol.body = responseJSON(importID: importID)

        _ = try await client.conflictPage(importID: importID, pageToken: nil)

        let recorded = try #require(ImportRecordingProtocol.recorded.first)
        #expect(recorded.path == "/v1/family/import/\(importID.uuidString.lowercased())")
        #expect(recorded.query == nil)
    }

    /// An oversized page token is refused before it reaches the network, same
    /// as `FamilySyncHTTPClient.snapshot`.
    @Test func oversizedPageTokenNeverLeavesTheDevice() async throws {
        let (client, _) = try client()

        await #expect(throws: SyncErrorCode.invalidPageToken) {
            _ = try await client.conflictPage(importID: UUID(), pageToken: String(repeating: "a", count: 513))
        }
        #expect(ImportRecordingProtocol.recorded.isEmpty)
    }

    /// A conflict page discovered first also names the import for a later
    /// `resolveConflict` on the same instance.
    @Test func resolveConflictAfterConflictPageUsesThatImportID() async throws {
        let (client, _) = try client()
        let importID = UUID()
        ImportRecordingProtocol.body = responseJSON(importID: importID)
        _ = try await client.conflictPage(importID: importID, pageToken: nil)

        ImportRecordingProtocol.body = responseJSON(importID: importID)
        _ = try await client.resolveConflict(
            conflictID: UUID(), resolverRevision: 1,
            resolution: ImportConflictResolution(choice: .keepLocal, canonicalBabyID: nil),
            commandID: UUID()
        )

        let recorded = try #require(ImportRecordingProtocol.recorded.last)
        #expect(recorded.path == "/v1/family/import/\(importID.uuidString.lowercased())/resolve-conflict")
    }

    /// Non-2xx decodes the server's error envelope into `SyncErrorCode`.
    @Test func nonSuccessDecodesTheErrorEnvelope() async throws {
        let (client, _) = try client()
        ImportRecordingProtocol.status = 400
        ImportRecordingProtocol.body = Data(#"{"error":{"code":"invalid_request","message":"Invalid request","request_id":"","retryable":false}}"#.utf8)

        await #expect(throws: SyncErrorCode.invalidRequest) {
            _ = try await client.resolve(resolveRequest(importID: UUID()))
        }
    }

    /// A code this build has never heard of — an import-specific code with no
    /// case in `SyncErrorCode` — backs off rather than surfacing as if it
    /// were understood.
    @Test func anUnrecognisedCodeFallsBackToServiceUnavailable() async throws {
        let (client, _) = try client()
        ImportRecordingProtocol.status = 409
        ImportRecordingProtocol.body = Data(#"{"error":{"code":"import_target_already_assigned","message":"Import request could not be completed","request_id":"","retryable":false}}"#.utf8)

        await #expect(throws: SyncErrorCode.serviceUnavailable) {
            _ = try await client.resolve(resolveRequest(importID: UUID()))
        }
    }
}
