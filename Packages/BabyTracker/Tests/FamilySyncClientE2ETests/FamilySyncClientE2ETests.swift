import BabyProtocol
import BabyStore
import Foundation
import Testing
@testable import BabySync

/// The iOS client against the real server, over real TLS.
///
/// This is the gap every other test in this project leaves open. The protocol
/// gate drives a Go client; the Swift suites drive fakes; the shared fixtures
/// pin the RECORD shapes across both languages but say nothing about the
/// family and auth request envelopes. Those envelopes were where the bugs
/// were: every family command this client sent was malformed — no
/// `protocol_version`, no `capabilities`, four with no body at all, ownership
/// transfer without the account it was transferring to — and none of it was
/// visible to a suite that never spoke to a server.
///
/// Run by `protocol-e2e.sh`, after its own assertions, against the same
/// container and the same image it has already proved was built from this
/// tree. Without that script these return immediately: the script owns the
/// server, so the script is what decides a missing one is a failure.
@Suite("The iOS client against a live server", .serialized)
struct FamilySyncClientE2ETests {
    private struct LiveClients {
        let family: HTTPFamilyClient
        let endpoint: ConfirmedEndpoint
        let session: URLSession
        let environment: E2EEnvironment
    }

    private func liveClients() throws -> LiveClients? {
        guard let environment = E2EEnvironment.current() else { return nil }
        let endpoint = ConfirmedEndpoint(
            origin: try ServerOrigin(validating: environment.baseURL),
            serverInstanceID: environment.serverInstanceID
        )
        let session = PinnedCertificateDelegate.session(pinning: environment.certificate)
        let tokens = StaticToken(value: environment.outsiderAccessToken, identity: environment.outsiderIdentity)
        return LiveClients(
            family: HTTPFamilyClient(endpoint: endpoint, tokens: tokens, session: session),
            endpoint: endpoint, session: session, environment: environment
        )
    }

    /// A family-scoped access token for the account that just created one.
    private func refreshed(environment: E2EEnvironment) async throws -> String {
        var request = URLRequest(url: URL(string: environment.baseURL + "/v1/auth/refresh")!)
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = Data("""
        {"protocol_version":"v1","capabilities":["identity-v1"],"refresh_token":"\(environment.outsiderRefreshToken)"}
        """.utf8)
        let session = PinnedCertificateDelegate.session(pinning: environment.certificate)
        let (data, _) = try await session.data(for: request)
        let object = try JSONSerialization.jsonObject(with: data) as? [String: Any]
        return try #require(object?["access_token"] as? String, "refresh returned \(String(decoding: data, as: UTF8.self))")
    }

