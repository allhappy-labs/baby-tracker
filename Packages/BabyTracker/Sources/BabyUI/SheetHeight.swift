import SwiftUI

/// A sheet detent that is about as tall as the sheet's content, rather than
/// half the screen whatever is in it.
///
/// `.medium` was leaving these sheets 60% empty, with the choices pushed a
/// long way from the finger that opened them.
///
/// The height is stated rather than measured, and that is a deliberate second
/// attempt. Measuring the content and feeding the result back into
/// `presentationDetents` is the obvious version and it does not settle: the
/// detent change re-proposes a size, the measurement fires again, and the app
/// never reaches quiescence. It renders fine by eye — the cost showed up as
/// nine XCUITest runner restarts in one suite, because "wait for the app to
/// idle" never returns for an app that is still laying out.
///
/// `ScaledMetric` keeps the number honest across text sizes, and past the
/// accessibility sizes the sheet gives up and goes full height, which is what
/// it genuinely needs there.
private struct StatedSheetHeight: ViewModifier {
    let base: CGFloat
    @ScaledMetric(relativeTo: .body) private var scale: CGFloat = 100
    @Environment(\.dynamicTypeSize) private var typeSize

    func body(content: Content) -> some View {
        #if os(iOS)
        content.presentationDetents(
            typeSize.isAccessibilitySize ? [.large] : [.height(base * scale / 100)]
        )
        #else
        content
        #endif
    }
}

public extension View {
    /// Sizes the sheet this view is presented in.
    ///
    /// `points` is the height at the default text size; it scales from there.
    func sheetHeight(_ points: CGFloat) -> some View {
        modifier(StatedSheetHeight(base: points))
    }
}
