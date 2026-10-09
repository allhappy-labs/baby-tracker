import Foundation
import BabyStore
import Testing
@testable import BabySync

@Suite("Keychain session isolation and promotion")
struct KeychainSessionStoreTests {
    @Test func savedAccountRoundTrips() throws {
        let (store, _) = SessionFixture.store()
        let key = SessionFixture.accountKey()
        try store.saveAccount(SessionFixture.credentials(), for: key)
        #expect(try store.loadAccount(key) == SessionFixture.credentials())
    }

    /// Every one of the five identity fields must isolate. A key that collides
    /// on any of them hands one server's, family's, account's or device's
    /// credentials to another.
    @Test(arguments: [0, 1, 2, 3, 4])
    func everyIdentityFieldIsolatesTheItem(differingField: Int) throws {
        let (store, _) = SessionFixture.store()
        let key = SessionFixture.accountKey()
        try store.saveAccount(SessionFixture.credentials(), for: key)

        let other: AccountSessionKey = switch differingField {
        case 0: SessionFixture.accountKey(origin: SessionFixture.otherOrigin)
        case 1: SessionFixture.accountKey(instanceID: SessionFixture.otherInstanceID)
        case 2: SessionFixture.accountKey(accountID: SessionFixture.otherAccountID)
        case 3: SessionFixture.accountKey(deviceID: SessionFixture.otherDeviceID)
        default: SessionFixture.accountKey(epoch: 1)
        }
        #expect(try store.loadAccount(other) == nil)
    }

    @Test func familyItemsIsolateByFamily() throws {
        let (store, _) = SessionFixture.store()
        let key = SessionFixture.familyKey()
        try store.saveFamily(SessionFixture.credentials(), for: key)
        let otherFamily = SessionFixture.familyKey(familyID: UUID(uuidString: "99b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!)
        #expect(try store.loadFamily(otherFamily) == nil)
        #expect(try store.loadFamily(key) != nil)
    }

    /// Promotion is one checkpointed move, so there is never a moment with the
    /// same secret stored under two keys.
    @Test func promotionLeavesExactlyOneItemAndIsResumable() throws {
        let (store, backend) = SessionFixture.store()
        let accountKey = SessionFixture.accountKey()
        let familyKey = SessionFixture.familyKey()
        try store.saveAccount(SessionFixture.credentials(), for: accountKey)
        let checkpoint = UUID()

        try store.promote(accountKey, to: familyKey, checkpointID: checkpoint)

        #expect(try store.loadAccount(accountKey) == nil)
        #expect(try store.loadFamily(familyKey) == SessionFixture.credentials())
        // A relaunch reads the checkpoint and learns where the session went.
        let relaunched = KeychainSessionStore(backend: backend.snapshot())
        #expect(try relaunched.resumePromotion(checkpointID: checkpoint) == familyKey)
    }

    /// A promotion replayed after the move already happened must not fail or
    /// duplicate: a crash between the write and the delete looks exactly so.
    @Test func promotionIsIdempotentWhenAlreadyMoved() throws {
        let (store, _) = SessionFixture.store()
        let accountKey = SessionFixture.accountKey()
        let familyKey = SessionFixture.familyKey()
        try store.saveAccount(SessionFixture.credentials(), for: accountKey)
        let checkpoint = UUID()
        try store.promote(accountKey, to: familyKey, checkpointID: checkpoint)

        try store.promote(accountKey, to: familyKey, checkpointID: checkpoint)
        #expect(try store.loadFamily(familyKey) == SessionFixture.credentials())
    }

    @Test func rekeyingAnEpochMovesTheAccountItem() throws {
        let (store, _) = SessionFixture.store()
        let old = SessionFixture.accountKey(epoch: 0)
        let new = SessionFixture.accountKey(epoch: 1)
        try store.saveAccount(SessionFixture.credentials(), for: old)

        try store.rekeyAccount(old, to: new, checkpointID: UUID())

        #expect(try store.loadAccount(old) == nil)
        #expect(try store.loadAccount(new) != nil)
    }
    @Test func legacyKeychainDecodesWithoutInstallBinding() throws {
        let legacy = Data(#"{"accessToken":"old-access","refreshToken":"old-refresh","expiresAt":0,"recoveryEpoch":3,"deviceContinuity":{"deviceID":"33333333-3333-4333-8333-333333333333","credential":"proof"},"credentialVersion":"44444444-4444-4444-8444-444444444444"}"#.utf8)
        let value = try JSONDecoder().decode(SessionCredentials.self, from: legacy)
        #expect(value.installBinding == nil)
        #expect(value.accessToken == "old-access")
    }

    @Test func atomicKeychainValueContainsCredentialsAndBinding() throws {
        let (store, backend) = SessionFixture.store()
        let key = SessionFixture.accountKey()
        let baseline = SessionFixture.credentials()
        let binding = RefreshInstallBinding(key: AccountLeaseKey(canonicalOrigin: key.canonicalOrigin, serverInstanceID: key.serverInstanceID, accountID: key.accountID, deviceID: key.deviceID), recoveryEpoch: key.recoveryEpoch, attemptID: UUID(), parentCredentialVersion: baseline.credentialVersion, expectedCredentialVersion: UUID(), accessFingerprint: RefreshInstallBinding.fingerprint("new-access"), refreshFingerprint: RefreshInstallBinding.fingerprint("new-refresh"))
        let value = SessionCredentials(accessToken: "new-access", refreshToken: "new-refresh", expiresAt: baseline.expiresAt, recoveryEpoch: key.recoveryEpoch, deviceContinuity: baseline.deviceContinuity, credentialVersion: binding.expectedCredentialVersion, installBinding: binding)
        try store.saveAccount(value, for: key)
        let raw = try #require(try backend.read(service: SessionItemEncoding.service, account: SessionItemEncoding.account(key)))
        #expect(try JSONDecoder().decode(SessionCredentials.self, from: raw) == value)
        let relaunched = KeychainSessionStore(backend: backend.snapshot())
        #expect(try relaunched.loadAccount(key)?.installBinding == binding)
        try store.promote(key, to: SessionFixture.familyKey(), checkpointID: UUID())
        #expect(try store.loadFamily(SessionFixture.familyKey())?.installBinding == binding)
    }

}
