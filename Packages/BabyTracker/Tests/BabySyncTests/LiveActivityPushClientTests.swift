import BabyProtocol
import BabyStore
import Foundation
import Testing
@testable import BabySync

private final class PushURLProtocol: URLProtocol, @unchecked Sendable {
    struct Seen: Sendable {
        let method: String
        let url: URL
        let headers: [String: String]
        let body: Data
    }

    nonisolated(unsafe) static var respond: @Sendable (Seen, Int) -> (Int, Data) = { _, _ in (500, Data()) }
    nonisolated(unsafe) static var seen: [Seen] = []
    private static let lock = NSLock()

    static func reset(_ respond: @escaping @Sendable (Seen, Int) -> (Int, Data)) {
        lock.withLock {
            seen = []
            self.respond = respond
        }
    }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        var body = request.httpBody ?? Data()
        if body.isEmpty, let stream = request.httpBodyStream {
            stream.open()
            var buffer = [UInt8](repeating: 0, count: 4_096)
            while stream.hasBytesAvailable {
                let count = stream.read(&buffer, maxLength: buffer.count)
                if count <= 0 { break }
                body.append(buffer, count: count)
            }
            stream.close()
        }
        let entry = Seen(method: request.httpMethod ?? "", url: request.url!,
                         headers: request.allHTTPHeaderFields ?? [:], body: body)
        let (status, data) = Self.lock.withLock { () -> (Int, Data) in
            Self.seen.append(entry)
            return Self.respond(entry, Self.seen.count)
        }
        let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1",
                                       headerFields: ["Content-Type": "application/json", "Cache-Control": "no-store"])!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: data)
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}

private final class RotatingTokens: SessionTokenProviding, @unchecked Sendable {
    private let lock = NSLock()
    private var current: SessionCredentials?
    init(_ credentials: SessionCredentials?) { current = credentials }
    func replace(_ credentials: SessionCredentials?) { lock.withLock { current = credentials } }
    func currentSessionCredentials() async -> SessionCredentials? { lock.withLock { current } }
    func currentAccessToken() async -> String? { lock.withLock { current?.accessToken } }
}

private func credentials(_ access: String, version: UUID = UUID()) -> SessionCredentials {
    SessionCredentials(accessToken: access, refreshToken: "refresh-\(access)", expiresAt: .distantFuture, recoveryEpoch: PushFixture.epoch,
                       deviceContinuity: DeviceContinuity(deviceID: PushFixture.deviceID, credential: "continuity"), credentialVersion: version)
}

private func receiptJSON(version: Int64, tokenVersion: Int64? = nil, instance: UUID = PushFixture.builtIn.serverInstanceID, mapping: String = "") -> Data {
    let token = tokenVersion.map { ",\"accepted_token_version\":\($0)" } ?? ""
    return Data("""
    {"protocol_version":"v1","server_instance_id":"\(instance.uuidString.lowercased())","recovery_epoch":\(PushFixture.epoch),"accepted_registration_version":\(version)\(token),"generations":[\(mapping)]}
    """.utf8)
}

private func errorJSON(_ code: String) -> Data { Data("{\"error\":{\"code\":\"\(code)\",\"message\":\"x\"}}".utf8) }

@Suite(.serialized, .timeLimit(.minutes(1)))
struct LiveActivityPushClientTests {
    private func client(tokens: RotatingTokens, retry: (@Sendable (UUID) async throws -> SessionCredentials?)? = nil,
                        renew: (@Sendable () async throws -> Void)? = nil) -> LiveActivityPushClient {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [PushURLProtocol.self]
        return LiveActivityPushClient(endpoint: PushFixture.builtIn, tokens: tokens, session: URLSession(configuration: config),
                                      retryCredentials: retry, renew: renew)
    }

    private func deviceRegistration(version: Int64 = 1) throws -> LiveActivityDeviceRegistration {
        try LiveActivityDeviceRegistration(installationID: UUID(uuidString: "11111111-2222-4333-8444-555555555555")!, registrationVersion: version,
                                           apnsEnvironment: .sandbox, pushToStartToken: PushFixture.startToken, feedBreakMinutes: 45)
    }

