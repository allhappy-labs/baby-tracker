import BabyStore
import BabySync
import Foundation
import Testing
@testable import SettingsFeature

/// The words and marks the Caregivers screen shows, decided outside the view
/// so each one can be pinned without rendering it.
@Suite("Caregivers screen copy")
struct CaregiversScreenCopyTests {
    private var calendar: Calendar {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(identifier: "Europe/Kyiv")!
        return calendar
    }

    private func date(_ day: Int, _ hour: Int) -> Date {
        calendar.date(from: DateComponents(year: 2026, month: 9, day: day, hour: hour))!
    }

    @Test func expiryCountsCalendarDays() {
        let now = date(26, 23)
        #expect(PendingInvitationCopy.expiry(date(26, 23).addingTimeInterval(1800), now: now, calendar: calendar) == "Expires today")
        #expect(PendingInvitationCopy.expiry(date(27, 1), now: now, calendar: calendar) == "Expires tomorrow")
        #expect(PendingInvitationCopy.expiry(date(3 + 30, 9), now: now, calendar: calendar) == "Expires in 7 days")
        #expect(PendingInvitationCopy.expiry(date(2 + 30, 9), now: now, calendar: calendar) == "Expires in 6 days")
    }

    @Test func invitationTitleAndIconFollowTheRole() {
        #expect(PendingInvitationCopy.title(for: .caregiver) == "Caregiver invitation")
        #expect(PendingInvitationCopy.title(for: .viewer) == "View-only invitation")
        #expect(PendingInvitationCopy.systemImage(for: .caregiver) == "person.badge.plus")
        #expect(PendingInvitationCopy.systemImage(for: .viewer) == "eye")
    }

    /// Viewers used to be labelled "Caregiver"; the label is the role's own
    /// display name now, with "You" on your own row.
    @Test func roleLabelNamesEveryRoleAndMarksYou() {
        #expect(FamilyCopy.roleLabel(.viewer, isMe: false) == "View only")
        #expect(FamilyCopy.roleLabel(.caregiver, isMe: false) == "Caregiver")
        #expect(FamilyCopy.roleLabel(.owner, isMe: true) == "You · Owner")
    }

    @Test func shareMessageCarriesTheLinkAndTheCode() {
        let message = InviteSheetCopy.shareMessage(
            link: URL(string: "https://sync.example.test/invite#token=abc")!, code: "abc"
        )
        #expect(message == """
            Join our family in Baby Tracker: https://sync.example.test/invite#token=abc. Or open Baby Tracker → \
            Settings → Caregivers → Join and paste this code: abc
            """)
    }

    @Test func avatarInitialsTakeTheFirstTwoWords() {
        #expect(MemberAvatarStyle.initials(of: "Olena") == "O")
        #expect(MemberAvatarStyle.initials(of: "grandma rose petal") == "GR")
        #expect(MemberAvatarStyle.initials(of: "  ") == "?")
    }

    /// Derived from the id's bytes, never `hashValue`, which is reseeded on
    /// every launch and would repaint everyone each time the app opens.
    @Test func avatarColourIsFixedByTheMemberID() {
        // Literal indexes: a change to the derivation repaints every member
        // on every phone, and must show up here rather than in the field.
        #expect(MemberAvatarStyle.colourIndex(for: UUID(uuidString: "11111111-2222-4333-8444-555555555555")!) == 0)
        #expect(MemberAvatarStyle.colourIndex(for: UUID(uuidString: "0A000000-0000-4000-8000-000000000001")!) == 3)
        #expect(MemberAvatarStyle.colourIndex(for: UUID(uuidString: "0A000000-0000-4000-8000-000000000003")!) == 1)
    }

    @Test func invitationCodeIsTheLinksToken() {
        let invitation = FamilyInvitation(
            id: UUID(), shareURL: URL(string: "https://sync.example.test/invite#token=abcdefghijklmnop0123")!,
            role: .viewer, expiresAt: .distantFuture
        )
        #expect(InviteSheet<FamilySettingsViewModel>.code(of: invitation) == "abcdefghijklmnop0123")
    }
}

@MainActor
@Suite("Caregivers screen refresh")
struct CaregiversScreenRefreshTests {
    /// Pull-to-refresh: a caregiver who joined on another phone appears.
    @Test func reloadMembersPicksUpAMemberWhoJoinedElsewhere() async throws {
        let fixture = try await FamilySettingsFixture.ownerModelWithTwoMembers()
        let newcomer = FamilyMember(id: UUID(), displayName: "Nana", role: "viewer")
        fixture.client.membersValue.append(newcomer)

        try await fixture.model.reloadMembers()

        #expect(fixture.model.members.contains(newcomer))
    }

    /// The per-action copy reaches the sheet: a Remove confirmation names the
    /// member it removes, never the last lifecycle action's text.
    @Test func memberConfirmationCarriesItsOwnCopy() async throws {
        let fixture = try await FamilySettingsFixture.ownerModelWithTwoMembers()
        let sam = try #require(fixture.model.members.first { $0.displayName == "Sam" })

        let confirmation = FamilyLifecycleConfirmation(
            fixture.model.confirmation(for: .remove, member: sam), isDestructive: true
        ) {}

        #expect(confirmation.title == "Remove Sam?")
        #expect(confirmation.body.contains("Sam"))
        #expect(FamilyLifecycleConfirmation.disconnect {}.body == FamilyLifecycleCopy.disconnect)
    }
}
