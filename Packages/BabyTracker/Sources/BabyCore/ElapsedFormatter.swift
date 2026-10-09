import Foundation

public enum ElapsedFormatter {
    /// How long ago a past event happened — an AGE, not a length:
    /// "2h 14m", "55m", "just now".
    ///
    /// Takes the two timestamps rather than a count of seconds, deliberately.
    /// A bare `Int` is ambiguous between an age and a duration, and confusing
    /// the two has been this project's most repeated bug; demanding the
    /// event's own `Date` makes the mistake a compile error instead of a
    /// wrong string. If you hold a duration and no source `Date`, that is the
    /// signal you want `total`.
    ///
    /// `now` comes from the injected `DateProvider`, never a bare `Date()`.
    public static func compact(since: Date, now: Date) -> String {
        let total = max(0, Int(now.timeIntervalSince(since)))
        let hours = total / 3600
        let minutes = (total % 3600) / 60

        if hours > 0 {
            return minutes > 0 ? formatted("%dh %dm", hours, minutes) : formatted("%dh", hours)
        }
        if minutes > 0 {
            return formatted("%dm", minutes)
        }
        return String(localized: "just now", bundle: .module)
    }

    /// How long something LASTED — a length, not an age: "2h 14m", "42m", "0m".
    ///
    /// Use for naps, daily totals, averages and differences. Never returns
    /// "just now": a total of zero is a quantity, so it renders as one. For
    /// how long ago an event happened, use `compact`.
    public static func total(_ seconds: Int) -> String {
        let total = max(0, seconds)
        let hours = total / 3600
        let minutes = (total % 3600) / 60

        if hours > 0 {
            return minutes > 0 ? formatted("%dh %dm", hours, minutes) : formatted("%dh", hours)
        }
        return formatted("%dm", minutes)
    }

    /// How long one breast took: "26s", "8m 12s", "1h 2m".
    ///
    /// Seconds are kept while they are a meaningful fraction of the whole and
    /// dropped past an hour, where they are noise. This is the figure a parent
    /// reads to balance the next feed, so a side that ran 26 seconds says 26
    /// seconds rather than "<1 min", and one that ran 8m 50s does not round
    /// down to eight.
    public static func precise(_ seconds: Int) -> String {
        let total = max(0, seconds)
        let hours = total / 3600
        let minutes = (total % 3600) / 60
        let remainder = total % 60
        if hours > 0 { return minutes > 0 ? formatted("%dh %dm", hours, minutes) : formatted("%dh", hours) }
        if minutes > 0 { return remainder > 0 ? formatted("%dm %ds", minutes, remainder) : formatted("%dm", minutes) }
        return formatted("%ds", remainder)
    }

    /// A whole number of minutes, for durations quoted in minutes: "42 min".
    ///
    /// Anything under a minute reads "<1 min" rather than being rounded up to
    /// "1 min": a 20-second feed that reports a minute is a small lie, and the
    /// honest rendering costs one character.
    public static func minutes(_ seconds: Int) -> String {
        let whole = max(0, seconds) / 60
        return whole < 1 ? String(localized: "<1 min", bundle: .module) : formatted("%d min", whole)
    }

    private static func formatted(_ key: String, _ arguments: CVarArg...) -> String {
        String(format: NSLocalizedString(key, bundle: .module, comment: ""), locale: .current, arguments: arguments)
    }

    /// A running counter: "0:00", "18:42", "1:02:05".
    public static func timer(_ seconds: Int) -> String {
        let total = max(0, seconds)
        let hours = total / 3600
        let minutes = (total % 3600) / 60
        let secs = total % 60

        if hours > 0 {
            return String(format: "%d:%02d:%02d", hours, minutes, secs)
        }
        return String(format: "%d:%02d", minutes, secs)
    }
}