    @Test func deviceRegistrationIsAnAuthenticatedCapabilityPUT() async throws {
        PushURLProtocol.reset { _, _ in (200, receiptJSON(version: 1)) }
        let receipt = try await client(tokens: RotatingTokens(credentials("access-1"))).registerDevice(try deviceRegistration())
        #expect(receipt.acceptedRegistrationVersion == 1)
        let request = try #require(PushURLProtocol.seen.first)
        #expect(request.method == "PUT")
        #expect(request.url.absoluteString == "https://baby-sync.example-tailnet.ts.net/v1/live-activities/device")
        #expect(request.headers["Authorization"] == "Bearer access-1")
        #expect(request.headers["X-Baby-Tracker-Protocol"] == "v1")
        #expect(request.headers["X-Baby-Tracker-Capabilities"] == "live-activity-push-v1")
        #expect(request.headers["Content-Type"] == "application/json")
        let json = try #require(try JSONSerialization.jsonObject(with: request.body) as? [String: Any])
        #expect(json["installation_id"] as? String == "11111111-2222-4333-8444-555555555555")
        #expect(json["push_to_start_token"] as? String == PushFixture.startToken)
        #expect(json["feed_break_minutes"] as? Int == 45)
        #expect(json["apns_environment"] as? String == "sandbox")
    }

    @Test func activityRegistrationReturnsTheCanonicalGeneration() async throws {
        let scope = PushFixture.scope()
        let mapping = """
        {"activity_id":"activity-1","scope":{"server_instance_id":"\(scope.serverInstanceID.uuidString.lowercased())","recovery_epoch":\(scope.recoveryEpoch),"family_id":"\(scope.familyID.uuidString.lowercased())","baby_id":"\(scope.babyID.uuidString.lowercased())","kind_raw":"feed"},"generation_id":"\(PushFixture.canonicalGenerationID.uuidString.lowercased())"}
        """
        PushURLProtocol.reset { _, _ in (200, receiptJSON(version: 1, tokenVersion: 2, mapping: mapping)) }
        let registration = try LiveActivityTokenRegistration(installationID: UUID(), activityID: "activity-1", token: PushFixture.activityToken,
                                                             tokenVersion: 2, scope: scope, knownGenerationID: PushFixture.generationID, state: .active)
        let receipt = try await client(tokens: RotatingTokens(credentials("access-1"))).registerActivity(registration)
        #expect(receipt.generations.first?.generationID == PushFixture.canonicalGenerationID)
        let request = try #require(PushURLProtocol.seen.first)
        #expect(request.method == "PUT")
        #expect(request.url.path == "/v1/live-activities/activity")
    }

    @Test func unregisterIsAVersionedDELETE() async throws {
        PushURLProtocol.reset { _, _ in (204, Data()) }
        let installation = UUID(uuidString: "11111111-2222-4333-8444-555555555555")!
        try await client(tokens: RotatingTokens(credentials("access-1"))).unregisterDevice(installationID: installation, version: 7)
        let request = try #require(PushURLProtocol.seen.first)
        #expect(request.method == "DELETE")
        #expect(request.url.absoluteString == "https://baby-sync.example-tailnet.ts.net/v1/live-activities/device/11111111-2222-4333-8444-555555555555?version=7")
        #expect(request.headers["X-Baby-Tracker-Capabilities"] == "live-activity-push-v1")
    }

