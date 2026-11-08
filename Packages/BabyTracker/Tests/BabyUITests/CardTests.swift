import Testing
import SwiftUI
@testable import BabyUI

@MainActor
private func height<V: View>(_ view: V) throws -> Int {
    let framed = view.environment(\.colorScheme, .dark).frame(width: 320)
    return try #require(ImageRenderer(content: framed).cgImage).height
}

@MainActor
@Test func cardSectionHiddenRowTakesNoSpace() throws {
    let one = CardSection { Text("row") }
    let withHidden = CardSection {
        if false { Text("x") }
        Text("row")
    }
    #expect(try height(one) == height(withHidden))
}

@MainActor
@Test func cardSectionHeaderAddsHeight() throws {
    let bare = CardSection { Text("row") }
    let titled = CardSection("Title") { Text("row") }
    #expect(try height(titled) > height(bare))
}

@Test func listRowExpandRotation() {
    #expect(ListRow<EmptyView, EmptyView>.chevronRotation(for: .expand(isExpanded: false)) == .degrees(-90))
    #expect(ListRow<EmptyView, EmptyView>.chevronRotation(for: .expand(isExpanded: true)) == .zero)
}
