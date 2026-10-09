import Foundation
import BabyCore
import BabyStore

/// What the tools returned while answering one question.
@MainActor
final class AskToolRecord {
    static let callLimit = 6

    private(set) var toolNames: [String] = []
    private(set) var entrySources: [AskSource] = []
    private(set) var windows: [AskWindow] = []
    private(set) var usedTotals = false

    func begin(_ name: String) -> Bool {
        guard toolNames.count < Self.callLimit else { return false }
        toolNames.append(name)
        return true
    }

    func shown(_ source: AskSource) {
        if !entrySources.contains(source) { entrySources.append(source) }
    }

    func read(_ window: AskWindow, totals: Bool = false) {
        windows.append(window)
        if totals { usedTotals = true }
    }

    /// A handful of entries link back to History; anything broader gets a
    /// caption naming the range that was read instead.
    var sources: [AskSource] {
        !usedTotals && entrySources.count <= 5 ? entrySources : []
    }

    var queriedRange: AskWindow? {
        guard sources.isEmpty, let start = windows.map(\.start).min(),
              let end = windows.map(\.end).max() else { return nil }
        return AskWindow(start: start, end: end)
    }
}

/// Read-only answers to the three Ask tools, as compact text in local time.
/// It sees one child, never more than a year back, and never writes.
@MainActor
final class AskCareLog {
    static let limitReached = "Tool call limit reached; answer with what you have."
    static let unreadable = "The log could not be read right now."
    /// Sized for the on-device model's ~4K-token window.
    static let logLines = 15
    static let dayLines = 14

    let record: AskToolRecord
    private let store: BabyStore
    private let babyID: UUID
    private let now: Date
    private let text: AskText
    private let volumeUnit: VolumeUnit

    init(store: BabyStore, babyID: UUID, now: Date, timeZone: TimeZone, volumeUnit: VolumeUnit,
         record: AskToolRecord = AskToolRecord()) {
        self.store = store
        self.babyID = babyID
        self.now = now
        self.text = AskText(timeZone: timeZone)
        self.volumeUnit = volumeUnit
        self.record = record
    }

    func log(range: String, date: String, kind: String) -> String {
        guard record.begin("careLog") else { return Self.limitReached }
        guard ["feed", "sleep", "diaper", "med", "all"].contains(kind) else {
            return "Use kind feed, sleep, diaper, med or all."
        }
        let window: AskWindow
        switch resolve(range: range, date: date) {
        case .success(let value): window = value
        case .failure(let correction): return correction.message
        }
        do {
            let items = try store.entries(for: babyID, from: window.start, to: window.end)
                .filter { kind == "all" || Self.kind(of: $0) == kind }
            record.read(window)
            guard !items.isEmpty else {
                let what = kind == "all" ? "" : "\(kind) "
                return "No \(what)entries logged from \(text.time(window.start)) to \(text.time(window.end))."
            }
            let shown = items.prefix(Self.logLines)
            var lines = ["Logged entries from \(text.time(window.start)) to \(text.time(window.end)):",
                         entrySummary(items)]
            lines += shown.map { item in
                record.shown(Self.source(of: item))
                return line(for: item)
            }
            if items.count > shown.count {
                lines.append("\(items.count - shown.count) more entries not shown.")
            }
            return lines.joined(separator: "\n")
        } catch {
            return Self.unreadable
        }
    }

    func totals(range: String, date: String, byDay: Bool) -> String {
        guard record.begin("careTotals") else { return Self.limitReached }
        let window: AskWindow
        switch resolve(range: range, date: date) {
        case .success(let value): window = value
        case .failure(let correction): return correction.message
        }
        let days = dayBuckets(window)
        // The answering model can't ask again, so a long range still gets
        // its total, with a note instead of day lines.
        let tooManyDays = byDay && days.count > Self.dayLines
        let byDay = byDay && !tooManyDays
        let buckets = byDay ? days : [window]
        do {
            let data = try CareData(store: store, babyID: babyID, window: window)
            record.read(window, totals: true)
            var lines = buckets.map { bucket in
                let label = byDay
                    ? text.day(bucket.start) + (isPartialDay(bucket) ? " (partial)" : "")
                    : "\(text.time(window.start)) to \(text.time(window.end))"
                return "\(label): \(summary(data, in: bucket))"
            }
            if byDay && buckets.count > 1 { lines.append("Total: \(summary(data, in: window))") }
            if tooManyDays { lines.append("Daily lines omitted: \(days.count) days is more than \(Self.dayLines).") }
            return lines.joined(separator: "\n")
        } catch {
            return Self.unreadable
        }
    }

