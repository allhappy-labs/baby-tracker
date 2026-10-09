import Foundation
import BabyCore

public enum GrowthReferenceUnavailableReason: Sendable, Equatable {
    case referenceDataUnavailable
    case invalidReferenceData
    case invalidValue
    case invalidDate
    case noReferenceForMetric
    case unsupportedLengthPosition
    case missingLengthPosition
    case beforeReferenceStart
    case ageOutsideReferenceRange
}

/// Percentile is on a 0...100 scale; reference bands use kg for weight and cm otherwise.
public enum GrowthReferenceResult: Sendable, Equatable {
    case available(percentile: Double, median: Double, p3: Double, p97: Double, correctedAge: Bool)
    case unavailable(GrowthReferenceUnavailableReason)
}

/// One moment's reference percentile lines, in kg for weight and cm otherwise.
public struct GrowthCentiles: Sendable, Equatable {
    public let p3, p10, p25, p50, p75, p90, p97: Double
}

struct GrowthLMSRow: Sendable {
    let l: Double
    let m: Double
    let s: Double

    var isValid: Bool {
        guard l.isFinite, m.isFinite, s.isFinite, m > 0, s > 0,
              let low = quantile(-1.8807936081512509), let high = quantile(1.8807936081512509) else { return false }
        return low < m && m < high
    }

    func quantile(_ z: Double) -> Double? {
        let value: Double
        if l == 0 {
            value = m * exp(s * z)
        } else {
            let offset = l * s * z
            guard offset > -1 else { return nil }
            // log1p preserves the log-normal limit even for very small signed L.
            value = m * exp(log1p(offset) / l)
        }
        return value.isFinite && value > 0 ? value : nil
    }
}

public struct GrowthReferenceCalculator: Sendable {
    private let row: @Sendable (GrowthMetric, GrowthReferenceSex, Int) -> GrowthLMSRow?

    /// Loads only the validated Swiss PEZZ 2025 resource bundled with GrowthFeature.
    public init() {
        let reference = SwissGrowthReference()
        self.row = { reference.row(metric: $0, sex: $1, month: $2) }
    }

    init(row: @escaping @Sendable (GrowthMetric, GrowthReferenceSex, Int) -> GrowthLMSRow?) {
        self.row = row
    }

    /// Takes the recorded value (kg or cm). Position correction affects only comparison,
    /// never the stored measurement. The reference age uses the record's time zone.
    public func result(metric: GrowthMetric, value: Double, position: LengthPosition?, measuredAt: Date,
                       timeZoneID: String, birthDate: LocalDate, expectedDueDate: LocalDate?,
                       referenceSex: GrowthReferenceSex) -> GrowthReferenceResult {
        guard metric != .headCircumference else { return .unavailable(.noReferenceForMetric) }
        guard value.isFinite, value > 0 else { return .unavailable(.invalidValue) }
        let age: ReferenceAge
        switch referenceAge(measuredAt: measuredAt, timeZoneID: timeZoneID, birthDate: birthDate,
                            expectedDueDate: expectedDueDate) {
        case let .success(found): age = found
        case let .failure(reason): return .unavailable(reason.reason)
        }

        var comparisonValue = value
        if metric == .lengthHeight {
            guard let position else { return .unavailable(.missingLengthPosition) }
            if age.measured < age.secondBirthday && position == .standing { comparisonValue += 1 }
            if age.measured >= age.secondBirthday && position == .lying {
                return .unavailable(.unsupportedLengthPosition)
            }
        }
        guard comparisonValue.isFinite, comparisonValue > 0 else { return .unavailable(.invalidValue) }

        let row: GrowthLMSRow
        switch interpolatedRow(metric: metric, referenceSex: referenceSex, age: age) {
        case let .success(found): row = found
        case let .failure(reason): return .unavailable(reason.reason)
        }
        let z: Double
        if row.l == 0 {
            z = log(comparisonValue / row.m) / row.s
        } else {
            z = expm1(row.l * log(comparisonValue / row.m)) / (row.l * row.s)
        }
        guard z.isFinite, let p3 = row.quantile(-Self.bandZ), let p97 = row.quantile(Self.bandZ),
              p3 < row.m, row.m < p97 else { return .unavailable(.invalidReferenceData) }
        let percentile = 50 * erfc(-z / sqrt(2))
        guard percentile.isFinite else { return .unavailable(.invalidReferenceData) }
        return .available(percentile: percentile, median: row.m, p3: p3, p97: p97, correctedAge: age.corrected)
    }

    /// The reference's published percentile lines at one moment, for drawing
    /// the bands behind the readings; nil where `result` would have no
    /// comparison for the date. Lengths are the reference's own lying length
    /// before the second birthday and standing height after it.
    public func centiles(metric: GrowthMetric, at measuredAt: Date, timeZoneID: String, birthDate: LocalDate,
                         expectedDueDate: LocalDate?, referenceSex: GrowthReferenceSex) -> GrowthCentiles? {
        guard metric != .headCircumference,
              case let .success(age) = referenceAge(measuredAt: measuredAt, timeZoneID: timeZoneID,
                                                    birthDate: birthDate, expectedDueDate: expectedDueDate),
              case let .success(row) = interpolatedRow(metric: metric, referenceSex: referenceSex, age: age),
              let p3 = row.quantile(-Self.bandZ), let p10 = row.quantile(-Self.decileZ),
              let p25 = row.quantile(-Self.quartileZ), let p75 = row.quantile(Self.quartileZ),
              let p90 = row.quantile(Self.decileZ), let p97 = row.quantile(Self.bandZ) else { return nil }
        let values = [p3, p10, p25, row.m, p75, p90, p97]
        guard zip(values, values.dropFirst()).allSatisfy({ $0 < $1 }) else { return nil }
        return GrowthCentiles(p3: p3, p10: p10, p25: p25, p50: row.m, p75: p75, p90: p90, p97: p97)
    }

