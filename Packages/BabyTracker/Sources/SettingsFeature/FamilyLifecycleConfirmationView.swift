import BabyUI
import SwiftUI

/// The sheet standing between a tap and something that cannot be undone.
///
/// The explanatory copy is shown in full rather than truncated behind a
/// disclosure: the sentence a user needs — that disconnecting does not restore
/// a local copy — is exactly the one a "more" link would hide.
struct FamilyLifecycleConfirmationView: View {
    let confirmation: FamilyLifecycleConfirmation
    let onConfirm: () -> Void
    let onCancel: () -> Void

    var body: some View {
        // Presented with `fittedSheet`: as tall as the copy and the two
        // buttons; past the screen the copy scrolls and the buttons stay.
        ConfirmSheet(
            systemImage: confirmation.isDestructive ? "exclamationmark.circle.fill" : "person.crop.circle.badge.checkmark",
            tint: confirmation.isDestructive ? Palette.destructive : Palette.sleep,
            title: confirmation.title,
            message: confirmation.body,
            confirm: SheetAction(confirmation.confirmLabel, identifier: "confirmation.confirm", action: onConfirm),
            isDestructive: confirmation.isDestructive,
            cancel: SheetAction(String(localized: "Cancel", bundle: .module), identifier: "confirmation.cancel", action: onCancel),
            titleIdentifier: "confirmation.title",
            messageIdentifier: "confirmation.copy"
        )
    }
}
