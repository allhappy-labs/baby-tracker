import Testing
import SwiftUI
@testable import BabyUI

@Test func selectableChipColours() {
    let tint = Palette.feed
    #expect(SelectableChipStyle.fill(tint: tint, isSelected: true) == tint)
    #expect(SelectableChipStyle.labelColor(tint: tint, isSelected: true) == Palette.ground)
    #expect(SelectableChipStyle.fill(tint: tint, isSelected: false) == Surface.tint(tint, .soft))
    #expect(SelectableChipStyle.labelColor(tint: tint, isSelected: false) == tint)
}

@MainActor
private func chipHeight(width: CGFloat) throws -> Int {
    // A larger label font so wrapped lines exceed the style's 44 pt minimum
    // height; macOS ImageRenderer does not scale text with Dynamic Type.
    let view = Button { } label: { Text("Грудне молоко та суміш").font(.title3) }
        .buttonStyle(.selectableChip(Palette.feed, isSelected: false))
        .environment(\.colorScheme, .dark)
        .frame(width: width)
    return try #require(ImageRenderer(content: view).cgImage).height
}

@MainActor
@Test func selectableChipWrapsLongUkrainianLabel() throws {
    let narrow = try chipHeight(width: 120)
    let wide = try chipHeight(width: 400)
    #expect(narrow > wide)
}

/// A chip with a label of its own gets the same chrome as a text chip: the
/// same padding, font and fill, so the two render at the same size.
@MainActor
@Test func customLabelChipMatchesTextChip() throws {
    func size(_ view: some View) throws -> CGSize {
        let image = try #require(ImageRenderer(content: view.environment(\.colorScheme, .dark)).cgImage)
        return CGSize(width: image.width, height: image.height)
    }
    let text = try size(Chip("Same feed"))
    let custom = try size(Chip { Text("Same feed") })
    #expect(text == custom)
    #expect(text.width > 0)
}
