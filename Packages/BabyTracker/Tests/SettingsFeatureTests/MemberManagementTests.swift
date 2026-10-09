import BabyStore
import BabySync
import Foundation
import Testing
@testable import SettingsFeature

/// Member ordering, who may be offered ownership, and the copy for each
/// member-detail confirmation.
@MainActor
@Suite("Member management")
struct MemberManagementTests {
    @Test func membersAreOwnerFirstThenByName() async throws {
        let members = [
            FamilyMember(id: UUID(), displayName: "bob", role: "caregiver"),
            FamilyMember(id: FamilySettingsFixture.accountID, displayName: "Alex", role: "owner"),
            // Deliberately capitalized so a plain string sort — which puts
            // uppercase letters before lowercase ones — would get this wrong.
            FamilyMember(id: UUID(), displayName: "Adam", role: "viewer"),
        ]
        let fixture = try await FamilySettingsFixture.ownerModel(members: members)

        #expect(fixture.model.sortedMembers.map(\.displayName) == ["Alex", "Adam", "bob"])
    }

    @Test func viewerIsNeverOfferedOwnership() async throws {
        let owner = FamilyMember(id: FamilySettingsFixture.accountID, displayName: "Alex", role: "owner")
        let viewer = FamilyMember(id: UUID(), displayName: "Bob", role: "viewer")
        let caregiver = FamilyMember(id: UUID(), displayName: "Cy", role: "caregiver")

        let fixture = try await FamilySettingsFixture.ownerModel(members: [owner, viewer, caregiver])
        // The server only accepts a transfer to a caregiver — a viewer must
        // never be offered it.
        #expect(!fixture.model.canMakeOwner(viewer))
        #expect(fixture.model.canMakeOwner(caregiver))

        // Nor may anyone but the owner offer it, whatever the other
        // member's role.
        let asCaregiver = try FamilySettingsFixture.make(role: "caregiver", members: [owner, caregiver])
        try await asCaregiver.model.signInWithApple()
        #expect(!asCaregiver.model.canMakeOwner(caregiver))
    }

    @Test func eachMemberActionHasItsOwnConfirmationCopy() async throws {
        let fixture = try await FamilySettingsFixture.ownerModel()
        let model = fixture.model
        let member = FamilyMember(id: UUID(), displayName: "Sam", role: "caregiver")

        let remove = model.confirmation(for: .remove, member: member)
        #expect(remove.title.contains("Sam"))
        #expect(remove.body.contains("Sam"))

        let makeOwner = model.confirmation(for: .makeOwner, member: member)
        #expect(makeOwner.title.contains("Sam"))
        #expect(makeOwner.body.localizedCaseInsensitiveContains("you become a caregiver"))

        let changeToViewer = model.confirmation(for: .changeRole(to: .viewer), member: member)
        #expect(changeToViewer.title.contains("Sam"))
        #expect(changeToViewer.body.contains("Sam"))

        // Each action reads distinctly: none of them share the stale or
        // empty copy the old single `confirmationCopy` string produced for
        // "Remove" and "Make owner".
        #expect(remove.title != makeOwner.title)
        #expect(remove.body != makeOwner.body)
        #expect(remove.confirmLabel != makeOwner.confirmLabel)
        #expect(makeOwner.body != changeToViewer.body)
    }
}
