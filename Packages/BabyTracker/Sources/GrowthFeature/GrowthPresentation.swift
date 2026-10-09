import Foundation
import BabyCore

/// One unbroken stretch of the reference curve, by its first sample's date.
struct GrowthReferenceBand: Identifiable {
    let id: Date
    let samples: [(date: Date, centiles: GrowthCentiles)]
}

struct GrowthMetricPresentation {
    let summary: GrowthMetricSummary
    /// Threaded through every number/date/percentile format so tests (and a
    /// future per-user override) are not at the mercy of the host's region
    /// settings.
    var locale: Locale = .current

    var title: String {
        switch summary.metric {
        case .weight: GrowthCopy.text("Weight")
        case .lengthHeight: GrowthCopy.text("Length / height")
        case .headCircumference: GrowthCopy.text("Head circumference")
        }
    }
    var chartPoints: [GrowthPoint] { summary.chartPoints }
    func valueText(_ value: Double) -> String {
        let number = value.formatted(.number.precision(.significantDigits(1...6)).locale(locale))
        return GrowthCopy.format(summary.metric == .weight ? "%@ kg" : "%@ cm", number)
    }
    func dateText(_ point: GrowthPoint) -> String {
        point.measuredAt.formatted(Date.FormatStyle(date: .abbreviated, time: .shortened, locale: locale,
            timeZone: TimeZone(identifier: point.timeZoneID) ?? .current))
    }
    func referenceText(_ point: GrowthPoint) -> String {
        if summary.metric == .headCircumference {
            return GrowthCopy.text("No reference percentile in this release")
        }
        switch point.reference {
        case .missingProfile: return GrowthCopy.text("Choose a growth reference to enable comparisons.")
        case .calculated(.unavailable(.referenceDataUnavailable)), .calculated(.unavailable(.invalidReferenceData)):
            return GrowthCopy.text("Reference data is unavailable.")
        case .calculated(.unavailable(.noReferenceForMetric)):
            return GrowthCopy.text("No reference percentile in this release")
        case .calculated(.unavailable(.unsupportedLengthPosition)):
            return GrowthCopy.text("Lying length has no reference comparison from the second reference-age birthday.")
        case .calculated(.unavailable(.missingLengthPosition)):
            return GrowthCopy.text("Choose lying down or standing up for a reference comparison.")
        case .calculated(.unavailable(.beforeReferenceStart)):
            return GrowthCopy.text("This reading is before the birth or corrected-age reference start.")
        case .calculated(.unavailable(.ageOutsideReferenceRange)):
            return GrowthCopy.text("This reading is outside the birth-to-five reference range.")
        case .calculated(.unavailable(.invalidDate)):
            return GrowthCopy.text("The measurement date or time zone is unavailable for comparison.")
        case .calculated(.unavailable(.invalidValue)):
            return GrowthCopy.text("Reference comparison is unavailable for this measurement.")
        case let .calculated(.available(percentile, _, _, _, corrected)):
            // Classify tails before rounding: an estimated rank of 3.1 is 3,
            // while an actual rank below 3 is shown only as the published threshold.
            let value = percentile < 3 ? GrowthCopy.text("<3rd") : percentile > 97 ? GrowthCopy.text(">97th")
                : percentile.formatted(.number.precision(.fractionLength(0)).locale(locale))
            return GrowthCopy.format(corrected ? "Estimated Swiss PEZZ 2025 percentile %@ · corrected age"
                                              : "Estimated Swiss PEZZ 2025 percentile %@", value)
        }
    }
    func accessibilityLabel(for point: GrowthPoint) -> String {
        [title, dateText(point), valueText(point.value), referenceText(point)].joined(separator: ". ")
    }
    /// Never connect the reference across a gap in it, or draw a one-sample band.
    var referenceBands: [GrowthReferenceBand] {
        guard summary.metric != .headCircumference else { return [] }
        var result: [GrowthReferenceBand] = []
        var run: [(date: Date, centiles: GrowthCentiles)] = []
        func finish() {
            if run.count >= 2, let first = run.first { result.append(.init(id: first.date, samples: run)) }
            run = []
        }
        for sample in summary.referenceCurve {
            if let centiles = sample.centiles { run.append((sample.date, centiles)) }
            else { finish() }
        }
        finish()
        return result
    }
    /// The curve's right-hand end, where the band edges are labelled.
    var trailingCentiles: GrowthCentiles? { referenceBands.last?.samples.last?.centiles }
    /// Fitted to the bands and readings: the automatic scale rounds out to
    /// whole axis steps (0–10 kg for a 3–6 kg band), flattening the bands.
    var yDomain: ClosedRange<Double> {
        let values = referenceBands.flatMap(\.samples).flatMap { [$0.centiles.p3, $0.centiles.p97] }
            + chartPoints.map(\.value)
        guard let low = values.min(), let high = values.max() else { return 0...1 }
        // Equal readings still get a visible band of axis around them.
        let margin = high > low ? (high - low) * 0.06 : max(abs(low) * 0.05, 0.1)
        return (low - margin)...(high + margin)
    }
}

