import Foundation

/// Supplements carry an immutable logging instant. Foundation's fractional
/// ISO8601 parser/formatter can truncate to milliseconds, so this field uses
/// the server's lexical normalization and explicit microsecond rounding.
public enum SupplementTimestamp {
    public static func parse(_ value: String) throws -> Date {
        let canonical = try ProtocolCanonicalizer.lexicalTimestamp(value).canonical
        let seconds = String(canonical.prefix(19)) + "Z"
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime]
        guard let whole = formatter.date(from: seconds),
              let micros = Int(canonical.dropFirst(20).prefix(6)) else {
            throw ProtocolCanonicalizer.Error.invalidTimestamp(value)
        }
        return whole.addingTimeInterval(Double(micros) / 1_000_000)
    }

    public static func format(_ date: Date) -> String {
        // Split before multiplying: multiplying a large epoch value first
        // introduces another rounding step near the protocol's year bounds.
        let reference = date.timeIntervalSinceReferenceDate
        var whole = floor(reference)
        var micros = Int(((reference - whole) * 1_000_000).rounded(.toNearestOrEven))
        if micros == 1_000_000 { whole += 1; micros = 0 }
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime]
        formatter.timeZone = TimeZone(secondsFromGMT: 0)
        let seconds = formatter.string(from: Date(timeIntervalSinceReferenceDate: whole))
        return String(seconds.dropLast()) + String(format: ".%06dZ", micros)
    }
}
