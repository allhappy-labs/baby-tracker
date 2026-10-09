import BabyProtocol
import BabySync
import Foundation
import Testing
import CryptoKit

private struct ExportTestTokens: SessionTokenProviding {
    func currentAccessToken() async -> String? { "test-token" }
    func currentSessionIdentity() async -> SessionIdentity? { nil }
}

private final class ExportURLProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var response: @Sendable (URLRequest) -> (Int, [String: String], Data) = { _ in (500, [:], Data()) }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        let (status, headers, body) = Self.response(request)
        let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1", headerFields: headers)!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: body)
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}

@Test func exportRequestUsesCanonicalWireKeys() throws {
    let id = UUID(uuidString: "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE")!
    let json = try JSONSerialization.jsonObject(with: JSONEncoder().encode(CreateExportRequest(requestID: id))) as! [String: Any]
    #expect(Set(json.keys) == ["protocol_version", "capabilities", "request_id"])
    #expect(json["request_id"] as? String == "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
    #expect(json["capabilities"] as? [String] == ["export-v1"])
}

@Test func exportTemporaryFilesOnlyRemoveOwnDirectory() throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
    try FileManager.default.createDirectory(at: base, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: base) }
    let directory = base.appendingPathComponent("exports", isDirectory: true)
    let files = try ExportTemporaryFiles(directory: directory)
    let own = directory.appendingPathComponent("one.json")
    let unrelated = base.appendingPathComponent("keep.txt")
    try Data("private".utf8).write(to: own)
    try Data("keep".utf8).write(to: unrelated)
    try files.remove(unrelated)
    #expect(FileManager.default.fileExists(atPath: unrelated.path))
    try files.removeAll()
    #expect(try files.contents().isEmpty)
    #expect(FileManager.default.fileExists(atPath: unrelated.path))
}

@Suite(.serialized)
struct ExportHTTPTests {
    @Test func boundedEndpointRetryIsShownAsRateLimit() async throws {
        let base = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        defer { try? FileManager.default.removeItem(at: base) }
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [ExportURLProtocol.self]
        let client = try ExportClient(
            endpoint: ConfirmedEndpoint(origin: try ServerOrigin(validating: "https://sync.example.test"), serverInstanceID: UUID()),
            tokens: ExportTestTokens(), session: URLSession(configuration: config),
            files: try ExportTemporaryFiles(directory: base)
        )
        ExportURLProtocol.response = { _ in
            (429, ["Content-Type": "application/json", "Retry-After": "7"], Data("{\"error\":{\"code\":\"rate_limited\"}}".utf8))
        }
        await #expect(throws: ExportClientError.rateLimited(retryAfter: 7)) {
            try await client.request(requestID: UUID())
        }
    }

    @Test func verifiedDownloadAndHashMismatchLeavesNoFile() async throws {
        let id = UUID(uuidString: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")!
        let body = Data("{\"version\":\"v1\"}".utf8)
        let hash = SHA256.hash(data: body).map { String(format: "%02x", $0) }.joined()
        let status = Data("""
        {"protocol_version":"v1","export_id":"\(id.uuidString.lowercased())","state":"ready","created_at":"2026-09-27T10:00:00.000000Z","expires_at":"2026-09-28T10:00:00.000000Z","plaintext_bytes":\(body.count),"sha256":"\(hash)"}
        """.utf8)
        let base = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        defer { try? FileManager.default.removeItem(at: base) }
        let files = try ExportTemporaryFiles(directory: base)
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [ExportURLProtocol.self]
        let session = URLSession(configuration: config)
        let endpoint = ConfirmedEndpoint(
            origin: try ServerOrigin(validating: "https://sync.example.test"), serverInstanceID: UUID()
        )
        let client = try ExportClient(endpoint: endpoint, tokens: ExportTestTokens(), session: session, files: files)
        ExportURLProtocol.response = { request in
            guard request.value(forHTTPHeaderField: "X-Baby-Tracker-Protocol") == "v1",
                  request.value(forHTTPHeaderField: "X-Baby-Tracker-Capabilities") == "export-v1",
                  request.value(forHTTPHeaderField: "Authorization") == "Bearer test-token" else {
                return (400, ["Content-Type": "application/json"], Data("{\"error\":{\"code\":\"unsupported_capability\"}}".utf8))
            }
            if request.url!.path.hasSuffix("/download") {
                return (200, ["Content-Type": "application/json", "Content-Length": "\(body.count)"], body)
            }
            return (200, ["Content-Type": "application/json"], status)
        }
        #expect(try await client.request(requestID: id).exportID == id)
        let file = try await client.download(id: id)
        #expect(try Data(contentsOf: file) == body)
        try client.removeTemporaryFile(file)
        #expect(try files.contents().isEmpty)

        ExportURLProtocol.response = { request in
            if request.url!.path.hasSuffix("/download") {
                return (200, ["Content-Type": "application/json", "Content-Length": "\(body.count)"], Data(repeating: 0, count: body.count))
            }
            return (200, ["Content-Type": "application/json"], status)
        }
        await #expect(throws: ExportClientError.integrity) { try await client.download(id: id) }
        #expect(try files.contents().isEmpty)
    }
}
