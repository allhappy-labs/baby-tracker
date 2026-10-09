import Foundation
import Testing
import BabyCore
@testable import InsightsFeature

private let enUS = Locale(identifier: "en_US")

private func feeding(_ perDay: InsightAverage?, range: InsightRange = .week,
                     configure: (inout FeedingInsights) -> Void = { _ in }) -> InsightSection {
    var summary = InsightsSummary(range: range)
    summary.feeding.perDay = perDay
    configure(&summary.feeding)
    return InsightsPresentation(summary: summary, volumeUnit: .millilitres, locale: enUS).sections[0]
}

@Suite struct InsightsPresentationTests {
    @Test func countHeadlineAndChange() {
        let section = feeding(InsightAverage(value: 8.4, comparison: .change(0.62)))
        #expect(section.headline == "8.4")
        #expect(section.unit == "feeds / day")
        #expect(section.change == ChangeText(value: "▲ 0.6", caption: "vs prev. week"))
        #expect(section.accessibilityLabel == "Feeds: 8.4 feeds per day. 0.6 more than the previous week.")
    }

    @Test func wholeAveragesTakeTheirCountForm() {
        let one = feeding(InsightAverage(value: 1, comparison: .coverage(days: 3)))
        #expect(one.headline == "1")
        #expect(one.unit == "feed / day")
        #expect(one.accessibilityLabel == "Feeds: 1 feed per day. Average of 3 days.")
        // Shown to a tenth: 0.96 reads "1", and 2.96 reads "3".
        #expect(feeding(InsightAverage(value: 0.96, comparison: .coverage(days: 3))).unit == "feed / day")
        let three = feeding(InsightAverage(value: 2.96, comparison: .coverage(days: 3)))
        #expect(three.headline == "3")
        #expect(three.unit == "feeds / day")
        var summary = InsightsSummary(range: .week)
        summary.diapers.perDay = InsightAverage(value: 1, comparison: .coverage(days: 2))
        let diapers = InsightsPresentation(summary: summary, volumeUnit: .millilitres, locale: enUS).sections[2]
        #expect(diapers.unit == "diaper / day")
        #expect(diapers.accessibilityLabel == "Diapers: 1 diaper per day. Average of 2 days.")
    }

    @Test func aChangeThatRoundsToZeroReadsAsAboutTheSame() {
        let section = feeding(InsightAverage(value: 8, comparison: .change(-0.04)), range: .month)
        #expect(section.change == ChangeText(value: "≈", caption: "vs prev. month"))
        #expect(section.accessibilityLabel.hasSuffix("About the same as the previous month."))
    }

