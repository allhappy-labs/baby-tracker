import Foundation
import SwiftUI
import SwiftData
#if os(macOS)
import AppKit
#endif
import Testing
import BabyCore
import BabyStore
import BabyUI
@testable import GrowthFeature

@Suite @MainActor struct GrowthSectionTests {
    private func fixture() throws -> (BabyStore, Baby, Date) {
        let now = Date(timeIntervalSince1970: 1_700_000_000)
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(now))
        let baby = try store.addBaby(name: "Mira", birthDate: now.addingTimeInterval(-864000), referenceSex: .girls, expectedDueDate: nil)
        return (store, baby, now)
    }

    private func point(_ value: Double, daysAgo: Double, percentile: Double? = 48) -> GrowthPoint {
        GrowthPoint(id: UUID(), measuredAt: Date(timeIntervalSince1970: 1_790_000_000 - daysAgo * 86_400), timeZoneID: "UTC",
                    value: value, position: nil,
                    reference: percentile.map { .calculated(.available(percentile: $0, median: 5, p3: 3, p97: 7, correctedAge: false)) }
                        ?? .calculated(.unavailable(.noReferenceForMetric)))
    }

    @Test(arguments: GrowthMetric.allCases)
    func sparsePresentationAndAccessibility(_ metric: GrowthMetric) throws {
        let none = GrowthMetricPresentation(summary: .init(metric: metric, points: []))
        #expect(none.chartPoints.isEmpty)
        let point = GrowthPoint(id: UUID(), measuredAt: Date(timeIntervalSince1970: 1_700_000_000), timeZoneID: "UTC", value: 4.2, position: nil, reference: .missingProfile)
        let one = GrowthMetricPresentation(summary: .init(metric: metric, points: [point]))
        #expect(one.chartPoints.isEmpty)
        #expect(one.accessibilityLabel(for: point).contains(one.valueText(4.2)))
        #expect(one.accessibilityLabel(for: point).contains(one.dateText(point)))
        #expect(one.referenceText(point) == GrowthCopy.text(metric == .headCircumference ? "No reference percentile in this release" : "Choose a growth reference to enable comparisons."))
        let second = GrowthPoint(id: UUID(), measuredAt: point.measuredAt.addingTimeInterval(-86400), timeZoneID: "UTC", value: 4, position: nil, reference: .calculated(.unavailable(.referenceDataUnavailable)))
        let two = GrowthMetricPresentation(summary: .init(metric: metric, points: [point, second]))
        #expect(two.chartPoints.count == 2)
        #expect(two.referenceBands.isEmpty)
    }

    @Test func availableSyntheticReferenceHasPercentileAndOnlyContiguousBands() {
        func point(_ offset: Double, available: Bool) -> GrowthPoint {
            .init(id: UUID(), measuredAt: Date(timeIntervalSince1970: 1_700_000_000 + offset), timeZoneID: "UTC", value: 4,
                position: nil, reference: .calculated(available ? .available(percentile: 42, median: 5, p3: 2, p97: 8, correctedAge: true) : .unavailable(.referenceDataUnavailable)))
        }
        let points = [point(0, available: true), point(-100, available: false), point(-200, available: true), point(-300, available: true)]
        let display = GrowthMetricPresentation(summary: .init(metric: .weight, points: points))
        #expect(display.referenceText(points[0]).contains("42"))
        #expect(display.accessibilityLabel(for: points[0]).contains(display.referenceText(points[0])))
        #expect(display.referenceText(points[1]) == GrowthCopy.text("Reference data is unavailable."))
        // The bands follow the sampled curve, not the readings' own results.
        #expect(display.referenceBands.isEmpty)
        let one = GrowthMetricPresentation(summary: .init(metric: .weight, points: [points[0]]))
        #expect(one.referenceBands.isEmpty)
        #expect(one.chartPoints.isEmpty)
    }

    @Test func referenceBandsSplitAtCurveGapsAndLabelTheLastEnd() {
        func centiles(_ median: Double) -> GrowthCentiles {
            .init(p3: median - 3, p10: median - 2, p25: median - 1, p50: median, p75: median + 1, p90: median + 2, p97: median + 3)
        }
        let start = Date(timeIntervalSince1970: 1_700_000_000)
        let medians: [Double?] = [nil, 4, 4.1, nil, 4.3, nil, 4.5, 4.6, 4.7]
        var summary = GrowthMetricSummary(metric: .weight, points: [point(4.7, daysAgo: 0), point(4, daysAgo: 8)])
        summary.referenceCurve = medians.enumerated().map { index, median in
            GrowthReferenceSample(date: start.addingTimeInterval(Double(index) * 86_400), centiles: median.map(centiles))
        }
        let display = GrowthMetricPresentation(summary: summary)
        // A lone sample between gaps cannot make a band.
        #expect(display.referenceBands.map(\.samples.count) == [2, 3])
        #expect(display.referenceBands.first?.id == start.addingTimeInterval(86_400))
        #expect(display.trailingCentiles == centiles(4.7))
        #expect(GrowthMetricPresentation(summary: GrowthMetricSummary(metric: .headCircumference, points: summary.points,
                                                                      referenceCurve: summary.referenceCurve)).referenceBands.isEmpty)
        #expect(GrowthMetricPresentation(summary: .init(metric: .weight, points: summary.points)).trailingCentiles == nil)
    }

    @Test func swissRankRoundingNeverTurnsAnInteriorRankIntoATail() {
        for (rank, expected) in [(2.999, "<3rd"), (3.1, "3"), (42.4, "42"), (96.9, "97"), (97.001, ">97th")] {
            let point = GrowthPoint(id: UUID(), measuredAt: .now, timeZoneID: "UTC", value: 4, position: nil,
                reference: .calculated(.available(percentile: rank, median: 5, p3: 2, p97: 8, correctedAge: false)))
            let display = GrowthMetricPresentation(summary: .init(metric: .weight, points: [point]))
            #expect(display.referenceText(point) == "Estimated Swiss PEZZ 2025 percentile " + expected)
        }
        let corrected = GrowthPoint(id: UUID(), measuredAt: .now, timeZoneID: "UTC", value: 4, position: nil,
            reference: .calculated(.available(percentile: 42.4, median: 5, p3: 2, p97: 8, correctedAge: true)))
        #expect(GrowthMetricPresentation(summary: .init(metric: .weight, points: [corrected])).referenceText(corrected)
                == "Estimated Swiss PEZZ 2025 percentile 42 · corrected age")
    }

    @Test func headPresentationRetainsChartAndExposesReleaseScope() throws {
        let points = [36.0, 35.0].map { value in
            GrowthPoint(id: UUID(), measuredAt: Date(timeIntervalSince1970: 1_700_000_000 - (36 - value) * 86400), timeZoneID: "UTC", value: value, position: nil,
                        reference: .calculated(.unavailable(.noReferenceForMetric)))
        }
        let display = GrowthMetricPresentation(summary: .init(metric: .headCircumference, points: points))
        #expect(display.chartPoints.map(\.value) == [35, 36])
        #expect(display.referenceBands.isEmpty)
        #expect(display.referenceText(points[0]) == "No reference percentile in this release")
        #expect(display.accessibilityLabel(for: points[0]).contains("No reference percentile in this release"))
    }

    @Test func displayedSwissCopyIsLocalizedInEverySupportedLocale() throws {
        let catalog = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent().appending(path: "Sources/GrowthFeature/Localizable.xcstrings")
        let json = try #require(JSONSerialization.jsonObject(with: Data(contentsOf: catalog)) as? [String: Any])
        let strings = try #require(json["strings"] as? [String: [String: Any]])
        // "Swiss median and 3rd–97th percentile band" was the deleted
        // GrowthMetricCard's chart caption; it has no reader in the new
        // section view, so the catalog script retires it along with the
        // rest of that screen's copy.
        let keys = ["Estimated Swiss PEZZ 2025 percentile %@", "Estimated Swiss PEZZ 2025 percentile %@ · corrected age",
                    "<3rd", ">97th", "Swiss PEZZ 2025 reference",
                    "No reference percentile in this release", "Growth reference information"]
        #expect(keys.allSatisfy { strings[$0] != nil })
        // Both profile selectors must name the same reference as the resulting
        // comparison in every feature bundle, rather than silently fall back.
        let referenceKey = "Swiss PEZZ 2025 reference"
        let reference = try #require(strings[referenceKey]?["localizations"] as? [String: [String: Any]])
        for feature in ["BabyFeature", "SettingsFeature"] {
            let formCatalog = catalog.deletingLastPathComponent().deletingLastPathComponent()
                .appending(path: "\(feature)/Localizable.xcstrings")
            let formJSON = try #require(JSONSerialization.jsonObject(with: Data(contentsOf: formCatalog)) as? [String: Any])
            let formStrings = try #require(formJSON["strings"] as? [String: [String: Any]])
            let localizedReference = formStrings[referenceKey]?["localizations"] as? [String: [String: Any]]
            #expect(localizedReference != nil, "\(feature) must identify the Swiss comparison source")
            guard let localizedReference else { continue }
            for locale in ["en", "de", "fr", "es", "it", "uk"] {
                let unit = try #require(localizedReference[locale]?["stringUnit"] as? [String: String])
                let comparisonUnit = try #require(reference[locale]?["stringUnit"] as? [String: String])
                #expect(unit["state"] == "translated")
                #expect(unit["value"] == comparisonUnit["value"], "\(feature)/\(locale) must agree with Growth")
            }
            for (key, entry) in formStrings {
                #expect(!key.contains("WHO"))
                let localizations = entry["localizations"] as? [String: [String: Any]] ?? [:]
                for localization in localizations.values {
                    let value = (localization["stringUnit"] as? [String: String])?["value"] ?? ""
                    #expect(!["WHO", "OMS", "ВООЗ"].contains { value.contains($0) })
                }
            }
        }
        // Source explanations, unavailable reasons and chart/history copy must
        // all ship in each supported locale, not fall back to an English paragraph.
        for (key, entry) in strings {
            #expect(!key.contains("WHO"))
            let localizations = try #require(entry["localizations"] as? [String: [String: Any]])
            for locale in ["en", "de", "fr", "es", "it", "uk"] {
                let unit = try #require(localizations[locale]?["stringUnit"] as? [String: String], "Missing \(locale): \(key)")
                #expect(unit["state"] == "translated")
                #expect(unit["value"]?.isEmpty == false)
                #expect(unit["value"]?.contains("WHO") == false)
            }
        }
    }

    @Test func chipsShowLatestValueAndShortPercentile() {
        let presentation = GrowthSectionPresentation(summaries: [
            .init(metric: .weight, points: [point(5.62, daysAgo: 0, percentile: 48.4), point(5.2, daysAgo: 14)]),
            .init(metric: .lengthHeight, points: [point(58.5, daysAgo: 0, percentile: 1.2)]),
            .init(metric: .headCircumference, points: [point(39, daysAgo: 0, percentile: nil)]),
        ], birthDate: nil, locale: Locale(identifier: "en_US"))
        // In GrowthViewModel a head reading is always `.noReferenceForMetric`; the helper builds the same.
        #expect(presentation.chip(.weight).percentile == "P48")
        #expect(presentation.chip(.lengthHeight).percentile == "<P3")
        #expect(presentation.chip(.headCircumference).percentile == "—")
        #expect(presentation.chip(.weight).value == "5.62 kg")
    }

    @Test func missingProfileChipHasNoPercentileAndNoMeasurementShowsADash() {
        let missing = GrowthPoint(id: UUID(), measuredAt: .now, timeZoneID: "UTC", value: 4, position: nil, reference: .missingProfile)
        let presentation = GrowthSectionPresentation(summaries: [.init(metric: .weight, points: [missing])], birthDate: nil, locale: Locale(identifier: "en_US"))
        #expect(presentation.chip(.weight).percentile == nil)
        #expect(presentation.chip(.lengthHeight).value == "—")
    }

    @Test func weightChangeLineHasGramsPerDayAndLengthOnlyItsChange() throws {
        let presentation = GrowthSectionPresentation(summaries: [
            .init(metric: .weight, points: [point(5.62, daysAgo: 0), point(5.2, daysAgo: 14)]),
            .init(metric: .lengthHeight, points: [point(58.5, daysAgo: 0), point(56, daysAgo: 14)]),
        ], birthDate: nil, locale: Locale(identifier: "en_US"))
        let weight = try #require(presentation.changeLine(.weight))
        #expect(weight.hasPrefix("+420 g since "))
        #expect(weight.hasSuffix("≈ 30 g / day"))
        let length = try #require(presentation.changeLine(.lengthHeight))
        #expect(length.hasPrefix("+2.5 cm since "))
        #expect(!length.contains("/ day"))
        #expect(presentation.changeLine(.headCircumference) == nil)
    }

    @Test func weightReadingsUnderADayApartGiveTheChangeOnly() throws {
        let presentation = GrowthSectionPresentation(summaries: [
            .init(metric: .weight, points: [point(4.25, daysAgo: 0), point(4.2, daysAgo: 0.5)]),
        ], birthDate: nil, locale: Locale(identifier: "en_US"))
        let weight = try #require(presentation.changeLine(.weight))
        #expect(weight.hasPrefix("+50 g since "))
        #expect(!weight.contains("/ day"))
        #expect(!weight.contains("≈"))
    }

    @Test func headerGivesAgeInDaysUnderTwoWeeks() throws {
        let birth = Date(timeIntervalSince1970: 1_790_000_000 - 10 * 86_400)
        let presentation = GrowthSectionPresentation(summaries: [.init(metric: .weight, points: [point(3.6, daysAgo: 0)])], birthDate: birth, locale: Locale(identifier: "en_US"))
        let header = try #require(presentation.headerText)
        #expect(header.hasSuffix(" · 10d"))
        let twoWeeks = GrowthSectionPresentation(summaries: [.init(metric: .weight, points: [point(3.6, daysAgo: 0)])],
                                                 birthDate: birth.addingTimeInterval(-4 * 86_400), locale: Locale(identifier: "en_US"))
        #expect(try #require(twoWeeks.headerText).hasSuffix(" · 2w"))
    }

    @Test func headerShowsTheNewestCheckInAndAgeThen() throws {
        let birth = Date(timeIntervalSince1970: 1_790_000_000 - 63 * 86_400)
        let presentation = GrowthSectionPresentation(summaries: [.init(metric: .weight, points: [point(5.62, daysAgo: 0)])], birthDate: birth, locale: Locale(identifier: "en_US"))
        let header = try #require(presentation.headerText)
        #expect(header.contains("9"))   // nine weeks
        #expect(GrowthSectionPresentation(summaries: [], birthDate: birth, locale: Locale(identifier: "en_US")).isEmpty)
    }

    @Test func referenceChoiceKeepsADueDateAndRejectsAfterARealmSwitch() throws {
        let (store, baby, _) = try fixture()
        let profile = try #require(try store.growthProfile(for: baby.id))
        store.context.delete(profile)
        try store.context.save()
        let due = try LocalDate("2023-12-01")
        try store.setGrowthProfile(for: baby.id, referenceSex: .girls, expectedDueDate: due)
        let choice = GrowthReferenceChoice(store: store, babyID: baby.id)
        choice.choose(.boys)
        #expect(try store.growthProfile(for: baby.id)?.referenceSex == .boys)
        #expect(try store.growthProfile(for: baby.id)?.expectedDueDate == due)
        try store.setActiveRealm(.shared(canonicalOrigin: "https://example.com", serverInstanceID: UUID(), familyID: UUID()))
        choice.choose(.girls)
        #expect(choice.errorMessage != nil)
    }

    @Test func tappingTheChartPicksTheNearestPointAndClearsOnASecondTap() {
        let earlier = point(5, daysAgo: 10)
        let later = point(6, daysAgo: 0)
        let points = [later, earlier]
        #expect(GrowthChart.nearestPoint(to: later.measuredAt.addingTimeInterval(3_600), in: points)?.id == later.id)
        #expect(GrowthChart.nearestPoint(to: earlier.measuredAt.addingTimeInterval(-3_600), in: points)?.id == earlier.id)
        #expect(GrowthChart.nearestPoint(to: .now, in: []) == nil)

        let firstTap = GrowthChart.toggleSelection(tapped: later.measuredAt, in: points, current: nil)
        #expect(firstTap == later.measuredAt)
        // Tapping the already-selected point again clears it.
        #expect(GrowthChart.toggleSelection(tapped: later.measuredAt, in: points, current: firstTap) == nil)
        // Tapping near a different point while one is selected switches to it.
        #expect(GrowthChart.toggleSelection(tapped: earlier.measuredAt, in: points, current: firstTap) == earlier.measuredAt)
    }

    @Test func sectionRendersAtCompactWidthAndAccessibilitySize() throws {
        let (store, baby, now) = try fixture()
        let model = GrowthViewModel(store: store, babyID: baby.id)
        for count in 0...2 {
            if count > 0 {
                try store.logMeasurement(for: baby.id, at: now.addingTimeInterval(Double(-count) * 86_400), timeZoneID: "UTC",
                                         values: MeasurementValues(weightKG: Double(count + 3), lengthCM: 50 + Double(count), lengthPosition: .lying))
            }
            try model.load()
            var heights: [Int] = []
            for size in [DynamicTypeSize.large, .accessibility5] {
                let view = NavigationStack {
                    GrowthSectionView(model: model, store: store, babyID: baby.id, onOpenAllCheckIns: {})
                }
                .environment(\.dynamicTypeSize, size).environment(\.colorScheme, .dark)
                .frame(width: 320).background(Palette.ground)
                let image = try #require(ImageRenderer(content: view).cgImage)
                #expect(image.width == 320)
                heights.append(image.height)
            }
            if count > 0 { #expect(heights[1] > heights[0]) }
        }
    }
}
