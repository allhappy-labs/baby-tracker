import BabyStore
import BabySync
import Foundation
import Testing
@testable import SettingsFeature

private final class RecoveryCompositionProtocol: URLProtocol, @unchecked Sendable {
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        let isACK = request.url?.path == "/v1/auth/refresh-ack"
        let response = HTTPURLResponse(url: request.url!, statusCode: isACK ? 204 : 503, httpVersion: nil, headerFields: ["Content-Type": "application/json"])!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        if !isACK { client?.urlProtocol(self, didLoad: Data("{\"error\":{\"code\":\"service_unavailable\"}}".utf8)) }
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}

@MainActor
struct RecoveryCompositionDiagnosticsTests {
    @Test func actualCompositionFinishesExactKeychainInstallAndAcknowledgesWithPrivateTrace() async throws {
        let fixture = try FamilySettingsFixture.make()
        try await fixture.model.signInWithApple()
        guard case .shared(let family) = fixture.model.mode else { Issue.record("fixture family required"); return }
        let account = family.accountSessionKey
        let parent = try #require(try fixture.keychain.loadFamily(family))
        let leases = AppGroupFencedLease(container: fixture.store.container, lockURL: fixture.store.coordinationLockURL)
        let token = try #require(try leases.acquire(scope: .account(account.leaseKey), ownerID: UUID(), now: Date(), ttl: 30))
        let checkpoint = try fixture.store.beginRecoverableRefresh(for: account.leaseKey, recoveryEpoch: account.recoveryEpoch, credentialVersion: parent.credentialVersion, using: token, now: Date())
        let binding = RefreshInstallBinding(key: account.leaseKey, recoveryEpoch: account.recoveryEpoch, attemptID: checkpoint.attempt.attemptID, parentCredentialVersion: parent.credentialVersion, expectedCredentialVersion: UUID(), accessFingerprint: RefreshInstallBinding.fingerprint("composition-child-access"), refreshFingerprint: RefreshInstallBinding.fingerprint("composition-child-refresh"))
        try fixture.store.prepareRefreshInstall(binding, for: account.leaseKey, using: token, now: Date())
        let child = SessionCredentials(accessToken: "composition-child-access", refreshToken: "composition-child-refresh", expiresAt: .distantFuture, recoveryEpoch: account.recoveryEpoch, deviceContinuity: parent.deviceContinuity, credentialVersion: binding.expectedCredentialVersion, installBinding: binding)
        try fixture.keychain.saveFamily(child, for: family)
        try leases.cancel(token)
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [RecoveryCompositionProtocol.self]
        let session = URLSession(configuration: config)
        defer { session.invalidateAndCancel() }
        var events: [RefreshRecoveryEvent] = []
        let graph = FamilySyncComposition.assemble(store: fixture.store, keychainBackend: fixture.backend, identity: HTTPIdentityClient(configuration: config), session: session, defaults: fixture.defaults, recoveryDiagnostics: { events.append($0) })
        graph.model.restoreSession()
        await graph.model.startSyncing()
        for _ in 0..<100 where !events.contains(where: { $0.phase.rawValue == "acknowledged" }) {
            try await Task.sleep(for: .milliseconds(10))
        }
        graph.model.sceneEnteredBackground()
        #expect(try fixture.keychain.loadFamily(family) == child)
        #expect(try fixture.store.refreshRecoveryCheckpoint(for: account.leaseKey) == nil)
        #expect(events.map { $0.phase.rawValue }.contains("installed"))
        #expect(events.map { $0.phase.rawValue }.contains("ackPending"))
        #expect(events.map { $0.phase.rawValue }.contains("acknowledged"))
        let forbidden = [parent.accessToken, parent.refreshToken, parent.deviceContinuity.credential, child.accessToken, child.refreshToken, account.accountID.uuidString, family.familyID.uuidString, account.deviceID.uuidString, account.canonicalOrigin, checkpoint.attempt.attemptID.uuidString, binding.accessFingerprint, binding.refreshFingerprint]
        for event in events {
            #expect(Set(Mirror(reflecting: event).children.compactMap(\.label)) == ["phase", "reason", "durationMilliseconds", "retryCount", "correlation"])
            #expect((0...Int(Int32.max)).contains(event.durationMilliseconds))
            #expect((0...10).contains(event.retryCount))
            let trace = [event.phase.rawValue, event.reason?.rawValue ?? "", event.correlation.uuidString, String(event.durationMilliseconds), String(event.retryCount)].joined(separator: "|").lowercased()
            for secret in forbidden where !secret.isEmpty { #expect(!trace.contains(secret.lowercased())) }
            #expect(event.correlation != checkpoint.attempt.attemptID)
        }
    }
}
