import Foundation
import Testing
@testable import BabyCore

@Suite("LocalDate")
struct LocalDateTests {
    @Test func acceptsAndRoundTripsStrictCalendarDate() throws {
        let date = try LocalDate("2019-11-03")

        #expect(date.iso8601 == "2019-11-03")
        #expect(try JSONDecoder().decode(LocalDate.self, from: JSONEncoder().encode(date)) == date)
    }

    @Test(arguments: ["2019-1-03", "2019-02-29", "2019-11-03T00:00:00Z", "not-a-date"])
    func rejectsNonCanonicalOrInvalidDates(_ value: String) {
        #expect(throws: LocalDate.ValidationError.self) {
            try LocalDate(value)
        }
    }

    @Test func derivesDisplayedDateInExplicitTimeZone() throws {
        let instant = try #require(ISO8601DateFormatter().date(from: "2019-11-03T23:30:00Z"))

        #expect(try LocalDate(displaying: instant, in: TimeZone(identifier: "Europe/Zurich")!).iso8601 == "2019-11-04")
        #expect(try LocalDate(displaying: instant, in: TimeZone(identifier: "America/Los_Angeles")!).iso8601 == "2019-11-03")
    }
}
