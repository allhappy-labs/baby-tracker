import Testing
import Foundation
@testable import BabyCore

@Test func testClockAdvancesOnDemandOnly() {
    let start = Date(timeIntervalSince1970: 1_000_000)
    let clock = TestDateProvider(start)

    #expect(clock.now == start)

    clock.advance(by: 90)
    #expect(clock.now == start.addingTimeInterval(90))
}

@Test func systemClockMovesForward() {
    let clock = SystemDateProvider()
    let first = clock.now
    #expect(clock.now >= first)
}
