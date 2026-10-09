import Foundation
import Testing
import BabyCore
@testable import InsightsFeature

/// Wall-clock times in Zurich, counted back from Monday 15 June 2026.
enum Clock {
    static let zone = TimeZone(identifier: "Europe/Zurich")!
    static var calendar: Calendar { var c = Calendar(identifier: .gregorian); c.timeZone = zone; return c }
    static func at(_ daysAgo: Int, _ hour: Int, _ minute: Int = 0,
                   from base: DateComponents = DateComponents(year: 2026, month: 6, day: 15)) -> Date {
        let day = calendar.date(byAdding: .day, value: -daysAgo, to: calendar.date(from: base)!)!
        return calendar.date(bySettingHour: hour, minute: minute, second: 0, of: day)!
    }
    static func startOfDay(_ daysAgo: Int) -> Date { at(daysAgo, 0) }
    static let now = at(0, 12)
}

private func summary(_ input: InsightsInput, _ range: InsightRange = .week, now: Date = Clock.now) -> InsightsSummary {
    InsightsCalculator(timeZone: Clock.zone).summary(input, range: range, now: now)
}

private func diapers(_ perDay: [Int: Int]) -> InsightsInput {
    var input = InsightsInput()
    for (daysAgo, count) in perDay {
        for i in 0..<count { input.diapers.append(Clock.at(daysAgo, 8 + i)) }
    }
    return input
}

@Suite struct InsightsPeriodTests {
    @Test func weekAveragesCompleteDaysOnlyAndShowsTodayApart() {
        var perDay = Dictionary(uniqueKeysWithValues: (1...7).map { ($0, 6) })
        perDay[0] = 2
        let result = summary(diapers(perDay)).diapers
        #expect(result.perDay?.value == 6)
        #expect(result.bars.count == 8)
        #expect(result.bars.last?.isToday == true)
        #expect(result.bars.last?.value == 2)
        #expect(result.bars.first?.start == Clock.startOfDay(7))
    }

    @Test func weekComparesWithTheSevenDaysBefore() {
        var perDay = Dictionary(uniqueKeysWithValues: (1...7).map { ($0, 6) })
        for day in 8...14 { perDay[day] = 5 }
        #expect(summary(diapers(perDay)).diapers.perDay?.comparison == .change(1))
    }

    @Test func noPreviousPeriodGivesCoverage() {
        #expect(summary(diapers([1: 4, 2: 6])).diapers.perDay?.comparison == .coverage(days: 2))
    }

    @Test func daysWithNothingLoggedAreNotCounted() {
        let result = summary(diapers([1: 4, 3: 4])).diapers
        #expect(result.perDay?.value == 4)
        #expect(result.bars[result.bars.count - 3].value == nil) // two days ago: an empty slot
    }

    @Test func monthUsesThirtyDaysAndTheThirtyBefore() {
        let result = summary(diapers([30: 3, 31: 1]), .month).diapers
        #expect(result.perDay?.value == 3)
        #expect(result.perDay?.comparison == .change(2))
        #expect(result.bars.count == 31)
    }

    @Test func allUsesSevenDayBlocksCountingBackFromYesterday() {
        let result = summary(diapers([10: 3, 9: 3, 2: 6, 1: 6]), .all).diapers
        #expect(result.bars.count == 2)
        #expect(result.bars[0].start == Clock.startOfDay(10))
        #expect(result.bars[0].value == 3)   // days 10…8, two counted
        #expect(result.bars[1].start == Clock.startOfDay(7))
        #expect(result.bars[1].value == 6)
        #expect(result.bars.allSatisfy { !$0.isToday })
        #expect(result.perDay?.value == 4.5)
        #expect(result.perDay?.comparison == .since(Clock.startOfDay(10)))
    }

    @Test func allWithOnlyTodayIsEmpty() {
        let result = summary(diapers([0: 3]), .all).diapers
        #expect(result.perDay == nil)
        #expect(result.bars.isEmpty)
    }

