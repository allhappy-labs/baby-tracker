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
    @Environment(\.dynamicTypeSize) private var typeSize

    var body: some View {
        Button(action: onOpen) {
            ListRow(
                PendingInvitationCopy.title(for: invitation.role),
                subtitle: PendingInvitationCopy.expiry(invitation.expiresAt),
                accessory: .disclosure
            ) {
                // Not at the accessibility sizes: beside the title, the
                // scaled glyph left it too narrow for "invitation" whole.
                if !typeSize.isAccessibilitySize {
                    Image(systemName: PendingInvitationCopy.systemImage(for: invitation.role))
                        .font(.body)
                        .foregroundStyle(Palette.sleep)
                        .frame(minWidth: 28)
                        .accessibilityHidden(true)
                }
            } trailing: {
                EmptyView()
            }
        }
        .buttonStyle(.plain)
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
    /// Closes the sheet. Presented with `fittedSheet`, so the presenter's
    /// own state is what takes it down.
    let onClose: () -> Void

    var body: some View {
        ConfirmSheet(
            systemImage: PendingInvitationCopy.systemImage(for: invitation.role),
            tint: Palette.sleep,
            title: PendingInvitationCopy.title(for: invitation.role),
            message: PendingInvitationCopy.expiry(invitation.expiresAt) + "\n\n" + PendingInvitationCopy.unavailableMessage,
            confirm: SheetAction(InviteSheetCopy.revoke, identifier: "invite.revoke") {
                onRevoke()
                onClose()
            },
            isDestructive: true,
            cancel: SheetAction(PendingInvitationCopy.close, identifier: "invite.close", action: onClose),
            messageIdentifier: "invite.unavailable"
        )
    }
}
