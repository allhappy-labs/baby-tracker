import Testing
import Foundation
@testable import BabyCore

private func berlinInstant(hour: Int, minute: Int = 0) -> Date {
    var calendar = Calendar(identifier: .gregorian)
    calendar.timeZone = TimeZone(identifier: "Europe/Berlin")!
    var components = DateComponents()
    components.year = 2026; components.month = 9; components.day = 18
    components.hour = hour; components.minute = minute
    return calendar.date(from: components)!
}

@Test func lateEveningStartsAreNight() {
    #expect(SleepClassifier.classify(startedAt: berlinInstant(hour: 19), timeZoneID: "Europe/Berlin") == .night)
    #expect(SleepClassifier.classify(startedAt: berlinInstant(hour: 23), timeZoneID: "Europe/Berlin") == .night)
}

@Test func smallHoursAreNight() {
    #expect(SleepClassifier.classify(startedAt: berlinInstant(hour: 2), timeZoneID: "Europe/Berlin") == .night)
    #expect(SleepClassifier.classify(startedAt: berlinInstant(hour: 5, minute: 59), timeZoneID: "Europe/Berlin") == .night)
}

@Test func daytimeStartsAreNaps() {
    #expect(SleepClassifier.classify(startedAt: berlinInstant(hour: 6), timeZoneID: "Europe/Berlin") == .nap)
    #expect(SleepClassifier.classify(startedAt: berlinInstant(hour: 13), timeZoneID: "Europe/Berlin") == .nap)
    #expect(SleepClassifier.classify(startedAt: berlinInstant(hour: 18, minute: 59), timeZoneID: "Europe/Berlin") == .nap)
}

@Test func classificationFollowsTheEntrysTimezoneNotTheDevices() {
    // 19:00 Berlin is 13:00 New York: night there, nap here.
    let instant = berlinInstant(hour: 19)

    #expect(SleepClassifier.classify(startedAt: instant, timeZoneID: "Europe/Berlin") == .night)
    #expect(SleepClassifier.classify(startedAt: instant, timeZoneID: "America/New_York") == .nap)
}
