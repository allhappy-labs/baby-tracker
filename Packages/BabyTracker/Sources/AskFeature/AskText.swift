import Foundation

/// The one shape every time takes in what Ask shows the model: the device's
/// local wall clock, fixed English abbreviations and a 24-hour clock. The
/// model reads the same format on every device locale and writes tool
/// arguments back as `YYYY-MM-DD HH:mm` in the same zone.
struct AskText: Sendable {
    let timeZone: TimeZone

    init(timeZone: TimeZone) {
        self.timeZone = timeZone
    }

    var calendar: Calendar {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = timeZone
        return calendar
    }

    func time(_ date: Date) -> String { format(date, "EEE d MMM, HH:mm") }
    func timeWithYear(_ date: Date) -> String { format(date, "EEE d MMM yyyy, HH:mm") }
    func day(_ date: Date) -> String { format(date, "EEE d MMM") }
    func date(_ date: Date) -> String { format(date, "d MMM yyyy") }
    func isoDay(_ date: Date) -> String { format(date, "yyyy-MM-dd") }

    func span(_ start: Date, _ end: Date?) -> String {
        guard let end, end > start else { return time(start) }
        let ending = calendar.isDate(start, inSameDayAs: end) ? format(end, "HH:mm") : time(end)
        return "\(time(start))–\(ending)"
    }

    func parse(_ text: String) -> Date? {
        let trimmed = text.trimmingCharacters(in: .whitespaces)
        for pattern in ["yyyy-MM-dd HH:mm", "yyyy-MM-dd"] {
            let formatter = formatter(pattern)
            formatter.isLenient = false
            if let date = formatter.date(from: trimmed), formatter.string(from: date) == trimmed {
                return date
            }
        }
        return nil
    }

    func age(birth: Date, now: Date) -> String {
        let start = calendar.startOfDay(for: birth)
        let end = calendar.startOfDay(for: now)
        let days = max(0, calendar.dateComponents([.day], from: start, to: end).day ?? 0)
        if days < 14 { return days == 1 ? "1 day" : "\(days) days" }
        if days < 91 { return "\(days / 7) weeks" }
        let parts = calendar.dateComponents([.month, .day], from: start, to: end)
        let months = parts.month ?? 0
        let weeks = (parts.day ?? 0) / 7
        let monthText = months == 1 ? "1 month" : "\(months) months"
        if months >= 24 || weeks == 0 { return monthText }
        return "\(monthText) \(weeks == 1 ? "1 week" : "\(weeks) weeks")"
    }

    static func duration(_ seconds: Int) -> String {
        guard seconds >= 60 else { return "under 1 min" }
        let minutes = seconds / 60
        let hours = minutes / 60
        let rest = minutes % 60
        if hours == 0 { return "\(minutes) min" }
        return rest == 0 ? "\(hours) h" : "\(hours) h \(rest) min"
    }

    static func ago(_ seconds: Int) -> String {
        seconds < 172_800 ? duration(seconds) : "\(seconds / 86_400) days"
    }

    private func format(_ date: Date, _ pattern: String) -> String {
        formatter(pattern).string(from: date)
    }

    private func formatter(_ pattern: String) -> DateFormatter {
        let formatter = DateFormatter()
        formatter.calendar = calendar
        formatter.timeZone = timeZone
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.dateFormat = pattern
        return formatter
    }
}