    func lastEntries() -> String {
        guard record.begin("lastEntries") else { return Self.limitReached }
        do {
            let items = try store.entries(for: babyID, from: now.addingTimeInterval(-365 * 86_400), to: now)
            return ["feed", "sleep", "diaper", "med"].map { kind in
                guard let item = items.first(where: { Self.kind(of: $0) == kind }) else {
                    return "\(kind): none logged in the past year"
                }
                record.shown(Self.source(of: item))
                let ago = AskText.ago(Int(now.timeIntervalSince(item.startedAt)))
                return "\(kind), \(ago) ago: \(line(for: item))"
            }.joined(separator: "\n")
        } catch {
            return Self.unreadable
        }
    }

    /// Runs one planned read and labels its output so the answer can tell
    /// several reads apart.
    func run(_ read: AskRead) -> String {
        let period = read.range == "date" ? read.date : read.range
        switch read.tool {
        case "entries":
            let label = read.kind == "all" ? "entries, \(period)" : "entries, \(period), \(read.kind)"
            return "[\(label)]\n" + log(range: read.range, date: read.date, kind: read.kind)
        case "latest":
            return "[latest entries]\n" + lastEntries()
        default:
            return "[totals, \(period)]\n" + totals(range: read.range, date: read.date, byDay: read.byDay)
        }
    }

    // MARK: Lines

    /// Counts, the newest entry and the longest sleep, worked out here: the
    /// small model misreads them from a long list.
    private func entrySummary(_ items: [HistoryItem]) -> String {
        var parts: [String] = []
        let feeds = items.filter { Self.kind(of: $0) == "feed" }
        if let newest = feeds.first {
            parts.append("feed entries \(feeds.count), newest \(text.time(newest.startedAt))")
        }
        let sleeps = items.compactMap { item -> SleepEntry? in
            if case .sleep(let entry) = item { return entry }
            return nil
        }
        if let newest = sleeps.first {
            var part = "sleeps \(sleeps.count), newest \(text.span(newest.startedAt, newest.endedAt))"
            if let longest = sleeps.max(by: { ($0.durationSeconds ?? 0) < ($1.durationSeconds ?? 0) }),
               let seconds = longest.durationSeconds {
                part += ", longest \(AskText.duration(seconds)) (\(text.span(longest.startedAt, longest.endedAt)))"
            }
            parts.append(part)
        }
        let diapers = items.compactMap { item -> DiaperEntry? in
            if case .diaper(let entry) = item { return entry }
            return nil
        }
        if let newest = diapers.first {
            let split = DiaperContents.allCases.compactMap { contents -> String? in
                let count = diapers.filter { $0.contents == contents }.count
                return count == 0 ? nil : "\(contents.rawValue) \(count)"
            }
            parts.append("diapers \(diapers.count) (\(split.joined(separator: ", "))), newest \(text.time(newest.startedAt))")
        }
        let meds = items.filter { Self.kind(of: $0) == "med" }
        if let newest = meds.first {
            parts.append("meds \(meds.count), newest \(text.time(newest.startedAt))")
        }
        return "Summary: \(parts.joined(separator: "; "))."
    }

    func line(for item: HistoryItem) -> String {
        switch item {
        case .feed(let entry):
            let steps = entry.components.map { Self.describe($0, volumeUnit: volumeUnit) }
            let detail = steps.isEmpty ? "no details" : steps.joined(separator: ", ")
            return "\(text.span(entry.startedAt, entry.endedAt)) · feed · \(detail)\(Self.note(entry.note))"
        case .sleep(let entry):
            let length = entry.durationSeconds.map(AskText.duration) ?? "no end time"
            return "\(text.span(entry.startedAt, entry.endedAt)) · sleep · \(length)\(Self.note(entry.note))"
        case .diaper(let entry):
            return "\(text.time(entry.startedAt)) · diaper · \(entry.contents.rawValue)\(Self.note(entry.note))"
        case .supplement(let entry):
            return "\(text.time(entry.startedAt)) · med · \(entry.name)\(Self.note(entry.note))"
        }
    }

