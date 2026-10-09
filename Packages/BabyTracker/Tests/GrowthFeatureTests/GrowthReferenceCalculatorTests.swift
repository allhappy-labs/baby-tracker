import Foundation
import Testing
import BabyCore
@testable import GrowthFeature

// Synthetic LMS fixtures isolate arithmetic and calendar behavior from source data.
struct GrowthReferenceCalculatorTests {
    let birth = try! LocalDate("2020-01-01")
    func date(_ text: String) -> Date { ISO8601DateFormatter().date(from: text)! }
    func calculator(month: Int? = nil, row: GrowthLMSRow = .init(l: 1, m: 10, s: 0.1)) -> GrowthReferenceCalculator {
        GrowthReferenceCalculator { _, _, age in month == nil || month == age ? row : nil }
    }
    func result(_ calculator: GrowthReferenceCalculator, at: String = "2020-01-01T12:00:00Z",
                value: Double = 10, metric: GrowthMetric = .weight, position: LengthPosition? = nil,
                zone: String = "UTC", due: LocalDate? = nil, birth: LocalDate? = nil,
                sex: GrowthReferenceSex = .girls) -> GrowthReferenceResult {
        calculator.result(metric: metric, value: value, position: position, measuredAt: date(at),
                          timeZoneID: zone, birthDate: birth ?? self.birth, expectedDueDate: due, referenceSex: sex)
    }
    func available(_ result: GrowthReferenceResult, percentile: Double = 50, median: Double = 10,
                   p3: Double? = nil, p97: Double? = nil, corrected: Bool = false) {
        guard case let .available(p, m, low, high, c) = result else { Issue.record("Expected available: \(result)"); return }
        #expect(abs(p - percentile) < 0.000001)
        #expect(abs(m - median) < 0.000001)
        #expect(low.isFinite && high.isFinite && low < m && m < high)
        if let p3 { #expect(abs(low - p3) < 0.000001) }
        if let p97 { #expect(abs(high - p97) < 0.000001) }
        #expect(c == corrected)
    }
    @Test func shippingProviderLoadsSwissWeightAtBirth() {
        available(result(GrowthReferenceCalculator(), value: 3.357), median: 3.357)
    }
    @Test func syntheticLMSBandsAndPercentile() {
        available(result(calculator(month: 0)), p3: 8.11920639184875, p97: 11.88079360815125)
        available(result(calculator(), value: 11), percentile: 84.1344746068543)
        available(result(calculator(row: .init(l: 0, m: 10, s: 0.1))), p3: 8.285489503, p97: 12.069292944)
    }
    @Test func nearZeroNonzeroLRetainsLogNormalLimit() {
        for l in [1e-16, -1e-16] {
            available(result(calculator(row: .init(l: l, m: 10, s: 0.1)), value: 11.051709180756477),
                      percentile: 84.1344746068543, p3: 8.285489503, p97: 12.069292944)
        }
    }
    @Test func negativeLRetainsItsPublishedSign() {
        available(result(calculator(row: .init(l: -1, m: 10, s: 0.1)), value: 11.11111111111111),
                  percentile: 84.1344746068543, p3: 8.416946148394613, p97: 12.316474686540138)
    }
    @Test func centilesFollowTheSameRowAndAgeAsThePercentile() throws {
        let lines = try #require(calculator(month: 0).centiles(metric: .weight, at: date("2020-01-01T12:00:00Z"),
            timeZoneID: "UTC", birthDate: birth, expectedDueDate: nil, referenceSex: .girls))
        // L = 1: each line is M(1 + S·z).
        for (value, z) in [(lines.p3, -1.8807936081512509), (lines.p10, -1.2815515655446004), (lines.p25, -0.6744897501960817),
                           (lines.p50, 0), (lines.p75, 0.6744897501960817), (lines.p90, 1.2815515655446004), (lines.p97, 1.8807936081512509)] {
            #expect(abs(value - 10 * (1 + 0.1 * z)) < 0.000001)
        }
        let shipping = GrowthReferenceCalculator()
        let swiss = try #require(shipping.centiles(metric: .weight, at: date("2020-01-01T12:00:00Z"), timeZoneID: "UTC",
            birthDate: birth, expectedDueDate: nil, referenceSex: .girls))
        // The bundled row's published girls' weight at birth.
        #expect(abs(swiss.p50 - 3.357) < 0.000001)
        func none(metric: GrowthMetric = .weight, at: String, due: LocalDate? = nil) -> Bool {
            shipping.centiles(metric: metric, at: date(at), timeZoneID: "UTC", birthDate: birth,
                              expectedDueDate: due, referenceSex: .girls) == nil
        }
        #expect(none(metric: .headCircumference, at: "2020-01-01T12:00:00Z"))
        #expect(none(at: "2020-01-15T12:00:00Z", due: try LocalDate("2020-02-01")))
        #expect(none(at: "2025-02-01T12:00:00Z"))
        #expect(calculator(month: 1).centiles(metric: .weight, at: date("2020-01-15T12:00:00Z"), timeZoneID: "UTC",
            birthDate: birth, expectedDueDate: nil, referenceSex: .girls) == nil)
    }
    @Test func storedTimeZoneDeterminesCalendarNode() {
        available(result(calculator(month: 0), at: "2020-01-02T01:00:00Z", zone: "America/Los_Angeles"))
        available(result(calculator(month: 1), at: "2020-02-01T01:00:00Z", zone: "Europe/Zurich"))
        #expect(result(calculator(month: 1), at: "2020-02-01T01:00:00Z", zone: "America/Los_Angeles")
                == .unavailable(.referenceDataUnavailable))
        #expect(result(calculator(), zone: "Invalid/Zone") == .unavailable(.invalidDate))
    }
    @Test func correctionStartsAtDueDateAndEndsAtThirdBirthday() throws {
        let due = try LocalDate("2020-02-01")
        #expect(result(calculator(), due: due) == .unavailable(.beforeReferenceStart))
        available(result(calculator(month: 0), at: "2020-02-01T12:00:00Z", due: due), corrected: true)
        available(result(calculator(), at: "2022-12-31T12:00:00Z", due: due), corrected: true)
        available(result(calculator(month: 36), at: "2023-01-01T12:00:00Z", due: due))
        available(result(calculator(month: 0), due: try LocalDate("2019-12-01")))
    }
    @Test func storedZoneControlsDueAndThirdBirthdayBoundaries() throws {
        let due = try LocalDate("2020-02-01")
        #expect(result(calculator(), at: "2020-02-01T01:00:00Z", zone: "America/Los_Angeles", due: due)
                == .unavailable(.beforeReferenceStart))
        available(result(calculator(), at: "2023-01-01T01:00:00Z", zone: "America/Los_Angeles", due: due), corrected: true)
        available(result(calculator(month: 36), at: "2023-01-01T01:00:00Z", zone: "Europe/Zurich", due: due))
    }
    @Test func interpolationUsesElapsedCalendarDays() {
        let calculator = GrowthReferenceCalculator { _, _, month in .init(l: 1, m: 10 + Double(month), s: 0.1) }
        available(result(calculator, at: "2020-02-15T12:00:00Z", value: 11.482758620689656), median: 11.482758620689656)
        available(result(calculator, at: "2022-04-01T12:00:00Z", value: 36.98342541436464), median: 36.98342541436464)
    }
    @Test func interpolationIncludesLandSAsWellAsMedian() {
        let calculator = GrowthReferenceCalculator { _, _, month in
            month == 0 ? .init(l: 0, m: 10, s: 0.1) : .init(l: 1, m: 12, s: 0.3)
        }
        // Jan 16 is 15/31 of the way between Jan 1 and Feb 1.
        available(result(calculator, at: "2020-01-16T12:00:00Z", value: 13.235730164970818),
                  percentile: 84.1344746068543, median: 10.967741935483872)
    }
    @Test func leapDayOriginUsesOriginalMonthAnniversaries() throws {
        let born = try LocalDate("2020-02-29")
        let calculator = GrowthReferenceCalculator { _, _, month in .init(l: 1, m: 10 + Double(month), s: 0.1) }
        available(result(calculator, at: "2021-02-28T12:00:00Z", value: 22, birth: born), median: 22)
        available(result(calculator, at: "2021-03-14T12:00:00Z", value: 22.482758620689656, birth: born), median: 22.482758620689656)
        available(result(calculator, at: "2025-02-28T12:00:00Z", value: 70, birth: born), median: 70)
        #expect(result(calculator, at: "2025-03-01T12:00:00Z", birth: born) == .unavailable(.ageOutsideReferenceRange))
    }
    @Test func thirtyFirstOriginDoesNotDriftAfterFebruary() throws {
        let born = try LocalDate("2021-01-31")
        let calculator = GrowthReferenceCalculator { _, _, month in .init(l: 1, m: 10 + Double(month), s: 0.1) }
        available(result(calculator, at: "2021-02-28T12:00:00Z", value: 11, birth: born), median: 11)
        available(result(calculator, at: "2021-03-31T12:00:00Z", value: 12, birth: born), median: 12)
        available(result(calculator, at: "2021-03-15T12:00:00Z", value: 11.483870967741936, birth: born), median: 11.483870967741936)
    }
    @Test func infantStandingAddsOneCentimeterBeforeSecondCalendarBirthday() {
        available(result(calculator(), at: "2021-12-31T12:00:00Z", value: 9, metric: .lengthHeight, position: .standing))
        available(result(calculator(month: 24), at: "2022-01-01T12:00:00Z", metric: .lengthHeight, position: .standing))
        available(result(calculator(), at: "2021-12-31T12:00:00Z", metric: .lengthHeight, position: .lying))
    }
    @Test func olderLyingHasNoReferenceComparison() {
        #expect(result(calculator(), at: "2022-01-01T12:00:00Z", metric: .lengthHeight, position: .lying)
                == .unavailable(.unsupportedLengthPosition))
    }
    @Test func lengthPositionBoundaryUsesCorrectedCalendarBirthday() throws {
        let due = try LocalDate("2020-02-01")
        available(result(calculator(), at: "2022-01-31T12:00:00Z", value: 9, metric: .lengthHeight, position: .standing, due: due), corrected: true)
        available(result(calculator(month: 24), at: "2022-02-01T12:00:00Z", metric: .lengthHeight, position: .standing, due: due), corrected: true)
        #expect(result(calculator(), at: "2022-02-01T12:00:00Z", metric: .lengthHeight, position: .lying, due: due)
                == .unavailable(.unsupportedLengthPosition))
    }
    @Test func missingAndInvalidBracketRowsNeverSkipAcrossSourceNodes() {
        #expect(result(calculator(month: 0), at: "2020-01-02T12:00:00Z") == .unavailable(.referenceDataUnavailable))
        let missing = GrowthReferenceCalculator { _, _, month in month == 30 ? nil : .init(l: 1, m: 10, s: 0.1) }
        #expect(result(missing, at: "2022-04-01T12:00:00Z") == .unavailable(.referenceDataUnavailable))
        let invalid = GrowthReferenceCalculator { _, _, month in .init(l: 1, m: month == 1 ? .nan : 10, s: 0.1) }
        #expect(result(invalid, at: "2020-01-02T12:00:00Z") == .unavailable(.invalidReferenceData))
        available(result(invalid)) // An exact node does not depend on the following row.
        let invalidBand = GrowthReferenceCalculator { _, _, month in
            .init(l: 2, m: 10, s: month == 1 ? 0.3 : 0.1)
        }
        #expect(result(invalidBand, at: "2020-01-02T12:00:00Z") == .unavailable(.invalidReferenceData))
    }
    @Test func headNeverHasAReferenceEvenWithSyntheticRows() {
        for sex in [GrowthReferenceSex.girls, .boys] {
            #expect(result(calculator(), metric: .headCircumference, sex: sex) == .unavailable(.noReferenceForMetric))
        }
    }
    @Test func nonfiniteMeasurementDateIsUnavailable() {
        let result = calculator().result(metric: .weight, value: 10, position: nil,
                                         measuredAt: Date(timeIntervalSinceReferenceDate: .nan), timeZoneID: "UTC",
                                         birthDate: birth, expectedDueDate: nil, referenceSex: .girls)
        #expect(result == .unavailable(.invalidDate))
    }
    @Test func fifthCalendarBirthdayIsInclusive() {
        #expect(result(calculator(), at: "2019-12-31T12:00:00Z") == .unavailable(.beforeReferenceStart))
        available(result(calculator(month: 60), at: "2025-01-01T12:00:00Z"))
        #expect(result(calculator(), at: "2025-01-02T12:00:00Z") == .unavailable(.ageOutsideReferenceRange))
    }
    @Test func publishedModelJoinNeighborhoodsHaveFiniteResults() {
        for sex in [GrowthReferenceSex.girls, .boys] {
            for metric in [GrowthMetric.weight, .lengthHeight] {
                for at in ["2021-03-13T12:00:00Z", "2021-03-15T12:00:00Z", "2024-03-31T12:00:00Z", "2024-04-02T12:00:00Z"] {
                    let output = result(GrowthReferenceCalculator(), at: at, value: metric == .weight ? 15 : 100,
                                        metric: metric, position: metric == .lengthHeight ? .standing : nil, sex: sex)
                    guard case let .available(p, m, low, high, _) = output else { Issue.record("Missing join result: \(output)"); continue }
                    #expect(p.isFinite && m.isFinite && low.isFinite && high.isFinite)
                }
            }
        }
    }
    @Test func invalidMeasurementsAndRows() {
        for value in [Double.nan, .infinity, -.infinity, 0, -1] {
            #expect(result(calculator(), value: value) == .unavailable(.invalidValue))
        }
        #expect(result(calculator(), metric: .lengthHeight) == .unavailable(.missingLengthPosition))
        for row in [GrowthLMSRow(l: .nan, m: 10, s: 0.1), .init(l: 1, m: 0, s: 0.1),
                    .init(l: 1, m: 10, s: 0), .init(l: 1, m: .infinity, s: 1), .init(l: 2, m: 10, s: 1)] {
            #expect(result(calculator(row: row)) == .unavailable(.invalidReferenceData))
        }
    }
}
