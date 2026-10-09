import Foundation

public struct LocalDate: Codable, Sendable, Hashable, Comparable, CustomStringConvertible {
    public enum ValidationError: Error, Sendable, Equatable { case invalid(String) }

    public let iso8601: String

    public init(_ iso8601: String) throws {
        guard iso8601.utf8.count == 10,
              iso8601[iso8601.index(iso8601.startIndex, offsetBy: 4)] == "-",
              iso8601[iso8601.index(iso8601.startIndex, offsetBy: 7)] == "-",
              let date = Self.formatter.date(from: iso8601),
              Self.formatter.string(from: date) == iso8601 else {
            throw ValidationError.invalid(iso8601)
        }
        self.iso8601 = iso8601
    }

    public init(displaying date: Date, in timeZone: TimeZone) throws {
        var calendar = Calendar(identifier: .gregorian)
        calendar.locale = Locale(identifier: "en_US_POSIX")
        calendar.timeZone = timeZone
        let components = calendar.dateComponents([.year, .month, .day], from: date)
        guard let year = components.year, let month = components.month, let day = components.day else {
            throw ValidationError.invalid(String(describing: date))
        }
        try self.init(String(format: "%04d-%02d-%02d", year, month, day))
    }

    public static func < (lhs: LocalDate, rhs: LocalDate) -> Bool { lhs.iso8601 < rhs.iso8601 }
    public var description: String { iso8601 }

    public init(from decoder: any Decoder) throws {
        let container = try decoder.singleValueContainer()
        try self.init(container.decode(String.self))
    }

    public func encode(to encoder: any Encoder) throws {
        var container = encoder.singleValueContainer()
        try container.encode(iso8601)
    }

    private static let formatter: DateFormatter = {
        let formatter = DateFormatter()
        formatter.calendar = Calendar(identifier: .gregorian)
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.timeZone = TimeZone(secondsFromGMT: 0)
        formatter.dateFormat = "yyyy-MM-dd"
        formatter.isLenient = false
        return formatter
    }()
}
