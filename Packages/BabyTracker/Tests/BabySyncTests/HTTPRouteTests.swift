import BabyProtocol
import BabyStore
import Foundation
import Testing
@testable import BabySync

/// Captures the URL and method of every request without a server.
final class RecordingProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var recorded: [(method: String, path: String)] = []
    /// Bodies, keyed by path. Read through `httpBodyStream` as well as
    /// `httpBody`: URLSession converts a body to a stream before a URLProtocol
    /// sees it, so the obvious property is nil exactly when there is a body.
    nonisolated(unsafe) static var headers: [String: [String: String]] = [:]
    nonisolated(unsafe) static var bodies: [String: Data] = [:]
    nonisolated(unsafe) static var status = 200
    nonisolated(unsafe) static var body = Data("{}".utf8)

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        Self.recorded.append((request.httpMethod ?? "", request.url?.path ?? ""))
        Self.headers[request.url?.path ?? ""] = request.allHTTPHeaderFields ?? [:]
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
        headers = [:]
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [RecordingProtocol.self]
        return URLSession(configuration: configuration)
    }
}

struct NoTokens: SessionTokenProviding {
    func currentAccessToken() async -> String? { "token" }
}

/// Pins the client's URLs against the server's real router.
///
/// Every one of these was wrong when first written — the client had invented a
/// plausible REST shape (`/v1/families/transfer/{id}`, `DELETE /v1/account`)
/// that the server does not serve. Nothing caught it, because every other test
/// uses a fake client. A 404 against a live server is a poor way to learn that
/// a route name was a guess.
/// Serialized: `RecordingProtocol` keeps its captures in shared static state,
/// and Swift Testing runs tests in parallel by default — one suite's requests
/// would otherwise appear in another's assertions.
@Suite("HTTP routes match the server's router", .serialized)
struct HTTPRouteTests {
    private var endpoint: ConfirmedEndpoint {
        get throws {
            ConfirmedEndpoint(
                origin: try ServerOrigin(validating: "https://sync.example"),
                serverInstanceID: UUID()
            )
        }
    }

    private func client() throws -> (HTTPFamilyClient, URLSession) {
        let session = RecordingProtocol.session()
        return (HTTPFamilyClient(endpoint: try endpoint, tokens: NoTokens(), session: session), session)
    }

    /// What the server's `2026-09-27T12:00:00.123456Z` actually decodes to,
    /// computed the same way `RFC3339.date` does, so the assertion is a real
    /// pin on the wire format rather than a value copied from the code under test.
    private static func rfc3339(_ raw: String) -> Date {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return formatter.date(from: raw) ?? Date.distantPast
    }

    @Test func familyLifecycleRoutesMatch() async throws {
        let (client, _) = try client()
        RecordingProtocol.body = Data("""
        {"family_id":"\(UUID().uuidString.lowercased())","account_id":"\(UUID().uuidString.lowercased())",
         "device_id":"\(UUID().uuidString.lowercased())","recovery_epoch":0}
        """.utf8)

        _ = try? await client.createFamily(name: "Family", displayName: "Alex")
        _ = try? await client.confirmJoin(token: "abcdefghijklmnop0123456789", displayName: "Alex")
        try? await client.leaveFamily()
        try? await client.scheduleFamilyDeletion()
        try? await client.forfeitFamilyRecovery()
        try? await client.logout()

        let recorded = RecordingProtocol.recorded
        #expect(recorded.contains { $0 == ("POST", "/v1/family") })
        #expect(recorded.contains { $0 == ("POST", "/v1/family/invitations/redeem") })
        #expect(recorded.contains { $0 == ("POST", "/v1/family/leave") })
        #expect(recorded.contains { $0 == ("POST", "/v1/family/delete") })
        #expect(recorded.contains { $0 == ("POST", "/v1/family/recovery/forfeit") })
        #expect(recorded.contains { $0 == ("POST", "/v1/auth/logout") })
    }