enum GrowthCopy {
    static func text(_ key: String) -> String {
        String(localized: String.LocalizationValue(key), bundle: .module)
    }
    static func format(_ key: String, _ values: CVarArg...) -> String {
        String(format: text(key), locale: Locale.current, arguments: values)
    }
}

struct GrowthChip: Equatable {
    let metric: GrowthMetric
    let title: String
    let value: String
    /// "P48", "<P3", ">P97" for no reference; nil when the girls/boys
    /// reference has not been chosen.
    let percentile: String?
}

/// The Insights growth section's words. Growth ignores the range picker.
struct GrowthSectionPresentation {
    let summaries: [GrowthMetricSummary]
    let birthDate: Date?
    /// Threaded through every number/date/measurement format, as with
    /// `GrowthMetricPresentation`.
    var locale: Locale = .current

    func summary(_ metric: GrowthMetric) -> GrowthMetricSummary {
        summaries.first { $0.metric == metric } ?? GrowthMetricSummary(metric: metric, points: [])
    }

    var isEmpty: Bool { summaries.allSatisfy { $0.points.isEmpty } }

    func chip(_ metric: GrowthMetric) -> GrowthChip {
        let summary = summary(metric)
        let display = GrowthMetricPresentation(summary: summary, locale: locale)
        let title = switch metric {
        case .weight: GrowthCopy.text("Weight")
        case .lengthHeight: GrowthCopy.text("Length")
        case .headCircumference: GrowthCopy.text("Head")
        }
        return GrowthChip(metric: metric, title: title,
                          value: summary.latest.map { display.valueText($0.value) } ?? "—",
                          percentile: summary.latest.flatMap(shortReference) ?? (summary.latest == nil ? "—" : nil))
    }

    func shortReference(_ point: GrowthPoint) -> String? {
        switch point.reference {
        case .missingProfile: return nil
        case let .calculated(.available(percentile, _, _, _, _)):
            // Tails are classified before rounding, as in the full wording.
            if percentile < 3 { return GrowthCopy.text("<P3") }
            if percentile > 97 { return GrowthCopy.text(">P97") }
            return GrowthCopy.format("P%@", percentile.formatted(.number.precision(.fractionLength(0)).locale(locale)))
        case .calculated(.unavailable): return "—"
        }
    }

    /// "+420 g since 18 Sep · ≈ 30 g / day"; length and head give the change
    /// only, and so does weight when the two readings are under a day apart
    /// (a rate over hours would be scaled up into noise).
    func changeLine(_ metric: GrowthMetric) -> String? {
        let points = summary(metric).points
        guard points.count >= 2 else { return nil }
        let latest = points[0], previous = points[1]
        let delta = latest.value - previous.value
        let zone = TimeZone(identifier: previous.timeZoneID) ?? .current
        let since = previous.measuredAt.formatted(Date.FormatStyle(locale: locale, timeZone: zone).day().month(.abbreviated))
        let amount = metric == .weight
            ? signed(delta * 1000, unit: UnitMass.grams, fraction: 0)
            : signed(delta, unit: UnitLength.centimeters, fraction: 1)
        var line = GrowthCopy.format("%1$@ since %2$@", amount, since)
        let days = latest.measuredAt.timeIntervalSince(previous.measuredAt) / 86_400
        if metric == .weight, days >= 1 {
            let perDay = Measurement(value: (delta * 1000 / days).rounded(), unit: UnitMass.grams)
                .formatted(.measurement(width: .abbreviated, usage: .asProvided,
                    numberFormatStyle: .number.precision(.fractionLength(0)).locale(locale)).locale(locale))
            line += " · " + GrowthCopy.format("≈ %@ / day", perDay)
        }
        return line
    }

    /// "2 Oct · 9 wk": the newest check-in of any metric and the baby's age
    /// then — in days under two weeks, where "0w" or "1w" says too little.
    var headerText: String? {
        guard let latest = summaries.compactMap(\.latest).max(by: { $0.measuredAt < $1.measuredAt }) else { return nil }
        let zone = TimeZone(identifier: latest.timeZoneID) ?? .current
        let date = latest.measuredAt.formatted(Date.FormatStyle(locale: locale, timeZone: zone).day().month(.abbreviated))
        guard let birthDate, latest.measuredAt > birthDate else { return date }
        let formatter = DateComponentsFormatter()
        formatter.unitsStyle = .abbreviated
        formatter.maximumUnitCount = 1
        formatter.calendar = { var c = Calendar.current; c.locale = locale; return c }()
        let weeks = latest.measuredAt.timeIntervalSince(birthDate) / (7 * 86_400)
        formatter.allowedUnits = weeks < 2 ? [.day] : weeks < 13 ? [.weekOfMonth] : [.month]
        guard let age = formatter.string(from: birthDate, to: latest.measuredAt) else { return date }
        return "\(date) · \(age)"
    }

    private func signed(_ value: Double, unit: Dimension, fraction: Int) -> String {
        let magnitude = Measurement(value: abs(value), unit: unit)
            .formatted(.measurement(width: .abbreviated, usage: .asProvided,
                                    numberFormatStyle: .number.precision(.fractionLength(0...fraction)).locale(locale)).locale(locale))
        return (value < 0 ? "−" : "+") + magnitude
    }
}
