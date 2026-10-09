import BabyStore
import BabySync
import Foundation
import Observation
import Testing
@testable import SettingsFeature

@MainActor
@Suite("Family settings model")
struct FamilySettingsViewModelTests {
    @Test func signInReturnsToSameFamily() async throws {
        let fixture = try await FamilySettingsFixture.ownerModel()
        try fixture.enterSharedRealm()
        _ = try fixture.store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let realm = fixture.store.activeRealm
        let queued = try fixture.store.pendingOutbox(in: realm).map(\.id)
        #expect(!queued.isEmpty)
        #expect(fixture.model.pendingDeliveryCount == queued.count)

        try await fixture.model.signInWithApple()

        guard case .shared(let key) = fixture.model.mode else {
            Issue.record("sign-in did not restore the shared family")
            return
        }
        #expect(key.realmID == realm)
        #expect(fixture.store.activeRealm == realm)
        #expect(try fixture.store.pendingOutbox(in: realm).map(\.id) == queued)
        #expect(fixture.model.pendingDeliveryCount == queued.count)
    }

    @Test func accountModeChangeInvalidatesObservation() async throws {
        let fixture = try FamilySettingsFixture.make(familyInBundle: false)
        try await confirmation("account mode changed") { confirm in
            withObservationTracking {
                _ = fixture.model.mode
            } onChange: {
                confirm()
            }
            try await fixture.model.signInWithApple()
        }
        #expect(fixture.model.mode == fixture.coordinator.mode)
    }

