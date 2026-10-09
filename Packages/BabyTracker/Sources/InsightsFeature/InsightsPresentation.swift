import Foundation
import BabyCore
import BabyUI

struct ChangeText: Equatable, Sendable {
    let value: String
    let caption: String
}

struct InsightRow: Identifiable, Equatable, Sendable {
    let id: String
    let label: String
    let value: String
    /// Fractions 0…1 for the thin split bar; empty for plain rows.
    let shares: [Double]
}

/// One bar in chart units (counts, or hours for sleep), with its words.
struct ChartBar: Identifiable, Equatable, Sendable {
    let id: String
    let primary: Double
    let secondary: Double
    let isEmpty: Bool
    let isToday: Bool
    let label: String
    let valueText: String
    let callout: String
}

struct InsightSection: Identifiable, Equatable, Sendable {
    enum Kind: String, Sendable { case feeding, sleep, diapers }
    let kind: Kind
    let tracker: Tracker
    let title: String
    let headline: String?
    let unit: String
    let change: ChangeText?
    let rows: [InsightRow]
    let bars: [ChartBar]
    /// The period average in chart units, for the dashed rule.
    let average: Double?
    let gridlines: [Double]
    let gridLabels: [String]
    let axisStart: String
    let axisEnd: String
    let accessibilityLabel: String
    var id: String { kind.rawValue }
}

/// Everything Insights says, worded. Factual, never evaluative: a change is
/// an arrow and an amount, in grey.
struct InsightsPresentation {
    let summary: InsightsSummary
    let volumeUnit: VolumeUnit
    /// Threaded through every number, percent and date format so tests (and
    /// a future per-user override) are not at the mercy of the host's
    /// region settings. `ElapsedFormatter.total` and `Volume.formatted` are
    /// app-wide BabyCore formatters and stay on `Locale.current`.
    var locale: Locale = .current

    var sections: [InsightSection] { [feeding, sleep, diapers] }

    // MARK: Sections

    private var feeding: InsightSection {
        let f = summary.feeding
        var rows: [InsightRow] = []
        if f.perDay != nil {
            if let v = f.betweenFeeds { rows.append(row("between", L10n.tr("Between feeds"), duration(v))) }
            if let v = f.longestGap { rows.append(row("gap", L10n.tr("Longest gap"), duration(v))) }
            let kinds = [(L10n.tr("Breast"), f.breastOnly), (L10n.tr("Both"), f.both), (L10n.tr("Bottle"), f.bottleOnly)]
                .filter { $0.1 > 0 }
            if !kinds.isEmpty {
                let shares = Self.percentShares(kinds.map(\.1))
                rows.append(InsightRow(id: "split", label: kinds.map(\.0).joined(separator: " · "),
                                       value: shares.map { percent($0) }.joined(separator: " · "),
                                       shares: shares.map { Double($0) / 100 }))
            }
            if f.leftSeconds + f.rightSeconds > 0 {
                let shares = Self.percentShares([f.leftSeconds, f.rightSeconds])
                rows.append(row("sides", L10n.tr("Left · right"), shares.map { percent($0) }.joined(separator: " · ")))
            }
            if let ml = f.bottlePerDayML { rows.append(row("bottle", L10n.tr("Bottle / day"), volume(ml))) }
        }
        return section(.feeding, tracker: .feed, title: L10n.tr("Feeds"),
                       unit: { self.countUnit($0, "%lld feeds / day", fraction: "feeds / day") },
                       spokenUnit: { self.countUnit($0, "%lld feeds per day", fraction: "feeds per day") }, average: f.perDay, bars: f.bars, rows: rows, scale: 1,
                       headline: self.decimal, change: self.countChange)
    }

    private var sleep: InsightSection {
        let s = summary.sleep
        var rows: [InsightRow] = []
        if s.perDay != nil {
            if let v = s.longestStretch { rows.append(row("stretch", L10n.tr("Longest stretch"), duration(Double(v)))) }
            if let night = s.nightPerDay, let naps = s.napsPerDay {
                rows.append(row("nightNaps", L10n.tr("Night · naps"), "\(duration(night)) · \(duration(naps))"))
            }
            if let v = s.napCountPerDay, v > 0 { rows.append(row("naps", L10n.tr("Naps / day"), self.decimal(v))) }
            if let v = s.awakeBetween { rows.append(row("awake", L10n.tr("Awake between sleeps"), duration(v))) }
        }
        return section(.sleep, tracker: .sleep, title: L10n.tr("Sleep"), unit: { _ in L10n.tr("/ day") },
                       spokenUnit: { _ in L10n.tr("per day") }, average: s.perDay, bars: s.bars, rows: rows, scale: 1.0 / 3600,
                       headline: { self.duration($0) }, change: Self.durationChange)
    }

