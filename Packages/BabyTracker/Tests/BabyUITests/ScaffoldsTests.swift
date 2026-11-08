import Testing
import SwiftUI
@testable import BabyUI

@MainActor
@Test func partsRowDropsOldestFirst() {
    let three: [Range<Int>] = [0..<3, 1..<3, 2..<3]
    let one: [Range<Int>] = [0..<1]
    let actual3 = PartsRow.visibleSlices(count: 3)
    let actual1 = PartsRow.visibleSlices(count: 1)
    let actual0 = PartsRow.visibleSlices(count: 0)
    #expect(actual3 == three)
    #expect(actual1 == one)
    #expect(actual0.isEmpty)
}

@MainActor
private func height(of view: some View, width: CGFloat) throws -> Int {
    let framed = view.environment(\.colorScheme, .dark).frame(width: width)
    return try #require(ImageRenderer(content: framed).cgImage).height
}

// macOS ImageRenderer does not scale text with Dynamic Type, so wrapping at a
// narrow width stands in for the larger-text case.
@MainActor
@Test func emptyStateMessageWraps() throws {
    let view = EmptyState(systemImage: "tray", title: "Нічого немає",
                          message: "Тут з'являться записи, щойно ви додасте перший годування або сон.")
    #expect(try height(of: view, width: 120) > height(of: view, width: 400))
}

@MainActor
@Test func inlineErrorWraps() throws {
    let view = InlineError("Не вдалося зберегти запис. Спробуйте ще раз пізніше, будь ласка.")
    #expect(try height(of: view, width: 120) > height(of: view, width: 400))
}

@MainActor
@Test func partsRowEarlierTotalAddsDropped() {
    #expect(PartsRow.earlierTotal(earlierCount: 2, dropped: 1) == 3)
    #expect(PartsRow.earlierTotal(earlierCount: 0, dropped: 0) == 0)
}
