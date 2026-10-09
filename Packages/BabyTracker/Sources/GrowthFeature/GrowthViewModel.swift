import Foundation
import Observation
import BabyCore
import BabyStore

public enum GrowthProfileState: Sendable, Equatable {
    case notLoaded
    case missing
    case configured
}

/// Missing profile needs a user selection; unavailable calculation needs an
/// explanation, not a guessed reference or a substitute percentile.
public enum GrowthPointReference: Sendable, Equatable {
    case missingProfile
    case calculated(GrowthReferenceResult)
}

/// Immutable presentation snapshot. ID identifies the original check-in for
/// editing; values remain recorded kg/cm, never position-adjusted values.
public struct GrowthPoint: Identifiable, Sendable, Equatable {
    public let id: UUID
    public let measuredAt: Date
    public let timeZoneID: String
    public let value: Double
    public let position: LengthPosition?
    public let reference: GrowthPointReference
}

/// The reference lines at one moment of the chart; nil where the reference
/// has no comparison (before the corrected-age start, past five years).
public struct GrowthReferenceSample: Identifiable, Sendable, Equatable {
    public let date: Date
    public let centiles: GrowthCentiles?
    public var id: Date { date }
}

public struct GrowthMetricSummary: Identifiable, Sendable, Equatable {
    public let metric: GrowthMetric
    /// Newest first; equal timestamps use ascending check-in UUID, matching
    /// the store. Distinct same-day and same-time readings are never merged.
    public let points: [GrowthPoint]
    /// Chronological, whole days apart from the first to the last chart point;
    /// empty without a chosen reference, a reference for the metric, or a chart.
    public var referenceCurve: [GrowthReferenceSample] = []
    public var id: GrowthMetric { metric }
    public var latest: GrowthPoint? { points.first }
    public var previousChange: Double? {
        guard points.count >= 2 else { return nil }
        return points[0].value - points[1].value
    }
    /// Chronological chart order, with no invented points between recordings.
    /// A lone reading can have a percentile, but cannot make a trend chart.
    public var chartPoints: [GrowthPoint] {
        points.count >= 2 ? Array(points.reversed()) : []
    }
}

/// The input boundary permits synthetic reference results in model tests
/// without bundling reference data or changing the shipping calculator.
struct GrowthReferenceInput {
    let metric: GrowthMetric
    let value: Double
    let position: LengthPosition?
    let measuredAt: Date
    let timeZoneID: String
    let birthDate: LocalDate
    let expectedDueDate: LocalDate?
    let referenceSex: GrowthReferenceSex
}

struct GrowthCentilesInput: Equatable {
    let metric: GrowthMetric
    let date: Date
    let timeZoneID: String
    let birthDate: LocalDate
    let expectedDueDate: LocalDate?
    let referenceSex: GrowthReferenceSex
}

@MainActor @Observable
public final class GrowthViewModel {
    private let store: BabyStore
    private let babyID: UUID
    private let reference: (GrowthReferenceInput) -> GrowthReferenceResult
    private let centiles: (GrowthCentilesInput) -> GrowthCentiles?
    public private(set) var profileState: GrowthProfileState = .notLoaded
    public private(set) var summaries: [GrowthMetricSummary] = []
    public private(set) var birthDate: Date?
    public private(set) var checkInCount = 0

    public init(store: BabyStore, babyID: UUID, calculator: GrowthReferenceCalculator = .init()) {
        self.store = store
        self.babyID = babyID
        self.reference = { input in
            calculator.result(metric: input.metric, value: input.value, position: input.position,
                measuredAt: input.measuredAt, timeZoneID: input.timeZoneID, birthDate: input.birthDate,
                expectedDueDate: input.expectedDueDate, referenceSex: input.referenceSex)
        }
        self.centiles = { input in
            calculator.centiles(metric: input.metric, at: input.date, timeZoneID: input.timeZoneID,
                birthDate: input.birthDate, expectedDueDate: input.expectedDueDate, referenceSex: input.referenceSex)
        }
    }

    init(store: BabyStore, babyID: UUID,
         reference: @escaping (GrowthReferenceInput) -> GrowthReferenceResult,
         centiles: @escaping (GrowthCentilesInput) -> GrowthCentiles? = { _ in nil }) {
        self.store = store
        self.babyID = babyID
        self.reference = reference
        self.centiles = centiles
    }