    // Inverse standard-normal CDF at 0.97, 0.90 and 0.75 (mathematical constants).
    private static let bandZ = 1.8807936081512509
    private static let decileZ = 1.2815515655446004
    private static let quartileZ = 0.6744897501960817

    private struct Unavailable: Error { let reason: GrowthReferenceUnavailableReason }

    /// UTC calendar days for the measurement and the reference origin (birth,
    /// or the due date while correcting).
    private struct ReferenceAge {
        let measured: Date
        let start: Date
        let secondBirthday: Date
        let corrected: Bool
        let calendar: Calendar
    }

    private func referenceAge(measuredAt: Date, timeZoneID: String, birthDate: LocalDate,
                              expectedDueDate: LocalDate?) -> Result<ReferenceAge, Unavailable> {
        guard measuredAt.timeIntervalSinceReferenceDate.isFinite,
              let zone = TimeZone(identifier: timeZoneID),
              let measuredDay = try? LocalDate(displaying: measuredAt, in: zone) else {
            return .failure(.init(reason: .invalidDate))
        }
        // Use UTC only after extracting the stored-zone calendar day. Day differences
        // then stay stable across daylight-saving changes and device travel.
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(secondsFromGMT: 0)!
        func date(_ day: LocalDate) -> Date? {
            let parts = day.iso8601.split(separator: "-").compactMap { Int($0) }
            guard parts.count == 3 else { return nil }
            return calendar.date(from: DateComponents(year: parts[0], month: parts[1], day: parts[2]))
        }
        guard let birth = date(birthDate), let measured = date(measuredDay),
              let thirdBirthday = calendar.date(byAdding: .year, value: 3, to: birth) else {
            return .failure(.init(reason: .invalidDate))
        }
        guard measured >= birth else { return .failure(.init(reason: .beforeReferenceStart)) }
        let corrected = expectedDueDate.map { $0 > birthDate } == true && measured < thirdBirthday
        let start: Date
        if corrected, let due = expectedDueDate, let dueDate = date(due) {
            start = dueDate
        } else {
            start = birth
        }
        guard measured >= start else { return .failure(.init(reason: .beforeReferenceStart)) }
        guard let fifthBirthday = calendar.date(byAdding: .month, value: 60, to: start),
              let secondBirthday = calendar.date(byAdding: .month, value: 24, to: start) else {
            return .failure(.init(reason: .invalidDate))
        }
        guard measured <= fifthBirthday else { return .failure(.init(reason: .ageOutsideReferenceRange)) }
        return .success(ReferenceAge(measured: measured, start: start, secondBirthday: secondBirthday,
                                     corrected: corrected, calendar: calendar))
    }

    private func interpolatedRow(metric: GrowthMetric, referenceSex: GrowthReferenceSex,
                                 age: ReferenceAge) -> Result<GrowthLMSRow, Unavailable> {
        let calendar = age.calendar, measured = age.measured
        // Every anniversary is added to the original origin, so short months never
        // drift later anniversaries. Interpolation is an app-authored estimate.
        var lower: (month: Int, date: Date)?
        var selected: GrowthLMSRow?
        for month in SwissGrowthReference.months {
            guard let anniversary = calendar.date(byAdding: .month, value: month, to: age.start) else {
                return .failure(.init(reason: .invalidDate))
            }
            if measured == anniversary {
                guard let exact = row(metric, referenceSex, month) else {
                    return .failure(.init(reason: .referenceDataUnavailable))
                }
                guard exact.isValid else { return .failure(.init(reason: .invalidReferenceData)) }
                selected = exact
                break
            }
            if measured < anniversary {
                guard let lower,
                      let left = row(metric, referenceSex, lower.month),
                      let right = row(metric, referenceSex, month) else {
                    return .failure(.init(reason: .referenceDataUnavailable))
                }
                guard left.isValid, right.isValid else { return .failure(.init(reason: .invalidReferenceData)) }
                guard let elapsed = calendar.dateComponents([.day], from: lower.date, to: measured).day,
                      let span = calendar.dateComponents([.day], from: lower.date, to: anniversary).day,
                      span > 0 else { return .failure(.init(reason: .invalidDate)) }
                let fraction = Double(elapsed) / Double(span)
                selected = GrowthLMSRow(l: left.l + fraction * (right.l - left.l),
                                        m: left.m + fraction * (right.m - left.m),
                                        s: left.s + fraction * (right.s - left.s))
                break
            }
            lower = (month, anniversary)
        }
        guard let row = selected else { return .failure(.init(reason: .referenceDataUnavailable)) }
        guard row.isValid else { return .failure(.init(reason: .invalidReferenceData)) }
        return .success(row)
    }
}
