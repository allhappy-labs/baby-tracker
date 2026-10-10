import BabyProtocol
import Foundation
import Testing
@testable import BabySync

@MainActor
final class RecordingPushTransport: PushTokenTransport, @unchecked Sendable {
    private(set) var puts: [PushTokenRequest] = []
    private(set) var deletes = 0
    var failNext = false
    func putPushToken(_ request: PushTokenRequest) async throws {
        if failNext { failNext = false; throw SyncErrorCode.serviceUnavailable }
        puts.append(request)
    }
    func deletePushToken() async throws { deletes += 1 }
}

/// A PUT held open until `releasePut()`, and a DELETE that may never answer.
/// `events` says when each request *landed*: a PUT once it returns, a DELETE
/// once it is sent.
@MainActor
final class GatedPushTransport: PushTokenTransport, @unchecked Sendable {
    private(set) var events: [String] = []
    private(set) var putStarted = false
    private(set) var deleteStarted = false
    private(set) var deleteFinished = false
    var holdsPut = false
    var deleteNeverReturns = false
    private var heldPut: CheckedContinuation<Void, Never>?

    func putPushToken(_ request: PushTokenRequest) async throws {
        putStarted = true
        if holdsPut { await withCheckedContinuation { heldPut = $0 } }
        events.append("PUT")
    }

    func deletePushToken() async throws {
        deleteStarted = true
        events.append("DELETE")
        // Long enough that only the registrar's deadline can end the wait;
        // cancelled by it, so it never reports finishing.
        if deleteNeverReturns { try await Task.sleep(for: .seconds(30)) }
        deleteFinished = true
    }

    func releasePut() {
        holdsPut = false
        heldPut?.resume()
        heldPut = nil
    }
}

/// Polls `condition` on the main actor until it holds or `limit` passes.
@MainActor
private func eventually(within limit: Duration = .seconds(5), _ condition: () -> Bool) async -> Bool {
    let end = ContinuousClock.now + limit
    while !condition() {
        if ContinuousClock.now >= end { return false }
        try? await Task.sleep(for: .milliseconds(10))
    }
    return true
}

struct FixedEnvironment: PushEnvironmentResolving {
    let value: PushEnvironment?
    func environment() -> PushEnvironment? { value }
}

@MainActor
@Suite("Push token registrar")
struct PushTokenRegistrarTests {
    static let token = Data(repeating: 0xab, count: 32)
    static func session() -> PushSession {
        PushSession(serverInstanceID: SyncFixtures.instanceID, accountID: SyncFixtures.accountID, deviceID: SyncFixtures.deviceID,
                    familyID: SyncFixtures.familyID)
    }
    private func make(capabilities: [String] = ["records-v1", "push-token-v1"], environment: PushEnvironment? = .sandbox)
        -> (PushTokenRegistrar, RecordingPushTransport, UserDefaults) {
        let defaults = UserDefaults(suiteName: "push-registrar-\(UUID().uuidString)")!
        let registrar = PushTokenRegistrar(environment: FixedEnvironment(value: environment), defaults: defaults,
                                           capabilities: { capabilities })
        return (registrar, RecordingPushTransport(), defaults)
    }

