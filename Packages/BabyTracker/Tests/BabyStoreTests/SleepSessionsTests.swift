import Testing
import Foundation
import BabyCore
@testable import BabyStore

@MainActor @Suite struct SleepSessionsTests {
    @Test func combineAndSeparateSleeps() throws {
        let (store, baby, clock) = try fixture()
        // The gap between the two (1,600 s) exceeds the standard sleep break
        // limit (900 s), so `logSleep` does not auto-join them — this test
        // is about the manual `combineSleeps`/`separateSleep` verbs.
        let a = try store.logSleep(from: clock.now.addingTimeInterval(-8_200), to: clock.now.addingTimeInterval(-4_600), for: baby)
        let b = try store.logSleep(from: clock.now.addingTimeInterval(-3_000), to: clock.now, for: baby)
        try store.combineSleeps([a.id, b.id], for: baby)
        var sessions = try store.sleepSessions(for: baby, from: .distantPast, to: .distantFuture)
        #expect(sessions.count == 1)
        #expect(sessions[0].sleepIDs == [a.id, b.id])
        #expect(sessions[0].asleepSeconds == 3_600 + 3_000)
        try store.separateSleep(at: b.id, for: baby)
        sessions = try store.sleepSessions(for: baby, from: .distantPast, to: .distantFuture)
        #expect(sessions.count == 2)
    }

    /// Ported from History's old menu test: splitting a three-part sleep at
    /// its middle part keeps the first part alone and moves the rest into a
    /// new session together. The session sheet's sleep "Split here" relies
    /// on exactly this.
    @Test func separatingTheMiddleOfThreeJoinedSleepsSplitsTheRestTogether() throws {
        let (store, baby, clock) = try fixture()
        let a = try store.logSleep(from: clock.now.addingTimeInterval(-14_000), to: clock.now.addingTimeInterval(-12_200), for: baby)
        let b = try store.logSleep(from: clock.now.addingTimeInterval(-9_000), to: clock.now.addingTimeInterval(-7_200), for: baby)
        let c = try store.logSleep(from: clock.now.addingTimeInterval(-4_000), to: clock.now.addingTimeInterval(-2_200), for: baby)
        try store.combineSleeps([a.id, b.id, c.id], for: baby)
        #expect(try store.sleepSessions(for: baby, from: .distantPast, to: .distantFuture).map(\.sleepIDs) == [[a.id, b.id, c.id]])

        try store.separateSleep(at: b.id, for: baby)

        let sessions = try store.sleepSessions(for: baby, from: .distantPast, to: .distantFuture)
        #expect(sessions.map(\.sleepIDs) == [[a.id], [b.id, c.id]])
    }

    @Test func aJoinedSleepBelongsToTheDayOfItsFirstPart() throws {
        let (store, baby, clock) = try fixture()
        // Same note as above: the gap exceeds the break limit, so joining
        // here is still the manual verb under test, not auto-join.
        let a = try store.logSleep(from: clock.now.addingTimeInterval(-8_200), to: clock.now.addingTimeInterval(-4_600), for: baby)
        let b = try store.logSleep(from: clock.now.addingTimeInterval(-3_000), to: clock.now, for: baby)
        try store.combineSleeps([a.id, b.id], for: baby)
        let window = try store.sleepSessions(for: baby, from: clock.now.addingTimeInterval(-3_100), to: clock.now)
        #expect(window.isEmpty)   // the session started before the window
    }
}