    @Test func memberAndAccountRoutesMatch() async throws {
        let (client, _) = try client()
        let member = UUID()
        RecordingProtocol.body = Data("""
        {"account_id":"\(member.uuidString.lowercased())","display_name":"Alex","role":"caregiver"}
        """.utf8)

        _ = try? await client.profile()
        _ = try? await client.updateDisplayName("Taylor")
        _ = try? await client.members()
        try? await client.perform(.removeMember(member))
        try? await client.perform(.transferOwnership(member))
        try? await client.perform(.changeRole(member, .viewer))
        try? await client.perform(.deleteAccount)

        let recorded = RecordingProtocol.recorded
        let id = member.uuidString.lowercased()
        #expect(recorded.contains { $0 == ("GET", "/v1/account") })
        #expect(recorded.contains { $0 == ("PATCH", "/v1/account") })
        // `GET /v1/family`, not `/v1/family/actors`. Actors are display
        // labels and carry no role; the roles live on the family, and the
        // live-server gate is what showed that.
        #expect(recorded.contains { $0 == ("GET", "/v1/family") })
        #expect(recorded.contains { $0 == ("POST", "/v1/family/members/\(id)/remove") })
        #expect(recorded.contains { $0 == ("POST", "/v1/family/ownership-transfer") })
        #expect(recorded.contains { $0 == ("POST", "/v1/family/members/\(id)/role") })
        #expect(recorded.contains { $0 == ("POST", "/v1/account/delete") })
    }

    @Test func invitationRoutesMatch() async throws {
        let (client, _) = try client()
        let invitation = UUID()
        RecordingProtocol.body = Data("""
        {"id":"\(invitation.uuidString.lowercased())","link":"https://sync.example/invite#token=abcdefghijklmnop0123456789"}
        """.utf8)

        _ = try? await client.createInvitation(role: .viewer)
        try? await client.revokeInvitation(invitation)

        let recorded = RecordingProtocol.recorded
        #expect(recorded.contains { $0 == ("POST", "/v1/family/invitations") })
        #expect(recorded.contains { $0 == ("POST", "/v1/family/invitations/\(invitation.uuidString.lowercased())/revoke") })
    }

    /// The response the server actually sends once `Role` and `CreatedAt`
    /// joined `InvitationResponse`: the create call must decode both rather
    /// than silently drop them into a `FamilyInvitation` that never carried
    /// them.
    @Test func createInvitationDecodesRoleAndExpiry() async throws {
        let (client, _) = try client()
        let invitationID = UUID()
        RecordingProtocol.body = Data("""
        {"id":"\(invitationID.uuidString.lowercased())","link":"https://sync.example/invite#token=abcdefghijklmnop0123456789",
         "role":"viewer","expires_at":"2026-09-27T12:00:00.123456Z"}
        """.utf8)

        let invitation = try await client.createInvitation(role: .viewer)

        #expect(invitation.id == invitationID)
        #expect(invitation.role == .viewer)
        #expect(invitation.expiresAt == Self.rfc3339("2026-09-27T12:00:00.123456Z"))
    }

    /// `GET /v1/family/invitations`, decoded with the viewer's role and the
    /// fractional-second timestamps the server actually emits.
    @Test func pendingInvitationsRouteAndDecodingMatch() async throws {
        let (client, _) = try client()
        let invitationID = UUID()
        RecordingProtocol.body = Data("""
        {"invitations":[{"id":"\(invitationID.uuidString.lowercased())","family_id":"\(UUID().uuidString.lowercased())",
         "role":"viewer","status":"pending","expires_at":"2026-09-27T12:00:00.123456Z","created_at":"2026-09-26T09:30:00Z"}]}
        """.utf8)

        let invitations = try await client.pendingInvitations()

        #expect(RecordingProtocol.recorded.contains { $0 == ("GET", "/v1/family/invitations") })
        #expect(invitations.count == 1)
        #expect(invitations[0].id == invitationID)
        #expect(invitations[0].role == .viewer)
        #expect(invitations[0].expiresAt == Self.rfc3339("2026-09-27T12:00:00.123456Z"))
    }

