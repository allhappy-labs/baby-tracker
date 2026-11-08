import SwiftUI

/// A short yes/no question in a `fittedSheet`: an icon, a title and a
/// message centred, then the confirm button with cancel below it.
public struct ConfirmSheet: View {
    private let systemImage: String
    private let tint: Color
    private let title: String
    private let message: String
    private let confirm: SheetAction
    private let isDestructive: Bool
    private let cancel: SheetAction
    private let titleIdentifier: String?
    private let messageIdentifier: String?
    @ScaledMetric(relativeTo: .largeTitle) private var iconSize: CGFloat = 48

    public init(systemImage: String, tint: Color, title: String, message: String,
                confirm: SheetAction, isDestructive: Bool, cancel: SheetAction,
                titleIdentifier: String? = nil, messageIdentifier: String? = nil) {
        self.titleIdentifier = titleIdentifier
        self.messageIdentifier = messageIdentifier
        self.systemImage = systemImage
        self.tint = tint
        self.title = title
        self.message = message
        self.confirm = confirm
        self.isDestructive = isDestructive
        self.cancel = cancel
    }

    public var body: some View {
        VStack(spacing: 0) {
            // The words scroll at the largest text sizes; the buttons stay put
            // under them, as in `SheetScaffold`.
            ScrollView {
                VStack(spacing: Spacing.m) {
                    Image(systemName: systemImage)
                        .font(.system(size: iconSize))
                        .foregroundStyle(tint)
                        .accessibilityHidden(true)
                    Text(title)
                        .font(.title3.bold())
                        .foregroundStyle(Palette.primaryText)
                        .accessibilityAddTraits(.isHeader)
                        .modifier(OptionalIdentifier(titleIdentifier))
                    // A question that says it all in the title has no message.
                    if !message.isEmpty {
                        Text(message)
                            .font(.body)
                            .foregroundStyle(Palette.secondaryText)
                            .modifier(OptionalIdentifier(messageIdentifier))
                    }
                }
                .multilineTextAlignment(.center)
                .fixedSize(horizontal: false, vertical: true)
                .frame(maxWidth: .infinity)
                .padding(.horizontal, Spacing.xl)
                .padding(.top, Spacing.xl + Spacing.s)
                .padding(.bottom, Spacing.xl)
            }
            .scrollBounceBehavior(.basedOnSize)

            VStack(spacing: Spacing.s) {
                confirmButton
                SheetActionButton(cancel).buttonStyle(.secondary)
            }
            .padding(.horizontal, Spacing.xl)
            .padding(.vertical, Spacing.s)
        }
    }

    @ViewBuilder private var confirmButton: some View {
        if isDestructive {
            SheetActionButton(confirm).buttonStyle(DestructiveFillButtonStyle())
        } else {
            SheetActionButton(confirm).buttonStyle(.primary)
        }
    }
}

/// `PrimaryButtonStyle`'s size and shape, filled with the destructive colour.
struct DestructiveFillButtonStyle: ButtonStyle {
    @Environment(\.isEnabled) private var isEnabled

    func makeBody(configuration: Configuration) -> some View {
        let size = PrimaryButtonStyle.Size.regular
        let shape = RoundedRectangle(cornerRadius: size.cornerRadius, style: .continuous)
        return configuration.label
            .font(.headline)
            .foregroundStyle(Palette.ground)
            .padding(.horizontal, Spacing.l)
            .frame(maxWidth: .infinity, minHeight: size.minHeight)
            .background(shape.fill(Palette.destructive))
            .contentShape(shape)
            .opacity(PrimaryButtonStyle.opacity(isEnabled: isEnabled))
            .pressScale(configuration.isPressed)
    }
}

#if DEBUG
#Preview("ConfirmSheet") {
    VStack(spacing: Spacing.xl) {
        ConfirmSheet(systemImage: "trash", tint: Palette.destructive, title: "Delete this feeding?",
                     message: "It is removed for everyone in the family.",
                     confirm: SheetAction("Delete", identifier: "preview.confirm") {},
                     isDestructive: true,
                     cancel: SheetAction("Keep", identifier: "preview.cancel") {})
        ConfirmSheet(systemImage: "person.2.fill", tint: Palette.feed, title: "Join the Smith family?",
                     message: "You will see and log the same entries.",
                     confirm: SheetAction("Join", identifier: "preview.join") {},
                     isDestructive: false,
                     cancel: SheetAction("Not now", identifier: "preview.notNow") {})
    }
    .background(Palette.ground)
    .preferredColorScheme(.dark)
}
#endif
