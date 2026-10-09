import BabyStore
import BabySync
import Foundation
import Testing
@testable import SettingsFeature

/// The pending-invitations list: what the server says is still open, and
/// this phone's own saved copy of each one's link — the only place a code
/// can be read back from, since the server only ever answers with it once.
@MainActor
@Suite("Pending invitations")
struct PendingInvitationsTests {
    @Test func twoInvitationsAreBothListedAndEachRevokesIndependently() async throws {
        let fixture = try await FamilySettingsFixture.ownerModel()
        let model = fixture.model

        let idA = UUID()
        let idB = UUID()
        fixture.client.invitationsToReturn = [
            FamilyInvitation(
                id: idA,
                shareURL: URL(string: "https://sync.example.test/invite?token=aaaaaaaaaaaaaaaaaaaaaaaaaaa")!,
                role: .caregiver, expiresAt: .distantFuture
            ),
            FamilyInvitation(
                id: idB,
                shareURL: URL(string: "https://sync.example.test/invite?token=bbbbbbbbbbbbbbbbbbbbbbbbbbb")!,
                role: .viewer, expiresAt: .distantFuture
            ),
        ]

        _ = try await model.createInvitation(role: .caregiver)
        _ = try await model.inviteViewer()

        #expect(Set(model.pendingInvitations.map(\.id)) == [idA, idB])
        #expect(model.shareURL(for: idA) != nil)
        #expect(model.shareURL(for: idB) != nil)

        try await model.revokeInvitation(idA)

        #expect(fixture.client.revokedInvitations == [idA])
        #expect(model.pendingInvitations.map(\.id) == [idB])
        // Revoking the first must not touch the second's saved code.
        #expect(model.shareURL(for: idA) == nil)
        #expect(model.shareURL(for: idB) != nil)
    }

    @Test func creatingSavesTheCodeAndItCanBeShownAgain() async throws {
        let fixture = try await FamilySettingsFixture.ownerModel()
        let model = fixture.model

        let invitation = try await model.createInvitation(role: .caregiver)

        #expect(model.shareURL(for: invitation.id) == invitation.shareURL)
    }

    @Test func reloadPrunesCodesTheServerNoLongerLists() async throws {
        let fixture = try await FamilySettingsFixture.ownerModel()
        let model = fixture.model

        let invitation = try await model.createInvitation(role: .caregiver)
        #expect(model.shareURL(for: invitation.id) != nil)

        // The server no longer lists it — revoked or expired elsewhere — so
        // a plain reload, not only a revoke, must drop the orphaned code.
        fixture.client.pending = []
        await model.reloadInvitations()

        #expect(model.shareURL(for: invitation.id) == nil)
    }

    /// The server lists an invitation this phone never minted — created on
    /// another device, or before a reinstall — so it has no saved code for
    /// it, even though the invitation itself is still perfectly pending.
    @Test func pendingInvitationWithoutStoredCodeHasNoShareURL() async throws {
        let fixture = try await FamilySettingsFixture.ownerModel()
        let model = fixture.model

        let mintedElsewhere = PendingInvitation(id: UUID(), role: .caregiver, expiresAt: .distantFuture, createdAt: .now)
        fixture.client.pending = [mintedElsewhere]
        await model.reloadInvitations()

        #expect(model.pendingInvitations.map(\.id) == [mintedElsewhere.id])
        #expect(model.shareURL(for: mintedElsewhere.id) == nil)
    }

    @Test func revokingAnAlreadyRedeemedInvitationJustReloads() async throws {
        let fixture = try await FamilySettingsFixture.ownerModel()
        let model = fixture.model

        let invitation = try await model.createInvitation(role: .caregiver)
        // The server already dropped it from the list — redeemed or revoked
        // from another device — before this phone got to revoke it itself.
        fixture.client.pending = []
        fixture.client.revokeInvitationErrors = [HTTPClientError.status(409, "invitation_consumed")]

        // Must not throw: the invitation is already unusable, which is all
        // "revoke" ever promised.
        try await model.revokeInvitation(invitation.id)

        #expect(model.pendingInvitations.isEmpty)
        #expect(model.shareURL(for: invitation.id) == nil)
    }

