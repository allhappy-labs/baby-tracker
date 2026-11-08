import SwiftUI

/// A centred "nothing here" message: glyph, title, optional explanation.
public struct EmptyState: View {
    private let icon: BabyIcon?
    private let systemImage: String?
    private let title: String
    private let message: String?

    public init(icon: BabyIcon? = nil, systemImage: String? = nil, title: String, message: String?) {
        self.icon = icon
        self.systemImage = systemImage
        self.title = title
        self.message = message
    }

    public var body: some View {
        VStack(spacing: Spacing.s) {
            if let icon {
                BabyIconView(icon, size: 40).foregroundStyle(Palette.secondaryText)
            } else if let systemImage {
                Image(systemName: systemImage)
                    .font(.system(size: 40))
                    .foregroundStyle(Palette.secondaryText)
                    .accessibilityHidden(true)
            }
            Text(title)
                .textRole(.cardTitle)
                .multilineTextAlignment(.center)
            if let message {
                Text(message)
                    .textRole(.caption)
                    .multilineTextAlignment(.center)
            }
        }
        .frame(maxWidth: .infinity)
        .padding(Spacing.xl)
        .accessibilityElement(children: .combine)
    }
}
