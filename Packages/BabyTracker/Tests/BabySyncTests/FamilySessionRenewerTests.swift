import BabyCore
import BabyStore
import Foundation
import Testing
@testable import BabySync

/// Stubs `POST /v1/auth/refresh` without a server, recording every request
/// this suite sends. `RecordingProtocol` (`HTTPRouteTests.swift`) keeps its
/// captures in shared static state, so this suite is `.serialized` too.
final class RefreshProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var recorded: [(method: String, path: String)] = []
    nonisolated(unsafe) static var bodies: [String: Data] = [:]
    nonisolated(unsafe) static var status = 200
    nonisolated(unsafe) static var body = Data("{}".utf8)

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        Self.recorded.append((request.httpMethod ?? "", request.url?.path ?? ""))
        if let path = request.url?.path, let sent = Self.sentBody(of: request) {
            Self.bodies[path] = sent
        }
        let response = HTTPURLResponse(
            url: request.url!, statusCode: Self.status, httpVersion: nil, headerFields: nil
        )!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        let data: Data
        if request.url?.path == "/v1/server" {
            data = Data("""
            {"instance_id":"\(SessionFixture.instanceID.uuidString.lowercased())","canonical_origin":"https://sync.example.test","recovery_epoch":0,"capabilities":["identity-v1"],"apple_authorization_mode":"self_hosted"}
            """.utf8)
        } else { data = Self.body }
        client?.urlProtocol(self, didLoad: data)
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

    static func reset() {
        recorded = []
        bodies = [:]
        status = 200
        body = Data("{}".utf8)
    }

    static func configuration() -> URLSessionConfiguration {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [RefreshProtocol.self]
        return configuration
    }
}

@MainActor
@Suite("Family session renewal over HTTPS", .serialized)
struct FamilySessionRenewerTests {
    private struct Harness {
        let store: BabyStore
        let keychain: KeychainSessionStore
        let leases: AppGroupFencedLease
        let identity: HTTPIdentityClient
    }

    private func harness() throws -> Harness {
        let directory = FileManager.default.temporaryDirectory
            .appending(path: "renew-\(UUID().uuidString)", directoryHint: .isDirectory)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let container = try BabySchema.container(
            storeURL: directory.appending(path: "store.sqlite"),
            bootstrapLockURL: directory.appending(path: "bootstrap.lock")
        )
        let lockURL = directory.appending(path: "coordination.lock")
        let store = BabyStore(
            container: container, dateProvider: TestDateProvider(.now),
            deviceID: "renew-test", coordinationLockURL: lockURL
        )
        return Harness(
            store: store,
            keychain: KeychainSessionStore(backend: InMemoryKeychainBackend()),
            leases: AppGroupFencedLease(container: container, lockURL: lockURL),
            identity: HTTPIdentityClient(configuration: RefreshProtocol.configuration())
        )
    }

    private func seed(_ harness: Harness) throws -> FamilySessionKey {
        let familyKey = SessionFixture.familyKey()
        try harness.store.markAccountAuthenticated(
            familyKey.leaseKey, credentialVersion: SessionFixture.credentials().credentialVersion
        )
        try harness.keychain.saveFamily(SessionFixture.credentials(), for: familyKey)
        return familyKey
    }

    private func renewer(_ harness: Harness) -> FamilySessionRenewer {
        FamilySessionRenewer(
            store: harness.store, keychain: harness.keychain, leases: harness.leases,
            identity: harness.identity, endpoint: SessionFixture.endpoint
        )
    }

    private func respondWithFreshSession() {
        RefreshProtocol.body = Data("""
        {"access_token":"new-access","refresh_token":"new-refresh",
         "expires_at":"2030-01-01T00:00:00Z","reauthenticate_at":"2030-01-02T00:00:00Z",
         "recovery_epoch":0,"device_id":"\(SessionFixture.deviceID.uuidString.lowercased())",
         "account":{"id":"\(SessionFixture.accountID.uuidString.lowercased())","display_name":"Alex","profile_required":false}}
        """.utf8)
    }

    @Test func renewalPostsRefreshAndSavesTheNewSession() async throws {
        RefreshProtocol.reset()
        let harness = try harness()
        let familyKey = try seed(harness)
        respondWithFreshSession()

        try await renewer(harness).renew(familyKey: familyKey)

        #expect(RefreshProtocol.recorded.contains { $0 == ("POST", "/v1/auth/refresh") })
        let sentData = try #require(RefreshProtocol.bodies["/v1/auth/refresh"])
        let sent = try #require(try JSONSerialization.jsonObject(with: sentData) as? [String: Any])
        #expect(sent["protocol_version"] as? String == "v1")
        #expect(sent["capabilities"] as? [String] == ["identity-v1"])
        #expect(sent["refresh_token"] as? String == "refresh-token")
        #expect(Set(sent.keys) == ["protocol_version", "capabilities", "refresh_token"])

        let saved = try harness.keychain.loadFamily(familyKey)
        #expect(saved?.accessToken == "new-access")
        #expect(saved?.refreshToken == "new-refresh")
        // The response carried no `device_credential`, so the device's own
        // continuity proof must survive the rotation untouched.
        #expect(saved?.deviceContinuity == SessionFixture.continuity)
    }

    /// Two concurrent renewals of the same family must send exactly one
    /// request: the durable in-flight marker refuses the second the moment it
    /// notices the first is still outstanding, before any network I/O.
    @Test func concurrentRenewDoesNotIssueASecondRequest() async throws {
        RefreshProtocol.reset()
        let harness = try harness()
        let familyKey = try seed(harness)
        respondWithFreshSession()
        let renewer = renewer(harness)

        async let first: Void = renewer.renew(familyKey: familyKey)
        async let second: Void = renewer.renew(familyKey: familyKey)
        _ = try? await first
        _ = try? await second

        #expect(RefreshProtocol.recorded.filter { $0 == ("POST", "/v1/auth/refresh") }.count == 1)
        #expect(try harness.keychain.loadFamily(familyKey)?.accessToken == "new-access")
    }
}