    @Test func listFailureKeepsLastInvitationsAndFlagsIt() async throws {
        let fixture = try await FamilySettingsFixture.ownerModel()
        let model = fixture.model

        let invitation = try await model.createInvitation(role: .caregiver)
        #expect(!model.invitationsRefreshFailed)
        let lastKnown = model.pendingInvitations

        struct Offline: Error {}
        fixture.client.pendingInvitationsErrors = [Offline()]
        await model.reloadInvitations()

        #expect(model.invitationsRefreshFailed)
        // The last-known list survives a failed refresh rather than going
        // blank.
        #expect(model.pendingInvitations == lastKnown)
        #expect(model.pendingInvitations.map(\.id) == [invitation.id])
    }

    /// This fixture never runs a real sync engine, so `store.activeRealm`
    /// stays `.local` on its own and `leaveFamily()`'s purge would not move
    /// it anywhere — which would let a save-then-clear bug pass unnoticed,
    /// since both the save and the (missing) clear would silently agree on
    /// `.local`. Moving the store into the shared realm first, the way a
    /// real engine's activation does, is what makes this test able to fail:
    /// the code is saved under the *shared* realm, `leaveFamily` purges it
    /// back to `.local`, and the assertion reads the shared realm's own
    /// `InvitationCodeStore` directly rather than through the model, whose
    /// `shareURL(for:)` would by then be reading the now-local realm too.
    @Test func leavingClearsStoredCodes() async throws {
        let fixture = try await FamilySettingsFixture.ownerModel()
        let model = fixture.model
        try fixture.enterSharedRealm()

        let invitation = try await model.createInvitation(role: .caregiver)
        let sharedCodes = InvitationCodeStore(backend: fixture.backend, realm: FamilySettingsFixture.sharedRealm)
        #expect(sharedCodes.shareURL(for: invitation.id) != nil)

        try await model.leaveFamily()

        #expect(sharedCodes.shareURL(for: invitation.id) == nil)
    }

    /// The same proof as `leavingClearsStoredCodes`, for the other path that
    /// purges the shared realm without deleting the account: disconnecting
    /// just this device.
    @Test func disconnectingClearsStoredCodes() async throws {
        let fixture = try await FamilySettingsFixture.ownerModel()
        let model = fixture.model
        try fixture.enterSharedRealm()

        let invitation = try await model.createInvitation(role: .caregiver)
        let sharedCodes = InvitationCodeStore(backend: fixture.backend, realm: FamilySettingsFixture.sharedRealm)
        #expect(sharedCodes.shareURL(for: invitation.id) != nil)

        try await model.disconnectCurrentFamily()

        #expect(sharedCodes.shareURL(for: invitation.id) == nil)
    }

    /// The same proof for handing the family over: an ex-owner never reloads
    /// the invitation list again, so nothing would ever prune its kept codes.
    /// The invitations themselves stay live, for the new owner to manage.
    @Test func transferringOwnershipClearsStoredCodes() async throws {
        let fixture = try await FamilySettingsFixture.ownerModelWithTwoMembers()
        let model = fixture.model
        try fixture.enterSharedRealm()

        let invitation = try await model.createInvitation(role: .caregiver)
        let sharedCodes = InvitationCodeStore(backend: fixture.backend, realm: FamilySettingsFixture.sharedRealm)
        #expect(sharedCodes.shareURL(for: invitation.id) != nil)

        try await model.transferOwnership(to: FamilySettingsFixture.successorMemberID)

        #expect(sharedCodes.shareURL(for: invitation.id) == nil)
        #expect(model.pendingInvitations.isEmpty)
    }

    @Test func invitationLimitShowsTheLimitMessage() {
        let message = PendingInvitationCopy.createFailureMessage(for: HTTPClientError.status(422, "invitation_limit"))

        #expect(message == FamilyCopy.invitationLimitReached)
    }
}
