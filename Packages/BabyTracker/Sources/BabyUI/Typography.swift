import SwiftUI

public enum NumeralSize {
    case hero      // the running timer
    case display   // idle status rows
    case compact   // totals

    var textStyle: Font.TextStyle {
        switch self {
        case .hero: .largeTitle
        case .display: .title
        case .compact: .title3
        }
    }
}

/// The running timer, and nothing else on the screen.
///
/// Bigger than `.largeTitle`, which is where SwiftUI's built-in scale stops.
/// While a feed or a nap is running the elapsed time is the one thing a
/// parent is looking at, and at `.largeTitle` it carried exactly the same
/// weight as the baby's name in the heading above it. The screen has the room
/// — dropping the ring returned about 200pt of it.
///
/// `ScaledMetric` rather than a literal keeps the promise the built-in styles
/// make: the size is stated relative to `.largeTitle` and grows with Dynamic
/// Type, it is not pinned at 68pt.
private struct HeroNumeral: ViewModifier {
    @ScaledMetric(relativeTo: .largeTitle) private var size: CGFloat = 82

    func body(content: Content) -> some View {
        content
            .font(.system(size: size, weight: .bold, design: .rounded))
            .monospacedDigit()
            .foregroundStyle(Palette.primaryText)
    }
}

public extension View {
    /// Rounded numerals with tabular figures, so a live-updating timer does not
    /// jitter as digits change width. Scales with Dynamic Type — never fixed.
    @ViewBuilder
    func numeralStyle(_ size: NumeralSize) -> some View {
        switch size {
        case .hero:
            modifier(HeroNumeral())
        case .display, .compact:
            self
                .font(.system(size.textStyle, design: .rounded, weight: .bold))
                .monospacedDigit()
                .foregroundStyle(Palette.primaryText)
        }
    }
}