    nonisolated static func describe(_ component: FeedComponent, volumeUnit: VolumeUnit) -> String {
        switch component {
        case .breast(_, let side, let seconds, _, _):
            "\(side.rawValue) breast \(AskText.duration(seconds))"
        case .bottle(_, let volumeML, let contents, _, _, _):
            "\(milk(contents)) bottle \(Volume(millilitres: volumeML).formatted(as: volumeUnit))"
        case .solids(_, let foods, let amount):
            "solids \(foods.joined(separator: ", ")) (\(amount.rawValue))"
        }
    }

    nonisolated static func milk(_ contents: BottleContents) -> String {
        switch contents {
        case .breastMilk: "breast milk"
        case .formula: "formula"
        case .mixed: "mixed milk"
        }
    }

    static func kind(of item: HistoryItem) -> String {
        switch item {
        case .feed: "feed"
        case .sleep: "sleep"
        case .diaper: "diaper"
        case .supplement: "med"
        }
    }

    static func source(of item: HistoryItem) -> AskSource {
        let kind = switch item {
        case .feed: "feed"
        case .sleep: "sleep"
        case .diaper: "diaper"
        case .supplement: "supplement"
        }
        return .entry(kind: kind, id: item.id)
    }

    private static func note(_ note: String?) -> String {
        guard let note, !note.isEmpty else { return "" }
        return " · note: \"\(note.prefix(120))\""
    }

    // MARK: Windows

    /// The periods the model picks from. Named ranges spare the small
    /// on-device model date arithmetic it gets wrong; `date` covers any
    /// other single day.
    nonisolated static let ranges = ["today", "yesterday", "last 24 hours", "last 3 days", "last 7 days",
                         "previous 7 days", "last 30 days", "date"]

    struct Correction: Error {
        let message: String
    }

    func window(range: String, date: String) -> AskWindow? {
        try? resolve(range: range, date: date).get()
    }

    private func resolve(range: String, date: String) -> Result<AskWindow, Correction> {
        let calendar = text.calendar
        let today = calendar.startOfDay(for: now)
        func day(_ offset: Int) -> Date { calendar.date(byAdding: .day, value: offset, to: today)! }
        switch range {
        case "today": return .success(AskWindow(start: today, end: now))
        case "yesterday": return .success(AskWindow(start: day(-1), end: today))
        case "last 24 hours": return .success(AskWindow(start: now.addingTimeInterval(-86_400), end: now))
        case "last 3 days": return .success(AskWindow(start: day(-2), end: now))
        case "last 7 days": return .success(AskWindow(start: day(-6), end: now))
        case "previous 7 days": return .success(AskWindow(start: day(-13), end: day(-6)))
        case "last 30 days": return .success(AskWindow(start: day(-29), end: now))
        case "date":
            guard let parsed = text.parse(date) else {
                return .failure(Correction(message: "Use YYYY-MM-DD for date."))
            }
            let start = calendar.startOfDay(for: parsed)
            guard start <= now else { return .failure(Correction(message: "That date is in the future.")) }
            guard start >= day(-365) else {
                return .failure(Correction(message: "The log tool reads at most one year back."))
            }
            let next = calendar.date(byAdding: .day, value: 1, to: start)!
            return .success(AskWindow(start: start, end: min(next, now)))
        default:
            return .failure(Correction(message: "Use range \(Self.ranges.dropLast().joined(separator: ", ")) or date."))
        }
    }

    private func dayBuckets(_ window: AskWindow) -> [AskWindow] {
        let calendar = text.calendar
        var buckets: [AskWindow] = []
        var start = window.start
        while start < window.end {
            let nextDay = calendar.date(byAdding: .day, value: 1, to: calendar.startOfDay(for: start))!
            let end = min(nextDay, window.end)
            buckets.append(AskWindow(start: start, end: end))
            start = end
        }
        return buckets
    }

