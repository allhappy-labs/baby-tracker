import BabyCore
import BabyProtocol
import BabyStore
import Foundation

public enum SyncReason: Sendable, Equatable {
    case launch, foreground, localWrite, liveHint, backgroundPush, extensionRequest
}

/// The P2 sync endpoints.
public protocol FamilySyncTransport: Sendable {
    func sync(_ request: SyncRequest) async throws -> SyncResponse
    func snapshot(pageToken: String?) async throws -> SnapshotResponse
}

public struct SyncRequest: Codable, Sendable, Equatable {
    public let protocolVersion: String
    public let capabilities: [String]
    public let cursor: Cursor?
    public let mutations: [WireMutation]
    public let pageToken: String?

    enum CodingKeys: String, CodingKey {
        case protocolVersion = "protocol_version", capabilities, cursor, mutations, pageToken = "page_token"
    }

    public init(protocolVersion: String, capabilities: [String], cursor: Cursor?, mutations: [WireMutation], pageToken: String?) {
        self.protocolVersion = protocolVersion
        self.capabilities = capabilities
        self.cursor = cursor
        self.mutations = mutations
        self.pageToken = pageToken
    }
}

/// Drives one sync attempt from the store's outbox to the server and back.
///
/// Its job is order and permission, not transport. Two rules dominate:
/// every request is gated on the durable account state, so a signed-out or
/// read-only account cannot sync from any process; and a paged response is
/// applied to completion before its cursor is believed.
@MainActor
public final class SyncCoordinator {
    /// What a sync request advertises: exactly the route's set, never a
    /// superset. The server compares the header against the set the batch
    /// needs and refuses anything else with `unsupported_capability`
    /// (`ValidateRouteCapabilities`, `Server/internal/protocol/sync.go`) —
    /// `records-v1`, plus `timer-commands-v1` when a timer mutation rides
    /// along. `live-hints-v1` is the live socket's, not this route's.
    public static func capabilities(for mutations: [WireMutation]) -> [String] {
        mutations.contains { $0.entityKind == .timer } ? ["records-v1", "timer-commands-v1"] : ["records-v1"]
    }

    public private(set) var currentIssue: SyncIssue?
    /// The network was unreachable. Distinct from an issue: nothing is wrong
    /// with the data and nothing is asked of the user.
    public private(set) var isOffline = false
    public private(set) var isSyncing = false
    /// Whether the last attempt got all the way through. Distinct from "no
    /// issue and not offline": a transient server answer backs off without
    /// either, and a caller must not report that as up to date.
    public private(set) var lastAttemptSucceeded = false
    public private(set) var lastReason: SyncReason?
    /// Shrinks when the server says the batch was too big, and is remembered
    /// so the next attempt does not walk into the same refusal.
    public private(set) var batchLimit = 100
    /// The batch size in force before a refused request started halving it,
    /// restored once the refused entry is quarantined.
    private var limitBeforeRefusal: Int?

    private let store: BabyStore
    private let transport: any FamilySyncTransport
    private let accountKey: AccountSessionKey
    private let policy: SyncRetryPolicy
    private let onRevoked: (RealmID) async -> Void
    private let onFreshProofRequired: () async -> Void
    private let onRefreshRequired: () async throws -> Void
    private let diagnosticSession: SyncDiagnosticSession?
    private let onPhase: (@MainActor (SyncOperationalStatus.Phase) -> Void)?

    func recordHintReceived() { diagnosticSession?.record(.hintReceived) }
    func stopDiagnostics() { diagnosticSession?.stop() }