    private var diapers: InsightSection {
        section(.diapers, tracker: .diaper, title: L10n.tr("Diapers"),
                unit: { self.countUnit($0, "%lld diapers / day", fraction: "diapers / day") },
                spokenUnit: { self.countUnit($0, "%lld diapers per day", fraction: "diapers per day") }, average: summary.diapers.perDay, bars: summary.diapers.bars, rows: [], scale: 1,
                headline: self.decimal, change: self.countChange)
    }

    /// `unit` is drawn beside the headline; `spokenUnit` is the same unit
    /// in words for VoiceOver, which would read "/ day" as "slash day".
    /// Both take the average, whose number decides the unit's form.
    private func section(_ kind: InsightSection.Kind, tracker: Tracker, title: String, unit: (Double) -> String,
                         spokenUnit: (Double) -> String, average: InsightAverage?, bars: [InsightBar], rows: [InsightRow], scale: Double,
                         headline: @escaping (Double) -> String, change: @escaping (Double) -> String) -> InsightSection {
        let changeText = average.map { changeText($0.comparison, change: change) }
        let chartBars = bars.map { chartBar($0, kind: kind, scale: scale, headline: headline) }
        let top = Self.niceTop(chartBars.map { $0.primary + $0.secondary }.max() ?? 0)
        let gridLabel: (Double) -> String = kind == .sleep ? { self.duration($0 * 3600) } : self.decimal
        let label: String
        if let average {
            label = L10n.fmt("%1$@: %2$@ %3$@.", title, headline(average.value), spokenUnit(average.value)) + " " + spoken(average.comparison, change: change)
        } else {
            label = "\(title). \(L10n.tr("Nothing logged in this period."))"
        }
        return InsightSection(
            kind: kind, tracker: tracker, title: title,
            headline: average.map { headline($0.value) }, unit: average.map { unit($0.value) } ?? "", change: changeText,
            rows: rows, bars: chartBars, average: average.map { $0.value * scale },
            gridlines: [top / 2, top], gridLabels: [gridLabel(top / 2), gridLabel(top)],
            axisStart: bars.first.map { axisLabel($0.start) } ?? "",
            axisEnd: summary.range == .all ? (bars.last.map { axisLabel($0.start) } ?? "") : L10n.tr("Today"),
            accessibilityLabel: label
        )
    }

    // MARK: Pieces

    private func row(_ id: String, _ label: String, _ value: String) -> InsightRow {
        InsightRow(id: id, label: label, value: value, shares: [])
    }

    private func chartBar(_ bar: InsightBar, kind: InsightSection.Kind, scale: Double,
                          headline: (Double) -> String) -> ChartBar {
        let naps = (bar.secondary ?? 0)
        let value = bar.value ?? 0
        let label = bar.isToday ? L10n.tr("Today") : barLabel(bar.start)
        var valueText = bar.value.map(headline) ?? "—"
        if bar.isToday { valueText = L10n.fmt("%@, so far", valueText) }
        var callout = "\(label) · \(valueText)"
        if kind == .sleep, bar.value != nil {
            callout += "\n" + L10n.fmt("Night %1$@ · naps %2$@", duration(value - naps), duration(naps))
        }
        return ChartBar(id: bar.start.ISO8601Format(), primary: (kind == .sleep ? value - naps : value) * scale,
                        secondary: kind == .sleep ? naps * scale : 0, isEmpty: bar.value == nil,
                        isToday: bar.isToday, label: label, valueText: valueText, callout: callout)
    }

    private func changeText(_ comparison: InsightComparison, change: (Double) -> String) -> ChangeText {
        switch comparison {
        case .change(let delta):
            return ChangeText(value: change(delta),
                              caption: summary.range == .month ? L10n.tr("vs prev. month") : L10n.tr("vs prev. week"))
        case .coverage(let days):
            return ChangeText(value: L10n.plural("%lld days", days), caption: "")
        case .since(let date):
            return ChangeText(value: L10n.fmt("since %@", axisLabel(date)), caption: "")
        }
    }

