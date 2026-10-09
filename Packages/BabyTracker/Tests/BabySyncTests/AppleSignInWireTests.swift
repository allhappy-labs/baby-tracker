import BabyStore
import BabyCore
import Foundation
import Testing
@testable import BabySync

/// Captures each identity request body the client sends, and answers with a
/// canned body per path.
final class IdentityWireProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var sent: [String: Data] = [:]
    nonisolated(unsafe) static var responses: [String: Data] = [:]

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        let path = request.url?.path ?? ""
        if let body = Self.body(of: request) { Self.sent[path] = body }
        let response = HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: nil, headerFields: nil)!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Self.responses[path] ?? Data("{}".utf8))
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}

    private static func body(of request: URLRequest) -> Data? {
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
        return data
    }

    static func configuration() -> URLSessionConfiguration {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [IdentityWireProtocol.self]
        return configuration
    }
}

private final class ProofClient: AppleSignInClient, @unchecked Sendable {
    func requestProof(nonce: String) async throws -> AppleProof {
        AppleProof(identityToken: "fixture.identity.token", authorizationCode: "apple-code", userIdentifier: "apple-user", fullName: "Apple Name")
    }
}

private final class AuthorizedState: AppleCredentialStateClient, @unchecked Sendable {
    func credentialState(forUserIdentifier userIdentifier: String) async throws -> AppleCredentialState { .authorized }
}

/// The Apple sign-in requests, byte for byte against what the server's strict
/// decoders accept. Pinned to the server's own fixtures
/// (`Server/api/fixtures/auth-*.json`, `server-info.json`), which the Go
/// suite decodes with the same `DecodeRequest` the routes use.
///
/// Every one of these was wrong: the challenge was `{}`, the exchange carried
/// a `nonce` the server refuses and none of `protocol_version`,
/// `capabilities`, `command_id`, `challenge` or `installation_id`, and the
/// server probe read `server_instance_id` where the server writes
/// `instance_id`. No phone could have signed in.
@MainActor
@Suite("Apple sign-in on the wire", .serialized)
struct AppleSignInWireTests {
    static let fixtures = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        .deletingLastPathComponent().deletingLastPathComponent()
        .appending(path: "Server/api/fixtures")

    static func fixture(_ name: String) throws -> [String: Any] {
        let data = try Data(contentsOf: fixtures.appending(path: name))
        return try #require(try JSONSerialization.jsonObject(with: data) as? [String: Any])
    }

    static func object(_ data: Data?) throws -> NSDictionary {
        let data = try #require(data)
        return try #require(try JSONSerialization.jsonObject(with: data) as? NSDictionary)
    }

