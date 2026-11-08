import Foundation
import SwiftData
import Testing
@testable import BabyStore
@testable import BabyCore

@MainActor
@Suite("Durable refresh installation checkpoints")
struct RefreshRecoveryStoreTests {
    private let now = Date(timeIntervalSince1970: 1_700_000_000)

    private func prepared() throws -> (LeaseTestHarness, FencedLeaseToken, RefreshInstallBinding) {
        let h = try LeaseTestHarness()
        let queued = SyncOutboxEntry(id: UUID(uuidString: "66666666-6666-4666-8666-666666666666")!, realm: .local, entityID: UUID(), entityKind: .baby, operation: .create, payload: Data("queued-fixture".utf8), sequence: 41, createdAt: now)
        queued.payloadHash = Data([205,242,73,14,221,103,225,91,70,143,108,165,141,250,227,178,212,60,36,0,239,119,211,130,122,167,200,58,67,103,114,103])
        queued.deliveryState = .inFlightOrUncertain
        h.store.context.insert(queued)
        try h.store.context.save()
        let parent = UUID()
        try h.store.markAccountAuthenticated(h.accountKey, credentialVersion: parent)
        let token = try #require(try h.leases.acquire(scope: .account(h.accountKey), ownerID: h.ownerA, now: now, ttl: 10))
        let checkpoint = try h.store.beginRecoverableRefresh(for: h.accountKey, recoveryEpoch: 3, credentialVersion: parent, using: token, now: now)
        let binding = RefreshInstallBinding(key: h.accountKey, recoveryEpoch: 3, attemptID: checkpoint.attempt.attemptID, parentCredentialVersion: parent, expectedCredentialVersion: UUID(), accessFingerprint: String(repeating: "a", count: 64), refreshFingerprint: String(repeating: "b", count: 64))
        try h.store.prepareRefreshInstall(binding, for: h.accountKey, using: token, now: now)
        return (h, token, binding)
    }

    private func assertQueuedDataUnchanged(_ h: LeaseTestHarness) throws {
        let rows = try h.reopenStore().context.fetch(FetchDescriptor<SyncOutboxEntry>())
        #expect(rows.count == 1)
        let row = try #require(rows.first)
        #expect(row.id.uuidString.lowercased() == "66666666-6666-4666-8666-666666666666")
        #expect(row.payload == Data("queued-fixture".utf8))
        #expect(row.payloadHash.map { String(format: "%02x", $0) }.joined() == "cdf2490edd67e15b468f6ca58dfae3b2d43c2400ef77d3827aa7c83a43677267")
        #expect(row.sequence == 41)
        #expect(row.deliveryState == .inFlightOrUncertain)
    }

    @Test func successorRetainsPredecessorUntilInstalledAndLateAckCannotClearIt() throws {
        let (h, token, binding) = try prepared()
        try h.store.completeRefreshInstall(binding, for: h.accountKey, using: token, now: now) {}
        let successor = try h.store.beginRecoverableRefresh(for: h.accountKey, recoveryEpoch: 3, credentialVersion: binding.expectedCredentialVersion, supersedingBinding: binding, using: token, now: now)
        #expect(successor.predecessorAcknowledgement?.binding == binding)
        #expect(try h.reopenStore().refreshRecoveryCheckpoint(for: h.accountKey)?.predecessorAcknowledgement?.binding == binding)
        let next = RefreshInstallBinding(key: h.accountKey, recoveryEpoch: 3, attemptID: successor.attempt.attemptID, parentCredentialVersion: binding.expectedCredentialVersion, expectedCredentialVersion: UUID(), accessFingerprint: String(repeating: "c", count: 64), refreshFingerprint: String(repeating: "d", count: 64))
        try h.store.prepareRefreshInstall(next, for: h.accountKey, using: token, now: now)
        try h.store.completeRefreshInstall(next, for: h.accountKey, using: token, now: now) {}
        try h.store.updateRefreshAcknowledgement(nil, observing: binding, for: h.accountKey, using: token, now: now)
        let installed = try #require(try h.reopenStore().refreshRecoveryCheckpoint(for: h.accountKey))
        #expect(installed.installBinding == next)
        #expect(installed.acknowledgement?.binding == next)
        #expect(installed.predecessorAcknowledgement == nil)
        try assertQueuedDataUnchanged(h)
    }

