import Testing
import Foundation
@testable import LoggingFeature

@Suite struct TimeSpanTests {
    let now = Date(timeIntervalSince1970: 1_700_000_000)

    @Test func lengthIsEndMinusStart() {
        let span = TimeSpan(start: now.addingTimeInterval(-1080), end: now)
        #expect(span.seconds == 1080)
        #expect(span.lengthText == "18m")
    }

    @Test func noEndHasNoLengthAndNoProblem() {
        let span = TimeSpan(start: now.addingTimeInterval(-60), end: nil)
        #expect(span.seconds == nil)
        #expect(span.lengthText == nil)
        #expect(span.problem(latest: now) == nil)
    }

    @Test func anEndAtOrBeforeTheStartIsRefused() {
        #expect(TimeSpan(start: now, end: now).problem(latest: now) == .endBeforeStart)
        #expect(TimeSpan(start: now, end: now.addingTimeInterval(-60)).problem(latest: now) == .endBeforeStart)
    }

    @Test func aStartOrEndAfterNowIsRefused() {
        #expect(TimeSpan(start: now.addingTimeInterval(60), end: nil).problem(latest: now) == .inTheFuture)
        #expect(TimeSpan(start: now.addingTimeInterval(-60), end: now.addingTimeInterval(60))
            .problem(latest: now) == .inTheFuture)
    }

    @Test func shorterThanTheMinimumIsRefused() {
        let span = TimeSpan(start: now.addingTimeInterval(-20), end: now)
        #expect(span.problem(latest: now, minimumSeconds: 30) == .tooShort)
        #expect(span.problem(latest: now) == nil)
    }

    @Test func crossingMidnightIsOneInterval() throws {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = .gmt
        let start = try #require(calendar.date(from: DateComponents(year: 2026, month: 10, day: 8, hour: 23, minute: 40)))
        let span = TimeSpan(start: start, end: start.addingTimeInterval(40 * 60))
        #expect(span.problem(latest: start.addingTimeInterval(3600)) == nil)
        #expect(span.lengthText == "40m")
    }

    @Test func everyProblemHasCopy() {
        for problem in [TimeSpan.Problem.endBeforeStart, .inTheFuture, .tooShort] {
            #expect(!TimeSpanCopy.message(problem).isEmpty)
        }
    }
}
