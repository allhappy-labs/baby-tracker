import BabyCore
import BabyProtocol
import BabyStore
@testable import BabySync
import CryptoKit
import Darwin
import Foundation
import Testing

// All files belong to the disposable runner. Token expectations never enter
// SQLite or logs. Each invocation reopens the same store and protected files.
private func privateWrite<T: Encodable>(_ value: T, to url: URL) throws {
    try JSONEncoder().encode(value).write(to: url, options: .atomic)
    try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
}

private func crash(_ boundary: String) -> Never {
    let message = "refresh-recovery crash: \(boundary)\n"
    FileHandle.standardOutput.write(Data(message.utf8))
    _exit(86)
}

final class PersistentRecoveryKeychain: KeychainBackend, @unchecked Sendable {
    let directory: URL
    private let lock = NSLock()
    private var boundary: String?
    init(_ directory: URL) throws {
        self.directory = directory
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
    }
    func arm(_ boundary: String) { lock.withLock { self.boundary = boundary } }
    private func path(_ service: String, _ account: String) -> URL {
        directory.appending(path: SHA256.hash(data: Data((service + "|" + account).utf8)).map { String(format: "%02x", $0) }.joined())
    }
    func read(service: String, account: String) throws -> Data? {
        let url = path(service, account)
        return FileManager.default.fileExists(atPath: url.path) ? try Data(contentsOf: url) : nil
    }
    func write(_ data: Data, service: String, account: String) throws {
        let point = lock.withLock { boundary }
        if point != nil, let credentials = try? JSONDecoder().decode(SessionCredentials.self, from: data), credentials.installBinding != nil {
            try privateWrite(credentials, to: directory.deletingLastPathComponent().appending(path: "install.json"))
            if point == "before_keychain" { crash("before_keychain") }
        }
        let url = path(service, account)
        try data.write(to: url, options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
        if point == "after_keychain", (try? JSONDecoder().decode(SessionCredentials.self, from: data).installBinding) != nil { crash("after_keychain") }
    }
    func delete(service: String, account: String) throws {
        let url = path(service, account)
        if FileManager.default.fileExists(atPath: url.path) { try FileManager.default.removeItem(at: url) }
    }
}

private struct RecoveryProcessState: Codable {
    let owner: FamilySessionKey
    let caregiver: FamilySessionKey
    let babyID: UUID
    let ownerSubject: String
    let caregiverSubject: String
    let ownerSuite: String
    let caregiverSuite: String
    let parent: SessionCredentials
    let boundary: String
}
private struct DecodedExpectation: Codable {
    let credentials: SessionCredentials
    let attemptID: UUID
    let deadline: Date
}

private actor DecodeCrashTransport: SessionRecoveryTransport {
    let identity: HTTPIdentityClient
    let directory: URL
    let boundary: String
    init(identity: HTTPIdentityClient, directory: URL, boundary: String) {
        self.identity = identity; self.directory = directory; self.boundary = boundary
    }
    func serverInfo(_ origin: ServerOrigin) async throws -> ServerInfo { try await identity.serverInfo(origin) }
    func refresh(refreshToken: String, at endpoint: ConfirmedEndpoint) async throws -> SessionCredentials {
        throw E2EFailure("recovery process attempted legacy fallback")
    }
    func refreshRecoverable(parent: SessionCredentials, expectedAccountID: UUID, attemptID: UUID, at endpoint: ConfirmedEndpoint) async throws -> RecoverableRefreshResult {
        let result = try await identity.refreshRecoverable(parent: parent, expectedAccountID: expectedAccountID, attemptID: attemptID, at: endpoint)
        try privateWrite(DecodedExpectation(credentials: result.credentials, attemptID: attemptID, deadline: result.reauthenticateAt), to: directory.appending(path: "decoded.json"))
        if boundary == "after_decode" { crash("after_decode") }
        return result
    }
    func acknowledgeRefresh(attemptID: UUID, credentials: SessionCredentials, at endpoint: ConfirmedEndpoint) async throws {
        try await identity.acknowledgeRefresh(attemptID: attemptID, credentials: credentials, at: endpoint)
    }
}

@MainActor
@Suite("Refresh recovery through separate processes", .serialized)
struct RefreshRecoveryE2ETests {
    @Test(.enabled(if: ProcessInfo.processInfo.environment["BABY_TRACKER_RECOVERY_PHASE"] != nil))
    func recoverableRefreshAcrossServerAndClientRestart() async throws {
        let variables = ProcessInfo.processInfo.environment
        let phase = try #require(variables["BABY_TRACKER_RECOVERY_PHASE"])
        let environment = try #require(E2EEnvironment.current(), "runner must supply real server state and pinned TLS")
        let directory = URL(fileURLWithPath: try #require(variables["BABY_TRACKER_RECOVERY_DIRECTORY"]))
        let boundary = try #require(variables["BABY_TRACKER_RECOVERY_BOUNDARY"])
        try #require(["after_decode", "before_keychain", "after_keychain", "after_completion", "after_ack"].contains(boundary))
        let live = try LiveServer(environment)
        let clock = TestDateProvider(Date(timeIntervalSince1970: floor(Date().timeIntervalSince1970) - 3600))
        let stateURL = directory.appending(path: "process.json")
        if phase == "bootstrap" {
            let ownerBackend = try PersistentRecoveryKeychain(directory.appending(path: "owner-keychain"))
            let caregiverBackend = try PersistentRecoveryKeychain(directory.appending(path: "caregiver-keychain"))
            let ownerSubject = UUID().uuidString.lowercased(), caregiverSubject = UUID().uuidString.lowercased()
            let ownerSuite = "recovery-owner-\(UUID())", caregiverSuite = "recovery-caregiver-\(UUID())"
            let owner = try Phone(name: "recovery-owner", live: live, clock: clock, storeURL: directory.appending(path: "owner.store"), keychain: ownerBackend, suiteName: ownerSuite, appleSubject: ownerSubject)
            let caregiver = try Phone(name: "recovery-caregiver", live: live, clock: clock, storeURL: directory.appending(path: "caregiver.store"), keychain: caregiverBackend, suiteName: caregiverSuite, appleSubject: caregiverSubject)
            let baby = try owner.store.addBaby(name: "Recovery fixture", birthDate: clock.now.addingTimeInterval(-86400), referenceSex: .girls, expectedDueDate: nil)
            _ = try await owner.signIn(displayName: "Recovery owner", live: live)
            try await owner.model.createFamily(name: "Recovery fixture")
            let invitation = try await owner.model.createInvitation()
            _ = try await caregiver.signIn(displayName: "Recovery caregiver", live: live)
            try await caregiver.model.acceptInvitation(InvitationParser.parse(invitation.shareURL.absoluteString))
            let key = try owner.familyKey(), caregiverKey = try caregiver.familyKey()
            await owner.model.engine?.stop(); await caregiver.model.engine?.stop()
            let parent = try owner.credentials(key)
            try privateWrite(RecoveryProcessState(owner: key, caregiver: caregiverKey, babyID: baby.id, ownerSubject: ownerSubject, caregiverSubject: caregiverSubject, ownerSuite: ownerSuite, caregiverSuite: caregiverSuite, parent: parent, boundary: boundary), to: stateURL)
            owner.defaults.synchronize(); caregiver.defaults.synchronize()
            ownerBackend.arm(boundary)
            let keychain = KeychainSessionStore(backend: ownerBackend)
            let leases = AppGroupFencedLease(container: owner.store.container, lockURL: directory.appending(path: "owner.store.coordination.lock"))
            let transport = DecodeCrashTransport(identity: live.identityClient(), directory: directory, boundary: boundary)
            let refresher = SessionRefresher(store: owner.store, keychain: keychain, leases: leases, endpoint: live.endpoint, recovery: transport)
            try await refresher.refresh(accountKey: key.accountSessionKey, familyKey: key)
            if boundary == "after_completion" { crash(boundary) }
            if boundary == "after_ack" {
                let installed = try owner.credentials(key)
                let checkpoint = try #require(try owner.store.refreshRecoveryCheckpoint(for: key.accountSessionKey.leaseKey))
                try await live.identityClient().acknowledgeRefresh(attemptID: checkpoint.attempt.attemptID, credentials: installed, at: live.endpoint)
                crash(boundary)
            }
            throw E2EFailure("requested process crash boundary was not reached")
        }
        try #require(phase == "verify", "unknown recovery process phase")
        let state = try JSONDecoder().decode(RecoveryProcessState.self, from: Data(contentsOf: stateURL))
        try #require(state.boundary == boundary)
        let owner = try Phone(name: "recovery-owner", live: live, clock: clock, storeURL: directory.appending(path: "owner.store"), keychain: PersistentRecoveryKeychain(directory.appending(path: "owner-keychain")), suiteName: state.ownerSuite, appleSubject: state.ownerSubject)
        let caregiver = try Phone(name: "recovery-caregiver", live: live, clock: clock, storeURL: directory.appending(path: "caregiver.store"), keychain: PersistentRecoveryKeychain(directory.appending(path: "caregiver-keychain")), suiteName: state.caregiverSuite, appleSubject: state.caregiverSubject)
        defer { owner.forgetDefaults(); caregiver.forgetDefaults() }
        let decoded = try JSONDecoder().decode(DecodedExpectation.self, from: Data(contentsOf: directory.appending(path: "decoded.json")))
        let before = try #require(try owner.store.refreshRecoveryCheckpoint(for: state.owner.accountSessionKey.leaseKey))
        try #require(before.attempt.attemptID == decoded.attemptID)
        let expectedPhase: RefreshRecoveryCheckpoint.Phase = boundary == "after_decode" ? .awaitingReply : (["before_keychain", "after_keychain"].contains(boundary) ? .installPrepared : .ackPending)
        try #require(before.phase == expectedPhase)
        let leases = AppGroupFencedLease(container: owner.store.container, lockURL: directory.appending(path: "owner.store.coordination.lock"))
        let renewer = FamilySessionRenewer(store: owner.store, keychain: KeychainSessionStore(backend: owner.keychain), leases: leases, identity: live.identityClient(), endpoint: live.endpoint)
        let installed = try #require(try await renewer.credentialsForRetry(after: state.parent.credentialVersion, familyKey: state.owner))
        guard installed.accessToken == decoded.credentials.accessToken, installed.refreshToken == decoded.credentials.refreshToken else {
            throw E2EFailure("recovery changed exact child credentials")
        }
        try #require(installed.installBinding?.attemptID == decoded.attemptID)
        if boundary != "after_decode" {
            let prepared = try JSONDecoder().decode(SessionCredentials.self, from: Data(contentsOf: directory.appending(path: "install.json")))
            try #require(installed.credentialVersion == prepared.credentialVersion && installed.installBinding == prepared.installBinding)
        }
        if boundary != "after_ack" {
            let replay = try await live.identityClient().refreshRecoverable(parent: state.parent, expectedAccountID: state.owner.accountID, attemptID: decoded.attemptID, at: live.endpoint)
            guard replay.credentials.accessToken == installed.accessToken, replay.credentials.refreshToken == installed.refreshToken,
                  replay.reauthenticateAt == decoded.deadline else { throw E2EFailure("replay changed exact child or deadline") }
        }
        // Retrying acknowledgement includes an absent receipt after a lost ack
        // response. Both calls use independently authenticated current authority.
        for _ in 0..<2 { try await live.identityClient().acknowledgeRefresh(attemptID: decoded.attemptID, credentials: installed, at: live.endpoint) }
        owner.model.restoreSession(); caregiver.model.restoreSession()
        await owner.model.startSyncing(); await caregiver.model.startSyncing()
        owner.model.sceneEnteredBackground(); caregiver.model.sceneEnteredBackground()
        let record = try owner.store.logDiaper(.mixed, for: state.babyID, consistency: "soft")
        try await owner.sync(); try await caregiver.sync()
        let records = try caregiver.history().diapers.filter { $0.id == record.id }
        try #require(records.count == 1 && records[0].contents == record.contents && records[0].consistency == record.consistency)
        let versionProof = boundary == "after_decode" ? "not_yet_prepared" : "true"
        print("refresh-recovery proof: completed boundary=\(boundary) exact_child=true exact_prepared_version=\(versionProof) ordinary_sync=exactly_once")
    }
}