    @Test func coverageAndSince() {
        #expect(feeding(InsightAverage(value: 10.5, comparison: .coverage(days: 2))).change == ChangeText(value: "2 days", caption: ""))
        #expect(feeding(InsightAverage(value: 10, comparison: .coverage(days: 1))).change == ChangeText(value: "1 day", caption: ""))
        #expect(feeding(InsightAverage(value: 10.5, comparison: .coverage(days: 2))).accessibilityLabel
                == "Feeds: 10.5 feeds per day. Average of 2 days.")
        #expect(feeding(InsightAverage(value: 10, comparison: .coverage(days: 1))).accessibilityLabel
                == "Feeds: 10 feeds per day. Average of 1 day.")
        let since = feeding(InsightAverage(value: 9, comparison: .since(Date(timeIntervalSince1970: 1_785_000_000))), range: .all)
        #expect(since.change?.value.hasPrefix("since ") == true)
    }

    @Test func nothingLoggedHasNoHeadline() {
        let section = feeding(nil)
        #expect(section.headline == nil)
        #expect(section.rows.isEmpty)
        #expect(section.accessibilityLabel == "Feeds. Nothing logged in this period.")
    }

    @Test func feedingRowsAppearOnlyWhenTheyApply() {
        let breastOnly = feeding(InsightAverage(value: 8, comparison: .coverage(days: 3))) {
            $0.betweenFeeds = 2.5 * 3600; $0.longestGap = 5 * 3600 + 600
            $0.breastOnly = 8; $0.leftSeconds = 480; $0.rightSeconds = 520
        }
        #expect(breastOnly.rows.map(\.label) == ["Between feeds", "Longest gap", "Breast", "Left · right"])
        #expect(breastOnly.rows.map(\.value) == ["2h 30m", "5h 10m", "100%", "48% · 52%"])
        let mixed = feeding(InsightAverage(value: 8, comparison: .coverage(days: 3))) {
            $0.breastOnly = 16; $0.both = 2; $0.bottleOnly = 7; $0.bottlePerDayML = 410.4
        }
        #expect(mixed.rows.map(\.label) == ["Breast · Both · Bottle", "Bottle / day"])
        #expect(mixed.rows[0].value == "64% · 8% · 28%")
        #expect(mixed.rows[1].value == "410 ml")
    }

    @Test func percentSharesAlwaysSumToAHundred() {
        #expect(InsightsPresentation.percentShares([1, 1, 1]) == [34, 33, 33])
        #expect(InsightsPresentation.percentShares([2, 1]) == [67, 33])
        #expect(InsightsPresentation.percentShares([0, 0]) == [0, 0])
    }

    @Test func sleepHeadlineRowsAndStackedBars() {
        var summary = InsightsSummary(range: .week)
        summary.sleep.perDay = InsightAverage(value: 14 * 3600 + 20 * 60, comparison: .change(25 * 60))
        summary.sleep.longestStretch = 5 * 3600 + 40 * 60
        summary.sleep.nightPerDay = 10 * 3600 + 5 * 60
        summary.sleep.napsPerDay = 4 * 3600 + 15 * 60
        summary.sleep.napCountPerDay = 3.4
        summary.sleep.bars = [InsightBar(start: Date(timeIntervalSince1970: 1_785_000_000), value: 14 * 3600, secondary: 4 * 3600, isToday: false)]
        let section = InsightsPresentation(summary: summary, volumeUnit: .millilitres, locale: enUS).sections[1]
        #expect(section.headline == "14h 20m")
        #expect(section.unit == "/ day")
        #expect(section.change?.value == "▲ 25m")
        #expect(section.rows.map(\.label) == ["Longest stretch", "Night · naps", "Naps / day"])
        #expect(section.rows[1].value == "10h 5m · 4h 15m")
        #expect(section.bars[0].primary == 10)    // night hours
        #expect(section.bars[0].secondary == 4)   // nap hours
        #expect(section.bars[0].callout.contains("Night 10h · naps 4h"))
        #expect(section.accessibilityLabel == "Sleep: 14h 20m per day. 25m more than the previous week.")
    }

    @Test func todayBarSaysSoFarAndAxisEndsAtToday() {
        var summary = InsightsSummary(range: .week)
        summary.diapers.perDay = InsightAverage(value: 7, comparison: .coverage(days: 1))
        summary.diapers.bars = [
            InsightBar(start: Date(timeIntervalSince1970: 1_785_000_000), value: 7, secondary: nil, isToday: false),
            InsightBar(start: Date(timeIntervalSince1970: 1_785_086_400), value: 2, secondary: nil, isToday: true),
        ]
        let section = InsightsPresentation(summary: summary, volumeUnit: .millilitres, locale: enUS).sections[2]
        #expect(section.axisEnd == "Today")
        #expect(section.bars[1].valueText == "2, so far")
        #expect(section.gridlines == [4, 8])
        #expect(section.average == 7)
        #expect(section.unit == "diapers / day")
        #expect(section.accessibilityLabel == "Diapers: 7 diapers per day. Average of 1 day.")
    }

    // Review focus 5. The decimal separator follows the device locale because
    // `Volume.formatted` (BabyCore) is app-wide and unparametrized (R2); pin
    // the one-decimal rounding rule without pinning to one host's separator.
    @Test func ounceBottleRowKeepsOneDecimal() {
        var summary = InsightsSummary(range: .week)
        summary.feeding.perDay = InsightAverage(value: 8, comparison: .change(0))
        summary.feeding.bottlePerDayML = 410
        let oz = InsightsPresentation(summary: summary, volumeUnit: .ounces, locale: enUS).sections[0]
        #expect(["13.9 oz", "13,9 oz"].contains(oz.rows.last?.value ?? ""))
    }
}
