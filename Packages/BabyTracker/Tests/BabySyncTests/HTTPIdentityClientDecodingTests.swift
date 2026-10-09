import Foundation
import Testing
@testable import BabySync

/// Stubs `POST /v1/auth/apple` without a server, recording every request this
/// suite sends. Kept separate from `RecordingProtocol`
/// (`HTTPRouteTests.swift`) so this suite's static captures never interleave
/// with another suite's, the way `HTTPRouteTests` warns its own does.
final class AppleExchangeProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var body = Data("{}".utf8)

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        let response = HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: nil, headerFields: nil)!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Self.body)
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}

    static func configuration() -> URLSessionConfiguration {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [AppleExchangeProtocol.self]
        return configuration
    }
}

/// Pins `HTTPIdentityClient.exchangeApple`'s decoding against a body shaped
/// exactly like the server's `SessionResponse`
/// (`Server/internal/protocol/auth.go`) — nested `account`/`family_slot`
/// objects and an RFC3339Nano `expires_at`, not the flattened, differently
/// named fields the client used to expect. Nothing had fed this decoder a
/// realistic body before: every existing test used `try?` against `"{}"`.
/// `.serialized`: `AppleExchangeProtocol.body` is shared static state, and
/// Swift Testing runs a suite's tests in parallel by default — without this,
/// one test's response body randomly wins for another's request.
@Suite("HTTPIdentityClient decodes the server's real SessionResponse", .serialized)
struct HTTPIdentityClientDecodingTests {
    private var endpoint: ConfirmedEndpoint {
        get throws {
            ConfirmedEndpoint(origin: try ServerOrigin(validating: "https://sync.example"), serverInstanceID: UUID())
        }
    }

    private func expectedDate(_ raw: String) -> Date {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return formatter.date(from: raw)!
    }

    @Test func decodesARealisticActiveFamilySessionResponse() async throws {
        let accountID = UUID()
        let deviceID = UUID()
        let familyID = UUID()
        // Go's `time.RFC3339Nano` keeps a genuine sub-second component rather
        // than trimming it to whole seconds, and this is exactly that shape.
        let expiresAtRaw = "2030-06-15T12:34:56.123456789Z"
        AppleExchangeProtocol.body = Data("""
        {"protocol_version":"v1","access_token":"access-abc","refresh_token":"refresh-abc",
         "expires_at":"\(expiresAtRaw)","reauthenticate_at":"2030-06-16T00:00:00Z",
         "recovery_epoch":3,"device_id":"\(deviceID.uuidString.lowercased())",
         "device_credential":"device-cred-xyz",
         "account":{"id":"\(accountID.uuidString.lowercased())","display_name":"Alex","profile_required":false},
         "family_slot":{"family_id":"\(familyID.uuidString.lowercased())","state":"active"}}
        """.utf8)
        let identity = HTTPIdentityClient(configuration: AppleExchangeProtocol.configuration())

        let bundle = try await identity.exchangeApple(
            AppleExchangeRequest(challenge: "c", identityToken: "t", authorizationCode: "", installationID: "i", displayName: nil, deviceContinuity: nil),
            at: try endpoint
        )

        #expect(bundle == SessionBundle(
            accountID: accountID, deviceID: deviceID, recoveryEpoch: 3,
            accessToken: "access-abc", refreshToken: "refresh-abc", expiresAt: expectedDate(expiresAtRaw),
            deviceCredential: "device-cred-xyz", familyID: familyID, isRecoveryOnly: false
        ))
    }

    /// `family_slot.state == "family_recovery"` is the server's only signal
    /// that this session is recovery-only; nothing else in the response says so.
    @Test func familyRecoveryStateMarksTheSessionRecoveryOnly() async throws {
        let accountID = UUID()
        let deviceID = UUID()
        let familyID = UUID()
        AppleExchangeProtocol.body = Data("""
        {"protocol_version":"v1","access_token":"a","refresh_token":"r",
         "expires_at":"2030-01-01T00:00:00Z","reauthenticate_at":"2030-01-02T00:00:00Z",
         "recovery_epoch":1,"device_id":"\(deviceID.uuidString.lowercased())",
         "account":{"id":"\(accountID.uuidString.lowercased())","display_name":"Alex","profile_required":false},
         "family_slot":{"family_id":"\(familyID.uuidString.lowercased())","state":"family_recovery"}}
        """.utf8)
        let identity = HTTPIdentityClient(configuration: AppleExchangeProtocol.configuration())

        let bundle = try await identity.exchangeApple(
            AppleExchangeRequest(challenge: "c", identityToken: "t", authorizationCode: "", installationID: "i", displayName: nil, deviceContinuity: nil),
            at: try endpoint
        )

        #expect(bundle.familyID == familyID)
        #expect(bundle.isRecoveryOnly == true)
    }

    /// No family yet: `family_slot` is absent entirely (the server omits it,
    /// it does not send a null), and there is no `device_credential` either.
    @Test func absentFamilySlotAndDeviceCredentialDecodeToNil() async throws {
        let accountID = UUID()
        let deviceID = UUID()
        AppleExchangeProtocol.body = Data("""
        {"protocol_version":"v1","access_token":"a","refresh_token":"r",
         "expires_at":"2030-01-01T00:00:00Z","reauthenticate_at":"2030-01-02T00:00:00Z",
         "recovery_epoch":0,"device_id":"\(deviceID.uuidString.lowercased())",
         "account":{"id":"\(accountID.uuidString.lowercased())","display_name":"Alex","profile_required":false}}
        """.utf8)
        let identity = HTTPIdentityClient(configuration: AppleExchangeProtocol.configuration())

        let bundle = try await identity.exchangeApple(
            AppleExchangeRequest(challenge: "c", identityToken: "t", authorizationCode: "", installationID: "i", displayName: nil, deviceContinuity: nil),
            at: try endpoint
        )

        #expect(bundle.familyID == nil)
        #expect(bundle.deviceCredential == nil)
        #expect(bundle.isRecoveryOnly == false)
    }
}