    @Test func uploadsTheFirstTokenOnceASessionIsActive() async {
        let (registrar, transport, _) = make()
        await registrar.deviceTokenReceived(Self.token)
        #expect(transport.puts.isEmpty) // no session yet
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn)
        #expect(transport.puts == [PushTokenRequest(token: Self.token, environment: .sandbox)])
        #expect(transport.puts.first?.token == String(repeating: "ab", count: 32))
    }

    @Test func doesNothingWhenTheServerDoesNotAdvertisePush() async {
        let (registrar, transport, _) = make(capabilities: ["records-v1"])
        await registrar.deviceTokenReceived(Self.token)
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn)
        #expect(transport.puts.isEmpty)
    }

    @Test func doesNothingWhenTheBuildIsNotProvisionedForPush() async {
        let (registrar, transport, _) = make(environment: nil)
        await registrar.deviceTokenReceived(Self.token)
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn)
        #expect(transport.puts.isEmpty)
    }

    @Test func skipsAnUnchangedTokenAtLaunch() async {
        let (registrar, transport, defaults) = make()
        await registrar.deviceTokenReceived(Self.token)
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn)
        let relaunched = PushTokenRegistrar(environment: FixedEnvironment(value: .sandbox), defaults: defaults, capabilities: { ["push-token-v1"] })
        await relaunched.deviceTokenReceived(Self.token)
        await relaunched.sessionActivated(Self.session(), transport: transport, activation: .restored)
        #expect(transport.puts.count == 1)
    }

    @Test func uploadsAgainAfterReauthentication() async {
        let (registrar, transport, _) = make()
        await registrar.deviceTokenReceived(Self.token)
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn)
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .restored) // a launch: unchanged, skipped
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn) // re-authentication: uploaded
        #expect(transport.puts.count == 2)
    }

    @Test func uploadsAChangedTokenAndRetriesAFailedUpload() async {
        let (registrar, transport, _) = make()
        await registrar.deviceTokenReceived(Self.token)
        transport.failNext = true
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn)
        #expect(transport.puts.isEmpty)
        await registrar.deviceTokenReceived(Data(repeating: 0xcd, count: 32)) // a new token arrives and retries
        #expect(transport.puts.map(\.token) == [String(repeating: "cd", count: 32)])
    }

    /// A failed upload leaves nothing remembered, so the next launch's
    /// fingerprint check still sees it owed.
    @Test func aFailedUploadIsRetriedByTheNextLaunch() async {
        let (registrar, transport, defaults) = make()
        await registrar.deviceTokenReceived(Self.token)
        transport.failNext = true
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn)
        #expect(transport.puts.isEmpty)
        let relaunched = PushTokenRegistrar(environment: FixedEnvironment(value: .sandbox), defaults: defaults, capabilities: { ["push-token-v1"] })
        await relaunched.deviceTokenReceived(Self.token)
        await relaunched.sessionActivated(Self.session(), transport: transport, activation: .restored)
        #expect(transport.puts.count == 1)
    }

    @Test func signOutDeletesTheTokenAndForgetsTheFingerprint() async {
        let (registrar, transport, defaults) = make()
        await registrar.deviceTokenReceived(Self.token)
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn)
        await registrar.sessionEnding()
        #expect(transport.deletes == 1)
        #expect(defaults.string(forKey: "push.uploaded-fingerprint") == nil)
        #expect(defaults.dictionaryRepresentation().values.allSatisfy { !"\($0)".contains("abab") }) // no raw token persisted
    }

    /// A DELETE that never answers holds sign-out up only until the deadline.
    /// Asserted by the DELETE never finishing rather than a tight wall-clock
    /// bound: other suites in this target block the main actor for seconds
    /// at a time (see `aSyncPastTheDeadlineReportsFailedInTime`).
    @Test func aDeleteThatNeverReturnsEndsWithinTheDeadline() async {
        let defaults = UserDefaults(suiteName: "push-registrar-\(UUID().uuidString)")!
        let registrar = PushTokenRegistrar(environment: FixedEnvironment(value: .sandbox), defaults: defaults,
                                           capabilities: { ["push-token-v1"] }, endingDeadline: .milliseconds(100))
        let transport = GatedPushTransport()
        transport.deleteNeverReturns = true
        await registrar.deviceTokenReceived(Self.token)
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn)

        let start = ContinuousClock.now
        await registrar.sessionEnding()

        #expect(transport.deleteStarted)
        #expect(!transport.deleteFinished)
        #expect(ContinuousClock.now - start < .seconds(20))
        #expect(defaults.string(forKey: "push.uploaded-fingerprint") == nil)
    }

    /// An upload already on the wire lands before the DELETE, so it cannot
    /// register the token again after sign-out.
    @Test func anUploadInFlightLandsBeforeTheDelete() async {
        let (registrar, _, _) = make()
        let transport = GatedPushTransport()
        transport.holdsPut = true
        await registrar.deviceTokenReceived(Self.token)
        let activating = Task { await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn) }
        #expect(await eventually { transport.putStarted })

        let ending = Task { await registrar.sessionEnding() }
        // Time for a DELETE that did not wait to go out.
        try? await Task.sleep(for: .milliseconds(100))
        transport.releasePut()
        await activating.value
        await ending.value

        #expect(transport.events == ["PUT", "DELETE"])
    }

    /// A sign-in marks the upload owed before its activation can run. The
    /// fingerprint is forgotten then, so an app killed before the upload
    /// still uploads at the next launch, where the activation is `.restored`.
    @Test func aForgottenFingerprintUploadsAtTheNextLaunch() async {
        let (registrar, transport, defaults) = make()
        await registrar.deviceTokenReceived(Self.token)
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn)
        registrar.forgetUploadedFingerprint()

        let relaunched = PushTokenRegistrar(environment: FixedEnvironment(value: .sandbox), defaults: defaults,
                                            capabilities: { ["push-token-v1"] })
        await relaunched.deviceTokenReceived(Self.token)
        await relaunched.sessionActivated(Self.session(), transport: transport, activation: .restored)

        #expect(transport.puts.count == 2)
    }
}
