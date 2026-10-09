import Foundation
import BabyCore
import BabyStore
import BabyUI

/// What the PDF prints, every string already in the reader's language: the
/// baby, the dates, then each day oldest first — the day's totals and its
/// entries in the order they happened, as a diary reads.
public struct HistoryReport: Equatable {
    public struct Entry: Equatable {
        /// "10:23", or "10:23–11:05" for an entry with a length.
        public let time: String
        /// The tracker's name: "Feed", "Sleep", "Diaper", "Meds", "Growth".
        public let type: String
        /// History's own words for the entry: its chips joined with " · ",
        /// or its text.
        public let details: String
        public let note: String?
    }

    public struct Day: Equatable, Identifiable {
        public let id: DayKey
        /// The full date. Never "Today": a printout is read on other days.
        public let title: String
        public let totals: HistoryDayTotals
        /// "8 feeds · 13h 20m sleep · 540 ml bottle · 6 diapers".
        public let totalsLine: String
        public let entries: [Entry]
    }

    public let babyName: String
    public let rangeText: String
    public let generatedText: String
    public let days: [Day]
}

@MainActor
enum HistoryReportBuilder {
    /// The report of every tracker from the start of `first` to the end of
    /// `last`, or nil when nothing was logged in those days.
    static func make(store: BabyStore, babyID: UUID, babyName: String, from first: DayKey, through last: DayKey,
                     volumeUnit: VolumeUnit, zone: String = TimeZone.current.identifier) throws -> HistoryReport? {
        let builder = HistoryDayBuilder(store: store, babyID: babyID, volumeUnit: volumeUnit, zone: zone)
        let built = try builder.build(from: first, through: last, filter: Set(HistoryViewModel.trackers))
        let days = built.days.keys.sorted().compactMap { key -> HistoryReport.Day? in
            guard let day = built.days[key], !day.entries.isEmpty else { return nil }
            return HistoryReport.Day(
                id: key,
                title: title(key, zone: zone),
                totals: day.totals,
                totalsLine: day.totals.phrases(volumeUnit: volumeUnit).joined(separator: " · "),
                entries: day.entries.reversed().map(entry)
            )
        }
        guard !days.isEmpty else { return nil }
        return HistoryReport(
            babyName: babyName,
            rangeText: rangeText(from: first, through: last, zone: zone),
            generatedText: store.now.formatted(date: .long, time: .shortened),
            days: days
        )
    }

    /// "1–7 Oct 2026"; a single day reads as that one date.
    static func rangeText(from first: DayKey, through last: DayKey, zone: String) -> String {
        let timeZone = TimeZone(identifier: zone) ?? .gmt
        let start = DayBucket.bounds(of: first, timeZoneID: zone).start
        let end = DayBucket.bounds(of: last, timeZoneID: zone).start
        if first == last {
            var style = Date.FormatStyle(date: .abbreviated, time: .omitted)
            style.timeZone = timeZone
            return start.formatted(style)
        }
        var calendar = Calendar.current
        calendar.timeZone = timeZone
        let style = Date.IntervalFormatStyle(date: .abbreviated, time: .omitted, calendar: calendar, timeZone: timeZone)
        return (start..<end).formatted(style)
    }

    private static func title(_ key: DayKey, zone: String) -> String {
        var style = Date.FormatStyle(date: .complete, time: .omitted)
        style.timeZone = TimeZone(identifier: zone) ?? .gmt
        return DayBucket.bounds(of: key, timeZoneID: zone).start.formatted(style)
    }

    private static func entry(_ built: BuiltEntry) -> HistoryReport.Entry {
        let row = built.row
        return HistoryReport.Entry(
            time: row.end.map { row.start + "–" + $0 } ?? row.start,
            type: row.tracker.title,
            details: row.chips.isEmpty ? row.text ?? "" : row.chips.map(\.text).joined(separator: " · "),
            note: built.note
        )
    }
}
