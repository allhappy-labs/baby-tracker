import BabyProtocol
import BabyStore
import BabySync
import Foundation
import Testing
@testable import SettingsFeature

/// A server that advertises `push-token-v1` for the fixture's instance and
/// takes push tokens; everything it was sent is kept.
private final class PushServerProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var sent: [(method: String, path: String, authorization: String?)] = []

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        let path = request.url?.path ?? ""
        Self.sent.append((request.httpMethod ?? "", path, request.value(forHTTPHeaderField: "Authorization")))
        let (status, body): (Int, Data) = switch path {
        // `FamilySettingsFixture.instanceID` and `.origin`, spelled out: the
        // fixture is main-actor isolated and this answers off it.
        case "/v1/server":
            (200, Data("""
            {"instance_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d",
             "canonical_origin":"https://sync.example.test","recovery_epoch":0,
             "capabilities":["records-v1","push-token-v1"],"apple_authorization_mode":"hosted"}
            """.utf8))
        case "/v1/devices/push-token": (204, Data())
        default: (503, Data(#"{"error":{"code":"service_unavailable"}}"#.utf8))
        }
        let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: nil, headerFields: ["Content-Type": "application/json"])!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: body)
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}

@MainActor
@Suite("Push composition", .serialized)
struct PushCompositionTests {
    /// The assembled graph, not a stand-in: the model's lifecycle registers
    /// with APNs, uploads through a family-token transport once the server
    /// advertises push, and deletes on the way out; the wake handler asks the
    /// model for a background sync.
    @Test func theGraphRegistersUploadsDeletesAndWakesThroughTheModel() async throws {
        let fixture = try FamilySettingsFixture.make()
        try await fixture.model.signInWithApple()
        guard case .shared(let family) = fixture.model.mode else { Issue.record("fixture family required"); return }
        let familyAccess = try #require(try fixture.keychain.loadFamily(family)).accessToken
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [PushServerProtocol.self]
        let session = URLSession(configuration: config)
        defer { session.invalidateAndCancel() }
        PushServerProtocol.sent = []
        var registrations = 0

        let graph = FamilySyncComposition.assemble(
            store: fixture.store, keychainBackend: fixture.backend, identity: HTTPIdentityClient(configuration: config),
            session: session, defaults: fixture.defaults,
            registerForRemoteNotifications: { registrations += 1 },
            pushEnvironment: FixedPushEnvironment()
        )
        // No engine yet: nothing to sync, which is a failed wake.
        #expect(await graph.wake.receiveSilentPush() == .failed)

        let lifecycle = try #require(graph.model.pushLifecycle)
        await lifecycle.activated(family, FamilySettingsFixture.endpoint, .signedIn)
        #expect(registrations == 1)
        await graph.push.deviceTokenReceived(Data(repeating: 0x5a, count: 32))
        // An upload owed: the registrar forgets what it uploaded.
        #expect(fixture.defaults.string(forKey: "push.uploaded-fingerprint") != nil)
        lifecycle.invalidate()
        #expect(fixture.defaults.string(forKey: "push.uploaded-fingerprint") == nil)
        await lifecycle.ending()

        let pushRoutes = PushServerProtocol.sent.filter { $0.path == "/v1/devices/push-token" }
        #expect(pushRoutes.map(\.method) == ["PUT", "DELETE"])
        #expect(pushRoutes.allSatisfy { $0.authorization == "Bearer \(familyAccess)" })
        // The push capability is asked once per server identity per launch,
        // not again for the DELETE.
        #expect(PushServerProtocol.sent.filter { $0.path == "/v1/server" }.count == 1)
    }
}

private struct FixedPushEnvironment: PushEnvironmentResolving {
    func environment() -> PushEnvironment? { .sandbox }
}
