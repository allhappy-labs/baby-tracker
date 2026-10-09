import Foundation
import BabyCore

/// How far back Insights reaches. Today is never part of a period: a day
/// still in progress compared with whole days says nothing true.
public enum InsightRange: String, CaseIterable, Identifiable, Sendable {
    case week, month, all

    public var id: String { rawValue }
    /// Complete days in the period; nil for All, which starts at the first
    /// logged day.
    var days: Int? {
        switch self {
        case .week: 7
        case .month: 30
        case .all: nil
        }
    }
    public var title: String {
        switch self {
        case .week: L10n.tr("Week")
        case .month: L10n.tr("Month")
        case .all: L10n.tr("All")
        }
    }
}

// MARK: - Inputs

/// One feeding session, as `BabyStore.feedingSessions` projects it.
struct FeedSessionInput: Equatable, Sendable {
    var startedAt: Date
    var hasBreast = false
    var hasBottle = false
    var leftSeconds = 0
    var rightSeconds = 0
}

/// One measured bottle, on the day its step happened (`measuredBottleSteps`).
struct BottleInput: Equatable, Sendable {
    var at: Date
    var volumeML: Double
}

/// One sleep session. `endedAt` is nil while it is still running.
struct SleepSessionInput: Equatable, Sendable {
    var startedAt: Date
    var endedAt: Date?
    var asleepSeconds: Int
    var kind: SleepKind
    /// Each completed part's length: one continuous sleep each.
    var partSeconds: [Int]
}

struct InsightsInput: Equatable, Sendable {
    var feeds: [FeedSessionInput] = []
    var bottles: [BottleInput] = []
    var sleeps: [SleepSessionInput] = []
    var diapers: [Date] = []
}

// MARK: - Output

enum InsightComparison: Equatable, Sendable {
    /// Current average minus the previous period's, in the metric's own unit.
    case change(Double)
    /// No days in the previous period: the average covers this many days.
    case coverage(days: Int)
    /// All: the first counted day.
    case since(Date)
}

struct InsightAverage: Equatable, Sendable {
    let value: Double
    let comparison: InsightComparison
}

/// One bar: a day (Week, Month) or a 7-day block (All). `value` is nil when
/// nothing of the section's kind was logged; `secondary` is the naps share
/// of a sleep bar.
struct InsightBar: Equatable, Sendable {
    let start: Date
    let value: Double?
    let secondary: Double?
    let isToday: Bool
}

struct FeedingInsights: Equatable, Sendable {
    var perDay: InsightAverage?
    var bars: [InsightBar] = []
    var betweenFeeds: TimeInterval?
    var longestGap: TimeInterval?
    var breastOnly = 0
    var both = 0
    var bottleOnly = 0
    var leftSeconds = 0
    var rightSeconds = 0
    /// nil when no bottle was given in the period.
    var bottlePerDayML: Double?
}

struct SleepInsights: Equatable, Sendable {
    /// Seconds asleep per day.
    var perDay: InsightAverage?
    var bars: [InsightBar] = []
    var longestStretch: Int?
    var nightPerDay: Double?
    var napsPerDay: Double?
    var napCountPerDay: Double?
    var awakeBetween: TimeInterval?
}

struct DiaperInsights: Equatable, Sendable {
    var perDay: InsightAverage?
    var bars: [InsightBar] = []
}

struct InsightsSummary: Equatable, Sendable {
    var range: InsightRange
    var feeding = FeedingInsights()
    var sleep = SleepInsights()
    var diapers = DiaperInsights()

    static let empty = InsightsSummary(range: .week)
}

// MARK: - Calculator

/// Every number on Insights, from plain values. Days are indexes relative to
/// today in the device's zone: 0 is today, -1 yesterday.
struct InsightsCalculator {
    let calendar: Calendar

    init(timeZone: TimeZone) {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = timeZone
        self.calendar = calendar
    }

