import BabyStore
import BabySync
import CryptoKit
import Foundation

/// Synthetic process fixture only. This executable is not an app dependency.
/// All credential bytes are generated test values under the supplied temp dir.
@MainActor
enum RecoveryProbe {
    static func run(store: BabyStore, leases: AppGroupFencedLease, directory: URL, now: Date) async throws {
        guard let accountBytes = Data(base64Encoded: try argument("account")),
              let familyBytes = Data(base64Encoded: try argument("family")) else { throw ProbeError.badArgument("identity") }
        let account = try JSONDecoder().decode(AccountSessionKey.self, from: accountBytes)
        let family = try JSONDecoder().decode(FamilySessionKey.self, from: familyBytes)
        let endpoint = ConfirmedEndpoint(origin: try ServerOrigin(validating: account.canonicalOrigin), serverInstanceID: account.serverInstanceID)
        let keychain = KeychainSessionStore(backend: RecoveryProbeBackend(directory: directory.appending(path: "keychain")))
        let transport = RecoveryProbeTransport(directory: directory, account: account, wait: try argument("wait") == "true")
        let refresher = SessionRefresher(store: store, keychain: keychain, leases: leases, endpoint: endpoint, recovery: transport, now: { now })
        do {
            try await refresher.refresh(accountKey: account, familyKey: family)
            emit(["result": "installed"])
        } catch SessionRefresher.RefreshError.lockUnavailable { emit(["result": "held"]) }
        catch FencedLeaseError.staleOwner { emit(["result": "stale"]) }
        catch FencedLeaseError.expired { emit(["result": "stale"]) }
    }
}

private struct RecoveryProbeBackend: KeychainBackend {
    let directory: URL
    func path(_ service: String, _ account: String) -> URL {
        let name = SHA256.hash(data: Data((service + "|" + account).utf8)).map { String(format: "%02x", $0) }.joined()
        return directory.appending(path: name)
    }
    func read(service: String, account: String) throws -> Data? {
        let url = path(service, account)
        return FileManager.default.fileExists(atPath: url.path) ? try Data(contentsOf: url) : nil
    }
    func write(_ data: Data, service: String, account: String) throws { try data.write(to: path(service, account), options: .atomic) }
    func delete(service: String, account: String) throws { try FileManager.default.removeItem(at: path(service, account)) }
}

private actor RecoveryProbeTransport: SessionRecoveryTransport {
    struct Receipt: Codable {
        let parent: SessionCredentials
        let accountID: UUID
        let attemptID: UUID
        let child: SessionCredentials
    }
    struct Request: Codable {
        let parent: SessionCredentials
        let accountID: UUID
        let attemptID: UUID
    }
    let directory: URL
    let account: AccountSessionKey
    let wait: Bool
    init(directory: URL, account: AccountSessionKey, wait: Bool) {
        self.directory = directory; self.account = account; self.wait = wait
    }
    func serverInfo(_ origin: ServerOrigin) async throws -> ServerInfo {
        ServerInfo(serverInstanceID: account.serverInstanceID, canonicalOrigin: account.canonicalOrigin, recoveryEpoch: account.recoveryEpoch, capabilities: ["refresh-recovery-v1"], appleAuthorizationMode: "self_hosted", maxItemsPerPage: 100, maxEncodedBytes: 10000, sourceVersion: "synthetic-process", sourceURL: "")
    }
    func refresh(refreshToken: String, at endpoint: ConfirmedEndpoint) async throws -> SessionCredentials { throw ProbeError.badArgument("legacy-fallback") }
    func acknowledgeRefresh(attemptID: UUID, credentials: SessionCredentials, at endpoint: ConfirmedEndpoint) async throws { throw ProbeError.badArgument("unexpected-ack") }
    func refreshRecoverable(parent: SessionCredentials, expectedAccountID: UUID, attemptID: UUID, at endpoint: ConfirmedEndpoint) async throws -> RecoverableRefreshResult {
        guard expectedAccountID == account.accountID else { throw ProbeError.badArgument("account") }
        let encoder = JSONEncoder(); encoder.outputFormatting = [.sortedKeys]
        try encoder.encode(Request(parent: parent, accountID: expectedAccountID, attemptID: attemptID)).write(to: directory.appending(path: "request-\(UUID()).json"), options: .atomic)
        let url = directory.appending(path: "receipt.json")
        let receipt: Receipt
        if FileManager.default.fileExists(atPath: url.path) {
            receipt = try JSONDecoder().decode(Receipt.self, from: Data(contentsOf: url))
            guard receipt.parent == parent, receipt.accountID == expectedAccountID, receipt.attemptID == attemptID else { throw ProbeError.badArgument("divergent-replay") }
        } else {
            let child = SessionCredentials(accessToken: "process-child-access", refreshToken: "process-child-refresh", expiresAt: .distantFuture, recoveryEpoch: parent.recoveryEpoch, deviceContinuity: parent.deviceContinuity)
            receipt = Receipt(parent: parent, accountID: expectedAccountID, attemptID: attemptID, child: child)
            try encoder.encode(receipt).write(to: url, options: .atomic)
        }
        if wait {
            FileHandle.standardOutput.write(Data("{\"status\":\"awaiting\"}\n".utf8))
            _ = readLine(strippingNewline: true)
        }
        return RecoverableRefreshResult(credentials: receipt.child, attemptID: attemptID, accountID: expectedAccountID, reauthenticateAt: .distantFuture)
    }
}
