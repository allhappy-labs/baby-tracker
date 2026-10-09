import Foundation
import Testing
@testable import BabyCore
@testable import BabyStore

@MainActor
@Suite("App Group fenced lease and account access")
struct AppGroupFencedLeaseTests {
    @Test func expiredOwnerIsSupersededAndCannotCommit() throws {
        let harness = try LeaseTestHarness()
        let scope = LeaseScope.realm(.local)
        let old = try #require(try harness.leases.acquire(scope: scope, ownerID: harness.ownerA, now: .init(timeIntervalSince1970: 0), ttl: 1))
        let replacement = try #require(try harness.leases.acquire(scope: scope, ownerID: harness.ownerB, now: .init(timeIntervalSince1970: 2), ttl: 10))

        #expect(replacement.generation > old.generation)
        #expect(throws: FencedLeaseError.staleOwner) {
            try harness.leases.withFencedCommit(old, now: .init(timeIntervalSince1970: 2)) {}
        }
    }

    @Test func renewalAndCancellationRequireExactFence() throws {
        let harness = try LeaseTestHarness()
        let token = try #require(try harness.leases.acquire(scope: .realm(.local), ownerID: harness.ownerA, now: .init(timeIntervalSince1970: 0), ttl: 2))
        let renewed = try harness.leases.renew(token, now: .init(timeIntervalSince1970: 1), ttl: 3)
        try harness.leases.cancel(renewed)

        #expect(try harness.leases.acquire(scope: .realm(.local), ownerID: harness.ownerB, now: .init(timeIntervalSince1970: 1), ttl: 2) != nil)
        #expect(throws: FencedLeaseError.staleOwner) { try harness.leases.cancel(token) }
    }

    @Test func authorizationAndUncertainRefreshPersistAcrossReopen() throws {
        let harness = try LeaseTestHarness()
        let key = harness.accountKey
        let credential = UUID()
        try harness.store.markAccountAuthenticated(key, credentialVersion: credential)
        let token = try #require(try harness.leases.acquire(scope: .account(key), ownerID: harness.ownerA, now: .init(timeIntervalSince1970: 0), ttl: 10))
        let attempt = try harness.store.beginRefreshAttempt(for: key, credentialVersion: credential, using: token, now: .init(timeIntervalSince1970: 1))
        let reopened = try harness.reopenStore()

        #expect(try reopened.accountAllowsOrdinarySync(key))
        #expect(try reopened.refreshAttemptState(for: key) == .inFlightUncertain(attempt))
    }

    @Test func staleRefreshOwnerCannotInvokeCredentialInstaller() throws {
        let harness = try LeaseTestHarness()
        let key = harness.accountKey
        let credential = UUID()
        try harness.store.markAccountAuthenticated(key, credentialVersion: credential)
        let old = try #require(try harness.leases.acquire(scope: .account(key), ownerID: harness.ownerA, now: .init(timeIntervalSince1970: 0), ttl: 1))
        let attempt = try harness.store.beginRefreshAttempt(for: key, credentialVersion: credential, using: old, now: .init(timeIntervalSince1970: 0))
        _ = try #require(try harness.leases.acquire(scope: .account(key), ownerID: harness.ownerB, now: .init(timeIntervalSince1970: 2), ttl: 10))
        var installed = false

        #expect(throws: FencedLeaseError.staleOwner) {
            try harness.store.finishRefreshAttempt(attempt.attemptID, for: key, using: old, installingCredentialVersion: UUID()) {
                installed = true
            }
        }
        #expect(!installed)
        #expect(try harness.store.refreshAttemptState(for: key) == .inFlightUncertain(attempt))
    }

    @Test func signOutFailsClosedAndDoesNotClearUncertainRefresh() throws {
        let harness = try LeaseTestHarness()
        let key = harness.accountKey
        let credential = UUID()
        try harness.store.markAccountAuthenticated(key, credentialVersion: credential)
        let token = try #require(try harness.leases.acquire(scope: .account(key), ownerID: harness.ownerA, now: .init(timeIntervalSince1970: 0), ttl: 10))
        let attempt = try harness.store.beginRefreshAttempt(for: key, credentialVersion: credential, using: token, now: .init(timeIntervalSince1970: 1))

        try harness.store.markAccountSignedOut(key)

        #expect(!(try harness.store.accountAllowsOrdinarySync(key)))
        #expect(try harness.store.refreshAttemptState(for: key) == .inFlightUncertain(attempt))
    }

    @Test func twoProcessesRejectSuspendedStaleOwnerAndSerializeAccountScope() throws {
        let harness = try LeaseTestHarness()
        let result = try harness.runTwoProcessTakeover(scope: .account(harness.accountKey))

        #expect(result.replacement == .committed)
        #expect(result.suspended == .staleLease)
    }
}