    @Test func statusCodesMapToRetryOrFailClosed() async throws {
        // The status/code pairs the server actually writes
        // (httpapi/live_activity.go writeLiveActivityError, sync.go
        // writeSyncCode/writeSyncFamilyCode). Only the dedicated codes are
        // version or registration verdicts; a 409 or 403 with another code
        // is a refusal that says nothing about this registration.
        let cases: [(Int, String, LiveActivityPushClientError, Bool)] = [
            (409, "live_activity_version_conflict", .versionConflict, false),
            (403, "live_activity_registration_denied", .registrationDenied, false),
            (409, "unsupported_capability", .rejected("unsupported_capability"), false),
            (400, "unsupported_protocol", .rejected("unsupported_protocol"), false),
            (400, "invalid_request", .rejected("invalid_request"), false),
            (403, "membership_revoked", .rejected("membership_revoked"), false),
            (403, "read_only_caregiver", .rejected("read_only_caregiver"), false),
            (404, "family_unavailable", .rejected("family_unavailable"), false),
            (413, "body_too_large", .rejected("body_too_large"), false),
            (503, "service_unavailable", .unavailable, true),
            (429, "rate_limited", .rateLimited, true),
        ]
        for (status, code, expected, retryable) in cases {
            PushURLProtocol.reset { _, _ in (status, errorJSON(code)) }
            await #expect(throws: expected) {
                _ = try await client(tokens: RotatingTokens(credentials("access-1"))).registerDevice(try deviceRegistration())
            }
            #expect(expected.isRetryable == retryable)
        }
    }

    /// A DELETE refused because the capability is momentarily withdrawn is
    /// not a removal the server has done: it stays pending and is retried.
    @MainActor
    @Test func aRemovalRefusedForAWithdrawnCapabilityStaysPending() async throws {
        let session: URLSession = {
            let config = URLSessionConfiguration.ephemeral
            config.protocolClasses = [PushURLProtocol.self]
            return URLSession(configuration: config)
        }()
        let tokens = RotatingTokens(credentials("access-1"))
        PushURLProtocol.reset { seen, _ in
            if seen.method == "DELETE" { return (409, errorJSON("unsupported_capability")) }
            let version = (try? JSONSerialization.jsonObject(with: seen.body) as? [String: Any])?["registration_version"] as? Int ?? 1
            return (200, receiptJSON(version: Int64(version)))
        }
        let harness = PushHarness()
        let coordinator = LiveActivityPushCoordinator(
            events: harness.events, store: harness.store,
            makeClient: { context in LiveActivityPushClient(endpoint: context.endpoint, tokens: tokens, session: session) },
            sleep: { try await harness.sleeper.sleep($0) }
        )
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(PushURLProtocol.seen.count == 1)
        await coordinator.waitUntilIdle()

        harness.events.send(.activitiesEnabled(false))
        await coordinator.settle(harness.events)
        #expect(PushURLProtocol.seen.contains { $0.method == "DELETE" })
        await coordinator.waitUntilIdle()
        #expect(try harness.store.read().pendingRemovals.count == 1)

        PushURLProtocol.reset { seen, _ in seen.method == "DELETE" ? (204, Data()) : (200, receiptJSON(version: 1)) }
        await coordinator.reconcile()
        #expect(PushURLProtocol.seen.map(\.method) == ["DELETE"])
        #expect(try harness.store.read().pendingRemovals.isEmpty)
        harness.events.finish()
    }

    @Test func expiredAccessUsesTheFencedRenewerOnceThenRetries() async throws {
        let tokens = RotatingTokens(credentials("stale"))
        PushURLProtocol.reset { seen, _ in
            seen.headers["Authorization"] == "Bearer fresh" ? (200, receiptJSON(version: 1)) : (401, errorJSON("access_expired"))
        }
        let renewals = SleepRecorder()
        let client = client(tokens: tokens, retry: { _ in nil }, renew: {
            try await renewals.sleep(.zero)
            tokens.replace(credentials("fresh"))
        })
        _ = try await client.registerDevice(try deviceRegistration())
        #expect(renewals.durations.count == 1)
        #expect(PushURLProtocol.seen.map { $0.headers["Authorization"] } == ["Bearer stale", "Bearer fresh"])
    }

    @Test func aCredentialAnotherRenewalAlreadyInstalledIsUsedWithoutRefreshing() async throws {
        let usedVersion = UUID()
        let tokens = RotatingTokens(credentials("stale", version: usedVersion))
        PushURLProtocol.reset { seen, _ in
            seen.headers["Authorization"] == "Bearer verified" ? (200, receiptJSON(version: 1)) : (401, errorJSON("access_expired"))
        }
        let client = client(tokens: tokens, retry: { version in
            version == usedVersion ? credentials("verified") : nil
        }, renew: { Issue.record("renewed although a verified credential existed") })
        _ = try await client.registerDevice(try deviceRegistration())
        #expect(PushURLProtocol.seen.count == 2)
    }

    /// The sync engine's own renewal holds the account lease when the push
    /// client's 401 arrives: nothing is wrong with the session, the renewal
    /// just could not run yet. That is retried on the ordinary schedule, not
    /// failed closed with the registration lost until the next trigger.
    @Test func aRenewalFencedOutByAnotherProcessIsRetriedNotFailedClosed() async throws {
        PushURLProtocol.reset { _, _ in (401, errorJSON("access_expired")) }
        let client = client(tokens: RotatingTokens(credentials("stale")),
                            retry: { _ in throw SessionRefresher.RefreshError.lockUnavailable },
                            renew: { throw SessionRefresher.RefreshError.lockUnavailable })
        await #expect(throws: LiveActivityPushClientError.unavailable) {
            _ = try await client.registerDevice(try deviceRegistration())
        }
        #expect(PushURLProtocol.seen.count == 1)
        #expect(LiveActivityPushClientError.unavailable.isRetryable)
    }

    /// A renewal the server or the durable gate refused for good is not a
    /// transient: the client stops, and the parent is offered the sign-in.
    @Test func aRenewalRefusedOutrightStillStops() async throws {
        let refusals: [any Error] = [
            FencedLeaseError.cancelled,
            HTTPClientError.status(401, "session_revoked"),
            SessionRefresher.RefreshError.uncertainAttemptRequiresFreshProof,
            CredentialBoundAuthFailure(underlying: .status(401, "refresh_reused"), credentialVersion: UUID(), requiresFreshProof: true),
            // An irreconcilable checkpoint: terminal for the sync engine too.
            RefreshRecoveryError.invalidBinding,
        ]
        for refusal in refusals {
            PushURLProtocol.reset { _, _ in (401, errorJSON("access_expired")) }
            let client = client(tokens: RotatingTokens(credentials("stale")), retry: { _ in nil }, renew: { throw refusal })
            await #expect(throws: LiveActivityPushClientError.unauthorized, "\(refusal)") {
                _ = try await client.registerDevice(try deviceRegistration())
            }
            #expect(PushURLProtocol.seen.count == 1)
        }
    }

    /// The coordinator's side of the same event: a registration the client
    /// could not send because its renewal was fenced out is retried after
    /// two seconds, and the retry registers.
    @MainActor
    @Test func aRegistrationBlockedByABusyRenewalIsRetriedAfterTwoSeconds() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        client.onDevice = { registration, index in
            if index == 1 { throw LiveActivityPushClientError.unavailable }
            return try FakePushClient.accept(registration)
        }
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 2)
        #expect(harness.sleeper.durations == [.seconds(2)])
        #expect(try harness.store.read().device?.accepted == true)
        harness.events.finish()
    }

    @Test func persistentUnauthorizedStops() async throws {
        PushURLProtocol.reset { _, _ in (401, errorJSON("access_expired")) }
        await #expect(throws: LiveActivityPushClientError.unauthorized) {
            _ = try await client(tokens: RotatingTokens(credentials("stale")), retry: { _ in nil }, renew: {}).registerDevice(try deviceRegistration())
        }
        #expect(PushURLProtocol.seen.count == 2)
    }

    @Test func noSessionMeansNoRequest() async throws {
        PushURLProtocol.reset { _, _ in (200, receiptJSON(version: 1)) }
        await #expect(throws: LiveActivityPushClientError.unauthorized) {
            _ = try await client(tokens: RotatingTokens(nil)).registerDevice(try deviceRegistration())
        }
        #expect(PushURLProtocol.seen.isEmpty)
    }

    @Test func aReceiptFromAnotherInstanceIsRejected() async throws {
        PushURLProtocol.reset { _, _ in (200, receiptJSON(version: 1, instance: UUID())) }
        await #expect(throws: LiveActivityPushClientError.invalidReceipt) {
            _ = try await client(tokens: RotatingTokens(credentials("access-1"))).registerDevice(try deviceRegistration())
        }
    }
}