    /// The whole caregiver lifecycle, in the order a family lives it.
    ///
    /// One test rather than six: each step needs the state the previous one
    /// left, and splitting them would mean either re-running the whole
    /// sequence per assertion or sharing mutable state between tests that the
    /// runner is free to reorder.
    @Test func aFamilyIsCreatedSharedAndNarrowedThroughTheRealAPI() async throws {
        guard let clients = try liveClients() else { return }
        let environment = clients.environment

        // Creating a family is the first command an owner ever sends, and it
        // carries a display name and nothing else.
        let membership = try await clients.family.createFamily(name: "E2E", displayName: "E2E Family")
        #expect(membership.familyID != membership.accountID)

        let profile = try await clients.family.profile()
        #expect(profile.role == "owner", "the creator is not the owner: \(profile.role)")

        // An ordinary invitation and a view-only one are different commands
        // now, and the server has to accept both.
        let caregiverLink = try await clients.family.createInvitation(role: .caregiver)
        let viewerLink = try await clients.family.createInvitation(role: .viewer)
        #expect(caregiverLink.id != viewerLink.id)
        #expect(!viewerLink.shareURL.absoluteString.isEmpty)

        // Revoking the one we are not going to use proves the revoke envelope
        // as well as leaving the family tidy.
        try await clients.family.revokeInvitation(viewerLink.id)

        // The members list, which is what Settings renders. Before the
        // refresh below, not after: refreshing rotates the session and
        // revokes the lineage behind it, so the family client's token is dead
        // from that point on. That is the server behaving correctly and it is
        // why these steps are one test in a fixed order.
        let members = try await clients.family.members()
        #expect(members.contains { $0.familyRole == .owner }, "members=\(members)")

        // Syncing needs a FAMILY-scoped session, and the one this test started
        // with was minted before the account had a family. Refreshing is what
        // the app does at exactly this point — `promoteAfterMembership` then
        // `SessionRefresher` — and without it the next call is a 401. Done
        // with a plain request rather than through a client, because the
        // Keychain plumbing that owns refresh is not what is under test here.
        let scoped = try await refreshed(environment: environment)
        let sync = FamilySyncHTTPClient(
            endpoint: clients.endpoint, tokens: StaticToken(value: scoped, identity: environment.outsiderIdentity),
            session: clients.session
        )

        // A record, through the sync route — built from the SAME fixture the
        // Go protocol tests send, so the bytes both sides agree on are the
        // bytes this client actually puts on the wire.
        let mutation = try babyMutation()
        let applied = try await sync.sync(SyncRequest(
            protocolVersion: "v1", capabilities: ["records-v1"],
            cursor: nil, mutations: [mutation], pageToken: nil
        ))
        #expect(applied.outcomes.count == 1, "outcomes=\(applied.outcomes)")
        #expect(applied.outcomes.first?.status == .applied, "outcome=\(String(describing: applied.outcomes.first))")

        // And read back, which exercises the response decoding this client has
        // never had checked against a real body.
        let pulled = try await sync.sync(SyncRequest(
            protocolVersion: "v1", capabilities: ["records-v1"],
            cursor: nil, mutations: [], pageToken: nil
        ))
        #expect(pulled.changes.count >= 1, "nothing came back: \(pulled)")

        let snapshot = try await sync.snapshot(pageToken: nil)
        #expect(!snapshot.entities.isEmpty, "snapshot was empty")

    }
}

private struct StaticToken: SessionTokenProviding {
    let value: String
    let identity: SessionIdentityFields

    func currentAccessToken() async -> String? { value }

    /// The same three fields `KeychainTokenProvider` reads off its lookup key.
    /// Supplied here from the session the bootstrap minted, because a family
    /// creation cannot key a realm without them.
    func currentSessionIdentity() async -> SessionIdentity? {
        SessionIdentity(
            accountID: identity.accountID, deviceID: identity.deviceID,
            recoveryEpoch: identity.recoveryEpoch
        )
    }
}

/// `baby.json` from the shared corpus, with fresh identifiers.
///
/// Hand-writing a mutation here would test this file's idea of the payload
/// shape rather than the server's. The fixture is the one both languages
/// already agree on — `ProtocolFixtureConformanceTests` decodes it, and the
/// Go protocol suite sends it.
private func babyMutation() throws -> WireMutation {
    let root = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent().deletingLastPathComponent()
        .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
    let loader = ProtocolFixtureLoader(directory: root.appending(path: "Server/api/fixtures/v1"))
    let fixture = try loader.decode(WireMutation.self, named: "baby.json")
    // Only the mutation id is replaced, exactly as the Go protocol suite does
    // it. The entity id is inside the payload as well as beside it and the
    // server checks that they agree, so a fresh one here is a 400 — which is
    // how this test first failed.
    return WireMutation(
        mutationID: UUID(), entityID: fixture.entityID, entityKind: fixture.entityKind,
        operation: fixture.operation, payloadVersion: fixture.payloadVersion,
        observedEntityVersion: fixture.observedEntityVersion,
        clientMutationTime: fixture.clientMutationTime,
        importID: nil, payload: fixture.payload
    )
}
