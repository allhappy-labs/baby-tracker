import Foundation
import Testing
@testable import BabyCore

/// `compact` takes two `Date`s, so the tests spell the age out as a distance
/// back from a fixed `now`.
private let now = Date(timeIntervalSince1970: 1_700_000_000)

private func ago(_ seconds: Int) -> Date {
    now.addingTimeInterval(-Double(seconds))
}

@Test func compactFormatsRelativeAges() {
    #expect(ElapsedFormatter.compact(since: ago(0), now: now) == "just now")
    #expect(ElapsedFormatter.compact(since: ago(45), now: now) == "just now")
    #expect(ElapsedFormatter.compact(since: ago(60), now: now) == "1m")
    #expect(ElapsedFormatter.compact(since: ago(8_040), now: now) == "2h 14m")
    #expect(ElapsedFormatter.compact(since: ago(7_200), now: now) == "2h")
}

@Test func compactClampsFutureDatesToJustNow() {
    // Clock skew or an edit dated in the future must not render "-3m".
    #expect(ElapsedFormatter.compact(since: ago(-180), now: now) == "just now")
}

@Test func compactAndTotalDisagreeAtZeroOnPurpose() {
    // The distinction the signature now enforces: an age of zero is a moment
    // ("just now"), a duration of zero is a quantity ("0m"). If these two ever
    // agree, one of the formatters has drifted into the other's job.
    #expect(ElapsedFormatter.compact(since: now, now: now) == "just now")
    #expect(ElapsedFormatter.total(0) == "0m")
}

@Test func timerFormatsRunningCounters() {
    #expect(ElapsedFormatter.timer(0) == "0:00")
    #expect(ElapsedFormatter.timer(1_122) == "18:42")
    #expect(ElapsedFormatter.timer(3_725) == "1:02:05")
}

@Test func totalFormatsCumulativeDurationsAndNeverSaysJustNow() {
    // The whole point of `total`: zero is a quantity, not a moment.
    #expect(ElapsedFormatter.total(0) == "0m")
    #expect(ElapsedFormatter.total(3_600) == "1h")
    #expect(ElapsedFormatter.total(8_040) == "2h 14m")
}

@Test func totalRendersSubMinuteAmountsAsZeroMinutes() {
    // "Total sleep today: just now" is nonsense; 45 seconds of sleep is 0m.
    #expect(ElapsedFormatter.total(45) == "0m")
    #expect(ElapsedFormatter.total(-180) == "0m")
}

// MARK: - Whole minutes

@Test func minutesRoundsDownAndNeverInventsAWholeMinute() {
    #expect(ElapsedFormatter.minutes(0) == "<1 min")
    #expect(ElapsedFormatter.minutes(20) == "<1 min")
    #expect(ElapsedFormatter.minutes(59) == "<1 min")
    #expect(ElapsedFormatter.minutes(60) == "1 min")
    #expect(ElapsedFormatter.minutes(119) == "1 min")
    #expect(ElapsedFormatter.minutes(720) == "12 min")
    #expect(ElapsedFormatter.minutes(-30) == "<1 min")
}

/// How long a single breast took — seconds while they matter, and not once
/// they stop mattering.
///
/// "<1 min" was the answer for anything under a minute, so a 26-second side
/// and a 59-second one read identically; and whole minutes above that hid
/// the difference between an 8-minute side and one that ran to 8m 50s.
@Test func preciseDurationKeepsSecondsUntilTheyStopMattering() {
    #expect(ElapsedFormatter.precise(0) == "0s")
    #expect(ElapsedFormatter.precise(26) == "26s")
    #expect(ElapsedFormatter.precise(60) == "1m")
    #expect(ElapsedFormatter.precise(492) == "8m 12s")
    #expect(ElapsedFormatter.precise(3_600) == "1h")
    // Past an hour the seconds are noise.
    #expect(ElapsedFormatter.precise(3_725) == "1h 2m")
    #expect(ElapsedFormatter.precise(-5) == "0s")
}
