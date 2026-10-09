import BabySync
import BabyUI
import SwiftUI

/// One invitation that has not been redeemed yet: who it is for and how long
/// it has left. Tapping it shows the code again; swiping revokes it.
///
/// The swipe asks for no confirmation. It cannot be undone, but the cost of a
/// mistaken revoke is one new invitation, and a second sheet on every revoke
/// would cost more than that across all the ones that were meant.
struct PendingInvitationRow: View {
    let invitation: PendingInvitation
    let onOpen: () -> Void
    let onRevoke: () -> Void

    var body: some View {
        Button(action: onOpen) {
            HStack(spacing: 12) {
                Image(systemName: PendingInvitationCopy.systemImage(for: invitation.role))
                    .font(.body)
                    .foregroundStyle(Palette.sleep)
                    .frame(minWidth: 28)
                    .accessibilityHidden(true)
                VStack(alignment: .leading, spacing: 2) {
                    Text(PendingInvitationCopy.title(for: invitation.role))
                        .foregroundStyle(Palette.primaryText)
                    Text(PendingInvitationCopy.expiry(invitation.expiresAt))
                        .font(.subheadline)
                        .foregroundStyle(Palette.secondaryText)
                }
                .fixedSize(horizontal: false, vertical: true)
                Spacer(minLength: 0)
                Image(systemName: "chevron.right")
                    .font(.footnote.weight(.semibold))
                    .foregroundStyle(Palette.secondaryText)
                    .accessibilityHidden(true)
            }
            .frame(minHeight: 44)
            // Inside the label: a button's hit region is its label's content,
            // and the Spacer is exactly the middle of the row.
            .contentShape(Rectangle())
        }
        .accessibilityIdentifier("family.pending.\(invitation.id.uuidString)")
        .swipeActions(edge: .trailing) {
            Button(PendingInvitationCopy.revoke, role: .destructive, action: onRevoke)
        }
        // The swipe is invisible to VoiceOver; the same action, named.
        .accessibilityAction(named: PendingInvitationCopy.revoke, onRevoke)
    }
}

/// What a tap on an invitation this phone cannot show again opens: why the
/// code is gone, and the one thing still worth doing with it.
///
/// The server never returns a code after the moment it mints it, so a code
/// minted on another phone — or on this one before a reinstall — can only be
/// revoked here, never shown.
struct UnavailableInvitationSheet: View {
    let invitation: PendingInvitation
    let onRevoke: () -> Void
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: 16) {
                    Label(
                        PendingInvitationCopy.title(for: invitation.role),
                        systemImage: PendingInvitationCopy.systemImage(for: invitation.role)
                    )
                    .font(.headline)
                    Text(PendingInvitationCopy.expiry(invitation.expiresAt))
                        .font(.footnote)
                        .foregroundStyle(Palette.secondaryText)
                    Text(PendingInvitationCopy.unavailableMessage)
                        .font(.callout)
                        .fixedSize(horizontal: false, vertical: true)
                        .accessibilityIdentifier("invite.unavailable")
                    Button(role: .destructive) {
                        onRevoke()
                        dismiss()
                    } label: {
                        Text(InviteSheetCopy.revoke).frame(maxWidth: .infinity, minHeight: 44)
                    }
                    .buttonStyle(.bordered)
                    .accessibilityIdentifier("invite.revoke")
                }
                .padding()
            }
            .navigationTitle(PendingInvitationCopy.unavailableTitle)
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button(PendingInvitationCopy.close) { dismiss() }
                        .accessibilityIdentifier("invite.close")
                }
            }
        }
        .sheetHeight(320)
    }
}
