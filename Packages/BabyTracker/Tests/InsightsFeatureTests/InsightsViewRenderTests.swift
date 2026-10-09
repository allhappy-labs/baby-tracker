import Foundation
import SwiftUI
import Testing
import BabyCore
import BabyStore
import BabyUI
@testable import InsightsFeature

// Controller ruling R3: the brief's `screenRendersAtCompactWidthAndAccessibilitySize`
// asserts nothing — `ImageRenderer` never runs `.task`, so `InsightsView`'s model is
// never loaded and the two renders it compares are both of the unloaded state. It is
// dropped; `sectionCardRendersEverySectionKind` below is strengthened instead to check
// something real, mirroring `GrowthSectionTests.sectionRendersAtCompactWidthAndAccessibilitySize`.
@MainActor @Suite struct InsightsViewRenderTests {
    @Test func sectionCardRendersEverySectionKind() throws {
        // Every section needs a headline (otherwise it renders only the icon
        // and a single static "Nothing logged" line, which — like
        // `GrowthSectionTests`'s empty state — is too little content for
        // accessibility5 to come out taller than large).
        var summary = InsightsSummary(range: .week)
        summary.feeding.perDay = InsightAverage(value: 8.4, comparison: .change(0.6))
        summary.feeding.bars = [InsightBar(start: .now, value: 8, secondary: nil, isToday: false)]
        summary.feeding.breastOnly = 3
        summary.sleep.perDay = InsightAverage(value: 36_000, comparison: .change(600))
        summary.sleep.bars = [InsightBar(start: .now, value: 36_000, secondary: 7_200, isToday: false)]
        summary.sleep.nightPerDay = 28_800
        summary.sleep.napsPerDay = 7_200
        summary.diapers.perDay = InsightAverage(value: 6, comparison: .change(0.4))
        summary.diapers.bars = [InsightBar(start: .now, value: 6, secondary: nil, isToday: false)]
        for section in InsightsPresentation(summary: summary, volumeUnit: .millilitres).sections {
            var heights: [Int] = []
            for size in [DynamicTypeSize.large, .accessibility5] {
                let card = InsightSectionCard(section: section)
                    .environment(\.dynamicTypeSize, size).environment(\.colorScheme, .dark).frame(width: 360)
                let image = try #require(ImageRenderer(content: card).cgImage)
                heights.append(image.height)
            }
            #expect(heights[1] > heights[0], "\(section.kind) at accessibility5 should render taller than at large")
        }
    }

    // Controller ruling R7: the chart cannot use `.chartXSelection` (its drag gesture
    // clears the selection on finger-up, so a tap would only flash the callout).
    // `InsightSectionCard.toggledSelection` is the pure toggle rule behind the tap
    // handler in `GeometryReader { ... onTapGesture ... }`, mirroring
    // `GrowthChart.toggleSelection`/`nearestPoint`.
    @Test func toggledSelectionSelectsClearsEmptyAndUnknown() {
        let bars = [
            ChartBar(id: "a", primary: 1, secondary: 0, isEmpty: false, isToday: false, label: "A", valueText: "1", callout: "A"),
            ChartBar(id: "b", primary: 0, secondary: 0, isEmpty: true, isToday: false, label: "B", valueText: "—", callout: "B"),
        ]
        // Tapping a bar with nothing selected selects it.
        #expect(InsightSectionCard.toggledSelection(tapped: "a", current: nil, bars: bars) == "a")
        // Tapping the already-selected bar again clears it.
        #expect(InsightSectionCard.toggledSelection(tapped: "a", current: "a", bars: bars) == nil)
        // An empty bar never becomes the selection; the current one, if any, survives.
        #expect(InsightSectionCard.toggledSelection(tapped: "b", current: nil, bars: bars) == nil)
        #expect(InsightSectionCard.toggledSelection(tapped: "b", current: "a", bars: bars) == "a")
        // An id with no matching bar (a tap that resolved outside any bar) leaves the
        // current selection untouched.
        #expect(InsightSectionCard.toggledSelection(tapped: "missing", current: "a", bars: bars) == "a")
        // A tap that failed to resolve to any x value at all (nil) is the same as a miss.
        #expect(InsightSectionCard.toggledSelection(tapped: nil, current: "a", bars: bars) == "a")
    }
}
