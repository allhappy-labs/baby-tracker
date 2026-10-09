import Foundation
import Testing
@testable import AskFeature

private let zurich = TimeZone(identifier: "Europe/Zurich")!

private func local(_ year: Int, _ month: Int, _ day: Int, _ hour: Int = 0, _ minute: Int = 0) -> Date {
    var calendar = Calendar(identifier: .gregorian)
    calendar.timeZone = zurich
    return calendar.date(from: DateComponents(year: year, month: month, day: day, hour: hour, minute: minute))!
}

@Test func localTimesUseOneFixedEnglishShape() {
    let text = AskText(timeZone: zurich)
    #expect(text.time(local(2026, 10, 4, 21, 10)) == "Sun 4 Oct, 21:10")
    #expect(text.timeWithYear(local(2026, 10, 4, 21, 10)) == "Sun 4 Oct 2026, 21:10")
    #expect(text.day(local(2026, 10, 4, 21, 10)) == "Sun 4 Oct")
    #expect(text.date(local(2026, 5, 26)) == "26 May 2026")
    #expect(text.span(local(2026, 10, 4, 21, 10), local(2026, 10, 4, 21, 35)) == "Sun 4 Oct, 21:10–21:35")
    #expect(text.span(local(2026, 10, 4, 23, 50), local(2026, 10, 5, 0, 40)) == "Sun 4 Oct, 23:50–Mon 5 Oct, 00:40")
    #expect(text.span(local(2026, 10, 4, 21, 10), nil) == "Sun 4 Oct, 21:10")
    #expect(text.span(local(2026, 10, 4, 21, 10), local(2026, 10, 4, 21, 10)) == "Sun 4 Oct, 21:10")
}

@Test func toolArgumentsParseAsLocalWallClock() {
    let text = AskText(timeZone: zurich)
    #expect(text.parse("2026-10-04 21:10") == local(2026, 10, 4, 21, 10))
    #expect(text.parse(" 2026-10-04 ") == local(2026, 10, 4))
    #expect(text.parse("4 Oct") == nil)
    #expect(text.parse("2026-02-30 10:00") == nil)
    // Review focus 1: the repeated hour when clocks go back still parses.
    #expect(text.parse("2026-10-25 02:30") != nil)
}

@Test func durationsReadNaturally() {
    #expect(AskText.duration(30) == "under 1 min")
    #expect(AskText.duration(59 * 60) == "59 min")
    #expect(AskText.duration(5_400) == "1 h 30 min")
    #expect(AskText.duration(7_200) == "2 h")
    #expect(AskText.ago(3_600) == "1 h")
    #expect(AskText.ago(3 * 86_400 + 5) == "3 days")
}

@Test func ageUsesDaysThenWeeksThenMonths() {
    let text = AskText(timeZone: zurich)
    let now = local(2026, 10, 5, 4, 28)
    #expect(text.age(birth: local(2026, 10, 1), now: now) == "4 days")
    #expect(text.age(birth: local(2026, 8, 24), now: now) == "6 weeks")
    #expect(text.age(birth: local(2026, 5, 26), now: now) == "4 months 1 week")
    #expect(text.age(birth: local(2024, 1, 1), now: now) == "33 months")
}
