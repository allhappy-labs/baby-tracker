import Darwin
import BabyProtocol
import BabyCore
import BabyStore
import CryptoKit
import Foundation
import SwiftData
import Testing
@testable import BabySync

/// Disk persistence models an atomic Keychain item across separate store instances.
final class RecoveryDiskBackend: KeychainBackend, @unchecked Sendable {
    let directory: URL
    private let lock = NSLock()
    private var writesBlocked = false
    func blockWrites(_ blocked: Bool) { lock.lock(); writesBlocked = blocked; lock.unlock() }
    init(_ directory: URL) throws {
        self.directory = directory
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
    }
    func path(_ service: String, _ account: String) -> URL {
        directory.appending(path: SHA256.hash(data: Data((service + "|" + account).utf8)).map { String(format: "%02x", $0) }.joined())
    }
    func read(service: String, account: String) throws -> Data? {
        let url = path(service, account)
        return FileManager.default.fileExists(atPath: url.path) ? try Data(contentsOf: url) : nil
    }
    func write(_ data: Data, service: String, account: String) throws {
        lock.lock(); let blocked = writesBlocked; lock.unlock()
        if blocked { throw SessionStoreError.keychain(-25308) }
        try data.write(to: path(service, account), options: .atomic)
    }
    func delete(service: String, account: String) throws {
        let url = path(service, account)
        if FileManager.default.fileExists(atPath: url.path) { try FileManager.default.removeItem(at: url) }
    }
}

actor RecoveryTestTransport: SessionRecoveryTransport {
    var requests: [(SessionCredentials, UUID, UUID)] = []
    var legacyRequests = 0
    var failure: HTTPClientError?
    func fail(with error: HTTPClientError?) { failure = error }
    var lostReply: Bool
    var ackFails = false
    var acknowledgements: [UUID] = []
    var beforeReply: (@Sendable () async throws -> Void)?
    func onReply(_ hook: @escaping @Sendable () async throws -> Void) { beforeReply = hook }
    var capability = true
    var epoch: UInt64 = 0
    var child: SessionCredentials
    init(lostReply: Bool = false, expiresAt: Date = .distantFuture) {
        self.lostReply = lostReply
        child = SessionCredentials(accessToken: "recovered-access", refreshToken: "recovered-refresh", expiresAt: expiresAt, recoveryEpoch: 0, deviceContinuity: SessionFixture.continuity)
    }
    func configure(capability: Bool = true, epoch: UInt64 = 0, ackFails: Bool = false) {
        self.capability = capability; self.epoch = epoch; self.ackFails = ackFails
    }
    func serverInfo(_ origin: ServerOrigin) async throws -> ServerInfo {
        ServerInfo(serverInstanceID: SessionFixture.instanceID, canonicalOrigin: origin.canonicalString, recoveryEpoch: epoch, capabilities: capability ? ["refresh-recovery-v1"] : [], appleAuthorizationMode: "self_hosted", maxItemsPerPage: 100, maxEncodedBytes: 10000, sourceVersion: "fixture", sourceURL: "")
    }
    func refresh(refreshToken: String, at endpoint: ConfirmedEndpoint) async throws -> SessionCredentials {
        legacyRequests += 1
        return child
    }
    func refreshRecoverable(parent: SessionCredentials, expectedAccountID: UUID, attemptID: UUID, at endpoint: ConfirmedEndpoint) async throws -> RecoverableRefreshResult {
        requests.append((parent, expectedAccountID, attemptID))
        try await beforeReply?()
        if let failure { throw failure }
        if lostReply { lostReply = false; throw URLError(.networkConnectionLost) }
        if parent.refreshToken == "recovered-refresh" {
            child = SessionCredentials(accessToken: "descendant-access", refreshToken: "descendant-refresh", expiresAt: .distantFuture, recoveryEpoch: 0, deviceContinuity: SessionFixture.continuity)
        }
        return RecoverableRefreshResult(credentials: child, attemptID: attemptID, accountID: expectedAccountID, reauthenticateAt: .distantFuture)
    }
    func acknowledgeRefresh(attemptID: UUID, credentials: SessionCredentials, at endpoint: ConfirmedEndpoint) async throws {
        acknowledgements.append(attemptID)
        if ackFails { throw URLError(.networkConnectionLost) }
    }
}

@MainActor
struct RecoveryTestHarness {
    let directory: URL
    let store: BabyStore
    let keychain: KeychainSessionStore
    let backend: RecoveryDiskBackend
    let leases: AppGroupFencedLease
    let account = SessionFixture.accountKey()
    let family = SessionFixture.familyKey()
    init(directory: URL? = nil) throws {
        let directory = directory ?? FileManager.default.temporaryDirectory.appending(path: "recovery-client-\(UUID())")
        self.directory = directory
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let lock = directory.appending(path: "coordination.lock")
        let container = try BabySchema.container(storeURL: directory.appending(path: "store.sqlite"), bootstrapLockURL: directory.appending(path: "bootstrap.lock"))
        store = BabyStore(container: container, dateProvider: TestDateProvider(.now), deviceID: "recovery-test", coordinationLockURL: lock)
        leases = AppGroupFencedLease(container: container, lockURL: lock)
        backend = try RecoveryDiskBackend(directory.appending(path: "keychain"))
        keychain = KeychainSessionStore(backend: backend)
    }
    func seed(_ credentials: SessionCredentials = SessionFixture.credentials()) throws {
        try store.markAccountAuthenticated(account.leaseKey, credentialVersion: credentials.credentialVersion)
        try keychain.saveFamily(credentials, for: family)
    }
    func refresher(_ transport: RecoveryTestTransport) -> SessionRefresher {
        SessionRefresher(store: store, keychain: keychain, leases: leases, endpoint: SessionFixture.endpoint, recovery: transport)
    }
}

