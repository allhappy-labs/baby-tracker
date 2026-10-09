import BabyStore
import BabySync
import BabyUI
import SwiftUI

/// The Caregivers screen while this phone is in a family: who is in it, the
/// invitations still open, and every way out.
///
/// Sections only. The sheets they open are presented by `FamilySettingsView`
/// on its Form — a sheet attached to a lazily built Form row can fail to come
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
        Section {
            ForEach(model.sortedMembers) { member in
                if model.isOwner, !model.isMe(member) {
                    NavigationLink {
                        MemberDetailView(model: model, member: member)
                    } label: {
                        memberRow(member)
                    }
                    .accessibilityIdentifier("family.member.\(member.id.uuidString)")
                } else {
                    memberRow(member)
                        .accessibilityIdentifier("family.member.\(member.id.uuidString)")
                }
            }
        } header: {
            Text(FamilyCopy.membersHeader)
        } footer: {
            Text(model.isOwner ? FamilyCopy.ownerFooter : FamilyCopy.caregiverFooter)
        }
    }

    private func memberRow(_ member: FamilyMember) -> some View {
        HStack(spacing: 12) {
            MemberAvatar(member: member)
            // Name and role side by side at ordinary sizes, stacked when the
            // text is too large for both to fit on one line.
            ViewThatFits(in: .horizontal) {
                HStack {
                    Text(member.displayName)
                    Spacer(minLength: 8)
                    roleText(member)
                }
                VStack(alignment: .leading, spacing: 2) {
                    Text(member.displayName)
                    roleText(member)
                }
            }
        }
        .frame(minHeight: 44)
        // One element, so VoiceOver reads "Marta, Caregiver" rather than
        // stopping on two separate static texts.
        .accessibilityElement(children: .combine)
    }

    private func roleText(_ member: FamilyMember) -> some View {
        Text(FamilyCopy.roleLabel(member.familyRole, isMe: model.isMe(member)))
            .font(.subheadline)
            .foregroundStyle(Palette.secondaryText)
    }

    // MARK: Invitations

    @ViewBuilder
    private var invitationsSection: some View {
        if !model.pendingInvitations.isEmpty || model.invitationsRefreshFailed {
            Section {
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
                    HStack {
                        Text(PendingInvitationCopy.refreshFailed)
                            .foregroundStyle(Palette.secondaryText)
                        Spacer(minLength: 8)
                        Button(PendingInvitationCopy.retry, action: onRetryInvitations)
                            .accessibilityIdentifier("family.pending.retry")
                    }
                    .frame(minHeight: 44)
                }
            } header: {
                Text(PendingInvitationCopy.header)
            }
        }

        Section {
            Button(action: onInvite) {
                HStack(spacing: 12) {
                    Image(systemName: "plus")
                        .accessibilityHidden(true)
                    Text(FamilyCopy.inviteSomeone)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
            .frame(minHeight: 44)
            .accessibilityIdentifier("family.inviteSomeone")
        } footer: {
            Text(FamilyCopy.inviteFooter)
        }
    }

    // MARK: Leaving

    private var leavingSection: some View {
        Section {
            Button(FamilyCopy.disconnect, role: .destructive) {
                onConfirm(.disconnect { try await model.disconnectCurrentFamily() })
            }
            .frame(minHeight: 44)
            .accessibilityIdentifier("family.disconnect")

            Button(FamilyCopy.leaveFamily, role: .destructive) {
                onConfirm(.leave { try await model.leaveFamily() })
            }
            .frame(minHeight: 44)
            .accessibilityIdentifier("family.leave")

            if model.isOwner {
                Button(FamilyCopy.scheduleDeletion, role: .destructive) {
                    onConfirm(.scheduleDeletion { try await model.scheduleFamilyDeletion() })
                }
                .frame(minHeight: 44)
                .accessibilityIdentifier("family.scheduleDeletion")
            }
        } header: {
            Text(FamilyCopy.leavingHeader)
        }
    }
}
