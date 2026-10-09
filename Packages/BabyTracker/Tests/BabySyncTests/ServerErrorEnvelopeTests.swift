import Foundation
import Testing
@testable import BabySync

/// Every transport reads the server's refusal the way the server writes it.
///
/// The server's error body is `{"error": {"code": …}}`
/// (`protocol.ErrorResponse`). All three decoders read a top-level `code`
/// instead, so every refusal the real server sent became
/// `.serviceUnavailable`: the owner's first import after creating a family got
/// `access_expired`, was treated as an outage, and never renewed its session.
/// Found by the two-caregiver live-server gate. Pinned here to the server's own
/// fixtures rather than to a hand-written body, so the two cannot drift again.
@Suite("Server error envelope")
struct ServerErrorEnvelopeTests {
    static let fixtures: URL = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        .deletingLastPathComponent().deletingLastPathComponent()
        .appending(path: "Server/api/fixtures")

    /// Each named entry of a fixture file, re-encoded on its own as a body.
    static func bodies(_ file: String) throws -> [(code: String, body: Data)] {
        let data = try Data(contentsOf: fixtures.appending(path: file))
        let object = try #require(try JSONSerialization.jsonObject(with: data) as? [String: Any])
        return try object.keys.sorted().map { key in
            (key, try JSONSerialization.data(withJSONObject: try #require(object[key])))
        }
    }

    @Test(arguments: ["v1/errors.json", "auth-errors.json"])
    func everyDecoderReadsTheServersCode(file: String) throws {
        let bodies = try Self.bodies(file)
        #expect(!bodies.isEmpty)
        for (code, body) in bodies {
            #expect(ServerErrorEnvelope.decode(body) == code, "\(file): \(code)")
            #expect(FamilySyncHTTPClient.code(from: body) == code, "\(file): \(code)")
            #expect(HTTPImportResolverClient.code(from: body) == code, "\(file): \(code)")
        }
    }

    /// The case the live gate hit: an account-scoped session on a family
    /// route. It must reach the one action that renews, not the back-off.
    @Test func accessExpiredFromTheServerRenews() throws {
        let body = try #require(try Self.bodies("v1/errors.json").first { $0.code == "access_expired" }).body
        let code = try #require(SyncErrorCode(rawValue: FamilySyncHTTPClient.code(from: body) ?? ""))
        #expect(code.action == .refreshSession)
    }

    @Test func aStandaloneAuthErrorFixtureDecodes() throws {
        let body = try Data(contentsOf: Self.fixtures.appending(path: "auth-refresh-reused.json"))
        #expect(ServerErrorEnvelope.decode(body) == "refresh_reused")
    }
}
