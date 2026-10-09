import Foundation

public enum SleepClassifier {
    /// Local hour at or after this is night.
    public static let nightStartHour = 19
    /// Local hour at or after this is daytime again.
    public static let dayStartHour = 6

    /// Derived from when the sleep *started*, never asked at log time. A user
    /// override is stored separately on the entry and wins over this.
    public static func classify(startedAt: Date, timeZoneID: String) -> SleepKind {
        let calendar = DayBucket.calendar(for: timeZoneID)
        let hour = calendar.component(.hour, from: startedAt)

        return (hour >= nightStartHour || hour < dayStartHour) ? .night : .nap
    }
}