    @Test func freshSignInInstallsWithinResetAndFailedKeychainRetainsCheckpoint() throws {
        enum Locked: Error { case unavailable }
        let (h, token, binding) = try prepared()
        #expect(throws: Locked.unavailable) {
            try h.store.markAccountSignedIn(h.accountKey, credentialVersion: UUID()) { throw Locked.unavailable }
        }
        #expect(try h.reopenStore().refreshRecoveryCheckpoint(for: h.accountKey)?.installBinding == binding)
        #expect(try !h.store.accountAllowsOrdinarySync(h.accountKey))
        let replacement = UUID()
        var saved = false
        try h.store.markAccountSignedIn(h.accountKey, credentialVersion: replacement) { saved = true }
        #expect(saved)
        #expect(try h.reopenStore().refreshRecoveryCheckpoint(for: h.accountKey) == nil)
        #expect(throws: (any Error).self) {
            try h.store.completeRefreshInstall(binding, for: h.accountKey, using: token, now: now) { Issue.record("stale install won") }
        }
        try assertQueuedDataUnchanged(h)
    }

    @Test func cachedLegacyResponseCannotOverwriteFreshSignIn() throws {
        let h = try LeaseTestHarness()
        let parent = UUID()
        try h.store.markAccountAuthenticated(h.accountKey, credentialVersion: parent)
        let time = Date()
        let token = try #require(try h.leases.acquire(scope: .account(h.accountKey), ownerID: h.ownerA, now: time, ttl: 30))
        let attempt = try h.store.beginRefreshAttempt(for: h.accountKey, credentialVersion: parent, using: token, now: time)
        _ = try h.store.refreshAttemptState(for: h.accountKey)
        try h.reopenStore().markAccountSignedIn(h.accountKey, credentialVersion: UUID())
        var staleInstalled = false
        #expect(throws: (any Error).self) {
            try h.store.finishRefreshAttempt(attempt.attemptID, for: h.accountKey, using: token, installingCredentialVersion: UUID()) { staleInstalled = true }
        }
        #expect(!staleInstalled)
    }

    @Test func preparedInstallSurvivesReopen() throws {
        let (h, _, binding) = try prepared()
        let reopened = try h.reopenStore()
        let checkpoint = try #require(try reopened.refreshRecoveryCheckpoint(for: h.accountKey))
        #expect(checkpoint.phase == .installPrepared)
        #expect(checkpoint.installBinding == binding)
        #expect(checkpoint.recoveryEpoch == 3)
        #expect(checkpoint.attempt.protocolMode == .recoverableV1)
        #expect(try reopened.context.fetch(FetchDescriptor<AccountAccessState>()).first?.credentialVersion == binding.parentCredentialVersion)
    }

    @Test func completeBoundInstallUnderNewFence() throws {
        let (h, _, binding) = try prepared()
        let later = now.addingTimeInterval(11)
        let token = try #require(try h.leases.acquire(scope: .account(h.accountKey), ownerID: h.ownerB, now: later, ttl: 10))
        let reopened = try h.reopenStore()
        var installed = false
        try reopened.completeRefreshInstall(binding, for: h.accountKey, using: token, now: later) {
            let independent = try h.reopenStore()
            let row = try #require(try independent.context.fetch(FetchDescriptor<AccountAccessState>()).first)
            let data = try #require(try row.refreshRecoveryData)
            #expect(try JSONDecoder().decode(RefreshRecoveryCheckpoint.self, from: data).installBinding == binding)
            installed = true
        }
        #expect(installed)
        try assertQueuedDataUnchanged(h)
        let persisted = try h.reopenStore()
        #expect(try persisted.refreshRecoveryCheckpoint(for: h.accountKey)?.phase == .ackPending)
        #expect(try persisted.context.fetch(FetchDescriptor<AccountAccessState>()).first?.credentialVersion == binding.expectedCredentialVersion)
        #expect(try persisted.refreshAttemptState(for: h.accountKey) == .idle)
    }

    @Test func arbitraryChangedVersionDoesNotResolveAttempt() throws {
        let (h, token, binding) = try prepared()
        try h.store.markAccountAuthenticated(h.accountKey, credentialVersion: UUID())
        var installed = false
        #expect(throws: (any Error).self) {
            try h.store.completeRefreshInstall(binding, for: h.accountKey, using: token, now: now) { installed = true }
        }
        #expect(!installed)
        try assertQueuedDataUnchanged(h)
        #expect(try h.reopenStore().refreshRecoveryCheckpoint(for: h.accountKey)?.installBinding == binding)
        try h.store.markAccountAuthenticated(h.accountKey, credentialVersion: binding.parentCredentialVersion)
        try h.store.completeRefreshInstall(binding, for: h.accountKey, using: token, now: now) {}
        try h.store.markAccountAuthenticated(h.accountKey, credentialVersion: binding.parentCredentialVersion)
        #expect(throws: FencedLeaseError.cancelled) {
            try h.store.completeRefreshInstall(binding, for: h.accountKey, using: token, now: now) { Issue.record("Completed install invoked callback again") }
        }
        #expect(try h.reopenStore().refreshRecoveryCheckpoint(for: h.accountKey)?.phase == .ackPending)

    }

    @Test func staleFenceAndFreshSignInRejectInstall() throws {
        let (h, token, binding) = try prepared()
        let later = now.addingTimeInterval(11)
        let replacement = try #require(try h.leases.acquire(scope: .account(h.accountKey), ownerID: h.ownerB, now: later, ttl: 10))
        var installed = false
        #expect(throws: FencedLeaseError.staleOwner) {
            try h.store.completeRefreshInstall(binding, for: h.accountKey, using: token, now: later) { installed = true }
        }
        try h.store.markAccountSignedIn(h.accountKey, credentialVersion: UUID())
        #expect(try h.store.refreshRecoveryCheckpoint(for: h.accountKey) == nil)
        #expect(throws: (any Error).self) {
            try h.store.completeRefreshInstall(binding, for: h.accountKey, using: replacement, now: later) { installed = true }
        }
        #expect(!installed)
        try assertQueuedDataUnchanged(h)
    }

    @Test func authenticationPreservesRecoveryCheckpoint() throws {
        let (h, _, binding) = try prepared()
        try h.store.markAccountAuthenticated(h.accountKey, credentialVersion: binding.parentCredentialVersion)
        #expect(try h.reopenStore().refreshRecoveryCheckpoint(for: h.accountKey)?.installBinding == binding)
    }

    @Test func lockedKeychainRetainsPreparedCheckpoint() throws {
        enum Locked: Error { case unavailable }
        let (h, token, binding) = try prepared()
        #expect(throws: Locked.unavailable) {
            try h.store.completeRefreshInstall(binding, for: h.accountKey, using: token, now: now) { throw Locked.unavailable }
        }
        #expect(try h.reopenStore().refreshRecoveryCheckpoint(for: h.accountKey)?.phase == .installPrepared)
    }

    @Test func malformedRecoveryDataDoesNotBecomeIdle() throws {
        let (h, token, _) = try prepared()
        let row = try #require(try h.store.context.fetch(FetchDescriptor<AccountAccessState>()).first)
        row.refreshStateData = Data("not-json".utf8)
        try h.store.context.save()
        let reopened = try h.reopenStore()
        #expect(throws: RefreshRecoveryError.corruptCheckpoint) { try reopened.refreshRecoveryCheckpoint(for: h.accountKey) }
        #expect(throws: RefreshRecoveryError.corruptCheckpoint) {
            try reopened.beginRecoverableRefresh(for: h.accountKey, recoveryEpoch: 3, credentialVersion: UUID(), using: token, now: now)
        }
    }

    @Test func differentBindingAndEpochCannotOverwritePreparedInstall() throws {
        let (h, token, binding) = try prepared()
        let other = RefreshInstallBinding(key: h.accountKey, recoveryEpoch: 4, attemptID: binding.attemptID, parentCredentialVersion: binding.parentCredentialVersion, expectedCredentialVersion: UUID(), accessFingerprint: binding.accessFingerprint, refreshFingerprint: binding.refreshFingerprint)
        #expect(throws: (any Error).self) { try h.store.prepareRefreshInstall(other, for: h.accountKey, using: token, now: now) }
        #expect(try h.reopenStore().refreshRecoveryCheckpoint(for: h.accountKey)?.installBinding == binding)
    }

    @Test func acknowledgementCannotClearUninstalledCheckpoint() throws {
        let (h, token, binding) = try prepared()
        #expect(throws: (any Error).self) { try h.store.updateRefreshAcknowledgement(nil, observing: binding, for: h.accountKey, using: token, now: now) }
        #expect(try h.store.refreshRecoveryCheckpoint(for: h.accountKey)?.installBinding == binding)
        try h.store.completeRefreshInstall(binding, for: h.accountKey, using: token, now: now) {}
        try h.store.updateRefreshAcknowledgement(nil, observing: binding, for: h.accountKey, using: token, now: now)
        #expect(try h.reopenStore().refreshRecoveryCheckpoint(for: h.accountKey) == nil)
    }
    @Test func legacyStateDecoderAndSetterPreserveAdditiveRecovery() throws {
        let (h, _, binding) = try prepared()
        let reopened = try h.reopenStore()
        let row = try #require(try reopened.context.fetch(FetchDescriptor<AccountAccessState>()).first)
        // Frozen pre-change enum, without any recovery field or protocol mode.
        struct OldAttempt: Codable { let attemptID: UUID; let credentialVersion: UUID; let ownerGeneration: Int64; let startedAt: Date }
        enum OldState: Codable { case idle; case inFlightUncertain(OldAttempt) }
        let old = try JSONDecoder().decode(OldState.self, from: row.refreshStateData)
        guard case .inFlightUncertain(let attempt) = old else { Issue.record("Old decoder lost uncertainty"); return }
        #expect(attempt.attemptID == binding.attemptID)
        let before = try row.refreshRecoveryData
        row.refreshState = row.refreshState
        try reopened.context.save()
        #expect(try h.reopenStore().refreshRecoveryCheckpoint(for: h.accountKey)?.installBinding == binding)
        #expect(try row.refreshRecoveryData == before)
    }

    @Test func freshSignInSQLiteFailureAfterKeychainKeepsGateClosedUntilSuccessfulSignIn() throws {
        let (h, _, binding) = try prepared()
        try h.store.markAccountSignedOut(h.accountKey)
        let schema = Schema(versionedSchema: SchemaV12.self)
        let container = try BabySchema.makeContainer(schema: schema, configurations: [ModelConfiguration(schema: schema, url: h.storeURL, allowsSave: false)])
        let readOnly = BabyStore(container: container, dateProvider: TestDateProvider(now), deviceID: "read-only-signin", coordinationLockURL: h.lockURL)
        let credentialFile = h.storeURL.deletingLastPathComponent().appending(path: "synthetic-fresh-credential")
        let fresh = UUID()
        #expect(throws: (any Error).self) {
            try readOnly.markAccountSignedIn(h.accountKey, credentialVersion: fresh) {
                try Data(fresh.uuidString.utf8).write(to: credentialFile, options: .atomic)
            }
        }
        #expect(try String(contentsOf: credentialFile, encoding: .utf8) == fresh.uuidString)
        #expect(try !h.reopenStore().accountAllowsOrdinarySync(h.accountKey))
        #expect(try h.reopenStore().refreshRecoveryCheckpoint(for: h.accountKey)?.installBinding == binding)
        try assertQueuedDataUnchanged(h)
        try h.store.markAccountSignedIn(h.accountKey, credentialVersion: fresh) {}
        #expect(try h.reopenStore().accountAllowsOrdinarySync(h.accountKey))
        #expect(try h.reopenStore().refreshRecoveryCheckpoint(for: h.accountKey) == nil)
    }

    @Test func sqliteFailureAfterKeychainInstallRetainsSafeCheckpoint() throws {
        let (h, token, binding) = try prepared()
        let schema = Schema(versionedSchema: SchemaV12.self)
        let container = try BabySchema.makeContainer(schema: schema, configurations: [ModelConfiguration(schema: schema, url: h.storeURL, allowsSave: false)])
        let readOnly = BabyStore(container: container, dateProvider: TestDateProvider(now), deviceID: "read-only-test", coordinationLockURL: h.lockURL)
        var installed = false
        #expect(throws: (any Error).self) {
            try readOnly.completeRefreshInstall(binding, for: h.accountKey, using: token, now: now) { installed = true }
        }
        #expect(installed)
        try assertQueuedDataUnchanged(h)
        #expect(try h.reopenStore().refreshRecoveryCheckpoint(for: h.accountKey)?.phase == .installPrepared)
        try h.store.completeRefreshInstall(binding, for: h.accountKey, using: token, now: now) {}
        #expect(try h.reopenStore().refreshRecoveryCheckpoint(for: h.accountKey)?.phase == .ackPending)
    }

    @Test func boundRealmTransitionAndSignOutRejectInstallation() throws {
        let (h, token, binding) = try prepared()
        try h.store.beginTransition(to: .local, checkpointID: UUID())
        var installed = false
        #expect(throws: FencedLeaseError.cancelled) {
            try h.store.completeRefreshInstall(binding, for: h.accountKey, using: token, now: now) { installed = true }
        }
        #expect(!installed)
        try assertQueuedDataUnchanged(h)
        try h.store.recoverIncompleteTransition()
        try h.store.markAccountSignedOut(h.accountKey)
        #expect(throws: (any Error).self) {
            try h.store.completeRefreshInstall(binding, for: h.accountKey, using: token, now: now) { installed = true }
        }
        #expect(!installed)
        try assertQueuedDataUnchanged(h)
        #expect(try h.reopenStore().refreshRecoveryCheckpoint(for: h.accountKey)?.installBinding == binding)
    }

    @Test func legacyBeginCannotOverwritePendingRecoveryFromStaleContext() throws {
        let (h, token, binding) = try prepared()
        try h.store.completeRefreshInstall(binding, for: h.accountKey, using: token, now: now) {}
        #expect(throws: (any Error).self) {
            try h.store.beginRefreshAttempt(for: h.accountKey, credentialVersion: binding.expectedCredentialVersion, using: token, now: now)
        }
        #expect(try h.reopenStore().refreshRecoveryCheckpoint(for: h.accountKey)?.installBinding == binding)
        try assertQueuedDataUnchanged(h)
    }

    @Test func malformedOptionalRecoveryDataThrowsAndRetainsQueuedRows() throws {
        let (h, token, binding) = try prepared()
        let reopened = try h.reopenStore()
        let row = try #require(try reopened.context.fetch(FetchDescriptor<AccountAccessState>()).first)
        try row.setRecoveryPayload(state: row.refreshState, recovery: Data("invalid-checkpoint".utf8))
        try reopened.context.save()
        #expect(throws: RefreshRecoveryError.corruptCheckpoint) { try h.store.refreshRecoveryCheckpoint(for: h.accountKey) }
        #expect(throws: RefreshRecoveryError.corruptCheckpoint) {
            try h.store.completeRefreshInstall(binding, for: h.accountKey, using: token, now: now) { Issue.record("Corruption invoked installer") }
        }
        try assertQueuedDataUnchanged(h)
    }

    @Test func acknowledgementRetryIsBoundedAndSurvivesReopen() throws {
        let (h, token, binding) = try prepared()
        try h.store.completeRefreshInstall(binding, for: h.accountKey, using: token, now: now) {}
        let checkpoint = try #require(try h.store.refreshRecoveryCheckpoint(for: h.accountKey))
        let retry = AckRetry(attempt: checkpoint.attempt, binding: binding, nextRetryAt: now.addingTimeInterval(60), retryCount: Int.max)
        try h.store.updateRefreshAcknowledgement(retry, observing: binding, for: h.accountKey, using: token, now: now)
        let persisted = try #require(try h.reopenStore().refreshRecoveryCheckpoint(for: h.accountKey)?.acknowledgement)
        #expect(persisted.retryCount == 10)
        #expect(persisted.nextRetryAt == now.addingTimeInterval(60))
        #expect(persisted.binding == binding)
        try assertQueuedDataUnchanged(h)
    }

}
