import BabyStore
import BabyCore
import Foundation
import Testing
@testable import BabySync

@Suite("Account session lease identity")
struct AccountSessionLeaseTests {
    /// The projection is exactly four fields, and deliberately drops the
    /// recovery epoch: the refresh lease must serialise *across* an epoch
    /// rekey, so both sessions contend for one lease.
    @Test func projectsExactlyFourIdentityFields() {
        let key = SessionFixture.accountKey(epoch: 7)
        #expect(key.leaseKey == AccountLeaseKey(
            canonicalOrigin: key.canonicalOrigin,
            serverInstanceID: key.serverInstanceID,
            accountID: key.accountID,
            deviceID: key.deviceID
        ))
    }

    @Test func epochDoesNotChangeTheLeaseScope() {
        #expect(SessionFixture.accountKey(epoch: 0).leaseKey == SessionFixture.accountKey(epoch: 9).leaseKey)
        #expect(LeaseScope.account(SessionFixture.accountKey(epoch: 0).leaseKey)
            == .account(SessionFixture.accountKey(epoch: 9).leaseKey))
    }

    @Test(arguments: [0, 1, 2, 3])
    func anyDifferingIdentityFieldProducesADistinctScope(field: Int) {
        let base = SessionFixture.accountKey()
        let other: AccountSessionKey = switch field {
        case 0: SessionFixture.accountKey(origin: SessionFixture.otherOrigin)
        case 1: SessionFixture.accountKey(instanceID: SessionFixture.otherInstanceID)
        case 2: SessionFixture.accountKey(accountID: SessionFixture.otherAccountID)
        default: SessionFixture.accountKey(deviceID: SessionFixture.otherDeviceID)
        }
        #expect(LeaseScope.account(base.leaseKey) != .account(other.leaseKey))
    }

    /// A family session projects to the same account lease, so a promoted
    /// session keeps contending for the account's single refresh lease.
    @Test func familyKeyProjectsToTheSameAccountLease() {
        #expect(SessionFixture.familyKey().leaseKey == SessionFixture.accountKey().leaseKey)
    }

    @Test func familyKeyProducesItsSharedRealm() {
        #expect(SessionFixture.familyKey().realmID == .shared(
            canonicalOrigin: SessionFixture.origin.canonicalString,
            serverInstanceID: SessionFixture.instanceID,
            familyID: SessionFixture.familyID
        ))
    }
    @MainActor @Test func promotionPreservesRecoveryCheckpoint() throws {
        let now = Date(timeIntervalSince1970: 1_700_000_000)
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(now))
        let (sessions, _) = SessionFixture.store()
        let account = SessionFixture.accountKey(epoch: 3)
        let family = SessionFixture.familyKey(epoch: 3)
        let credentials = SessionFixture.credentials(epoch: 3)
        try sessions.saveAccount(credentials, for: account)
        try store.markAccountAuthenticated(account.leaseKey, credentialVersion: credentials.credentialVersion)
        let leases = AppGroupFencedLease(container: store.container, lockURL: store.coordinationLockURL)
        let lease = try #require(try leases.acquire(scope: .account(account.leaseKey), ownerID: UUID(), now: now, ttl: 30))
        let checkpoint = try store.beginRecoverableRefresh(for: account.leaseKey, recoveryEpoch: 3, credentialVersion: credentials.credentialVersion, using: lease, now: now)
        try sessions.promote(account, to: family, checkpointID: UUID())
        try store.markAccountAuthenticated(family.leaseKey, credentialVersion: credentials.credentialVersion)
        #expect(try store.refreshRecoveryCheckpoint(for: family.leaseKey) == checkpoint)
        #expect(try sessions.loadFamily(family)?.credentialVersion == credentials.credentialVersion)
        #expect(try sessions.loadAccount(account) == nil)
    }

}
