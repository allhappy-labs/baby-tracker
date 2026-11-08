import BabyStore
import BabySync
import BabyUI
import SwiftUI

/// The Caregivers screen while this phone is in a family: who is in it, the
/// invitations still open, and every way out.
///
/// Sections only. The sheets they open are presented by `FamilySettingsView`
/// on its List — a sheet attached to a lazily built List row can fail to come
/// up at all — so each tap here is handed back as a closure.
struct FamilySharedSections<Model: FamilySyncSettingsModel>: View {
    let model: Model
    let onInvite: () -> Void
    let onOpenInvitation: (PendingInvitation) -> Void
    let onRevokeInvitation: (PendingInvitation) -> Void
    let onRetryInvitations: () -> Void
    let onConfirm: (FamilyLifecycleConfirmation) -> Void

    var body: some View {
        membersSection
        if model.isOwner {
            invitationsSection
        }
        leavingSection
    }

    // MARK: Members

    private var membersSection: some View {
        CardListSection(FamilyCopy.membersHeader) {
            ForEach(model.sortedMembers) { member in
                if model.isOwner, !model.isMe(member) {
                    NavigationLink {
                        MemberDetailView(model: model, member: member)
                    } label: {
                        // The List draws a link's chevron.
                        memberRow(member)
                    }
                    .accessibilityIdentifier("family.member.\(member.id.uuidString)")
                } else {
                    memberRow(member)
                        // One element, so VoiceOver reads "Marta, Caregiver"
                        // rather than stopping on two separate static texts.
                        .accessibilityElement(children: .combine)
                        .accessibilityIdentifier("family.member.\(member.id.uuidString)")
                }
            }
        } footer: {
            Text(model.isOwner ? FamilyCopy.ownerFooter : FamilyCopy.caregiverFooter)
        }
    }

    /// Name and role side by side at ordinary sizes, the role under the
    /// name at the accessibility sizes.
    private func memberRow(_ member: FamilyMember) -> some View {
        ListRow(member.displayName) {
            MemberAvatar(member: member)
        } trailing: {
            roleText(member)
        }
    }

    private func roleText(_ member: FamilyMember) -> some View {
        Text(FamilyCopy.roleLabel(member.familyRole, isMe: model.isMe(member)))
            .font(.subheadline)
            .foregroundStyle(Palette.secondaryText)
            .fixedSize(horizontal: false, vertical: true)
    }

    // MARK: Invitations

    @ViewBuilder
    private var invitationsSection: some View {
        if !model.pendingInvitations.isEmpty || model.invitationsRefreshFailed {
            CardListSection(PendingInvitationCopy.header) {
                ForEach(model.pendingInvitations) { invitation in
                    PendingInvitationRow(
                        invitation: invitation,
                        onOpen: { onOpenInvitation(invitation) },
                        onRevoke: { onRevokeInvitation(invitation) }
                    )
                }
                if model.invitationsRefreshFailed {
                    // The last-known list stays above; this only says it may
                    // be out of date, and offers the one thing to do about it.
                    // Side by side while both fit; the button under the
                    // message once the text is too large for that.
                    ViewThatFits(in: .horizontal) {
                        HStack(spacing: Spacing.s) {
                            refreshFailedText
                            Spacer(minLength: Spacing.s)
                            retryButton
                        }
                        VStack(alignment: .leading, spacing: Spacing.xs) {
                            refreshFailedText
                            retryButton
                        }
                    }
                }
            }
        }

        CardListSection {
            ActionRow(FamilyCopy.inviteSomeone, identifier: "family.inviteSomeone", systemImage: "plus", action: onInvite)
        } footer: {
            Text(FamilyCopy.inviteFooter)
        }
    }

    private var refreshFailedText: some View {
        Text(PendingInvitationCopy.refreshFailed)
            .foregroundStyle(Palette.secondaryText)
            .fixedSize(horizontal: false, vertical: true)
    }

    private var retryButton: some View {
        Button(PendingInvitationCopy.retry, action: onRetryInvitations)
            .frame(minHeight: 44)
            .accessibilityIdentifier("family.pending.retry")
    }

    // MARK: Leaving

    private var leavingSection: some View {
        CardListSection(FamilyCopy.leavingHeader) {
            DestructiveButton(FamilyCopy.disconnect, identifier: "family.disconnect") {
                onConfirm(.disconnect { try await model.disconnectCurrentFamily() })
            }

            DestructiveButton(FamilyCopy.leaveFamily, identifier: "family.leave") {
                onConfirm(.leave { try await model.leaveFamily() })
            }

            if model.isOwner {
                DestructiveButton(FamilyCopy.scheduleDeletion, identifier: "family.scheduleDeletion") {
                    onConfirm(.scheduleDeletion { try await model.scheduleFamilyDeletion() })
                }
            }
        }
    }
}
