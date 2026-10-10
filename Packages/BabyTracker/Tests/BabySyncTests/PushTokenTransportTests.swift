import BabyProtocol
import Foundation
import Testing
@testable import BabySync

/// Answers every request with a scripted status and body, and keeps what was
/// sent. Its own class, not `RecordingProtocol`: that one's statics are shared
/// with other suites that run in parallel.
final class PushRouteProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var sent: [(method: String, path: String, headers: [String: String], body: Data?)] = []
    nonisolated(unsafe) static var status = 204
    nonisolated(unsafe) static var body = Data()

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        Self.sent.append((request.httpMethod ?? "", request.url?.path ?? "", request.allHTTPHeaderFields ?? [:], Self.sentBody(of: request)))
        let response = HTTPURLResponse(url: request.url!, statusCode: Self.status, httpVersion: nil, headerFields: nil)!
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

    static func session(status: Int, body: Data = Data()) -> URLSession {
        sent = []
        self.status = status
        self.body = body
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [PushRouteProtocol.self]
        return URLSession(configuration: configuration)
    }
}

/// `PUT`/`DELETE /v1/devices/push-token` (Task 14's contract) through the
/// sync transport.
@Suite("Push token transport", .serialized)
struct PushTokenTransportTests {
    private func client(status: Int, body: Data = Data()) throws -> FamilySyncHTTPClient {
        FamilySyncHTTPClient(
            endpoint: ConfirmedEndpoint(origin: try ServerOrigin(validating: "https://sync.example"), serverInstanceID: UUID()),
            tokens: NoTokens(), session: PushRouteProtocol.session(status: status, body: body)
        )
    }

    /// Both routes answer `204` with no body; neither may try to decode one.
    @Test func putAndDeleteSendTheContractAndAcceptAnEmpty204() async throws {
        let client = try client(status: 204)
        let request = PushTokenRequest(token: Data(repeating: 0xab, count: 32), environment: .sandbox)

        try await client.putPushToken(request)
        try await client.deletePushToken()

        let sent = PushRouteProtocol.sent
        #expect(sent.map(\.method) == ["PUT", "DELETE"])
        #expect(sent.map(\.path) == ["/v1/devices/push-token", "/v1/devices/push-token"])
        for request in sent {
            #expect(request.headers["X-Baby-Tracker-Protocol"] == "v1")
            #expect(request.headers["X-Baby-Tracker-Capabilities"] == "push-token-v1")
            #expect(request.headers["Authorization"] == "Bearer token")
        }
        #expect(try sent.first?.body.map { try JSONDecoder().decode(PushTokenRequest.self, from: $0) } == request)
        #expect(sent.last?.body == nil)
    }

    /// A server without push answers `404`: an error, not a silent success.
    @Test func aServerWithoutPushIsAnError() async throws {
        let client = try client(status: 404, body: Data(#"{"code":"not_found"}"#.utf8))
        await #expect(throws: (any Error).self) {
            try await client.putPushToken(PushTokenRequest(token: Data(repeating: 1, count: 32), environment: .sandbox))
        }
    }
}