    /// Enough samples for the interpolated monthly curves to read as smooth
    /// lines at card width.
    static let referenceSampleCount = 48

    /// From `first` to `last` in whole-day steps, plus `last` itself. The
    /// reference moves once per calendar day, so samples between whole days
    /// would draw the curve as a staircase.
    static func referenceSampleDates(from first: Date, to last: Date) -> [Date] {
        let days = last.timeIntervalSince(first) / 86_400
        guard days > 0 else { return [] }
        let step = max(1, (days / Double(referenceSampleCount - 1)).rounded(.up))
        var dates = stride(from: 0, to: days, by: step).map { first.addingTimeInterval($0 * 86_400) }
        dates.append(last)
        return dates
    }

    public func summary(for metric: GrowthMetric) -> GrowthMetricSummary {
        summaries.first { $0.metric == metric } ?? GrowthMetricSummary(metric: metric, points: [])
    }

    /// The view calls this initially and on store.revision changes, just as
    /// Insights does. Every load re-reads baby/profile dates and all readings;
    /// derived values never enter the store or its sync outbox.
    public func load() throws {
        do {
            guard let baby = try store.baby(babyID), !baby.isTombstoned else {
                throw GrowthStoreError.babyUnavailable
            }
            let profile = try store.growthProfile(for: babyID)
            let referenceSex = profile?.referenceSex
            let readings = try store.measurements(for: babyID)
            let newSummaries = GrowthMetric.allCases.map { metric in
                let points = readings.compactMap { reading -> GrowthPoint? in
                    let value: Double?
                    switch metric {
                    case .weight: value = reading.weightKG
                    case .lengthHeight: value = reading.lengthCM
                    case .headCircumference: value = reading.headCircumferenceCM
                    }
                    guard let value else { return nil }
                    let comparison: GrowthPointReference
                    if metric == .headCircumference {
                        comparison = .calculated(.unavailable(.noReferenceForMetric))
                    } else if let referenceSex {
                        // Legacy births are converted in the record's zone,
                        // keeping comparison stable when the device travels.
                        if let birth = baby.localBirthDate ?? TimeZone(identifier: reading.timeZoneID).flatMap({
                            try? LocalDate(displaying: baby.birthDate, in: $0)
                        }) {
                            comparison = .calculated(reference(GrowthReferenceInput(
                                metric: metric, value: value, position: reading.lengthPosition,
                                measuredAt: reading.measuredAt, timeZoneID: reading.timeZoneID,
                                birthDate: birth, expectedDueDate: profile?.expectedDueDate,
                                referenceSex: referenceSex)))
                        } else {
                            comparison = .calculated(.unavailable(.invalidDate))
                        }
                    } else {
                        comparison = .missingProfile
                    }
                    return GrowthPoint(id: reading.id, measuredAt: reading.measuredAt,
                        timeZoneID: reading.timeZoneID, value: value, position: reading.lengthPosition,
                        reference: comparison)
                }
                var summary = GrowthMetricSummary(metric: metric, points: points)
                // Sampled in the newest reading's zone, like that reading's own comparison.
                if metric != .headCircumference, let referenceSex, let first = summary.chartPoints.first,
                   let last = summary.chartPoints.last,
                   let birth = baby.localBirthDate ?? TimeZone(identifier: last.timeZoneID).flatMap({
                       try? LocalDate(displaying: baby.birthDate, in: $0)
                   }) {
                    summary.referenceCurve = Self.referenceSampleDates(from: first.measuredAt, to: last.measuredAt).map { date in
                        GrowthReferenceSample(date: date, centiles: centiles(GrowthCentilesInput(
                            metric: metric, date: date, timeZoneID: last.timeZoneID, birthDate: birth,
                            expectedDueDate: profile?.expectedDueDate, referenceSex: referenceSex)))
                    }
                }
                return summary
            }
            summaries = newSummaries
            birthDate = baby.birthDate
            checkInCount = readings.count
            profileState = referenceSex == nil ? .missing : .configured
        } catch {
            // A realm switch or failed read must not leave another family's
            // previous values presented as current data.
            summaries = []
            birthDate = nil
            checkInCount = 0
            profileState = .notLoaded
            throw error
        }
    }
}