    private func isPartialDay(_ bucket: AskWindow) -> Bool {
        let calendar = text.calendar
        let dayStart = calendar.startOfDay(for: bucket.start)
        let dayEnd = calendar.date(byAdding: .day, value: 1, to: dayStart)!
        return bucket.start != dayStart || bucket.end != dayEnd
    }

    // MARK: Totals

    /// One fetch per totals call; day buckets filter it in memory.
    private struct CareData {
        let items: [HistoryItem]
        let feedSessions: [FeedingSession]
        let sleepSessions: [SleepSession]

        @MainActor
        init(store: BabyStore, babyID: UUID, window: AskWindow) throws {
            // A bottle or breast step can start in the window inside a feed
            // that began the evening before; look one day back for those.
            items = try store.entries(for: babyID, from: window.start.addingTimeInterval(-86_400), to: window.end)
            feedSessions = try store.feedingSessions(for: babyID, from: window.start, to: window.end)
            sleepSessions = try store.sleepSessions(for: babyID, from: window.start, to: window.end)
        }
    }

    /// Counting follows Insights: a joined feed or nap counts once, on the
    /// day its first part began; a bottle step's volume follows the day the
    /// step began.
    private func summary(_ data: CareData, in bucket: AskWindow) -> String {
        func inside(_ date: Date) -> Bool { date >= bucket.start && date < bucket.end }
        var bottles: [BottleContents: Double] = [:]
        var breastSeconds = 0
        var diapers: [DiaperContents: Int] = [:]
        var meds: [String] = []
        for item in data.items {
            switch item {
            case .feed(let entry):
                for component in entry.components where inside(component.startedAt ?? entry.startedAt) {
                    if let volume = component.volumeML, let contents = component.bottleContents {
                        bottles[contents, default: 0] += volume
                    }
                    if let seconds = component.durationSeconds { breastSeconds += seconds }
                }
            case .sleep:
                break
            case .diaper(let entry):
                if inside(entry.startedAt) { diapers[entry.contents, default: 0] += 1 }
            case .supplement(let entry):
                if inside(entry.startedAt) { meds.append(entry.name) }
            }
        }
        let feeds = data.feedSessions.filter { inside($0.startedAt) }.count
        // A sleep joined across midnight counts, with all its parts, on the day it began.
        let completedSleeps = data.sleepSessions.filter { inside($0.startedAt) && $0.endedAt != nil }
        let sleeps = completedSleeps.count
        let sleepSeconds = completedSleeps.reduce(0) { $0 + $1.asleepSeconds }

        var parts = [Self.count(feeds, "feed")]
        let bottleTotal = bottles.values.reduce(0, +)
        if bottleTotal == 0 {
            parts.append("no bottles")
        } else {
            let split = BottleContents.allCases.compactMap { contents in
                bottles[contents].map { "\(Self.milk(contents)) \(volume($0))" }
            }
            parts.append("bottles \(volume(bottleTotal)) (\(split.joined(separator: ", ")))")
        }
        if breastSeconds > 0 { parts.append("breastfeeding \(AskText.duration(breastSeconds))") }
        parts.append(Self.count(sleeps, "sleep") + (sleepSeconds > 0 ? " \(AskText.duration(sleepSeconds))" : ""))
        let diaperSplit = DiaperContents.allCases.compactMap { contents in
            diapers[contents].map { "\(contents.rawValue) \($0)" }
        }
        parts.append(Self.count(diapers.values.reduce(0, +), "diaper")
            + (diaperSplit.isEmpty ? "" : " (\(diaperSplit.joined(separator: ", ")))"))
        var names: [String] = []
        for name in meds where !names.contains(name) { names.append(name) }
        parts.append(Self.count(meds.count, "med") + (names.isEmpty ? "" : " (\(names.joined(separator: ", ")))"))
        return parts.joined(separator: ", ")
    }

    private func volume(_ millilitres: Double) -> String {
        Volume(millilitres: millilitres).formatted(as: volumeUnit)
    }

    private static func count(_ number: Int, _ noun: String) -> String {
        "\(number) \(noun)\(number == 1 ? "" : "s")"
    }
}
