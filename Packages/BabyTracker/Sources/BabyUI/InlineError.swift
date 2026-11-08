import SwiftUI

/// A short error under a field or section, in the destructive colour.
public struct InlineError: View {
    private let message: String
    private let systemImage: String?

    public init(_ message: String, systemImage: String? = "exclamationmark.circle") {
        self.message = message
        self.systemImage = systemImage
    }

    public var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: Spacing.xs) {
            if let systemImage {
                Image(systemName: systemImage).accessibilityHidden(true)
            }
            Text(message).fixedSize(horizontal: false, vertical: true)
        }
        .font(.footnote)
        .foregroundStyle(Palette.destructive)
        .frame(maxWidth: .infinity, alignment: .leading)
        .accessibilityElement(children: .combine)
    }
}