    /// The period's day indexes and the one before it.
    struct Period {
        let days: ClosedRange<Int>?
        let previous: ClosedRange<Int>?
    }

    func summary(_ input: InsightsInput, range: InsightRange, now: Date) -> InsightsSummary {
        let today = calendar.startOfDay(for: now)
        let sleeps = input.sleeps.filter { $0.endedAt != nil }
        let firstDay = (input.feeds.map(\.startedAt) + sleeps.map(\.startedAt) + input.diapers)
            .map { index($0, today: today) }.min()
        let period = period(for: range, firstDay: firstDay)

        var result = InsightsSummary(range: range)
        var diaperDays: [Int: Double] = [:]
        for date in input.diapers { diaperDays[index(date, today: today), default: 0] += 1 }
        result.diapers.perDay = average(diaperDays, period: period, range: range, today: today)
        result.diapers.bars = bars(diaperDays, period: period, range: range, today: today)

        var feedDays: [Int: Double] = [:]
        for feed in input.feeds { feedDays[index(feed.startedAt, today: today), default: 0] += 1 }
        result.feeding.perDay = average(feedDays, period: period, range: range, today: today)
        result.feeding.bars = bars(feedDays, period: period, range: range, today: today)

        if let days = period.days {
            let inPeriod = { (date: Date) in days.contains(self.index(date, today: today)) }
            let starts = input.feeds.map(\.startedAt).sorted()
            let gaps = zip(starts, starts.dropFirst())
                .filter { inPeriod($0.1) }
                .map { $0.1.timeIntervalSince($0.0) }
            result.feeding.betweenFeeds = Self.median(gaps)
            result.feeding.longestGap = gaps.max()

            for feed in input.feeds where inPeriod(feed.startedAt) {
                switch (feed.hasBreast, feed.hasBottle) {
                case (true, true): result.feeding.both += 1
                case (true, false): result.feeding.breastOnly += 1
                case (false, true): result.feeding.bottleOnly += 1
                case (false, false): break   // solids only
                }
                result.feeding.leftSeconds += feed.leftSeconds
                result.feeding.rightSeconds += feed.rightSeconds
            }

            var bottleDays: [Int: Double] = [:]
            for step in input.bottles { bottleDays[index(step.at, today: today), default: 0] += step.volumeML }
            let counted = feedDays.keys.filter { days.contains($0) }
            if !counted.isEmpty, counted.contains(where: { (bottleDays[$0] ?? 0) > 0 }) {
                result.feeding.bottlePerDayML = counted.map { bottleDays[$0] ?? 0 }.reduce(0, +) / Double(counted.count)
            }
        }

        var sleepDays: [Int: Double] = [:]
        var nightDays: [Int: Double] = [:]
        var napDays: [Int: Double] = [:]
        var napCounts: [Int: Double] = [:]
        for sleep in sleeps {
            let day = index(sleep.startedAt, today: today)
            sleepDays[day, default: 0] += Double(sleep.asleepSeconds)
            switch sleep.kind {
            case .night: nightDays[day, default: 0] += Double(sleep.asleepSeconds)
            case .nap:
                napDays[day, default: 0] += Double(sleep.asleepSeconds)
                napCounts[day, default: 0] += 1
            }
        }
        result.sleep.perDay = average(sleepDays, period: period, range: range, today: today)
        result.sleep.bars = bars(sleepDays, secondary: napDays, period: period, range: range, today: today)
        if let days = period.days {
            let counted = sleepDays.keys.filter { days.contains($0) }
            if !counted.isEmpty {
                let mean = { (values: [Int: Double]) in counted.map { values[$0] ?? 0 }.reduce(0, +) / Double(counted.count) }
                result.sleep.nightPerDay = mean(nightDays)
                result.sleep.napsPerDay = mean(napDays)
                result.sleep.napCountPerDay = mean(napCounts)
            }
            let periodSleeps = sleeps.filter { days.contains(index($0.startedAt, today: today)) }
            result.sleep.longestStretch = periodSleeps.flatMap(\.partSeconds).max()

            let ordered = sleeps.sorted { $0.startedAt < $1.startedAt }
            let gaps: [Double] = zip(ordered, ordered.dropFirst()).compactMap { earlier, later in
                guard let end = earlier.endedAt else { return nil }
                let gap = later.startedAt.timeIntervalSince(end)
                let hour = calendar.component(.hour, from: end)
                guard gap > 0, gap <= 12 * 3600,
                      hour >= SleepClassifier.dayStartHour, hour < SleepClassifier.nightStartHour,
                      days.contains(index(end, today: today)) else { return nil }
                return gap
            }
            result.sleep.awakeBetween = Self.median(gaps)
        }
        return result
    }

