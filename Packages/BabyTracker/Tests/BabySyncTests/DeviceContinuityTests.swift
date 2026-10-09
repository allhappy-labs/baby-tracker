import Foundation
import Testing
@testable import BabySync

@Suite("Device continuity binding")
struct DeviceContinuityTests {
    /// Saving an account session retains its one-time device credential under
    /// the account-qualified continuity key, so a relaunch can still prove
    /// which device this is.
    @Test func continuitySurvivesRelaunch() throws {
        let (store, backend) = SessionFixture.store()
        let key = SessionFixture.accountKey()
        try store.saveAccount(SessionFixture.credentials(), for: key)

        let relaunched = KeychainSessionStore(backend: backend.snapshot())
        let binding = try relaunched.loadDeviceContinuity(for: key.continuityKey)

        #expect(binding == DeviceContinuityBinding(accountID: key.accountID, continuity: SessionFixture.continuity))
    }

    /// The binding is account-qualified, so another account on the same device
    /// and origin cannot pick it up and claim the device.
    @Test func continuityIsNotVisibleToAnotherAccount() throws {
        let (store, _) = SessionFixture.store()
        try store.saveAccount(SessionFixture.credentials(), for: SessionFixture.accountKey())

        let crossAccount = ContinuityKey(
            canonicalOrigin: SessionFixture.origin.canonicalString,
            serverInstanceID: SessionFixture.instanceID,
            accountID: SessionFixture.otherAccountID,
            deviceID: SessionFixture.deviceID
        )
        #expect(try store.loadDeviceContinuity(for: crossAccount) == nil)
    }

    /// Nor to another origin or instance: a proof minted for one server is
    /// meaningless to a different one, and must not even be offered.
    @Test func continuityIsScopedToOriginAndInstance() throws {
        let (store, _) = SessionFixture.store()
        try store.saveAccount(SessionFixture.credentials(), for: SessionFixture.accountKey())

        let otherOrigin = ContinuityKey(
            canonicalOrigin: SessionFixture.otherOrigin.canonicalString,
            serverInstanceID: SessionFixture.instanceID,
            accountID: SessionFixture.accountID,
            deviceID: SessionFixture.deviceID
        )
        let otherInstance = ContinuityKey(
            canonicalOrigin: SessionFixture.origin.canonicalString,
            serverInstanceID: SessionFixture.otherInstanceID,
            accountID: SessionFixture.accountID,
            deviceID: SessionFixture.deviceID
        )
        #expect(try store.loadDeviceContinuity(for: otherOrigin) == nil)
        #expect(try store.loadDeviceContinuity(for: otherInstance) == nil)
    }

    /// Credential-state loss erases every token but keeps the proof, because
    /// the proof is what lets a fresh Apple sign-in reuse this device instead
    /// of stranding its frozen mutations under a new one.
    @Test func invalidationErasesTokensAndKeepsContinuity() throws {
        let (store, _) = SessionFixture.store()
        let accountKey = SessionFixture.accountKey()
        let familyKey = SessionFixture.familyKey()
        try store.saveAccount(SessionFixture.credentials(), for: accountKey)
        try store.saveFamily(SessionFixture.credentials(), for: familyKey)

        try store.invalidateSessionsRetainingContinuity(for: accountKey)

        #expect(try store.loadAccount(accountKey) == nil)
        #expect(try store.loadFamily(familyKey) == nil)
        #expect(try store.loadDeviceContinuity(for: accountKey.continuityKey) != nil)
    }

    /// Exact targeting: another account's credentials on the same device are
    /// untouched by one account's revocation.
    @Test func invalidationLeavesAnotherAccountAlone() throws {
        let (store, _) = SessionFixture.store()
        let mine = SessionFixture.accountKey()
        let theirs = SessionFixture.accountKey(accountID: SessionFixture.otherAccountID)
        let theirFamily = SessionFixture.familyKey(accountID: SessionFixture.otherAccountID)
        try store.saveAccount(SessionFixture.credentials(), for: mine)
        try store.saveAccount(SessionFixture.credentials(access: "theirs"), for: theirs)
        try store.saveFamily(SessionFixture.credentials(access: "theirs"), for: theirFamily)

        try store.invalidateSessionsRetainingContinuity(for: mine)

        #expect(try store.loadAccount(theirs)?.accessToken == "theirs")
        #expect(try store.loadFamily(theirFamily)?.accessToken == "theirs")
    }

    /// Explicit account deletion is the path that does remove the binding.
    @Test func deletionRemovesContinuity() throws {
        let (store, _) = SessionFixture.store()
        let key = SessionFixture.accountKey()
        try store.saveAccount(SessionFixture.credentials(), for: key)

        try store.deleteDeviceContinuity(for: key.continuityKey)

        #expect(try store.loadDeviceContinuity(for: key.continuityKey) == nil)
    }
}
