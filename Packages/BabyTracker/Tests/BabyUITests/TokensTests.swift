import Testing
import SwiftUI
@testable import BabyUI

@Test func tokenValuesMatchSpec() {
    #expect(Radius.control == 14)
    #expect(Radius.button == 18)
    #expect(Radius.card == 20)
    #expect(Radius.sheet == 28)

    #expect(Spacing.xs == 4)
    #expect(Spacing.s == 8)
    #expect(Spacing.m == 12)
    #expect(Spacing.l == 16)
    #expect(Spacing.xl == 20)

    #expect(Surface.card == Color.white.opacity(0.06))
    #expect(Surface.raised == Color.white.opacity(0.10))
    #expect(Surface.separator == Color.white.opacity(0.07))
    #expect(Surface.Strength.soft.opacity == 0.16)
    #expect(Surface.Strength.strong.opacity == 0.30)
    #expect(Surface.tint(Palette.feed) == Palette.feed.opacity(0.16))
    #expect(Surface.tint(Palette.feed, .strong) == Palette.feed.opacity(0.30))
}

/// `swift test` runs on macOS, where `ImageRenderer` does not scale system text
/// styles with `dynamicTypeSize` (heights are identical at large and
/// accessibility5). So this asserts what is observable there: every role
/// renders, is non-empty, and never shrinks at the largest size. Real scaling
/// is covered by the simulator screenshot review at accessibility5.
@MainActor
@Test func everyTextRoleRendersAndScales() throws {
    for role in TextRole.allCases {
        var heights: [Int] = []
        for size in [DynamicTypeSize.large, .accessibility5] {
            let view = Text("Ag").textRole(role)
                .environment(\.dynamicTypeSize, size)
                .environment(\.colorScheme, .dark)
                .frame(width: 320)
            let image = try #require(ImageRenderer(content: view).cgImage)
            heights.append(image.height)
        }
        #expect(heights[0] > 0, "\(role) rendered empty")
        #expect(heights[1] >= heights[0], "\(role) shrank at accessibility5: \(heights)")
    }
}

@Test func textRolesMapToTheSpecifiedTextStylesAndWeights() {
    let expected: [(TextRole, Font.TextStyle, Font.Weight?, rounded: Bool, mono: Bool, secondary: Bool)] = [
        (.screenTitle, .title, .bold, true, false, false),
        (.sheetTitle, .headline, nil, false, false, false),
        (.cardTitle, .headline, nil, false, false, false),
        (.sectionLabel, .footnote, .semibold, false, false, true),
        (.body, .body, nil, false, false, false),
        (.value, .body, .semibold, false, true, false),
        (.caption, .footnote, nil, false, false, true),
    ]
    #expect(expected.count == TextRole.allCases.count)
    for (role, style, weight, rounded, mono, secondary) in expected {
        #expect(role.textStyle == style, "\(role) text style")
        #expect(role.weight == weight, "\(role) weight")
        #expect(role.isRounded == rounded, "\(role) rounded")
        #expect(role.isMonospacedDigit == mono, "\(role) monospaced digits")
        #expect(role.isSecondary == secondary, "\(role) secondary colour")
    }
}
