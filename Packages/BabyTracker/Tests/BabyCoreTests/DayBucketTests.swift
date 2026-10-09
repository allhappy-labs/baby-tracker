import Testing
import Foundation
@testable import BabyCore

@Test func dayIsAnchoredToTheEntrysOwnTimezone() {
    // 2026-09-18 02:00 Berlin == 2026-09-18 00:00 UTC == 2026-09-17 20:00 New York.
    var components = DateComponents()
    components.year = 2026; components.month = 9; components.day = 18
    components.hour = 0; components.minute = 0
    var utc = Calendar(identifier: .gregorian)
    utc.timeZone = TimeZone(identifier: "UTC")!
    let instant = utc.date(from: components)!

    let berlin = DayBucket.key(for: instant, timeZoneID: "Europe/Berlin")
    let newYork = DayBucket.key(for: instant, timeZoneID: "America/New_York")

    #expect(berlin == DayKey(year: 2026, month: 9, day: 18))
    #expect(newYork == DayKey(year: 2026, month: 9, day: 17))
}

@Test func springForwardDayIsStillOneDay() {
    // 2026-03-29 is the European spring-forward: that local day is 23 hours long.
    let key = DayKey(year: 2026, month: 3, day: 29)
    let bounds = DayBucket.bounds(of: key, timeZoneID: "Europe/Berlin")
    let length = bounds.end.timeIntervalSince(bounds.start)

    #expect(length == 23 * 3600)
}

@Test func autumnBackDayIsStillOneDay() {
    // 2026-10-25 is the European fall-back: that local day is 25 hours long.
    let key = DayKey(year: 2026, month: 10, day: 25)
    let bounds = DayBucket.bounds(of: key, timeZoneID: "Europe/Berlin")

    #expect(bounds.end.timeIntervalSince(bounds.start) == 25 * 3600)
}

@Test func keysSortChronologically() {
    let earlier = DayKey(year: 2026, month: 9, day: 17)
    let later = DayKey(year: 2026, month: 9, day: 18)
    let nextYear = DayKey(year: 2027, month: 1, day: 1)

    #expect(earlier < later)
    #expect(later < nextYear)
}
