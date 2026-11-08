import SwiftUI
import BabyUI

/// Stop and Wake up on the Live Activity cards and in the Dynamic Island: a
/// light capsule with dark text, the one primary action — the treatment of
/// the app's `PrimaryButtonStyle`. Put it in a `.plain` button: a button
/// with no style gets a system capsule of its own drawn behind this one.
struct PrimaryLabel: View {
    let title: String
    /// Full width under the card's text; false beside it (the pump card).
    var fillsWidth = true

    var body: some View {
        Text(title)
            .font(.headline)
            .lineLimit(1)
            .minimumScaleFactor(0.7)
            .foregroundStyle(Palette.ground)
            .padding(.horizontal, fillsWidth ? 0 : Spacing.xl)
            .frame(maxWidth: fillsWidth ? .infinity : nil, minHeight: 44)
            .background(Capsule().fill(Palette.primaryText))
            .contentShape(Capsule())
    }
}