    public init(
        store: BabyStore,
        transport: any FamilySyncTransport,
        accountKey: AccountSessionKey,
        policy: SyncRetryPolicy = SyncRetryPolicy(),
        onRevoked: @escaping (RealmID) async -> Void = { _ in },
        onFreshProofRequired: @escaping () async -> Void = {},
        onRefreshRequired: @escaping () async throws -> Void = {},
        diagnostics: (any SyncDiagnosticSink)? = nil,
        onPhase: (@MainActor (SyncOperationalStatus.Phase) -> Void)? = nil
    ) {
        self.store = store
        self.transport = transport
        self.accountKey = accountKey
        self.policy = policy
        self.onRevoked = onRevoked
        self.onFreshProofRequired = onFreshProofRequired
        self.onRefreshRequired = onRefreshRequired
        self.diagnosticSession = diagnostics.map(SyncDiagnosticSession.init)
        self.onPhase = onPhase
        if let diagnosticSession {
            store.onSyncMilestone = { [weak diagnosticSession] in diagnosticSession?.storeMilestone($0) }
        } else {
            store.onSyncMilestone = nil
        }
    }

    /// One attempt. Never throws: sync failing is an ordinary condition and
    /// the caller — a launch, a push, a widget — has nothing useful to do with
    /// an error except what this already does.
    public func syncOnce(reason: SyncReason) async {
        lastReason = reason
        // Cleared before any early return: an attempt that never ran must not
        // inherit the previous attempt's success.
        lastAttemptSucceeded = false
        guard !isSyncing else { return }

        // The gate, consulted before every request from every process. A
        // per-process flag would let an extension that never saw the sign-out
        // carry on talking to the server.
        guard (try? store.accountAllowsOrdinarySync(accountKey.leaseKey)) == true else { return }
        guard case .shared = store.activeRealm else { return }

        isSyncing = true
        defer { isSyncing = false }
        diagnosticSession?.beginAttempt()
        defer { diagnosticSession?.endAttempt() }

        do {
            try await drainPages()
            currentIssue = nil
            isOffline = false
            lastAttemptSucceeded = true
        } catch let failure as CredentialBoundAuthFailure {
            // A response to an older install cannot revoke a newer sign-in
            // or clear an existing terminal issue. Retry itself is owned by
            // the transport's verified snapshot callback.
            guard (try? store.accountCredentialIsCurrent(failure.credentialVersion, for: accountKey.leaseKey)) == true else { return }
            isOffline = false
            if failure.requiresFreshProof {
                await handle(.reauthenticationRequired)
            } else if case .status(_, let code) = failure.underlying {
                await handle(SyncErrorCode(rawValue: code ?? "") ?? .serviceUnavailable)
            }
        } catch let error as SyncErrorCode {
            // The server answered, so the network is not the problem.
            isOffline = false
            await handle(error)
        } catch {
            // A transport failure is not something to alarm a parent with:
            // the entries are safe and the next attempt will carry them.
            // Transient Keychain/lease/network failures provide no evidence
            // that an existing fresh-proof requirement has been resolved.
            if currentIssue.flatMap({ SyncErrorCode(rawValue: $0.code) })?.action != .freshProofRequired {
                currentIssue = nil
            }
            isOffline = true
        }
    }

    /// Sends the outbox and applies every page of the response before the
    /// cursor is believed.
    private func drainPages() async throws {
        var pageToken: String?
        repeat {
            let mutations = try store.nextDispatchBatch(limit: batchLimit)
            if !mutations.isEmpty {
                try store.markDispatched(mutationIDs: mutations.map(\.id))
                diagnosticSession?.record(.enqueue)
                onPhase?(.enqueue)
            }
            let sent = pageToken == nil ? mutations : []
            let request = SyncRequest(
                protocolVersion: "v1",
                capabilities: Self.capabilities(for: sent),
                cursor: try store.syncCursor(),
                mutations: sent,
                pageToken: pageToken
            )
            diagnosticSession?.record(.requestStart)
            onPhase?(.request)
            let response: SyncResponse
            do {
                response = try await transport.sync(request)
                diagnosticSession?.record(.requestComplete)
            } catch {
                diagnosticSession?.record(.requestComplete, status: diagnosticStatus(for: error))
                throw error
            }
            diagnosticSession?.record(.peerPull)
            onPhase?(.peerApply)
            try store.applySyncPage(response)
            pageToken = response.nextPageToken
        } while pageToken != nil
    }