    static let endpoint = try! ConfirmedEndpoint(
        origin: ServerOrigin(validating: "https://sync.example.test"),
        serverInstanceID: UUID(uuidString: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    )

    @Test func theChallengeRequestIsTheServersFixture() async throws {
        IdentityWireProtocol.sent = [:]
        IdentityWireProtocol.responses = ["/v1/auth/challenge": try JSONSerialization.data(
            withJSONObject: try #require(try Self.fixture("auth-challenge.json")["response"])
        )]
        let identity = HTTPIdentityClient(configuration: IdentityWireProtocol.configuration())

        let challenge = try await identity.challenge(Self.endpoint)

        #expect(challenge == "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8")
        let expected = try #require(try Self.fixture("auth-challenge.json")["request"] as? NSDictionary)
        #expect(try Self.object(IdentityWireProtocol.sent["/v1/auth/challenge"]) == expected)
    }

    /// Every field the server's `AppleExchangeRequest` has, under its name,
    /// and nothing else — the server's fixture, reproduced exactly.
    @Test func theExchangeRequestIsTheServersFixture() async throws {
        let fixture = try Self.fixture("auth-reauth-continuity.json")
        let expected = try #require(fixture["request"] as? NSDictionary)
        IdentityWireProtocol.sent = [:]
        IdentityWireProtocol.responses = ["/v1/auth/apple": try JSONSerialization.data(
            withJSONObject: try #require(fixture["response"])
        )]
        let identity = HTTPIdentityClient(configuration: IdentityWireProtocol.configuration())
        let request = AppleExchangeRequest(
            commandID: UUID(uuidString: "30000000-0000-4000-8000-000000000002")!,
            challenge: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
            identityToken: "fixture.identity.token",
            authorizationCode: "",
            installationID: "fixture-installation",
            displayName: nil,
            deviceContinuity: DeviceContinuity(
                deviceID: UUID(uuidString: "40000000-0000-4000-8000-000000000001")!,
                credential: "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
            )
        )

        let bundle = try await identity.exchangeApple(request, at: Self.endpoint)

        #expect(try Self.object(IdentityWireProtocol.sent["/v1/auth/apple"]) == expected)
        #expect(bundle.deviceID == UUID(uuidString: "40000000-0000-4000-8000-000000000001"))
    }

    /// `ProfileInput`: a display name and an explicit confirmation.
    @Test func aNamedExchangeCarriesTheConfirmedProfile() throws {
        let request = AppleExchangeRequest(
            challenge: "c", identityToken: "t", authorizationCode: "", installationID: "i",
            displayName: "Zoë", deviceContinuity: nil
        )
        let object = try Self.object(try JSONEncoder().encode(request))
        #expect(object["profile"] as? NSDictionary == ["display_name": "Zoë", "confirmed": true])
        #expect(object["device_continuity"] is NSNull)
        #expect(object["nonce"] == nil)
    }

    @Test(arguments: [("identity_token_only", ""), ("code_exchange_required", "apple-code")])
    func theAuthorizationCodeFollowsTheServersMode(mode: String, expected: String) {
        let proof = AppleProof(identityToken: "t", authorizationCode: "apple-code", userIdentifier: "u", fullName: nil)
        #expect(AppleExchangeRequest.authorizationCode(for: mode, proof: proof) == expected)
    }

    @Test func theServerProbeReadsTheServersInfo() throws {
        let data = try Data(contentsOf: Self.fixtures.appending(path: "server-info.json"))
        let info = try JSONDecoder().decode(ServerInfo.self, from: data)
        #expect(info.serverInstanceID == UUID(uuidString: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"))
        #expect(info.appleAuthorizationMode == "identity_token_only")
        #expect(info.canonicalOrigin == "https://sync.example.test")
    }

    /// The whole sign-in through `AccountCoordinator` and the real HTTP
    /// client: probe, challenge, exchange — with the self-hosted mode, the
    /// confirmed name and this install's persistent ID.
    @Test func signingInSendsTheModesCodeTheNameAndTheInstallation() async throws {
        let (coordinator, keychain, _) = try Self.coordinator(mode: "identity_token_only")

        try await coordinator.signInWithApple(displayName: "Alex")

        let sent = try Self.object(IdentityWireProtocol.sent["/v1/auth/apple"])
        #expect(sent["authorization_code"] as? String == "")
        #expect(sent["challenge"] as? String == "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8")
        #expect(sent["profile"] as? NSDictionary == ["display_name": "Alex", "confirmed": true])
        #expect(sent["installation_id"] as? String == (try keychain.installationID()))
        #expect(sent["protocol_version"] as? String == "v1")
        #expect(sent["capabilities"] as? [String] == ["identity-v1"])
        #expect((sent["command_id"] as? String).flatMap(UUID.init(uuidString:)) != nil)
        #expect(Set(sent.allKeys as! [String]) == Set(try #require(try Self.fixture("auth-reauth-continuity.json")["request"] as? [String: Any]).keys))
    }

    @Test func aHostedServerGetsApplesCode() async throws {
        let (coordinator, _, _) = try Self.coordinator(mode: "code_exchange_required")
        try await coordinator.signInWithApple(displayName: "Alex")
        #expect(try Self.object(IdentityWireProtocol.sent["/v1/auth/apple"])["authorization_code"] as? String == "apple-code")
    }

    /// A different server answering at the confirmed origin gets no Apple
    /// proof and no credential.
    @Test func aChangedInstanceIsRefusedBeforeAppleIsAsked() async throws {
        let (coordinator, _, _) = try Self.coordinator(mode: "identity_token_only", instance: UUID())
        await #expect(throws: EndpointIdentityError.self) { try await coordinator.signInWithApple(displayName: "Alex") }
        #expect(IdentityWireProtocol.sent["/v1/auth/apple"] == nil)
    }

    /// One install, one ID: created once, kept across sign-ins and launches.
    @Test func theInstallationIDIsCreatedOnceAndKept() throws {
        let backend = InMemoryKeychainBackend()
        let first = try KeychainSessionStore(backend: backend).installationID()
        #expect(try KeychainSessionStore(backend: backend).installationID() == first)
        #expect(UUID(uuidString: first) != nil)
        #expect(try KeychainSessionStore(backend: InMemoryKeychainBackend()).installationID() != first)
    }

    static func coordinator(mode: String, instance: UUID? = nil) throws
        -> (AccountCoordinator, KeychainSessionStore, UserDefaults) {
        IdentityWireProtocol.sent = [:]
        var info = try Self.fixture("server-info.json")
        info["apple_authorization_mode"] = mode
        if let instance { info["instance_id"] = instance.uuidString.lowercased() }
        let session = try Self.fixture("auth-session.json")
        IdentityWireProtocol.responses = [
            "/v1/server": try JSONSerialization.data(withJSONObject: info),
            "/v1/auth/challenge": try JSONSerialization.data(withJSONObject: try #require(try Self.fixture("auth-challenge.json")["response"])),
            "/v1/auth/apple": try JSONSerialization.data(withJSONObject: try #require(session["initial_response"])),
        ]
        let keychain = KeychainSessionStore(backend: InMemoryKeychainBackend())
        let defaults = try #require(UserDefaults(suiteName: "AppleSignInWireTests-\(UUID().uuidString)"))
        let coordinator = AccountCoordinator(
            store: try BabyStore.inMemory(dateProvider: TestDateProvider(.now)), keychain: keychain,
            identity: HTTPIdentityClient(configuration: IdentityWireProtocol.configuration()),
            apple: ProofClient(), credentialState: AuthorizedState(), defaults: defaults
        )
        coordinator.confirmEndpoint(Self.endpoint)
        return (coordinator, keychain, defaults)
    }
}