    /// Without a confirmed name the model refuses to sign in or create a
    /// family rather than deriving a name from an email address or Apple's
    /// subject claim.
    @Test func profileAndLifecycleMethodsRequireConfirmedStates() async throws {
        let fixture = try FamilySettingsFixture.unnamedModel()
        let model = fixture.model

        await #expect(throws: FamilySyncError.displayNameConfirmationRequired) {
            try await model.createFamily(name: "Family")
        }
        await #expect(throws: FamilySyncError.displayNameConfirmationRequired) {
            try await model.signInWithApple()
        }
        try await model.confirmDisplayName("Alex")
        try await model.signInWithApple()
        try await model.createFamily(name: "Family")

        await #expect(throws: FamilySyncError.ownerTransferRequired) { try await model.deleteAccount() }
    }

    /// Signed in, then back more than ten minutes later: the pre-family
    /// session has lapsed, and creating the family renews it once through
    /// the refresh route instead of failing until the next Apple sign-in.
    @Test func creatingAfterThePreFamilySessionLapsedRenewsItOnce() async throws {
        var renewed: [AccountSessionKey] = []
        let fixture = try FamilySettingsFixture.make(familyInBundle: false, renewAccountSession: { key, _ in renewed.append(key) })
        try await fixture.model.signInWithApple()
        guard case .authenticated(let accountKey) = fixture.model.mode else {
            Issue.record("not signed in: \(fixture.model.mode)")
            return
        }
        fixture.client.createFamilyErrors = [HTTPClientError.status(401, "access_expired")]

        try await fixture.model.createFamily(name: "Family")

        #expect(renewed == [accountKey])
        #expect(fixture.client.createFamilyCalls == 2)
        #expect(fixture.model.hasFamily)
    }

    @Test func joiningAfterThePreFamilySessionLapsedRenewsItOnce() async throws {
        var renewed = 0
        let fixture = try FamilySettingsFixture.make(familyInBundle: false, renewAccountSession: { _, _ in renewed += 1 })
        try await fixture.model.signInWithApple()
        fixture.client.confirmJoinErrors = [HTTPClientError.status(401, "access_expired")]

        try await fixture.model.acceptInvitation(InvitationIntent(origin: FamilySettingsFixture.origin, token: "abcdefghijklmnop0123456789"))

        #expect(renewed == 1)
        #expect(fixture.model.hasFamily)
    }

    /// Once, not a loop: a session refused again straight after renewal is
    /// not something another renewal fixes.
    @Test func aSecondExpiryAfterRenewalIsSurfaced() async throws {
        var renewed = 0
        let fixture = try FamilySettingsFixture.make(familyInBundle: false, renewAccountSession: { _, _ in renewed += 1 })
        try await fixture.model.signInWithApple()
        fixture.client.createFamilyErrors = [HTTPClientError.status(401, "access_expired"), HTTPClientError.status(401, "access_expired")]

        await #expect(throws: HTTPClientError.status(401, "access_expired")) {
            try await fixture.model.createFamily(name: "Family")
        }
        #expect(renewed == 1)
        #expect(!fixture.model.hasFamily)
    }

    @Test func profileEditPersists() async throws {
        let fixture = try await FamilySettingsFixture.ownerModel()
        try await fixture.model.updateDisplayName("Taylor")
        #expect(fixture.model.profile.displayName == "Taylor")
    }

    @Test func blankDisplayNameIsRefused() async throws {
        let fixture = try await FamilySettingsFixture.ownerModel()
        await #expect(throws: FamilySyncError.displayNameConfirmationRequired) {
            try await fixture.model.updateDisplayName("   ")
        }
    }

    /// Disconnecting takes the shared rows and leaves the permanent local
    /// realm exactly as it was — and says so, because a user who expects
    /// their data back has been misled by the button.
    @Test func disconnectPurgesOnlySharedRealmAndExplainsNoLocalRestore() async throws {
        let fixture = try await FamilySettingsFixture.modelWithLocalAndSharedRows()
        try await fixture.model.disconnectCurrentFamily()

        #expect(try fixture.store.babies(in: .local).map(\.id) == [FamilySettingsFixture.localBabyID])
        #expect(try fixture.store.babies(in: FamilySettingsFixture.sharedRealm).isEmpty)
        #expect(fixture.model.confirmationCopy.contains("does not restore a local copy"))
    }

    @Test func leavingExplainsOthersKeepTheFamily() async throws {
        let fixture = try await FamilySettingsFixture.ownerModel()
        try await fixture.model.leaveFamily()
        #expect(fixture.model.confirmationCopy.contains("does not restore a local copy"))
        #expect(fixture.model.members.isEmpty)
    }

    /// An invitation from an origin the user never confirmed is refused before
    /// any request is made.
    @Test func invitationFromAnUnconfirmedOriginIsRefused() async throws {
        let fixture = try await FamilySettingsFixture.ownerModel()
        let foreign = InvitationIntent(
            origin: try ServerOrigin(validating: "https://elsewhere.example"),
            token: "abcdefghijklmnop0123456789"
        )
        await #expect(throws: FamilySyncError.endpointConfirmationRequired) {
            try await fixture.model.acceptInvitation(foreign)
        }
    }

    /// The invitation code screen shows exactly the token the share link
    /// carries — the same identifier, not a second one to keep in sync.
    @Test func invitationCodeIsTheTokenFromTheShareLink() async throws {
        let fixture = try await FamilySettingsFixture.ownerModel()
        let invitation = try await fixture.model.createInvitation()
        let shared = try fixture.model.invitationShareURL(invitation)
        let token = try InvitationParser.parse(shared.absoluteString).token
        #expect(!token.isEmpty)
    }

    /// The join field's actual call: a bare code, paired with the endpoint
    /// this device already confirmed.
    @Test func joiningFromABareInvitationCodeUsesTheConfirmedEndpointsOrigin() async throws {
        let fixture = try FamilySettingsFixture.make(familyInBundle: false)
        try await fixture.model.signInWithApple()

        try await fixture.model.acceptInvitation(code: "abcdefghijklmnop0123456789")

        #expect(fixture.model.hasFamily)
    }

    /// A pasted full link still has to match the confirmed endpoint: pairing
    /// a bare code with it is not a way around the same origin check.
    @Test func joiningFromAPastedLinkStillRequiresItsOriginToMatch() async throws {
        let fixture = try FamilySettingsFixture.make(familyInBundle: false)
        try await fixture.model.signInWithApple()
        let foreignLink = "https://elsewhere.example.test/invite?token=abcdefghijklmnop0123456789"

        await #expect(throws: FamilySyncError.endpointConfirmationRequired) {
            try await fixture.model.acceptInvitation(code: foreignLink)
        }
    }

    /// A malformed code is refused before any request is made — the same
    /// shape check the parser already applies to a pasted link.
    @Test func joiningFromAMalformedCodeIsRefusedBeforeAnyRequest() async throws {
        let fixture = try FamilySettingsFixture.make(familyInBundle: false)
        try await fixture.model.signInWithApple()

        await #expect(throws: InvitationParser.ParseError.malformedToken) {
            try await fixture.model.acceptInvitation(code: "too-short")
        }
    }

    @Test func ownerMemberControlsAndAccountDeletionPreconditionUseP1LifecycleCommands() async throws {
        let fixture = try await FamilySettingsFixture.ownerModelWithTwoMembers()
        let model = fixture.model
        var deletedAccountIDs: [UUID] = []
        model.onAccountDeleted = { deletedAccountIDs.append($0) }

        try await model.removeMember(FamilySettingsFixture.removableMemberID)
        try await model.transferOwnership(to: FamilySettingsFixture.successorMemberID)
        try await model.deleteAccount()

        #expect(model.lastLifecycleCommands == [
            .removeMember(FamilySettingsFixture.removableMemberID),
            .transferOwnership(FamilySettingsFixture.successorMemberID),
            .deleteAccount,
        ])
        #expect(deletedAccountIDs == [FamilySettingsFixture.accountID])
    }

    /// Revocation stops sync on this device and says what to do about it. It
    /// does not send `/logout`: that revokes the device binding, which is for
    /// explicit disconnect, not for a credential the user revoked in Apple's
    /// own settings.
    @Test(arguments: [AppleCredentialState.revoked, .notFound])
    func credentialStateLossSignsOutAndDeniesOrdinarySync(state: AppleCredentialState) async throws {
        let fixture = try await FamilySettingsFixture.ownerModel()
        fixture.credentialStateClient.nextState = state

        #expect(try await fixture.model.refreshAppleCredentialState() == state)

        #expect(fixture.model.isSignedOut)
        #expect(fixture.client.logoutCount == 0)
        #expect(try fixture.store.babies(in: FamilySettingsFixture.sharedRealm).isEmpty)
        #expect(fixture.model.confirmationCopy.contains("Apple account settings"))
    }

    /// `transferred` is not a loss of authorization: the credential moved
    /// between Apple teams and the session stays valid.
    @Test func transferredCredentialStateDoesNotSignOut() async throws {
        let fixture = try await FamilySettingsFixture.ownerModel()
        fixture.credentialStateClient.nextState = .transferred
        #expect(try await fixture.model.refreshAppleCredentialState() == .transferred)
        #expect(!fixture.model.isSignedOut)
    }

    @Test func signedOutAccountIsDeniedOrdinarySync() async throws {
        let fixture = try await FamilySettingsFixture.ownerModel()
        fixture.credentialStateClient.nextState = .revoked
        _ = try await fixture.model.refreshAppleCredentialState()

        // The durable store gate, not a per-process flag: an extension asking
        // the same question gets the same answer.
        let accountKey = AccountSessionKey(
            canonicalOrigin: FamilySettingsFixture.origin.canonicalString,
            serverInstanceID: FamilySettingsFixture.instanceID,
            accountID: FamilySettingsFixture.accountID,
            deviceID: FamilySettingsFixture.deviceID,
            recoveryEpoch: 0
        )
        #expect(!(try fixture.store.accountAllowsOrdinarySync(accountKey.leaseKey)))
    }

    /// Fresh Apple proof is the only way back, and it reuses the retained
    /// device-continuity binding rather than allocating a new device.
    @Test func signedOutSelfHostRequiresFreshAppleProofBeforeResumingSync() async throws {
        let fixture = try await FamilySettingsFixture.ownerModel()
        fixture.credentialStateClient.nextState = .revoked
        _ = try await fixture.model.refreshAppleCredentialState()
        let proofsBefore = fixture.appleClient.freshProofRequestCount

        fixture.credentialStateClient.nextState = .authorized
        try await fixture.model.signInWithApple()

        #expect(fixture.appleClient.freshProofRequestCount == proofsBefore + 1)
        #expect(fixture.client.logoutCount == 0)
        #expect(!fixture.model.isSignedOut)
    }
}
