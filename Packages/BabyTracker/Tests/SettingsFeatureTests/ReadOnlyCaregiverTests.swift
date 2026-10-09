import BabyStore
import BabySync
import Foundation
import Testing
@testable import SettingsFeature

@MainActor
@Suite("Read-only caregivers")
struct ReadOnlyCaregiverTests {
    @Test func viewerProfileReportsReadOnlyAndCannotManage() async throws {
        let fixture = try FamilySettingsFixture.make(role: "viewer")
        try await fixture.model.signInWithApple()

        #expect(fixture.model.role == .viewer)
        #expect(fixture.model.isReadOnly)
        #expect(!fixture.model.isOwner)
    }

    /// The server is the authority; this mirrors its answer into the durable
    /// store gate so the widget and App Intents honour it too.
    @Test func signingInAsViewerRecordsTheDurableWriteGate() async throws {
        let fixture = try FamilySettingsFixture.make(role: "viewer")
        try await fixture.model.signInWithApple()

        #expect(try fixture.store.writePermission(in: FamilySettingsFixture.sharedRealm) == .viewer)

        // The local realm is untouched by a family role. Promotion records
        // membership but does not activate the shared realm — P3.4 does — so
        // this write goes to the user's own realm and must still succeed.
        // The shared-realm refusal itself is pinned in WritePermissionTests.
        _ = try fixture.store.logDiaper(.wet, for: FamilySettingsFixture.localBabyID)
        #expect(try fixture.store.writePermission(in: .local) == .owner)
    }

    @Test func caregiverSignInRecordsAWritableGate() async throws {
        let fixture = try FamilySettingsFixture.make(role: "caregiver")
        try await fixture.model.signInWithApple()
        #expect(try fixture.store.writePermission(in: FamilySettingsFixture.sharedRealm) == .caregiver)
    }

    /// A viewer must not be able to invite anyone, least of all another viewer.
    @Test func viewerCannotInvite() async throws {
        let fixture = try FamilySettingsFixture.make(role: "viewer")
        try await fixture.model.signInWithApple()

        await #expect(throws: FamilySyncError.ownerOnly) { _ = try await fixture.model.createInvitation() }
        await #expect(throws: FamilySyncError.ownerOnly) { _ = try await fixture.model.inviteViewer() }
        #expect(fixture.client.invitedRoles.isEmpty)
    }

    @Test func ordinaryCaregiverCannotChangeRoles() async throws {
        let fixture = try FamilySettingsFixture.make(role: "caregiver")
        try await fixture.model.signInWithApple()

        await #expect(throws: FamilySyncError.ownerOnly) {
            try await fixture.model.changeRole(of: FamilySettingsFixture.removableMemberID, to: .viewer)
        }
    }

    /// Both routes to view-only exist, and they are different promises: the
    /// invitation confers it on arrival, the role change withdraws writing
    /// from someone who already has it.
    @Test func ownerInvitesViewerWithTheViewerRole() async throws {
        let fixture = try await FamilySettingsFixture.ownerModelWithTwoMembers()
        _ = try await fixture.model.inviteViewer()
        #expect(fixture.client.invitedRoles == [.viewer])

        _ = try await fixture.model.createInvitation()
        #expect(fixture.client.invitedRoles == [.viewer, .caregiver])

        let target = FamilySettingsFixture.removableMemberID
        try await fixture.model.changeRole(of: target, to: .viewer)
        #expect(fixture.client.performed.contains(.changeRole(target, .viewer)))
    }

    @Test func ownerDemotesAndPromotesACaregiver() async throws {
        let fixture = try await FamilySettingsFixture.ownerModelWithTwoMembers()
        let target = FamilySettingsFixture.removableMemberID

        try await fixture.model.changeRole(of: target, to: .viewer)
        #expect(fixture.model.members.first { $0.id == target }?.familyRole == .viewer)

        try await fixture.model.changeRole(of: target, to: .caregiver)
        #expect(fixture.model.members.first { $0.id == target }?.familyRole == .caregiver)
        #expect(fixture.model.lastLifecycleCommands == [.changeRole(target, .viewer), .changeRole(target, .caregiver)])
    }

    /// An owner stepping down hands the family to a named successor. Demoting
    /// themselves would leave it with no owner at all.
    @Test func ownerCannotDemoteThemselves() async throws {
        let fixture = try await FamilySettingsFixture.ownerModelWithTwoMembers()
        await #expect(throws: FamilySyncError.ownerTransferRequired) {
            try await fixture.model.changeRole(of: FamilySettingsFixture.accountID, to: .viewer)
        }
    }

    /// An unrecognised role from a newer server must fail closed, not open.
    @Test func unknownServerRoleIsTreatedAsViewer() {
        let member = FamilyMember(id: UUID(), displayName: "Future", role: "superadmin")
        #expect(member.familyRole == .viewer)
        #expect(!member.familyRole.canWrite)
    }
}