    // MARK: Days

    func index(_ date: Date, today: Date) -> Int {
        calendar.dateComponents([.day], from: today, to: calendar.startOfDay(for: date)).day ?? 0
    }

    func date(_ index: Int, today: Date) -> Date {
        calendar.date(byAdding: .day, value: index, to: today) ?? today
    }

    func period(for range: InsightRange, firstDay: Int?) -> Period {
        if let n = range.days {
            return Period(days: -n ... -1, previous: (-2 * n) ... (-n - 1))
        }
        guard let firstDay, firstDay <= -1 else { return Period(days: nil, previous: nil) }
        return Period(days: firstDay ... -1, previous: nil)
    }

    /// The mean over counted days (days with an entry in `values`).
    func average(_ values: [Int: Double], period: Period, range: InsightRange, today: Date) -> InsightAverage? {
        guard let days = period.days else { return nil }
        let current = values.filter { days.contains($0.key) }
        guard let first = current.keys.min() else { return nil }
        let mean = current.values.reduce(0, +) / Double(current.count)
        guard let previousDays = period.previous else {
            return InsightAverage(value: mean, comparison: .since(date(first, today: today)))
        }
        let previous = values.filter { previousDays.contains($0.key) }
        guard !previous.isEmpty else {
            return InsightAverage(value: mean, comparison: .coverage(days: current.count))
        }
        let previousMean = previous.values.reduce(0, +) / Double(previous.count)
        return InsightAverage(value: mean, comparison: .change(mean - previousMean))
    }

    /// Week and Month: one bar per day plus today. All: one bar per 7-day
    /// block counting back from yesterday, oldest first.
    func bars(_ values: [Int: Double], secondary: [Int: Double]? = nil,
              period: Period, range: InsightRange, today: Date) -> [InsightBar] {
        func second(_ i: Int) -> Double? {
            guard let secondary, values[i] != nil else { return nil }
            return secondary[i] ?? 0
        }
        if let n = range.days {
            return (-n ... 0).map { i in
                InsightBar(start: date(i, today: today), value: values[i], secondary: second(i), isToday: i == 0)
            }
        }
        guard let days = period.days else { return [] }
        var blocks: [InsightBar] = []
        var end = -1
        while end >= days.lowerBound {
            let start = max(end - 6, days.lowerBound)
            let counted = (start ... end).filter { values[$0] != nil }
            let mean: (([Int]) -> Double?) = { indexes in
                indexes.isEmpty ? nil : indexes.map { values[$0]! }.reduce(0, +) / Double(indexes.count)
            }
            let naps: Double? = (secondary == nil || counted.isEmpty) ? nil
                : counted.map { secondary![$0] ?? 0 }.reduce(0, +) / Double(counted.count)
            blocks.append(InsightBar(start: date(start, today: today), value: mean(counted), secondary: naps, isToday: false))
            end = start - 1
        }
        return blocks.reversed()
    }

    static func median(_ values: [Double]) -> Double? {
        let sorted = values.sorted()
        guard !sorted.isEmpty else { return nil }
        let middle = sorted.count / 2
        return sorted.count.isMultiple(of: 2) ? (sorted[middle - 1] + sorted[middle]) / 2 : sorted[middle]
    }
}
