import Foundation

/// A calendar day in some specific timezone. Not an instant — two DayKeys with
/// the same fields from different timezones describe different stretches of time.
public struct DayKey: Hashable, Sendable, Comparable, Codable {
    public let year: Int
    public let month: Int
    public let day: Int

    public init(year: Int, month: Int, day: Int) {
        self.year = year
        self.month = month
        self.day = day
    }

    public static func < (lhs: DayKey, rhs: DayKey) -> Bool {
        (lhs.year, lhs.month, lhs.day) < (rhs.year, rhs.month, rhs.day)
    }
}

public enum DayBucket {
    /// Falls back to UTC rather than the device timezone when an identifier is
    /// unknown, so a corrupt value produces a stable wrong answer rather than
    /// one that changes as the user travels.
    static func calendar(for timeZoneID: String) -> Calendar {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(identifier: timeZoneID) ?? TimeZone(identifier: "UTC")!
        return calendar
    }

    public static func key(for date: Date, timeZoneID: String) -> DayKey {
        let parts = calendar(for: timeZoneID).dateComponents([.year, .month, .day], from: date)
        return DayKey(year: parts.year!, month: parts.month!, day: parts.day!)
    }

    /// Start of the local day and start of the next local day. Uses calendar
    /// arithmetic rather than adding 86,400 seconds, so DST days come out 23 or
    /// 25 hours long as they actually are.
    public static func bounds(of key: DayKey, timeZoneID: String) -> (start: Date, end: Date) {
        let calendar = calendar(for: timeZoneID)
        var components = DateComponents()
        components.year = key.year
        components.month = key.month
        components.day = key.day

        let start = calendar.date(from: components)!
        let end = calendar.date(byAdding: .day, value: 1, to: start)!
        return (start, end)
    }
}