    @Test func nothingInThePeriodLeavesNoAverage() {
        let result = summary(diapers([0: 3])).diapers
        #expect(result.perDay == nil)
        #expect(result.bars.count == 8)
    }

    // Review focus 1.
    @Test func dstWeekKeepsOneBarPerDay() {
        let base = DateComponents(year: 2026, month: 3, day: 30)   // Zurich moved to summer time on 29 March
        var input = InsightsInput()
        input.diapers = [Clock.at(1, 23, 30, from: base), Clock.at(2, 8, from: base)]
        let result = InsightsCalculator(timeZone: Clock.zone)
            .summary(input, range: .week, now: Clock.at(0, 12, from: base)).diapers
        #expect(result.bars.count == 8)
        #expect(Set(result.bars.map(\.start)).count == 8)
        #expect(result.bars[6].value == 1)   // 29 March, the short day
        #expect(result.bars[5].value == 1)   // 28 March
    }

    // Review focus 2.
    @Test func justAfterMidnightTodayIsNotCounted() {
        var input = diapers([1: 6])
        input.diapers.append(Clock.at(0, 0, 2))
        let result = summary(input, now: Clock.at(0, 0, 5)).diapers
        #expect(result.perDay?.value == 6)
        #expect(result.bars.last?.value == 1)
    }

}

private func breast(_ date: Date, left: Int = 600, right: Int = 0) -> FeedSessionInput {
    FeedSessionInput(startedAt: date, hasBreast: true, leftSeconds: left, rightSeconds: right)
}
private func bottle(_ date: Date) -> FeedSessionInput {
    FeedSessionInput(startedAt: date, hasBottle: true)
}

@Suite struct InsightsFeedingTests {
    @Test func feedsPerDayCountsSessions() {
        var input = InsightsInput()
        input.feeds = [breast(Clock.at(1, 6)), breast(Clock.at(1, 9)), breast(Clock.at(2, 6))]
        #expect(summary(input).feeding.perDay?.value == 1.5)
    }

    @Test func betweenFeedsIsTheMedianStartToStartAndLongestGapTheMax() {
        var input = InsightsInput()
        input.feeds = [Clock.at(8, 22), Clock.at(7, 6), Clock.at(7, 9), Clock.at(7, 12), Clock.at(7, 17)].map { breast($0) }
        let feeding = summary(input).feeding
        // Gaps whose later feed is in the week: 8h (from the previous week), 3h, 3h, 5h.
        #expect(feeding.betweenFeeds == 4.0 * 3600)
        #expect(feeding.longestGap == 8.0 * 3600)
    }

    @Test func splitCountsSessionsAndLeavesSolidsOut() {
        var input = InsightsInput()
        input.feeds = [
            breast(Clock.at(1, 6)), breast(Clock.at(1, 9)), breast(Clock.at(1, 12)),
            FeedSessionInput(startedAt: Clock.at(1, 15), hasBreast: true, hasBottle: true, leftSeconds: 300),
            bottle(Clock.at(1, 18)),
            FeedSessionInput(startedAt: Clock.at(1, 19)),   // solids only
        ]
        let feeding = summary(input).feeding
        #expect((feeding.breastOnly, feeding.both, feeding.bottleOnly) == (3, 1, 1))
        #expect(feeding.perDay?.value == 6)
    }

    @Test func sidesSumBreastTimeInThePeriod() {
        var input = InsightsInput()
        input.feeds = [breast(Clock.at(1, 6), left: 600, right: 300), breast(Clock.at(2, 6), left: 0, right: 900),
                       breast(Clock.at(9, 6), left: 5000)]   // previous week: not counted
        let feeding = summary(input).feeding
        #expect((feeding.leftSeconds, feeding.rightSeconds) == (600, 1200))
    }

    @Test func bottlePerDayAveragesOverDaysWithFeeds() {
        var input = InsightsInput()
        input.feeds = [bottle(Clock.at(1, 9)), breast(Clock.at(2, 9))]
        input.bottles = [BottleInput(at: Clock.at(1, 9), volumeML: 120), BottleInput(at: Clock.at(1, 15), volumeML: 180)]
        #expect(summary(input).feeding.bottlePerDayML == 150)
        input.bottles = []
        #expect(summary(input).feeding.bottlePerDayML == nil)
    }

