import Testing
import SwiftUI
@testable import BabyUI

@Test func everyTrackerHasADistinctColorAndIcon() {
    let colors = Tracker.allCases.map(\.color.description)
    let icons = Tracker.allCases.map(\.icon)

    #expect(Set(colors).count == Tracker.allCases.count)
    #expect(Set(icons).count == Tracker.allCases.count)
    // Color is never the only carrier of meaning.
}

@Test func trackerTitlesAreSentenceCase() {
    #expect(Tracker.feed.title == "Feed")
    #expect(Tracker.sleep.title == "Sleep")
    #expect(Tracker.diaper.title == "Diaper")
    #expect(Tracker.supplement.title == "Meds")
    #expect(Tracker.solids.title == "Solids")
    #expect(Tracker.growth.title == "Growth")
}

// MARK: - Contrast

/// WCAG 2.1 relative luminance and contrast ratio, computed from the resolved
/// sRGB components.
///
/// This exists because the UI suite's `AccessibilityAuditTests` excludes
/// `.contrast` from its assertions — `performAccessibilityAudit` samples
/// composited pixels and cannot see through this app's background gradient,
/// translucent card fills, glass dock and `.bar` material, and it contradicts
/// itself as a result. An exclusion is only honest if something else covers the
/// ground, so the palette's ratios are asserted here, arithmetically, where
/// there are no materials to confuse anyone.
enum WCAG {
    /// `Color.Resolved` exposes the linear-sRGB components directly, which is
    /// exactly the linearisation WCAG specifies, so there is no hand-rolled
    /// gamma curve here to get wrong.
    static func relativeLuminance(_ color: Color) -> Double {
        let resolved = color.resolve(in: EnvironmentValues())
        return 0.2126 * Double(resolved.linearRed)
            + 0.7152 * Double(resolved.linearGreen)
            + 0.0722 * Double(resolved.linearBlue)
    }

    static func contrastRatio(_ a: Color, _ b: Color) -> Double {
        let (la, lb) = (relativeLuminance(a), relativeLuminance(b))
        let (lighter, darker) = la > lb ? (la, lb) : (lb, la)
        return (lighter + 0.05) / (darker + 0.05)
    }
}

/// 4.5:1 is the WCAG AA threshold for normal-size text.
private let aaNormalText = 4.5

@Test func everyTrackerColorClearsAAAgainstTheGround() {
    for tracker in Tracker.allCases {
        let ratio = WCAG.contrastRatio(tracker.color, Palette.ground)
        #expect(
            ratio >= aaNormalText,
            "\(tracker.title) is \(ratio):1 against the ground, under \(aaNormalText):1"
        )
    }
}

@Test func bothTextColorsClearAAAgainstTheGround() {
    let primary = WCAG.contrastRatio(Palette.primaryText, Palette.ground)
    let secondary = WCAG.contrastRatio(Palette.secondaryText, Palette.ground)

    #expect(primary >= aaNormalText, "primaryText is \(primary):1 against the ground")
    #expect(secondary >= aaNormalText, "secondaryText is \(secondary):1 against the ground")
    // The secondary colour is the one the audit keeps flagging as a false
    // positive, so its margin over the threshold is worth being explicit about.
    #expect(secondary > 7, "secondaryText is \(secondary):1 — the audit's flag was a false positive")
}

/// The gradient's top stop is lighter than `Palette.ground`, so it is the harder
/// of the two backgrounds. Every foreground has to clear AA against it too, or
/// the top of the Home screen fails while the bottom passes.
@Test func foregroundsAlsoClearAAAgainstTheGradientsLightestStop() {
    let lightestStop = Color(red: 0.106, green: 0.145, blue: 0.278)
    let foregrounds = Tracker.allCases.map(\.color) + [Palette.primaryText, Palette.secondaryText]

    for color in foregrounds {
        let ratio = WCAG.contrastRatio(color, lightestStop)
        #expect(ratio >= aaNormalText, "\(color) is \(ratio):1 against the gradient's lightest stop")
    }
}

/// A ratio is only as good as the function computing it. Black on white is
/// exactly 21:1 and mid-grey on itself is exactly 1:1 — if those drift, every
/// assertion above is meaningless.
@Test func theContrastFunctionItselfIsCorrect() {
    let blackOnWhite = WCAG.contrastRatio(Color(red: 0, green: 0, blue: 0),
                                          Color(red: 1, green: 1, blue: 1))
    #expect(abs(blackOnWhite - 21) < 0.01, "black on white should be 21:1, got \(blackOnWhite)")

    let grey = Color(red: 0.5, green: 0.5, blue: 0.5)
    #expect(abs(WCAG.contrastRatio(grey, grey) - 1) < 0.0001)
}