@MainActor
@Suite("Resumable client refresh", .serialized)
struct RefreshRecoveryTests {
    @Test(arguments: ["terminal", "newerPendingParent", "freshSignIn", "logout", "locked", "lease", "unavailable"], ["reauthentication_required", "session_revoked", "refresh_reused", "epoch_changed", "refresh_receipt_unavailable", "refresh_attempt_conflict"])
    func pendingRecoveryFailureKeepsCorrectProofAction(_ scenario: String, _ refreshCode: String) async throws {
        let refreshStatus = refreshCode == "refresh_attempt_conflict" ? 409 : 401
        let h = try RecoveryTestHarness(); try h.seed()
        let queued = SyncOutboxEntry(id: UUID(), realm: .local, entityID: UUID(), entityKind: .baby, operation: .create, payload: Data("retained-command".utf8), sequence: 1, createdAt: .distantPast)
        h.store.context.insert(queued); try h.store.context.save()
        try h.store.setActiveRealm(.shared(canonicalOrigin: h.account.canonicalOrigin, serverInstanceID: h.account.serverInstanceID, familyID: h.family.familyID))
        let recovery = RecoveryTestTransport(lostReply: true)
        let renewer = FamilySessionRenewer(store: h.store, keychain: h.keychain, leases: h.leases, identity: recovery, endpoint: SessionFixture.endpoint)
        RecoveryAuthProtocol.handler = { _, _ in (401, Data("{\"error\":{\"code\":\"session_revoked\"}}".utf8)) }
        let config = URLSessionConfiguration.ephemeral; config.protocolClasses = [RecoveryAuthProtocol.self]
        let session = URLSession(configuration: config)
        defer { session.invalidateAndCancel(); RecoveryAuthProtocol.handler = nil }
        let family = h.family
        let client = FamilySyncHTTPClient(endpoint: SessionFixture.endpoint, tokens: KeychainTokenProvider(keychain: h.keychain, key: { .family(family) }), session: session, retryCredentials: { version in
            try await renewer.credentialsForRetry(after: version, familyKey: family)
        })
        var proofs = 0
        let coordinator = SyncCoordinator(store: h.store, transport: client, accountKey: h.account, onFreshProofRequired: { proofs += 1 })
        await coordinator.syncOnce(reason: .foreground)
        #expect(proofs == 1)
        let originalIssue = try #require(coordinator.currentIssue)
        try? await h.refresher(recovery).refresh(accountKey: h.account, familyKey: h.family)
        #expect(try h.store.refreshRecoveryCheckpoint(for: h.account.leaseKey)?.phase == .awaitingReply)
        #expect(SessionRefresher.requiresFreshProof(HTTPClientError.status(refreshStatus, refreshCode)))
        await recovery.fail(with: .status(refreshStatus, refreshCode))
        if scenario == "unavailable" {
            #expect(!SessionRefresher.requiresFreshProof(HTTPClientError.status(503, "service_unavailable")))
            await recovery.fail(with: .status(503, "service_unavailable"))
        }
        var held: FencedLeaseToken?
        if scenario == "lease" {
            held = try h.leases.acquire(scope: .account(h.account.leaseKey), ownerID: UUID(), now: Date(), ttl: 30)
        }
        defer { if let held { try? h.leases.cancel(held) } }
        if scenario == "locked" { await recovery.onReply { throw SessionStoreError.keychain(-25308) } }
        if scenario == "freshSignIn" || scenario == "logout" {
            await recovery.onReply {
                try await MainActor.run {
                    if scenario == "logout" { try h.store.markAccountSignedOut(h.account.leaseKey) }
                    else {
                        let fresh = SessionCredentials(accessToken: "fresh-after-reconcile", refreshToken: "fresh-after-reconcile", expiresAt: .distantFuture, recoveryEpoch: 0, deviceContinuity: SessionFixture.continuity)
                        try h.store.markAccountSignedIn(h.account.leaseKey, credentialVersion: fresh.credentialVersion) {
                            try h.keychain.saveFamily(fresh, for: h.family)
                        }
                    }
                }
            }
        }
        if scenario == "newerPendingParent" {
            RecoveryAuthProtocol.handler = { request, _ in
                #expect(request.value(forHTTPHeaderField: "Authorization") == "Bearer access-token")
                await recovery.fail(with: nil)
                try await h.refresher(recovery).refresh(accountKey: h.account, familyKey: h.family)
                await recovery.fail(with: .status(503, "lost-successor-reply"))
                try? await h.refresher(recovery).refresh(accountKey: h.account, familyKey: h.family)
                await recovery.fail(with: .status(refreshStatus, refreshCode))
                return (401, Data("{\"error\":{\"code\":\"session_revoked\"}}".utf8))
            }
        }
        await coordinator.syncOnce(reason: .foreground)
        #expect(try h.store.context.fetch(FetchDescriptor<SyncOutboxEntry>()).contains { $0.id == queued.id && $0.payload == Data("retained-command".utf8) })
        let terminal = scenario == "terminal" || scenario == "newerPendingParent"
        #expect(coordinator.currentIssue != nil)
        #expect(proofs == (terminal ? 2 : 1))
        if terminal {
            #expect(!coordinator.isOffline)
            #expect(coordinator.currentIssue?.code == "reauthentication_required")
        }
        else { #expect(coordinator.currentIssue == originalIssue) }
        if ["locked", "lease", "unavailable"].contains(scenario) { #expect(coordinator.isOffline) }
        if scenario == "freshSignIn" {
            #expect(try h.keychain.loadFamily(h.family)?.accessToken == "fresh-after-reconcile")
            #expect(try h.store.accountAllowsOrdinarySync(h.account.leaseKey))
        }
        if scenario == "logout" { #expect(try !h.store.accountAllowsOrdinarySync(h.account.leaseKey)) }
    }

    @Test(arguments: [false, true], ["readiness", "lease", "keychain", "corrupt", "binding", "receipt", "conflict", "unknown"])
    func preflightFailureIsVisibleAndRetryResumes(_ previouslyCurrent: Bool, _ outcome: String) async throws {
        let family = try Family()
        try family.joiner.setActiveRealm(family.joinerKey.realmID)
        var failure: (any Error)?
        var entered: CheckedContinuation<Void, Never>?
        var release: CheckedContinuation<Void, Never>?
        var suspend = false
        let engine = FamilySyncEngine(store: family.joiner, familyKey: family.joinerKey, transport: family.joinerTransport, importClient: EngineFixture.resolver(), renew: {}, prepareSession: {
            if suspend {
                entered?.resume(); entered = nil
                await withCheckedContinuation { release = $0 }
            }
            if let failure { throw failure }
        })
        if previouslyCurrent {
            await engine.syncNow(.foreground)
            guard case .upToDate = engine.status else { Issue.record("fixture must first sync"); return }
        }
        switch outcome {
        case "readiness": failure = HTTPClientError.status(503, "service_unavailable")
        case "lease": failure = SessionRefresher.RefreshError.lockUnavailable
        case "keychain": failure = SessionStoreError.keychain(-25308)
        case "corrupt": failure = RefreshRecoveryError.corruptCheckpoint
        case "binding": failure = RefreshRecoveryError.invalidBinding
        case "receipt": failure = HTTPClientError.status(401, "refresh_receipt_unavailable")
        case "conflict": failure = HTTPClientError.status(409, "refresh_attempt_conflict")
        default: failure = HTTPClientError.status(401, "future_unknown_error")
        }
        let before = family.server.requestBodies.count
        suspend = true
        let run = Task { await engine.syncNow(.foreground) }
        await withCheckedContinuation { entered = $0 }
        #expect(engine.status == .syncing)
        release?.resume()
        await run.value
        #expect(family.server.requestBodies.count == before)
        let terminal = ["corrupt", "binding", "receipt", "conflict"].contains(outcome)
        #expect(engine.status == (terminal ? .needsSignIn : .failed(SyncIssueCopy.retrying)))
        if terminal { return }
        failure = nil; suspend = false
        await engine.syncNow(.foreground)
        guard case .upToDate = engine.status else { Issue.record("retry must sync successfully"); return }
        #expect(family.server.requestBodies.count > before)
    }

    @Test(arguments: ["newSignIn", "logout", "stopped"], ["terminal", "corrupt", "success"])
    func stalePreflightCannotOverrideNewSessionOrStop(_ change: String, _ outcome: String) async throws {
        let family = try Family()
        try family.joiner.setActiveRealm(family.joinerKey.realmID)
        var entered: CheckedContinuation<Void, Never>?
        var release: CheckedContinuation<Void, Never>?
        let engine = FamilySyncEngine(store: family.joiner, familyKey: family.joinerKey, transport: family.joinerTransport, importClient: EngineFixture.resolver(), renew: {}, prepareSession: {
            entered?.resume(); entered = nil
            await withCheckedContinuation { release = $0 }
            if outcome == "terminal" { throw HTTPClientError.status(401, "refresh_receipt_unavailable") }
            if outcome == "corrupt" { throw RefreshRecoveryError.corruptCheckpoint }
        })
        let run = Task { await engine.syncNow(.foreground) }
        await withCheckedContinuation { entered = $0 }
        if change == "newSignIn" { try family.joiner.markAccountSignedIn(family.joinerKey.leaseKey, credentialVersion: UUID()) }
        if change == "logout" { try family.joiner.markAccountSignedOut(family.joinerKey.leaseKey) }
        let stopping = change == "stopped" ? Task { await engine.stop() } : nil
        if stopping != nil { await Task.yield() }
        let before = engine.status
        release?.resume()
        await run.value; await stopping?.value
        if change == "stopped" {
            #expect(family.server.requestBodies.isEmpty)
            #expect(engine.status == before)
        } else if outcome == "success" {
            if change == "logout" { #expect(engine.status == .needsSignIn) }
            else { guard case .upToDate = engine.status else { Issue.record("new valid sign-in may sync"); return } }
        } else {
            #expect(family.server.requestBodies.isEmpty)
            #expect(engine.status == .failed(SyncIssueCopy.retrying))
        }
    }

    @Test(arguments: ["current", "freshSignIn", "logout"], ["receipt", "conflict", "boundChild"])
    func realPreflightBindsTerminalOutcomeToItsActualParent(_ scenario: String, _ outcome: String) async throws {
        let h = try RecoveryTestHarness(); try h.seed()
        try h.store.setActiveRealm(h.family.realmID)
        let transport = RecoveryTestTransport(expiresAt: outcome == "boundChild" ? .distantPast : .distantFuture)
        await transport.onReply {
            if outcome == "boundChild", await transport.requests.count == 1 { return }
            if scenario == "freshSignIn" {
                try await MainActor.run {
                    let fresh = SessionFixture.credentials(access: "fresh-preflight-access", refresh: "fresh-preflight-refresh")
                    let replacement = SessionCredentials(accessToken: fresh.accessToken, refreshToken: fresh.refreshToken, expiresAt: .distantFuture, recoveryEpoch: fresh.recoveryEpoch, deviceContinuity: fresh.deviceContinuity)
                    try h.store.markAccountSignedIn(h.account.leaseKey, credentialVersion: replacement.credentialVersion) { try h.keychain.saveFamily(replacement, for: h.family) }
                }
            } else if scenario == "logout" { try await MainActor.run { try h.store.markAccountSignedOut(h.account.leaseKey) } }
            await transport.fail(with: outcome == "conflict" ? .status(409, "refresh_attempt_conflict") : .status(401, "refresh_receipt_unavailable"))
        }
        let renewer = FamilySessionRenewer(store: h.store, keychain: h.keychain, leases: h.leases, identity: transport, endpoint: SessionFixture.endpoint)
        let family = try Family()
        let engine = FamilySyncEngine(store: h.store, familyKey: h.family, transport: family.joinerTransport, importClient: EngineFixture.resolver(), renew: {}, prepareSession: { try await renewer.prepare(familyKey: h.family) })
        await engine.syncNow(.foreground)
        #expect(family.server.requestBodies.isEmpty)
        #expect(engine.status == (scenario == "current" ? .needsSignIn : .failed(SyncIssueCopy.retrying)))
        if outcome == "boundChild", scenario == "current" {
            #expect(try h.keychain.loadFamily(h.family)?.accessToken == "recovered-access")
            #expect(try h.store.refreshRecoveryCheckpoint(for: h.account.leaseKey)?.attempt.credentialVersion != SessionFixture.credentials().credentialVersion)
        }
    }

    @Test func corruptDurableCheckpointOffersSignInWithoutDiscardingQueuedEntries() async throws {
        let h = try RecoveryTestHarness(); try h.seed()
        try h.store.setActiveRealm(h.family.realmID)
        let queued = SyncOutboxEntry(id: UUID(), realm: .local, entityID: UUID(), entityKind: .baby, operation: .create, payload: Data("retained-preflight-command".utf8), sequence: 7, createdAt: .distantPast)
        h.store.context.insert(queued)
        let row = try #require(try h.store.context.fetch(FetchDescriptor<AccountAccessState>()).first)
        row.refreshStateData = Data("corrupt-sensitive-bytes".utf8)
        try h.store.context.save()
        let recovery = RecoveryTestTransport()
        let renewer = FamilySessionRenewer(store: h.store, keychain: h.keychain, leases: h.leases, identity: recovery, endpoint: SessionFixture.endpoint)
        let family = try Family()
        let engine = FamilySyncEngine(store: h.store, familyKey: h.family, transport: family.joinerTransport, importClient: EngineFixture.resolver(), renew: {}, prepareSession: { try await renewer.prepare(familyKey: h.family) })
        await engine.syncNow(.foreground)
        #expect(engine.status == .needsSignIn)
        #expect(family.server.requestBodies.isEmpty)
        #expect(await recovery.requests.isEmpty)
        #expect(try h.store.context.fetch(FetchDescriptor<SyncOutboxEntry>()).contains { $0.id == queued.id && $0.sequence == 7 && $0.payload == Data("retained-preflight-command".utf8) })
        #expect(try h.keychain.loadFamily(h.family)?.accessToken == "access-token")
    }

    @Test(arguments: ["transient", "terminal", "completedOnResume"])
    func recoveryDiagnosticsExplainDeferredTerminalAndCrashCompletion(_ scenario: String) async throws {
        let h = try RecoveryTestHarness(); try h.seed()
        let transport = RecoveryTestTransport()
        var events: [RefreshRecoveryEvent] = []
        let refresher = SessionRefresher(store: h.store, keychain: h.keychain, leases: h.leases, endpoint: SessionFixture.endpoint, recovery: transport, diagnostics: { events.append($0) })
        if scenario == "completedOnResume" {
            let parent = try #require(try h.keychain.loadFamily(h.family))
            let token = try #require(try h.leases.acquire(scope: .account(h.account.leaseKey), ownerID: UUID(), now: Date(), ttl: 30))
            let checkpoint = try h.store.beginRecoverableRefresh(for: h.account.leaseKey, recoveryEpoch: 0, credentialVersion: parent.credentialVersion, using: token, now: Date())
            let binding = RefreshInstallBinding(key: h.account.leaseKey, recoveryEpoch: 0, attemptID: checkpoint.attempt.attemptID, parentCredentialVersion: parent.credentialVersion, expectedCredentialVersion: UUID(), accessFingerprint: RefreshInstallBinding.fingerprint("child-access"), refreshFingerprint: RefreshInstallBinding.fingerprint("child-refresh"))
            try h.store.prepareRefreshInstall(binding, for: h.account.leaseKey, using: token, now: Date())
            try h.keychain.saveFamily(SessionCredentials(accessToken: "child-access", refreshToken: "child-refresh", expiresAt: .distantFuture, recoveryEpoch: 0, deviceContinuity: parent.deviceContinuity, credentialVersion: binding.expectedCredentialVersion, installBinding: binding), for: h.family)
            try h.leases.cancel(token)
            try await refresher.refresh(accountKey: h.account, familyKey: h.family)
            #expect(events.map { $0.phase.rawValue } == ["installed", "ackPending"])
            #expect(await transport.requests.isEmpty)
        } else {
            await transport.fail(with: scenario == "terminal" ? .status(401, "refresh_receipt_unavailable") : .status(503, "service_unavailable"))
            try? await refresher.refresh(accountKey: h.account, familyKey: h.family)
            #expect(events.last?.phase.rawValue == (scenario == "terminal" ? "terminal" : "deferred"))
            #expect(events.last?.reason?.rawValue == (scenario == "terminal" ? "refresh_receipt_unavailable" : "retry_later"))
        }
    }

    @Test(arguments: ["output", "exit"])
    func ownedRecoveryProbeTimeoutTerminatesOnlyItsChild(_ phase: String) throws {
        let h = try RecoveryTestHarness(); try h.seed()
        let probe = try RecoveryProcessProbe(h, now: 1_700_000_000, wait: true)
        defer { probe.stop() }
        #expect(try probe.next()["status"] == "awaiting")
        do {
            if phase == "output" { _ = try probe.next(timeout: 0.05) }
            else { try probe.waitForExit(timeout: 0.05) }
            Issue.record("A parked child must time out")
        } catch let error as RecoveryProcessProbe.Timeout {
            #expect(error.phase == phase)
            #expect(!probe.process.isRunning)
            #expect(error.exitStatus != nil)
            #expect(!error.description.contains("refresh-token"))
            #expect(!error.description.contains("device-credential-abc"))
        }
    }

    @Test func threeProcessRenewalHasOneLogicalAttempt() throws {
        let h = try RecoveryTestHarness(); try h.seed()
        let queued = SyncOutboxEntry(id: UUID(), realm: .local, entityID: UUID(), entityKind: .baby, operation: .create, payload: Data("synthetic-pending-command".utf8), sequence: 41, createdAt: .distantPast)
        queued.deliveryState = .inFlightOrUncertain
        h.store.context.insert(queued); try h.store.context.save()
        let originalHead = try h.store.syncCursor()
        let suspended = try RecoveryProcessProbe(h, now: 1_700_000_000, wait: true)
        defer { suspended.stop() }
        #expect(try suspended.next()["status"] == "awaiting")
        let pending = try #require(try h.store.refreshRecoveryCheckpoint(for: h.account.leaseKey))
        let competitor = try RecoveryProcessProbe(h, now: 1_700_000_000, wait: false)
        defer { competitor.stop() }
        #expect(try competitor.next()["result"] == "held")
        try competitor.waitForExit()
        let replacement = try RecoveryProcessProbe(h, now: 1_700_000_031, wait: false)
        defer { replacement.stop() }
        #expect(try replacement.next()["result"] == "installed")
        try replacement.waitForExit()
        suspended.resume()
        #expect(try suspended.next()["result"] == "stale")
        try suspended.waitForExit()
        let reopened = try RecoveryTestHarness(directory: h.directory)
        let installed = try #require(try reopened.keychain.loadFamily(h.family))
        #expect(installed.accessToken == "process-child-access")
        #expect(installed.refreshToken == "process-child-refresh")
        #expect(installed.installBinding?.attemptID == pending.attempt.attemptID)
        #expect(try reopened.store.refreshRecoveryCheckpoint(for: h.account.leaseKey)?.phase == .ackPending)
        let calls = try FileManager.default.contentsOfDirectory(at: h.directory, includingPropertiesForKeys: nil).filter { $0.lastPathComponent.hasPrefix("request-") }
        #expect(calls.count == 2)
        let bodies = try calls.map { try Data(contentsOf: $0) }
        #expect(bodies[0] == bodies[1])
        let retained = try reopened.store.context.fetch(FetchDescriptor<SyncOutboxEntry>())
        #expect(retained.count == 1)
        #expect(retained.first?.id == queued.id)
        #expect(retained.first?.payload == Data("synthetic-pending-command".utf8))
        #expect(retained.first?.sequence == 41)
        #expect(retained.first?.deliveryState == .inFlightOrUncertain)
        #expect(try reopened.store.syncCursor() == originalHead)
        let receipt = try JSONSerialization.jsonObject(with: Data(contentsOf: h.directory.appending(path: "receipt.json"))) as? [String: Any]
        let child = try #require(receipt?["child"] as? [String: Any])
        #expect(child["credentialVersion"] as? String == installed.credentialVersion.uuidString)
    }

    @Test func mismatchedAccountAndFamilyKeySendsNoCredentials() async throws {
        let h = try RecoveryTestHarness(); try h.seed()
        let other = SessionFixture.familyKey(accountID: SessionFixture.otherAccountID)
        try h.keychain.saveFamily(SessionFixture.credentials(), for: other)
        let transport = RecoveryTestTransport()
        await #expect(throws: RefreshRecoveryError.invalidBinding) {
            try await h.refresher(transport).refresh(accountKey: h.account, familyKey: other)
        }
        #expect(await transport.requests.isEmpty)
        #expect(try h.keychain.loadFamily(other)?.accessToken == "access-token")
    }

    @Test func unknownRecoveryErrorRetainsExactAttempt() async throws {
        for error in [HTTPClientError.status(409, "refresh_receipt_unavailable"), .status(401, "refresh_attempt_conflict"), .status(503, "refresh_receipt_unavailable"), .status(503, "refresh_attempt_conflict"), .status(401, "future_unknown_error"), .status(409, nil)] {
            #expect(!SessionRefresher.requiresFreshProof(error))
        }
        let h = try RecoveryTestHarness(); try h.seed()
        let transport = RecoveryTestTransport()
        await transport.fail(with: .status(503, "future_unknown_error"))
        try? await h.refresher(transport).refresh(accountKey: h.account, familyKey: h.family)
        let original = try #require(try h.store.refreshRecoveryCheckpoint(for: h.account.leaseKey))
        let reopened = try RecoveryTestHarness(directory: h.directory)
        await transport.fail(with: nil)
        try await reopened.refresher(transport).refresh(accountKey: h.account, familyKey: h.family)
        #expect(try reopened.keychain.loadFamily(h.family)?.installBinding?.attemptID == original.attempt.attemptID)
        #expect(await transport.legacyRequests == 0)
    }

    @Test func leaseExpiryRetriesUnderNewFence() async throws {
        let h = try RecoveryTestHarness(); try h.seed()
        let transport = RecoveryTestTransport()
        let clock = TestDateProvider(Date())
        await transport.onReply { clock.advance(by: 31) }
        let first = SessionRefresher(store: h.store, keychain: h.keychain, leases: h.leases, endpoint: SessionFixture.endpoint, recovery: transport, now: { clock.now })
        await #expect(throws: FencedLeaseError.expired) { try await first.refresh(accountKey: h.account, familyKey: h.family) }
        let original = try #require(try h.store.refreshRecoveryCheckpoint(for: h.account.leaseKey))
        await transport.onReply {}
        let reopened = try RecoveryTestHarness(directory: h.directory)
        let retry = SessionRefresher(store: reopened.store, keychain: reopened.keychain, leases: reopened.leases, endpoint: SessionFixture.endpoint, recovery: transport, now: { clock.now })
        try await retry.refresh(accountKey: h.account, familyKey: h.family)
        #expect(try reopened.keychain.loadFamily(h.family)?.installBinding?.attemptID == original.attempt.attemptID)
        let requests = await transport.requests
        #expect(requests.count == 2)
        #expect(requests[0].0 == requests[1].0)
    }

    @Test func freshSignInWinsOverStaleResponse() async throws {
        let h = try RecoveryTestHarness(); try h.seed()
        let fresh = SessionCredentials(accessToken: "fresh-signin-access", refreshToken: "fresh-signin-refresh", expiresAt: .distantFuture, recoveryEpoch: 0, deviceContinuity: SessionFixture.continuity)
        let transport = RecoveryTestTransport()
        await transport.onReply {
            try await MainActor.run {
                try h.store.markAccountSignedIn(h.account.leaseKey, credentialVersion: fresh.credentialVersion) {
                    try h.keychain.saveFamily(fresh, for: h.family)
                }
            }
        }
        await #expect(throws: (any Error).self) { try await h.refresher(transport).refresh(accountKey: h.account, familyKey: h.family) }
        let reopened = try RecoveryTestHarness(directory: h.directory)
        #expect(try reopened.keychain.loadFamily(h.family) == fresh)
        #expect(try reopened.store.refreshRecoveryCheckpoint(for: h.account.leaseKey) == nil)
        #expect(try reopened.store.accountAllowsOrdinarySync(h.account.leaseKey))
    }

    @Test func lockedKeychainRetainsAttemptAndExpectedVersion() async throws {
        let h = try RecoveryTestHarness(); try h.seed()
        let transport = RecoveryTestTransport()
        h.backend.blockWrites(true)
        await #expect(throws: SessionStoreError.self) { try await h.refresher(transport).refresh(accountKey: h.account, familyKey: h.family) }
        let prepared = try #require(try h.store.refreshRecoveryCheckpoint(for: h.account.leaseKey))
        #expect(prepared.phase == .installPrepared)
        let reopened = try RecoveryTestHarness(directory: h.directory)
        try await reopened.refresher(transport).refresh(accountKey: h.account, familyKey: h.family)
        #expect(try reopened.keychain.loadFamily(h.family)?.credentialVersion == prepared.installBinding?.expectedCredentialVersion)
        let requests = await transport.requests
        #expect(requests.count == 2)
        #expect(requests[0].2 == requests[1].2)
        #expect(requests[0].0 == requests[1].0)
    }

    @Test func promotionKeepsAttempt() async throws {
        let h = try RecoveryTestHarness()
        let parent = SessionFixture.credentials()
        try h.store.markAccountAuthenticated(h.account.leaseKey, credentialVersion: parent.credentialVersion)
        try h.keychain.saveAccount(parent, for: h.account)
        let transport = RecoveryTestTransport(lostReply: true)
        try? await h.refresher(transport).refreshAccount(accountKey: h.account)
        let before = try #require(try h.store.refreshRecoveryCheckpoint(for: h.account.leaseKey))
        try h.keychain.promote(h.account, to: h.family, checkpointID: UUID())
        let reopened = try RecoveryTestHarness(directory: h.directory)
        try await reopened.refresher(transport).refresh(accountKey: h.account, familyKey: h.family)
        #expect(try reopened.keychain.loadAccount(h.account) == nil)
        #expect(try reopened.keychain.loadFamily(h.family)?.installBinding?.attemptID == before.attempt.attemptID)
    }

    @Test(arguments: ["epoch", "version", "accessFingerprint", "refreshFingerprint", "device", "identity"])
    func divergentBindingFailsClosed(_ mismatch: String) async throws {
        let h = try RecoveryTestHarness(); try h.seed()
        let parent = try #require(try h.keychain.loadFamily(h.family))
        let time = Date()
        let token = try #require(try h.leases.acquire(scope: .account(h.account.leaseKey), ownerID: UUID(), now: time, ttl: 30))
        let checkpoint = try h.store.beginRecoverableRefresh(for: h.account.leaseKey, recoveryEpoch: 0, credentialVersion: parent.credentialVersion, using: token, now: time)
        let binding = RefreshInstallBinding(key: h.account.leaseKey, recoveryEpoch: 0, attemptID: checkpoint.attempt.attemptID, parentCredentialVersion: parent.credentialVersion, expectedCredentialVersion: UUID(), accessFingerprint: RefreshInstallBinding.fingerprint("child-access"), refreshFingerprint: RefreshInstallBinding.fingerprint("child-refresh"))
        try h.store.prepareRefreshInstall(binding, for: h.account.leaseKey, using: token, now: time)
        let forged = RefreshInstallBinding(key: SessionFixture.accountKey(accountID: SessionFixture.otherAccountID).leaseKey, recoveryEpoch: 0, attemptID: binding.attemptID, parentCredentialVersion: binding.parentCredentialVersion, expectedCredentialVersion: binding.expectedCredentialVersion, accessFingerprint: binding.accessFingerprint, refreshFingerprint: binding.refreshFingerprint)
        let child = SessionCredentials(accessToken: mismatch == "accessFingerprint" ? "other-access" : "child-access", refreshToken: mismatch == "refreshFingerprint" ? "other-refresh" : "child-refresh", expiresAt: .distantFuture, recoveryEpoch: mismatch == "epoch" ? 1 : 0, deviceContinuity: mismatch == "device" ? DeviceContinuity(deviceID: SessionFixture.otherDeviceID, credential: "device-credential-abc") : parent.deviceContinuity, credentialVersion: mismatch == "version" ? UUID() : binding.expectedCredentialVersion, installBinding: mismatch == "identity" ? forged : binding)
        try h.keychain.saveFamily(child, for: h.family)
        try h.leases.cancel(token)
        let reopened = try RecoveryTestHarness(directory: h.directory)
        let transport = RecoveryTestTransport()
        await #expect(throws: (any Error).self) { try await reopened.refresher(transport).refresh(accountKey: h.account, familyKey: h.family) }
        #expect(await transport.requests.isEmpty)
        #expect(try reopened.store.refreshRecoveryCheckpoint(for: h.account.leaseKey)?.phase == .installPrepared)
        #expect(try reopened.keychain.loadFamily(h.family) == child)
    }

    @Test func missingHistoricalContinuityUsesLegacyAndDiagnosticsContainNoSecrets() async throws {
        let h = try RecoveryTestHarness()
        let historical = SessionFixture.credentials(continuity: DeviceContinuity(deviceID: SessionFixture.deviceID, credential: ""))
        try h.seed(historical)
        let transport = RecoveryTestTransport()
        var events: [RefreshRecoveryEvent] = []
        let refresher = SessionRefresher(store: h.store, keychain: h.keychain, leases: h.leases, endpoint: SessionFixture.endpoint, recovery: transport, diagnostics: { events.append($0) })
        try await refresher.refresh(accountKey: h.account, familyKey: h.family)
        #expect(await transport.legacyRequests == 1)
        #expect(await transport.requests.isEmpty)
        #expect(try h.keychain.loadFamily(h.family)?.accessToken == "recovered-access")
        #expect(events.contains { $0.reason == .recoveryUnavailableMissingDeviceBinding })
        for event in events {
            #expect(Set(Mirror(reflecting: event).children.compactMap(\.label)) == ["phase", "reason", "durationMilliseconds", "retryCount", "correlation"])
            #expect(event.durationMilliseconds >= 0)
            let text = String(reflecting: event)
            for secret in ["access-token", "refresh-token", SessionFixture.accountID.uuidString, SessionFixture.origin.canonicalString, "device-credential-abc"] { #expect(!text.contains(secret)) }
        }
    }

    @Test func engineRestorationWaitsForCredentialRecoveryBeforeActivation() async throws {
        enum Locked: Error { case unavailable }
        let family = try Family()
        var locked = true
        let engine = FamilySyncEngine(store: family.joiner, familyKey: family.joinerKey, transport: family.joinerTransport, importClient: EngineFixture.resolver(), renew: {}, prepareSession: {
            if locked { throw Locked.unavailable }
        })
        await #expect(throws: Locked.unavailable) { try await engine.activateAsJoiner() }
        #expect(!family.joiner.realmIsShared)
        #expect(family.server.requestBodies.isEmpty)
        locked = false
        try await engine.activateAsJoiner()
        #expect(family.joiner.realmIsShared)
    }

    @Test func lateConsumedParentResponseRetriesInstalledChild() async throws {
        try await authRace(rejectChild: false)
    }

    @Test func currentCredentialRevocationStillRequiresProof() async throws {
        try await authRace(rejectChild: true)
    }

    private func authRace(rejectChild: Bool) async throws {
        let h = try RecoveryTestHarness(); try h.seed()
        let recovery = RecoveryTestTransport()
        let renewer = FamilySessionRenewer(store: h.store, keychain: h.keychain, leases: h.leases, identity: recovery, endpoint: SessionFixture.endpoint)
        let capture = RecoveryRequestCapture()
        let applyingServer = FakeFamilyServer()
        RecoveryAuthProtocol.handler = { request, data in
            let number = await capture.append(data, request.value(forHTTPHeaderField: "Authorization") ?? "")
            if number == 1 { try await h.refresher(recovery).refresh(accountKey: h.account, familyKey: h.family) }
            if number == 1 || rejectChild { return (401, Data("{\"error\":{\"code\":\"session_revoked\"}}".utf8)) }
            let decoded = try JSONDecoder().decode(SyncRequest.self, from: data)
            let applied = try await MainActor.run {
                try applyingServer.sync(decoded, accountID: SessionFixture.accountID, deviceID: SessionFixture.deviceID)
            }
            return (200, try JSONEncoder().encode(applied))
        }
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [RecoveryAuthProtocol.self]
        let session = URLSession(configuration: configuration)
        defer { session.invalidateAndCancel(); RecoveryAuthProtocol.handler = nil }
        let family = h.family
        let tokens = KeychainTokenProvider(keychain: h.keychain, key: { .family(family) })
        let client = FamilySyncHTTPClient(endpoint: SessionFixture.endpoint, tokens: tokens, session: session, retryCredentials: { version in
            try await renewer.credentialsForRetry(after: version, familyKey: family)
        })
        let mutation = WireMutation(mutationID: UUID(uuidString: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")!, entityID: UUID(), entityKind: .baby, operation: .create, payloadVersion: 1, observedEntityVersion: 0, clientMutationTime: "2026-10-01T00:00:00Z", importID: nil, payload: .object([:]))
        let request = SyncRequest(protocolVersion: "v1", capabilities: ["records-v1"], cursor: nil, mutations: [mutation], pageToken: nil)
        if rejectChild {
            do { _ = try await client.sync(request); Issue.record("current child revocation must remain terminal") }
            catch let failure as CredentialBoundAuthFailure {
                #expect(SessionRefresher.requiresFreshProof(failure.underlying))
                #expect(failure.credentialVersion == (try h.keychain.loadFamily(family)?.credentialVersion))
            }
        } else {
            let applied = try await client.sync(request)
            #expect(applied.highWater.sequence == 1)
            #expect(applyingServer.applied.map(\.mutationID) == [mutation.mutationID])
        }
        let bodies = await capture.bodies
        #expect(bodies.count == 2)
        #expect(bodies[0] == bodies[1])
        #expect(try JSONDecoder().decode(SyncRequest.self, from: bodies[1]).mutations[0].mutationID == mutation.mutationID)
        #expect(await capture.authorizations == ["Bearer access-token", "Bearer recovered-access"])
        #expect(await recovery.requests.count == 1)
    }

    @Test func acknowledgementNeverSendsAfterAccountGateCloses() async throws {
        let h = try RecoveryTestHarness(); try h.seed()
        let transport = RecoveryTestTransport()
        try await h.refresher(transport).refresh(accountKey: h.account, familyKey: h.family)
        try h.store.markAccountSignedOut(h.account.leaseKey)
        let ack = RefreshAcknowledgementCoordinator(store: h.store, keychain: h.keychain, leases: h.leases, endpoint: SessionFixture.endpoint, recovery: transport)
        await ack.retryPending(accountKey: h.account, familyKey: h.family)
        #expect(await transport.acknowledgements.isEmpty)
        #expect(try h.store.refreshRecoveryCheckpoint(for: h.account.leaseKey)?.phase == .ackPending)
    }

    @Test func ackFailureDoesNotBlockSyncAndPersistsBoundedRetry() async throws {
        let h = try RecoveryTestHarness(); try h.seed()
        let transport = RecoveryTestTransport()
        try await h.refresher(transport).refresh(accountKey: h.account, familyKey: h.family)
        await transport.configure(ackFails: true)
        var time = Date()
        var events: [RefreshRecoveryEvent] = []
        let ack = RefreshAcknowledgementCoordinator(store: h.store, keychain: h.keychain, leases: h.leases, endpoint: SessionFixture.endpoint, recovery: transport, now: { time }, diagnostics: { events.append($0) }, sleep: { _ in throw CancellationError() })
        for delay in [1, 2, 4, 8, 16, 32, 64, 128, 256, 300, 300] {
            await ack.retryPending(accountKey: h.account, familyKey: h.family)
            let checkpoint = try #require(try h.store.refreshRecoveryCheckpoint(for: h.account.leaseKey))
            let pending = try #require(checkpoint.acknowledgement)
            #expect(pending.nextRetryAt == time.addingTimeInterval(Double(delay)))
            #expect(pending.retryCount <= 10)
            #expect(events.last?.phase == .deferred)
            #expect(events.last?.retryCount == pending.retryCount)
            #expect(try h.store.accountAllowsOrdinarySync(h.account.leaseKey))
            time = pending.nextRetryAt
        }
        await transport.configure(ackFails: false)
        await ack.retryPending(accountKey: h.account, familyKey: h.family)
        #expect(try h.store.refreshRecoveryCheckpoint(for: h.account.leaseKey) == nil)
        #expect(try h.keychain.loadFamily(h.family)?.accessToken == "recovered-access")
        #expect(events.last?.phase == .acknowledged)
    }

    @Test func crashAfterKeychainResumesExactPreparedInstallWithoutParentRetry() async throws {
        let h = try RecoveryTestHarness(); try h.seed()
        let parent = try #require(try h.keychain.loadFamily(h.family))
        let time = Date()
        let token = try #require(try h.leases.acquire(scope: .account(h.account.leaseKey), ownerID: UUID(), now: time, ttl: 30))
        let pending = try h.store.beginRecoverableRefresh(for: h.account.leaseKey, recoveryEpoch: 0, credentialVersion: parent.credentialVersion, using: token, now: time)
        let binding = RefreshInstallBinding(key: h.account.leaseKey, recoveryEpoch: 0, attemptID: pending.attempt.attemptID, parentCredentialVersion: parent.credentialVersion, expectedCredentialVersion: UUID(), accessFingerprint: RefreshInstallBinding.fingerprint("child-access"), refreshFingerprint: RefreshInstallBinding.fingerprint("child-refresh"))
        try h.store.prepareRefreshInstall(binding, for: h.account.leaseKey, using: token, now: time)
        let child = SessionCredentials(accessToken: "child-access", refreshToken: "child-refresh", expiresAt: .distantFuture, recoveryEpoch: 0, deviceContinuity: parent.deviceContinuity, credentialVersion: binding.expectedCredentialVersion, installBinding: binding)
        try h.keychain.saveFamily(child, for: h.family)
        try h.leases.cancel(token)
        let reopened = try RecoveryTestHarness(directory: h.directory)
        let transport = RecoveryTestTransport()
        try await reopened.refresher(transport).refresh(accountKey: h.account, familyKey: h.family)
        #expect(await transport.requests.isEmpty)
        #expect(try reopened.keychain.loadFamily(h.family) == child)
        #expect(try reopened.store.refreshRecoveryCheckpoint(for: h.account.leaseKey)?.phase == .ackPending)
    }

    @Test func expiredRecoveredAccessRefreshesChild() async throws {
        let h = try RecoveryTestHarness(); try h.seed()
        let transport = RecoveryTestTransport(expiresAt: .distantPast)
        let renewer = FamilySessionRenewer(store: h.store, keychain: h.keychain, leases: h.leases, identity: transport, endpoint: SessionFixture.endpoint)
        try await renewer.renew(familyKey: h.family)
        #expect(try h.keychain.loadFamily(h.family)?.accessToken == "descendant-access")
        let requests = await transport.requests
        #expect(requests.count == 2)
        #expect(requests[0].0.refreshToken == "refresh-token")
        #expect(requests[1].0.refreshToken == "recovered-refresh")
        #expect(requests[0].2 != requests[1].2)
    }

    @Test func completedInstallRemainsVerifiableAfterAckButArbitraryReplacementDoesNot() async throws {
        let h = try RecoveryTestHarness(); try h.seed()
        let transport = RecoveryTestTransport()
        let parent = try #require(try h.keychain.loadFamily(h.family))
        try await h.refresher(transport).refresh(accountKey: h.account, familyKey: h.family)
        let ack = RefreshAcknowledgementCoordinator(store: h.store, keychain: h.keychain, leases: h.leases, endpoint: SessionFixture.endpoint, recovery: transport)
        await ack.retryPending(accountKey: h.account, familyKey: h.family)
        let renewer = FamilySessionRenewer(store: h.store, keychain: h.keychain, leases: h.leases, identity: transport, endpoint: SessionFixture.endpoint)
        #expect(try await renewer.credentialsForRetry(after: parent.credentialVersion, familyKey: h.family)?.accessToken == "recovered-access")
        let arbitrary = SessionCredentials(accessToken: "unproved", refreshToken: "unproved", expiresAt: .distantFuture, recoveryEpoch: 0, deviceContinuity: parent.deviceContinuity)
        try h.keychain.saveFamily(arbitrary, for: h.family)
        try h.store.markAccountAuthenticated(h.account.leaseKey, credentialVersion: arbitrary.credentialVersion)
        #expect(try await renewer.credentialsForRetry(after: parent.credentialVersion, familyKey: h.family) == nil)
    }

    @Test func validBoundLegacySessionUpgradesAtNextRenewal() async throws {
        let h = try RecoveryTestHarness(); try h.seed()
        let transport = RecoveryTestTransport()
        try await h.refresher(transport).refresh(accountKey: h.account, familyKey: h.family)
        let installed = try #require(try h.keychain.loadFamily(h.family))
        #expect(installed.accessToken == "recovered-access")
        #expect(installed.installBinding != nil)
        #expect(try h.store.refreshRecoveryCheckpoint(for: h.account.leaseKey)?.phase == .ackPending)
        #expect(await transport.legacyRequests == 0)
    }

    @Test func probeMismatchSendsNoCredentials() async throws {
        let h = try RecoveryTestHarness(); try h.seed()
        let transport = RecoveryTestTransport(); await transport.configure(epoch: 1)
        await #expect(throws: (any Error).self) { try await h.refresher(transport).refresh(accountKey: h.account, familyKey: h.family) }
        #expect(await transport.requests.isEmpty)
        #expect(await transport.legacyRequests == 0)
        #expect(try h.store.refreshRecoveryCheckpoint(for: h.account.leaseKey) == nil)
    }

    @Test func pendingRecoveryNeverFallsBackOnDowngrade() async throws {
        let h = try RecoveryTestHarness(); try h.seed()
        let transport = RecoveryTestTransport(lostReply: true)
        try? await h.refresher(transport).refresh(accountKey: h.account, familyKey: h.family)
        let original = try #require(try h.store.refreshRecoveryCheckpoint(for: h.account.leaseKey))
        await transport.configure(capability: false)
        let reopened = try RecoveryTestHarness(directory: h.directory)
        try? await reopened.refresher(transport).refresh(accountKey: h.account, familyKey: h.family)
        #expect(await transport.legacyRequests == 0)
        let requests = await transport.requests
        #expect(requests.allSatisfy { $0.2 == original.attempt.attemptID })
        #expect(try h.keychain.loadFamily(h.family)?.accessToken == "recovered-access")
    }
}

final class RecoveryAuthProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var handler: (@Sendable (URLRequest, Data) async throws -> (Int, Data))?
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        var body = request.httpBody ?? Data()
        if let stream = request.httpBodyStream {
            stream.open(); defer { stream.close() }
            var buffer = [UInt8](repeating: 0, count: 4096)
            while stream.hasBytesAvailable {
                let count = stream.read(&buffer, maxLength: buffer.count)
                if count <= 0 { break }
                body.append(contentsOf: buffer[0..<count])
            }
        }
        let bytes = body
        let request = self.request
        // `@Sendable`, so the captures are checked by conformance: this
        // double is `@unchecked Sendable`, and the URL loading system calls
        // it from its own threads. Left to region analysis, the capture of
        // `self` was refused or accepted depending on how the compiler
        // batched this file (SendingClosureRisksDataRace on fresh builds).
        Task { @Sendable in
            do {
                let (status, data) = try await RecoveryAuthProtocol.handler!(request, bytes)
                let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: nil, headerFields: nil)!
                client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
                client?.urlProtocol(self, didLoad: data)
                client?.urlProtocolDidFinishLoading(self)
            } catch { client?.urlProtocol(self, didFailWithError: error) }
        }
    }
    override func stopLoading() {}
}

actor RecoveryRequestCapture {
    var bodies: [Data] = []
    var authorizations: [String] = []
    func append(_ body: Data, _ authorization: String) -> Int {
        bodies.append(body); authorizations.append(authorization); return bodies.count
    }
}

@MainActor
final class RecoveryProcessProbe {
    let process = Process()
    let input = Pipe()
    let output = Pipe()
    let errors = Pipe()
    private let stderrCount = ProbeByteCount()
    private var stdoutBytes = 0
    struct Timeout: Error, CustomStringConvertible {
        let phase: String
        let exitStatus: Int32?
        let stdoutBytes: Int
        let stderrBytes: Int
        var description: String {
            "owned recovery probe timed out: phase=\(phase), exit=\(exitStatus.map(String.init) ?? "running"), stdoutBytes=\(stdoutBytes), stderrBytes=\(stderrBytes); output redacted"
        }
    }
    enum InvalidOutput: Error { case redacted }
    init(_ h: RecoveryTestHarness, now: Int, wait: Bool) throws {
        let package = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        process.executableURL = package.appending(path: ".build/debug/BabyStoreLeaseProbe")
        process.arguments = ["--action", "recovery-refresh", "--store", h.directory.appending(path: "store.sqlite").path,
                             "--lock", h.directory.appending(path: "coordination.lock").path, "--bootstrap", h.directory.appending(path: "bootstrap.lock").path,
                             "--ledger", h.directory.appending(path: "unused-ledger").path, "--now", String(now), "--wait", String(wait),
                             "--account", try JSONEncoder().encode(h.account).base64EncodedString(), "--family", try JSONEncoder().encode(h.family).base64EncodedString()]
        process.standardInput = input; process.standardOutput = output; process.standardError = errors
        let count = stderrCount
        errors.fileHandleForReading.readabilityHandler = { handle in
            let data = handle.availableData
            count.add(data.count)
            if data.isEmpty { handle.readabilityHandler = nil }
        }
        try process.run()
    }
    func next(timeout: TimeInterval = 10) throws -> [String: String] {
        let deadline = ProcessInfo.processInfo.systemUptime + timeout
        var data = Data()
        while data.count < 16_384 {
            let remaining = deadline - ProcessInfo.processInfo.systemUptime
            guard remaining > 0 else { throw timedOut("output") }
            var descriptor = pollfd(fd: output.fileHandleForReading.fileDescriptor, events: Int16(POLLIN), revents: 0)
            let ready = poll(&descriptor, 1, Int32(min(remaining * 1000 + 1, 10_000)))
            if ready == 0 { throw timedOut("output") }
            if ready < 0 {
                if errno == EINTR { continue }
                throw InvalidOutput.redacted
            }
            guard let byte = try output.fileHandleForReading.read(upToCount: 1), !byte.isEmpty else { break }
            stdoutBytes += byte.count
            if byte == Data([10]) { break }
            data.append(byte)
        }
        guard let value = try? JSONDecoder().decode([String: String].self, from: data),
              value.allSatisfy({ ["status", "result"].contains($0.key) && ["awaiting", "held", "installed", "stale"].contains($0.value) }) else { throw InvalidOutput.redacted }
        return value
    }
    func resume() { input.fileHandleForWriting.write(Data("resume\n".utf8)) }
    func waitForExit(timeout: TimeInterval = 10) throws {
        let deadline = ProcessInfo.processInfo.systemUptime + timeout
        while process.isRunning {
            guard ProcessInfo.processInfo.systemUptime < deadline else { throw timedOut("exit") }
            Thread.sleep(forTimeInterval: 0.01)
        }
        #expect(process.terminationStatus == 0)
    }
    private func timedOut(_ phase: String) -> Timeout {
        stop()
        return Timeout(phase: phase, exitStatus: process.isRunning ? nil : process.terminationStatus, stdoutBytes: stdoutBytes, stderrBytes: stderrCount.value)
    }
    func stop() {
        if process.isRunning {
            process.terminate()
            let graceful = ProcessInfo.processInfo.systemUptime + 0.5
            while process.isRunning && ProcessInfo.processInfo.systemUptime < graceful { Thread.sleep(forTimeInterval: 0.01) }
            if process.isRunning {
                // Only this Process's still-live owned PID; no name/group kill.
                _ = Darwin.kill(process.processIdentifier, SIGKILL)
                let forced = ProcessInfo.processInfo.systemUptime + 0.5
                while process.isRunning && ProcessInfo.processInfo.systemUptime < forced { Thread.sleep(forTimeInterval: 0.01) }
            }
        }
        errors.fileHandleForReading.readabilityHandler = nil
    }
}

private final class ProbeByteCount: @unchecked Sendable {
    private let lock = NSLock()
    private var count = 0
    func add(_ amount: Int) { lock.withLock { count += amount } }
    var value: Int { lock.withLock { count } }
}