    private func spoken(_ comparison: InsightComparison, change: (Double) -> String) -> String {
        let month = summary.range == .month
        switch comparison {
        case .change(let delta):
            let text = change(delta)
            if text == "≈" {
                return month ? L10n.tr("About the same as the previous month.") : L10n.tr("About the same as the previous week.")
            }
            let amount = String(text.dropFirst(2))
            if delta > 0 {
                return L10n.fmt(month ? "%@ more than the previous month." : "%@ more than the previous week.", amount)
            }
            return L10n.fmt(month ? "%@ less than the previous month." : "%@ less than the previous week.", amount)
        case .coverage(let days):
            return L10n.plural("Average of %lld days.", days)
        case .since(let date): return L10n.fmt("Since %@.", axisLabel(date))
        }
    }

    private func barLabel(_ date: Date) -> String {
        summary.range == .all
            ? axisLabel(date)
            : date.formatted(.dateTime.weekday(.abbreviated).day().month(.abbreviated).locale(locale))
    }

    private func axisLabel(_ date: Date) -> String {
        summary.range == .all
            ? date.formatted(.dateTime.day().month(.abbreviated).locale(locale))
            : date.formatted(.dateTime.weekday(.abbreviated).day().locale(locale))
    }

    /// The unit after a per-day count, in the form its number takes:
    /// "1 feed / day", "3 годування / день", "5 годувань / день". The number
    /// is shown to a tenth, so a whole one takes its plural form from the
    /// catalog and one with a tenth (8.4) takes the fraction form, the plain
    /// key. Every count form starts with its number (pinned by
    /// `InsightsPluralTests`), which is dropped: the headline draws it.
    private func countUnit(_ value: Double, _ countKey: String, fraction: String) -> String {
        let shown = (value * 10).rounded() / 10
        guard shown == shown.rounded(), abs(shown) < 1_000_000 else { return L10n.tr(fraction) }
        let count = Int(shown)
        let phrase = L10n.plural(countKey, count)
        let number = String(format: "%lld ", locale: L10n.language, count)
        return phrase.hasPrefix(number) ? String(phrase.dropFirst(number.count)) : L10n.tr(fraction)
    }

    private func duration(_ seconds: Double) -> String { ElapsedFormatter.total(Int(seconds.rounded())) }

    private func volume(_ ml: Double) -> String { Self.volumeText(ml, unit: volumeUnit) }

    private func decimal(_ value: Double) -> String { Self.decimal(value, locale: locale) }

    private func percent(_ value: Int) -> String { Self.percent(value, locale: locale) }

    private func countChange(_ delta: Double) -> String { Self.countChange(delta, locale: locale) }

    // MARK: Formatting rules (static so tests can pin them)

    static func decimal(_ value: Double, locale: Locale) -> String {
        value.formatted(.number.precision(.fractionLength(0...1)).locale(locale))
    }

    static func percent(_ value: Int, locale: Locale) -> String {
        (Double(value) / 100).formatted(.percent.precision(.fractionLength(0)).locale(locale))
    }

    /// Counts are shown to a tenth, so a change that rounds to 0.0 is "≈".
    static func countChange(_ delta: Double, locale: Locale) -> String {
        let rounded = (delta * 10).rounded() / 10
        guard rounded != 0 else { return "≈" }
        return (rounded > 0 ? "▲ " : "▼ ") + decimal(abs(rounded), locale: locale)
    }

    /// Durations are shown in whole minutes.
    static func durationChange(_ delta: Double) -> String {
        guard abs(delta) >= 60 else { return "≈" }
        return (delta > 0 ? "▲ " : "▼ ") + ElapsedFormatter.total(Int(abs(delta)))
    }

    static func volumeText(_ ml: Double, unit: VolumeUnit) -> String {
        unit == .millilitres
            ? Volume(millilitres: ml.rounded()).formatted(as: unit)
            : Volume(millilitres: ml).formatted(as: unit)
    }

    /// Whole percentages that sum to 100 (largest remainder; ties keep
    /// input order so equal shares are reproducible).
    static func percentShares(_ parts: [Int]) -> [Int] {
        let total = parts.reduce(0, +)
        guard total > 0 else { return parts.map { _ in 0 } }
        let exact = parts.map { Double($0) * 100 / Double(total) }
        var shares = exact.map { Int($0) }
        let remainder = exact.indices.map { exact[$0] - Double(shares[$0]) }
        let order = exact.indices.sorted { remainder[$0] != remainder[$1] ? remainder[$0] > remainder[$1] : $0 < $1 }
        for i in order.prefix(100 - shares.reduce(0, +)) { shares[i] += 1 }
        return shares
    }

    /// An even whole number at or above the tallest bar, at least 2.
    static func niceTop(_ value: Double) -> Double {
        let up = max(2, value.rounded(.up))
        return up.truncatingRemainder(dividingBy: 2) == 0 ? up : up + 1
    }
}
