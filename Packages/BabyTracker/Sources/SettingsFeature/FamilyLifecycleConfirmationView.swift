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
        VStack(spacing: 0) {
            ScrollView {
                VStack(alignment: .leading, spacing: 14) {
                    Image(systemName: confirmation.isDestructive ? "exclamationmark.circle.fill" : "person.crop.circle.badge.checkmark")
                        .font(.title2)
                        .foregroundStyle(confirmation.isDestructive ? Palette.destructive : Palette.sleep)
                        .accessibilityHidden(true)

                    Text(confirmation.title)
                        .font(.title3.weight(.bold))
                        .foregroundStyle(Palette.primaryText)
                        .accessibilityIdentifier("confirmation.title")

                    Text(confirmation.body)
                        .font(.callout)
                        .foregroundStyle(Palette.secondaryText)
                        .fixedSize(horizontal: false, vertical: true)
                        .accessibilityIdentifier("confirmation.copy")
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.horizontal, 24)
                .padding(.top, 28)
                .padding(.bottom, 20)
            }
            .scrollBounceBehavior(.basedOnSize)

            VStack(spacing: 10) {
                Button(confirmation.confirmLabel, role: confirmation.isDestructive ? .destructive : nil, action: onConfirm)
                    .font(.body.weight(.semibold))
                    .frame(maxWidth: .infinity, minHeight: 50)
                    .foregroundStyle(confirmation.isDestructive ? Palette.destructive : Palette.sleep)
                    .background(confirmation.isDestructive ? Palette.destructive.opacity(0.16) : Palette.sleep.opacity(0.16), in: RoundedRectangle(cornerRadius: 14))
                    .accessibilityIdentifier("confirmation.confirm")

                Button(String(localized: "Cancel", bundle: .module), action: onCancel)
                    .font(.body.weight(.semibold))
                    .frame(maxWidth: .infinity, minHeight: 50)
                    .foregroundStyle(Palette.primaryText)
                    .background(.thinMaterial, in: RoundedRectangle(cornerRadius: 14))
                    .accessibilityIdentifier("confirmation.cancel")
            }
            .buttonStyle(.plain)
            .padding(.horizontal, 20)
            .padding(.top, 14)
            .padding(.bottom, 20)
        }
    }
}