    /// `POST /v1/family/invitations/preview` decodes the family's display
    /// name without redeeming anything.
    @Test func previewInvitationRouteAndDecodingMatch() async throws {
        let (client, _) = try client()
        RecordingProtocol.body = Data("""
        {"family_display_name":"Our family","expires_at":"2026-09-27T12:00:00Z"}
        """.utf8)

        let preview = try await client.previewInvitation(token: "abcdefghijklmnop0123456789")

        #expect(RecordingProtocol.recorded.contains { $0 == ("POST", "/v1/family/invitations/preview") })
        #expect(preview.familyName == "Our family")
    }

    @Test func syncRoutesMatch() async throws {
        let session = RecordingProtocol.session()
        RecordingProtocol.body = Data("{}".utf8)
        let transport = FamilySyncHTTPClient(endpoint: try endpoint, tokens: NoTokens(), session: session)

        for pageToken: String? in [nil, "continuation"] {
            _ = try? await transport.sync(SyncRequest(
                protocolVersion: "v1", capabilities: ["records-v1"], cursor: nil, mutations: [], pageToken: pageToken
            ))
            _ = try? await transport.snapshot(pageToken: pageToken)
        }

        let recorded = RecordingProtocol.recorded
        #expect(recorded.contains { $0 == ("POST", "/v1/sync") })
        #expect(recorded.contains { $0 == ("GET", "/v1/sync/snapshot") })
        // Only the protocol and the route's capabilities are negotiated. The
        // per-feature markers (supplements, growth, timed bottle, feeding
        // session) are gone: the server reads none of them.
        for path in ["/v1/sync", "/v1/sync/snapshot"] {
            let headers = try #require(RecordingProtocol.headers[path])
            #expect(headers["X-Baby-Tracker-Protocol"] == "v1")
            #expect(headers.keys.filter { $0.hasPrefix("X-Baby-Tracker-") }.sorted()
                == ["X-Baby-Tracker-Capabilities", "X-Baby-Tracker-Protocol"])
        }
        let body = try #require(RecordingProtocol.bodies["/v1/sync"])
        let fields = try #require(JSONSerialization.jsonObject(with: body) as? [String: Any])
        #expect(fields["capabilities"] as? [String] == ["records-v1"])
        #expect(fields["supports_supplements"] == nil)
    }


    @Test func identityRoutesMatch() async throws {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [RecordingProtocol.self]
        RecordingProtocol.recorded = []
        RecordingProtocol.body = Data("{}".utf8)
        let identity = HTTPIdentityClient(configuration: configuration)

        _ = try? await identity.serverInfo(try ServerOrigin(validating: "https://sync.example"))
        _ = try? await identity.challenge(try endpoint)
        _ = try? await identity.exchangeApple(
            AppleExchangeRequest(challenge: "c", identityToken: "t", authorizationCode: "", installationID: "i", displayName: nil, deviceContinuity: nil),
            at: try endpoint
        )

        let recorded = RecordingProtocol.recorded
        #expect(recorded.contains { $0 == ("GET", "/v1/server") })
        #expect(recorded.contains { $0 == ("POST", "/v1/auth/challenge") })
        #expect(recorded.contains { $0 == ("POST", "/v1/auth/apple") })
    }

    /// An oversized page token is refused before it reaches the network.
    @Test func oversizedPageTokenNeverLeavesTheDevice() async throws {
        let session = RecordingProtocol.session()
        let transport = FamilySyncHTTPClient(endpoint: try endpoint, tokens: NoTokens(), session: session)

        await #expect(throws: SyncErrorCode.invalidPageToken) {
            _ = try await transport.snapshot(pageToken: String(repeating: "a", count: 513))
        }
        #expect(RecordingProtocol.recorded.isEmpty)
    }

    /// Pins the BODY of every family command against the server's request types.
    ///
    /// The routes were corrected once and the envelopes inside them were not. The
    /// server decodes each request into a typed struct that embeds `RequestMeta`
    /// and refuses a body whose `protocol_version` is not "v1", whose
    /// `capabilities` are missing, or that carries a field the type does not
    /// declare — `DisallowUnknownFields`, so there is no partial acceptance. Every
    /// family command this client sent was therefore a certain 400 or 409, and
    /// nothing noticed, because the route tests above assert on URLs and every
    /// other test in the suite uses a fake client.
    ///
    /// Asserting on the decoded JSON rather than on a string: field order is not
    /// part of the contract and a substring check would pass on a body that nests
    /// the metadata instead of flattening it, which is the one shape the server
    /// cannot read.
    /// Nested inside `HTTPRouteTests` on purpose. `.serialized` orders a suite's
    /// own tests; it does not order two sibling suites against each other, and
    /// both of these read the same static capture buffer. As a nested suite it
    /// inherits the parent's serialization and the two stop interleaving.
    @Suite("Family command bodies match the server's request types")
    struct FamilyCommandBodyTests {
        private var endpoint: ConfirmedEndpoint {
            get throws {
                ConfirmedEndpoint(
                    origin: try ServerOrigin(validating: "https://sync.example"),
                    serverInstanceID: UUID()
                )
            }
        }

        private func client() throws -> HTTPFamilyClient {
            HTTPFamilyClient(endpoint: try endpoint, tokens: NoTokens(), session: RecordingProtocol.session())
        }

        private func body(at path: String) throws -> [String: Any] {
            let raw = try #require(RecordingProtocol.bodies[path], "no body was sent to \(path)")
            let object = try JSONSerialization.jsonObject(with: raw)
            return try #require(object as? [String: Any], "body at \(path) is not a JSON object")
        }

        /// The three fields the server reads before it reads anything else.
        private func expectEnvelope(_ body: [String: Any], capability: String = "family-lifecycle-v1") {
            #expect(body["protocol_version"] as? String == "v1")
            #expect(body["capabilities"] as? [String] == [capability])
            let commandID = body["command_id"] as? String ?? ""
            #expect(UUID(uuidString: commandID) != nil, "command_id=\(commandID)")
        }

        @Test func everyFamilyCommandCarriesTheEnvelope() async throws {
            let client = try client()
            let member = UUID()
            RecordingProtocol.body = Data("{}".utf8)

            try? await client.leaveFamily()
            try? await client.perform(.removeMember(member))
            try? await client.perform(.changeRole(member, .viewer))
            try? await client.perform(.transferOwnership(member))
            _ = try? await client.createInvitation(role: .caregiver)

            for path in [
                "/v1/family/leave",
                "/v1/family/members/\(member.uuidString.lowercased())/remove",
                "/v1/family/members/\(member.uuidString.lowercased())/role",
                "/v1/family/ownership-transfer",
                "/v1/family/invitations",
            ] {
                expectEnvelope(try body(at: path))
            }
        }

        @Test func theRoleChangeNamesTheRoleAndNothingElse() async throws {
            let client = try client()
            let member = UUID()
            RecordingProtocol.body = Data("{}".utf8)

            try? await client.perform(.changeRole(member, .viewer))
            let sent = try body(at: "/v1/family/members/\(member.uuidString.lowercased())/role")
            #expect(sent["role"] as? String == "viewer")
            // `MemberRoleRequest` declares exactly these. Anything else is refused
            // outright rather than ignored.
            #expect(Set(sent.keys) == ["protocol_version", "capabilities", "command_id", "role"])
        }

        /// The account being handed the family travels in the body. It was dropped
        /// entirely: the route carries no id in its path, so the server was being
        /// asked to transfer ownership to nobody at all.
        @Test func ownershipTransferNamesTheNewOwner() async throws {
            let client = try client()
            let member = UUID()
            RecordingProtocol.body = Data("{}".utf8)

            try? await client.perform(.transferOwnership(member))
            let sent = try body(at: "/v1/family/ownership-transfer")
            #expect(sent["new_owner_account_id"] as? String == member.uuidString.lowercased())
            #expect(Set(sent.keys) == ["protocol_version", "capabilities", "command_id", "new_owner_account_id"])
        }

        /// The role is decided when the link is written, so it has to be ON the
    /// link's request. It used to be a parameter that never left the device.
    @Test func theInvitationNamesTheRoleItConfers() async throws {
        let client = try client()
        RecordingProtocol.body = Data("""
        {"id":"\(UUID().uuidString.lowercased())","link":"https://sync.example/invite#token=abc"}
        """.utf8)

        _ = try? await client.createInvitation(role: .viewer)
        let sent = try body(at: "/v1/family/invitations")
        #expect(sent["role"] as? String == "viewer")
        #expect(Set(sent.keys) == ["protocol_version", "capabilities", "command_id", "role"])
    }

    /// Destroying something takes an explicit confirmation, and the server
        /// reads it from the body rather than inferring it from the route.
        @Test func destructiveCommandsConfirm() async throws {
            let client = try client()
            RecordingProtocol.body = Data("{}".utf8)

            try? await client.scheduleFamilyDeletion()
            try? await client.perform(.deleteAccount)

            for path in ["/v1/family/delete", "/v1/account/delete"] {
                let sent = try body(at: path)
                expectEnvelope(sent)
                #expect(sent["confirmed"] as? Bool == true, "\(path) did not confirm")
            }
        }

        /// Recovery and logout are validated against their OWN capability lists.
        /// Sending `family-lifecycle-v1` to them is a 409, not a courtesy.
        @Test func recoveryAndLogoutClaimTheirOwnCapabilities() async throws {
            let client = try client()
            RecordingProtocol.body = Data(#"{"actors":[]}"#.utf8)

            try? await client.forfeitFamilyRecovery()
            _ = try? await client.recoverFamily()
            try? await client.logout()

            expectEnvelope(try body(at: "/v1/family/recovery/forfeit"), capability: "recovery-v1")
            expectEnvelope(try body(at: "/v1/family/recover"), capability: "recovery-v1")
            expectEnvelope(try body(at: "/v1/auth/logout"), capability: "identity-v1")
        }

        /// `CreateFamilyRequest` has one display name and no `name`, and redeeming
        /// an invitation takes a token and a confirmation and no display name. Both
        /// bodies carried an extra field, and an extra field is fatal here.
        @Test func creationAndRedemptionSendOnlyDeclaredFields() async throws {
            let client = try client()
            RecordingProtocol.body = Data("""
            {"family_id":"\(UUID().uuidString.lowercased())","account_id":"\(UUID().uuidString.lowercased())",
             "device_id":"\(UUID().uuidString.lowercased())","recovery_epoch":0}
            """.utf8)

            _ = try? await client.createFamily(name: "Family", displayName: "Alex")
            _ = try? await client.confirmJoin(token: "abcdefghijklmnop0123456789", displayName: "Alex")
            _ = try? await client.updateDisplayName("Taylor")

            let created = try body(at: "/v1/family")
            #expect(created["display_name"] as? String == "Alex")
            #expect(Set(created.keys) == ["protocol_version", "capabilities", "command_id", "display_name"])

            let redeemed = try body(at: "/v1/family/invitations/redeem")
            #expect(redeemed["token"] as? String == "abcdefghijklmnop0123456789")
            #expect(redeemed["confirmed"] as? Bool == true)
            #expect(Set(redeemed.keys) == ["protocol_version", "capabilities", "command_id", "token", "confirmed"])

            let renamed = try body(at: "/v1/account")
            #expect(renamed["display_name"] as? String == "Taylor")
            #expect(Set(renamed.keys) == ["protocol_version", "capabilities", "command_id", "display_name", "confirmed"])
        }

        /// `PreviewInvitationRequest` is `RequestMeta` plus a token — no
        /// `command_id` — because previewing mutates nothing and has no
        /// outcome to make idempotent. Sending one would be an undeclared
        /// field the server refuses outright.
        @Test func previewSendsOnlyTheEnvelopeAndToken() async throws {
            let client = try client()
            RecordingProtocol.body = Data("{}".utf8)

            _ = try? await client.previewInvitation(token: "abcdefghijklmnop0123456789")
            let sent = try body(at: "/v1/family/invitations/preview")
            #expect(sent["token"] as? String == "abcdefghijklmnop0123456789")
            #expect(Set(sent.keys) == ["protocol_version", "capabilities", "token"])
        }
    }
}