    // Review focus 4.
    @Test func aYearOfFeedsSummarisesQuickly() {
        var input = InsightsInput()
        for day in 1...365 { for hour in stride(from: 0, to: 24, by: 2) { input.feeds.append(breast(Clock.at(day, hour))) } }
        let clock = ContinuousClock()
        let elapsed = clock.measure { _ = summary(input, .all) }
        #expect(elapsed < .seconds(1))
        #expect(summary(input, .all).feeding.perDay?.value == 12)
    }
}

private func sleep(_ start: Date, _ end: Date?, kind: SleepKind = .nap, parts: [Int]? = nil) -> SleepSessionInput {
    let seconds = end.map { Int($0.timeIntervalSince(start)) } ?? 0
    return SleepSessionInput(startedAt: start, endedAt: end, asleepSeconds: parts?.reduce(0, +) ?? seconds,
                             kind: kind, partSeconds: parts ?? (end == nil ? [] : [seconds]))
}

@Suite struct InsightsSleepTests {
    @Test func totalsCountOnTheStartDayAndSplitNightFromNaps() {
        var input = InsightsInput()
        input.sleeps = [
            sleep(Clock.at(1, 9), Clock.at(1, 10)),                       // 1h nap
            sleep(Clock.at(1, 20), Clock.at(0, 4), kind: .night),         // 8h night, counted on day 1
        ]
        let result = summary(input).sleep
        #expect(result.perDay?.value == 9.0 * 3600)
        #expect(result.nightPerDay == 8.0 * 3600)
        #expect(result.napsPerDay == 3600)
        #expect(result.napCountPerDay == 1)
        let yesterday = result.bars[result.bars.count - 2]
        #expect(yesterday.value == 9.0 * 3600)
        #expect(yesterday.secondary == 3600)
    }

    @Test func longestStretchIsTheLongestPartNotTheSession() {
        var input = InsightsInput()
        input.sleeps = [sleep(Clock.at(1, 20), Clock.at(0, 5), kind: .night, parts: [3 * 3600, 5 * 3600])]
        #expect(summary(input).sleep.longestStretch == 5 * 3600)
    }

    @Test func awakeBetweenUsesDaytimeGapsUpToTwelveHours() {
        var input = InsightsInput()
        input.sleeps = [
            sleep(Clock.at(1, 9), Clock.at(1, 10)),
            sleep(Clock.at(1, 11, 30), Clock.at(1, 12, 30)),               // gap 1.5h, daytime
            sleep(Clock.at(1, 19, 30), Clock.at(1, 23), kind: .night),     // gap 7h, daytime
            sleep(Clock.at(1, 23, 30), Clock.at(0, 5), kind: .night),      // gap starts 23:00: night, skipped
            sleep(Clock.at(3, 6), Clock.at(3, 8)),
            sleep(Clock.at(3, 21), Clock.at(3, 23), kind: .night),         // gap 13h: skipped
        ]
        #expect(summary(input).sleep.awakeBetween == 4.25 * 3600)
    }

    @Test func runningSleepIsLeftOut() {
        var input = InsightsInput()
        input.sleeps = [sleep(Clock.at(1, 9), Clock.at(1, 10)), sleep(Clock.at(1, 21), nil, kind: .night)]
        let result = summary(input).sleep
        #expect(result.perDay?.value == 3600)
        #expect(result.awakeBetween == nil)
    }

    // Review focus 3.
    @Test func overlappingSleepsSkipNegativeGaps() {
        var input = InsightsInput()
        input.sleeps = [sleep(Clock.at(1, 9), Clock.at(1, 11)), sleep(Clock.at(1, 10), Clock.at(1, 12))]
        #expect(summary(input).sleep.awakeBetween == nil)
    }
}
