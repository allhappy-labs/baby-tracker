import Foundation
import Testing
@testable import BabySync

private final class RecoveryProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var response = Data()
    nonisolated(unsafe) static var status = 200
    nonisolated(unsafe) static var recorded: URLRequest?
    nonisolated(unsafe) static var recordedBody = Data()
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        Self.recorded = request
        if let body = request.httpBody { Self.recordedBody = body }
        else if let stream = request.httpBodyStream {
            stream.open()
            defer { stream.close() }
            var collected = Data()
            var buffer = [UInt8](repeating: 0, count: 4096)
            while stream.hasBytesAvailable {
                let size = stream.read(&buffer, maxLength: buffer.count)
                if size <= 0 { break }
                collected.append(contentsOf: buffer[..<size])
            }
            Self.recordedBody = collected
        }
        let response = HTTPURLResponse(url: request.url!, statusCode: Self.status, httpVersion: nil, headerFields: nil)!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        if !Self.response.isEmpty { client?.urlProtocol(self, didLoad: Self.response) }
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
    static func configuration() -> URLSessionConfiguration {
        let value = URLSessionConfiguration.ephemeral
        value.protocolClasses = [RecoveryProtocol.self]
        return value
    }
}

/// A real loopback HTTP redirect keeps this test independent of the app's
/// HTTPS origin validation while exercising URLSession's redirect delegate.
private final class LoopbackRedirectServer {
    let port: Int
    private let process: Process
    private let directory: URL
    private let auditURL: URL