    private func diagnosticStatus(for error: any Error) -> SyncDiagnosticStatus {
        if let code = error as? SyncErrorCode {
            switch code.action {
            case .freshProofRequired, .revokeAndPurge, .surfaceIssue: return .refused
            case .retryAfterBackoff, .refreshSession, .takeSnapshot, .quarantineMutation: return .retry
            }
        }
        if let failure = error as? CredentialBoundAuthFailure, failure.requiresFreshProof { return .refused }
        return .retry
    }

    /// Replaces the local base wholesale. Used when the cursor is meaningless.
    public func takeSnapshot() async throws {
        var pageToken: String?
        var started = false
        repeat {
            let page = try await transport.snapshot(pageToken: pageToken)
            if !started {
                store.beginSnapshot(snapshotID: page.snapshotID, highWater: page.highWater)
                started = true
            }
            try store.applySnapshotPage(page)
            pageToken = page.nextPageToken
        } while pageToken != nil
        // Only a snapshot seen through to its last page is worth committing.
        if started, let snapshotID = store.pendingSnapshotID {
            try store.finishSnapshot(snapshotID: snapshotID)
        }
    }

    /// The one place a server code turns into an action.
    private func handle(_ code: SyncErrorCode) async {
        switch code.action {
        case .retryAfterBackoff:
            if code.suggestsSmallerBatch { batchLimit = policy.nextBatchSize(after: batchLimit) }
            currentIssue = nil
        case .refreshSession:
            do {
                try await onRefreshRequired()
            } catch where SessionRefresher.requiresFreshProof(error) {
                // A terminally refused renewal needs fresh proof: only a
                // sign-in helps, so ask for one.
                await onFreshProofRequired()
                currentIssue = issue(for: .reauthenticationRequired)
            } catch is URLError {
                currentIssue = nil
                isOffline = true
            } catch {
                // Another process holding the lease, a server hiccup: the
                // next attempt renews. Not a reason to ask for Apple proof.
                currentIssue = nil
            }
        case .freshProofRequired:
            await onFreshProofRequired()
            currentIssue = issue(for: code)
        case .takeSnapshot:
            // A snapshot pages current rows and is not frozen (server Phase 2
            // snapshot ruling): a write committed while it paged can be half
            // visible — a stopped timer gone, its record not yet. One pull
            // from the snapshot's high water settles the device now rather
            // than at the next poll. A refusal of that pull is the next
            // attempt's to handle.
            do {
                try await takeSnapshot()
                try await drainPages()
            } catch {}
        case .revokeAndPurge:
            let realm = store.activeRealm
            try? store.markAccountSignedOut(accountKey.leaseKey)
            try? store.purgeSharedRealm(realm)
            await onRevoked(realm)
            currentIssue = issue(for: code, realm: realm)
        case .quarantineMutation:
            // The server refuses the whole request at decode
            // (`Server/internal/httpapi/sync.go`), so the refusal names the
            // batch, not an entry. Quarantining the head of it set good
            // entries aside until the bad one happened to reach the front.
            // Halving until the batch is one entry pins the refusal on the
            // entry that earned it; the good ones go through meanwhile.
            if batchLimit > 1 {
                limitBeforeRefusal = limitBeforeRefusal ?? batchLimit
                batchLimit = policy.nextBatchSize(after: batchLimit)
            } else {
                try? store.quarantineNextDispatch(limit: batchLimit)
                // The culprit is out of the way; the rest need not crawl.
                if let limitBeforeRefusal { batchLimit = limitBeforeRefusal }
                limitBeforeRefusal = nil
            }
            currentIssue = nil
        case .surfaceIssue:
            currentIssue = issue(for: code)
        }
    }

    private func issue(for code: SyncErrorCode, realm: RealmID? = nil) -> SyncIssue? {
        SyncIssueStore.issue(for: code, realm: realm ?? store.activeRealm)
    }

}