    init() throws {
        directory = FileManager.default.temporaryDirectory.appending(path: "recovery-redirect-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        auditURL = directory.appending(path: "audit.jsonl")
        let source = """
        import http.server, json, sys
        audit = sys.argv[1]
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
                with open(audit, 'a') as output:
                    output.write(json.dumps({'path': self.path, 'host': self.headers.get('Host'), 'authorization': bool(self.headers.get('Authorization')), 'body': bool(body)}) + '\\n')
                if self.path == '/steal':
                    self.send_response(204)
                else:
                    self.send_response(307)
                    self.send_header('Location', 'http://localhost:%d/steal' % self.server.server_port)
                self.send_header('Content-Length', '0')
                self.end_headers()
            def log_message(self, *_): pass
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        print(server.server_port, flush=True)
        server.serve_forever()
        """
        process = Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/python3")
        process.arguments = ["-u", "-c", source, auditURL.path]
        let output = Pipe()
        process.standardOutput = output
        process.standardError = Pipe()
        try process.run()
        var line = Data()
        while let byte = try output.fileHandleForReading.read(upToCount: 1), !byte.isEmpty {
            if byte == Data([10]) { break }
            line.append(byte)
        }
        guard let parsed = Int(String(decoding: line, as: UTF8.self)) else {
            process.terminate()
            throw HTTPClientError.invalidRecoveryResponse
        }
        port = parsed
    }

    func records() throws -> [[String: Any]] {
        let data = try Data(contentsOf: auditURL)
        return try String(decoding: data, as: UTF8.self).split(separator: "\n").map {
            try JSONSerialization.jsonObject(with: Data($0.utf8)) as! [String: Any]
        }
    }

    func stop() {
        process.terminate()
        process.waitUntilExit()
        try? FileManager.default.removeItem(at: directory)
    }
}

@Suite("Refresh recovery transport", .serialized)
struct RefreshRecoveryTransportTests {
    let instance = UUID(uuidString: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")!
    let device = UUID(uuidString: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")!
    let account = UUID(uuidString: "cccccccc-cccc-4ccc-8ccc-cccccccccccc")!
    let attempt = UUID(uuidString: "dddddddd-dddd-4ddd-8ddd-dddddddddddd")!
    var endpoint: ConfirmedEndpoint {
        get throws { ConfirmedEndpoint(origin: try ServerOrigin(validating: "https://sync.example"), serverInstanceID: instance) }
    }
    var parent: SessionCredentials {
        SessionCredentials(accessToken: "access-parent", refreshToken: "refresh-parent", expiresAt: Date(timeIntervalSince1970: 2_000_000_000), recoveryEpoch: 7, deviceContinuity: DeviceContinuity(deviceID: device, credential: "continuity"))
    }
    var response: Data { Data("""
        {"protocol_version":"v1","access_token":"access-child","refresh_token":"refresh-child",
        "expires_at":"2030-06-15T12:34:56.123456789Z","reauthenticate_at":"2030-06-16T00:00:00Z",
        "recovery_epoch":7,"device_id":"\(device.uuidString.lowercased())","account":{"id":"\(account.uuidString.lowercased())","display_name":"Alex","profile_required":false},
        "server_instance_id":"\(instance.uuidString.lowercased())","refresh_attempt_id":"\(attempt.uuidString.lowercased())"}
        """.utf8) }

    @Test func recoveryWireHasExactFields() async throws {
        RecoveryProtocol.response = response
        RecoveryProtocol.status = 200
        let client = HTTPIdentityClient(configuration: RecoveryProtocol.configuration())
        _ = try await client.refreshRecoverable(parent: parent, expectedAccountID: account, attemptID: attempt, at: endpoint)
        let request = try #require(RecoveryProtocol.recorded)
        #expect(request.url?.path == "/v1/auth/refresh-recoverable")
        let body = RecoveryProtocol.recordedBody
        let object = try #require(JSONSerialization.jsonObject(with: body) as? [String: Any])
        #expect(Set(object.keys) == ["protocol_version", "capabilities", "refresh_token", "refresh_attempt_id", "device_continuity"])
        #expect(object["capabilities"] as? [String] == ["identity-v1", "refresh-recovery-v1"])
        #expect(object["refresh_attempt_id"] as? String == attempt.uuidString.lowercased())
        #expect((object["device_continuity"] as? [String: String]) == ["device_id": device.uuidString.lowercased(), "credential": "continuity"])
    }

    @Test func recoveryResponseRequiresMatchingAttemptAndIdentity() async throws {
        let client = HTTPIdentityClient(configuration: RecoveryProtocol.configuration())
        RecoveryProtocol.response = response
        RecoveryProtocol.status = 200
        let valid = try await client.refreshRecoverable(parent: parent, expectedAccountID: account, attemptID: attempt, at: endpoint)
        #expect(valid.accountID == account)
        #expect(valid.credentials.refreshToken == "refresh-child")
        for (old, changed) in [
            (attempt.uuidString.lowercased(), UUID().uuidString.lowercased()),
            (device.uuidString.lowercased(), UUID().uuidString.lowercased()),
            (instance.uuidString.lowercased(), UUID().uuidString.lowercased()),
            (account.uuidString.lowercased(), UUID().uuidString.lowercased()),
            ("\"recovery_epoch\":7", "\"recovery_epoch\":8"),
            ("\"expires_at\":\"2030-06-15T12:34:56.123456789Z\",", ""),
            ("2030-06-15T12:34:56.123456789Z", "not-a-date"),
            ("\"reauthenticate_at\":\"2030-06-16T00:00:00Z\",", ""),
            ("2030-06-16T00:00:00Z", "not-a-date")
        ] {
            RecoveryProtocol.response = Data(String(decoding: response, as: UTF8.self).replacingOccurrences(of: old, with: changed).utf8)
            await #expect(throws: (any Error).self) { _ = try await client.refreshRecoverable(parent: parent, expectedAccountID: account, attemptID: attempt, at: endpoint) }
        }
    }

    @Test func ackAcceptsEmpty204() async throws {
        RecoveryProtocol.status = 204
        RecoveryProtocol.response = Data()
        let client = HTTPIdentityClient(configuration: RecoveryProtocol.configuration())
        try await client.acknowledgeRefresh(attemptID: attempt, credentials: parent, at: endpoint)
        let request = try #require(RecoveryProtocol.recorded)
        #expect(request.url?.path == "/v1/auth/refresh-ack")
        #expect(request.value(forHTTPHeaderField: "Authorization") == "Bearer access-parent")
        let body = try #require(JSONSerialization.jsonObject(with: RecoveryProtocol.recordedBody) as? [String: Any])
        #expect(Set(body.keys) == ["protocol_version", "capabilities", "refresh_attempt_id", "device_continuity"])
    }

    @Test func recoveryRefusesRedirect() async throws {
        let server = try LoopbackRedirectServer()
        defer { server.stop() }
        let client = HTTPIdentityClient()
        let session = client.makeRedirectRefusingSession()
        defer { session.invalidateAndCancel() }
        func send(_ path: String, body: Data, bearer: String?) async throws {
            var request = URLRequest(url: URL(string: "http://127.0.0.1:\(server.port)\(path)")!)
            request.httpMethod = "POST"
            request.httpBody = body
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            if let bearer { request.setValue("Bearer \(bearer)", forHTTPHeaderField: "Authorization") }
            let (_, response) = try await session.data(for: request)
            #expect((response as? HTTPURLResponse)?.statusCode == 307)
        }
        try await send("/v1/auth/refresh-recoverable", body: JSONEncoder().encode(RecoverableRefreshBody(parent: parent, attemptID: attempt)), bearer: nil)
        try await send("/v1/auth/refresh-ack", body: JSONEncoder().encode(RefreshAckBody(credentials: parent, attemptID: attempt)), bearer: parent.accessToken)
        let records = try server.records()
        #expect(records.count == 2)
        #expect(records.map { $0["path"] as? String } == ["/v1/auth/refresh-recoverable", "/v1/auth/refresh-ack"])
        #expect(records.allSatisfy { $0["host"] as? String == "127.0.0.1:\(server.port)" })
        #expect(records.allSatisfy { $0["body"] as? Bool == true })
        #expect(records.map { $0["authorization"] as? Bool } == [false, true])
    }

    @Test func serverInfoAdvertisesRecoveryOnlyWhenPresent() throws {
        let base = ServerInfo(serverInstanceID: instance, canonicalOrigin: "https://sync.example", recoveryEpoch: 7, capabilities: ["identity-v1"], appleAuthorizationMode: "identity_token_only", maxItemsPerPage: 0, maxEncodedBytes: 0, sourceVersion: "", sourceURL: "")
        let enabled = ServerInfo(serverInstanceID: instance, canonicalOrigin: "https://sync.example", recoveryEpoch: 7, capabilities: ["identity-v1", "refresh-recovery-v1"], appleAuthorizationMode: "identity_token_only", maxItemsPerPage: 0, maxEncodedBytes: 0, sourceVersion: "", sourceURL: "")
        #expect(!base.advertisesRefreshRecovery)
        #expect(enabled.advertisesRefreshRecovery)
    }
}
