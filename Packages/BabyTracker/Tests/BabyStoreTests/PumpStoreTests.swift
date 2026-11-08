import Foundation
import Testing
import BabyCore
@testable import BabyStore

@MainActor @Suite("Pump store")
struct PumpStoreTests {
    /// Local noon on a fixed day, so "today" holds whatever zone the tests run in.
    private func noonStore() throws -> (BabyStore, UUID, TestDateProvider) {
        var parts = DateComponents()
        parts.year = 2026; parts.month = 6; parts.day = 15; parts.hour = 12
        let clock = TestDateProvider(Calendar.current.date(from: parts)!)
        let store = try BabyStore.inMemory(dateProvider: clock)
        let baby = try store.addBaby(name: "Mira", birthDate: clock.now.addingTimeInterval(-90 * 86_400), referenceSex: .girls, expectedDueDate: nil)
        return (store, baby.id, clock)
    }

    @Test func aSecondStartIsRefusedWhileOneRuns() throws {
        let (store, _, _) = try fixture()
        let first = try store.startPump(side: .left)
        #expect(first.timeZoneID == TimeZone.current.identifier)
        #expect(throws: PumpError.alreadyRunning) { try store.startPump(side: .both) }
        #expect(try store.runningPump()?.id == first.id)
    }

    /// Pumping is not a timer: a bottle and a sleep keep running beside it,
    /// and stopping the pump leaves them exactly as they were.
    @Test func aPumpRunsAlongsideAFeedAndASleepTimer() throws {
        let (store, baby, clock) = try fixture()
        let bottle = try store.startPart(.bottle(.formula), for: baby)
        let sleep = try store.startSleep(for: baby)
        let pump = try store.startPump(side: .both)
        clock.advance(by: 600)
        try store.stopPump(pump)
        let timers = try store.activeTimers(for: baby)
        #expect(Set(timers.map(\.id)) == [bottle.id, sleep.id])
        #expect(timers.first { $0.id == bottle.id }?.currentBottleStartedAt != nil)
        #expect(try store.runningPump() == nil)
    }

    @Test func stoppingSetsTheEndAndTheAmountMayStayEmpty() throws {
        let (store, _, clock) = try fixture()
        let pump = try store.startPump(side: .right)
        clock.advance(by: 1200)
        let end = try store.stopPump(pump)
        #expect(end == clock.now)
        #expect(pump.endedAt == clock.now)
        #expect(pump.volumeML == nil)
        try store.setPumpAmount(pump, volumeML: 90)
        #expect(pump.volumeML == 90)
        try store.setPumpAmount(pump, volumeML: nil)
        #expect(pump.volumeML == nil)
        #expect(throws: PumpError.notRunning) { try store.stopPump(pump) }
    }

    // Review Focus 2.
    @Test func stoppingPastSixHoursEndsAtTheCap() throws {
        let (store, _, clock) = try fixture()
        let pump = try store.startPump(side: .both)
        clock.advance(by: 7 * 3600)
        let end = try store.stopPump(pump)
        #expect(end == pump.startedAt.addingTimeInterval(PumpRules.maxSeconds))
        #expect(pump.endedAt == end)
    }

    // Review Focus 3.
    @Test func anOunceAmountIsStoredToTwoDecimals() throws {
        let (store, _, _) = try fixture()
        let pump = try store.startPump(side: .both)
        try store.stopPump(pump)
        try store.setPumpAmount(pump, volumeML: Volume(ounces: 3).millilitres)
        #expect(pump.volumeML == 88.72)
        #expect(throws: PumpError.invalidVolume) { try store.setPumpAmount(pump, volumeML: 1000.5) }
        #expect(throws: PumpError.invalidVolume) { try store.setPumpAmount(pump, volumeML: -1) }
        #expect(pump.volumeML == 88.72)
    }

    @Test func aPastSessionIsCheckedLikeTheServer() throws {
        let (store, _, clock) = try fixture()
        let now = clock.now
        #expect(throws: PumpError.invalidTimes) {
            try store.logPump(side: .left, from: now.addingTimeInterval(-600), to: now.addingTimeInterval(-700), volumeML: nil, note: nil)
        }
        #expect(throws: PumpError.invalidTimes) {
            try store.logPump(side: .left, from: now.addingTimeInterval(-8 * 3600), to: now, volumeML: nil, note: nil)
        }
        #expect(throws: PumpError.invalidTimes) {
            try store.logPump(side: .left, from: now.addingTimeInterval(3600), to: now.addingTimeInterval(4000), volumeML: nil, note: nil)
        }
        #expect(try store.pumps(from: .distantPast, to: .distantFuture).isEmpty)
        let past = try store.logPump(side: .left, from: now.addingTimeInterval(-1800), to: now.addingTimeInterval(-600), volumeML: 120, note: "  morning  ")
        #expect(past.endedAt != nil)
        #expect(past.volumeML == 120)
        #expect(past.note == "morning")
        // A finished session is never in the way of a start.
        #expect(try store.startPump(side: .both).isRunning)
    }

    @Test func editAndDelete() throws {
        let (store, _, clock) = try fixture()
        let pump = try store.logPump(side: .left, from: clock.now.addingTimeInterval(-1800), to: clock.now.addingTimeInterval(-600), volumeML: nil, note: nil)
        try store.updatePump(pump, side: .both, startedAt: pump.startedAt.addingTimeInterval(-60), endedAt: pump.endedAt,
                             volumeML: 45.5, note: "edited")
        #expect(pump.side == .both)
        #expect(pump.volumeML == 45.5)
        #expect(pump.note == "edited")
        // A finished session cannot be reopened.
        #expect(throws: PumpError.invalidTimes) {
            try store.updatePump(pump, side: .both, startedAt: pump.startedAt, endedAt: nil, volumeML: nil, note: nil)
        }
        #expect(pump.endedAt != nil)
        try store.softDelete(pump)
        #expect(try store.pump(id: pump.id) == nil)
    }

    /// The Start of a session still running can be corrected; it keeps running.
    @Test func aRunningPumpsStartAndSideCanBeEdited() throws {
        let (store, _, clock) = try fixture()
        let pump = try store.startPump(side: .both)
        clock.advance(by: 300)
        try store.updatePump(pump, side: .left, startedAt: pump.startedAt.addingTimeInterval(-120), endedAt: nil, volumeML: nil, note: "night")
        #expect(pump.isRunning)
        #expect(pump.side == .left)
        #expect(pump.note == "night")
        #expect(throws: PumpError.invalidTimes) {
            try store.updatePump(pump, side: .left, startedAt: clock.now.addingTimeInterval(3600), endedAt: nil, volumeML: nil, note: nil)
        }
    }

    /// One session shows on every baby's screens: the same row, not copies.
    @Test func pumpsAppearInEveryBabysHistoryAndHome() throws {
        let (store, mira, clock) = try noonStore()
        let theo = try store.addBaby(name: "Theo", birthDate: clock.now.addingTimeInterval(-30 * 86_400), referenceSex: .boys, expectedDueDate: nil).id
        let pump = try store.logPump(side: .both, from: clock.now.addingTimeInterval(-3600), to: clock.now.addingTimeInterval(-2400), volumeML: 80, note: nil)
        for baby in [mira, theo] {
            let items = try store.entries(for: baby, from: clock.now.addingTimeInterval(-86_400), to: clock.now.addingTimeInterval(60))
            #expect(items.compactMap { if case .pump(let p) = $0 { p.id } else { nil } } == [pump.id])
            let day = DayBucket.key(for: clock.now, timeZoneID: TimeZone.current.identifier)
            #expect(try store.entries(for: baby, on: day).compactMap { if case .pump(let p) = $0 { p.id } else { nil } } == [pump.id])
            if case .pump(let found)? = try store.entry(id: pump.id, for: baby) {
                #expect(found.id == pump.id)
            } else {
                Issue.record("entry(id:for:) missed the pump")
            }
            #expect(try store.homeSnapshot(for: baby).lastPumpEndedAt == pump.endedAt)
        }
    }

    @Test func deletingABabyLeavesPumps() throws {
        let (store, baby, clock) = try fixture()
        let pump = try store.logPump(side: .left, from: clock.now.addingTimeInterval(-900), to: clock.now, volumeML: 50, note: nil)
        let running = try store.startPump(side: .both)
        try store.deleteBaby(try #require(try store.baby(baby)))
        #expect(try store.pump(id: pump.id)?.isTombstoned == false)
        #expect(try store.runningPump()?.id == running.id)
    }

    /// Today's total counts finished sessions that started today. One without
    /// an amount counts toward "last" but adds nothing; one still running and
    /// one that started yesterday add nothing either.
    @Test func todaysTotalAndLastSession() throws {
        let (store, baby, clock) = try noonStore()
        let dayStart = Calendar.current.startOfDay(for: clock.now)
        _ = try store.logPump(side: .both, from: dayStart.addingTimeInterval(-1200), to: dayStart.addingTimeInterval(-600), volumeML: 200, note: nil)
        _ = try store.logPump(side: .left, from: clock.now.addingTimeInterval(-5400), to: clock.now.addingTimeInterval(-4800), volumeML: 60, note: nil)
        _ = try store.logPump(side: .left, from: clock.now.addingTimeInterval(-4000), to: clock.now.addingTimeInterval(-3700), volumeML: 25.5, note: nil)
        let unmeasured = try store.logPump(side: .right, from: clock.now.addingTimeInterval(-3600), to: clock.now.addingTimeInterval(-3000), volumeML: nil, note: nil)
        _ = try store.startPump(side: .both)
        let snapshot = try store.homeSnapshot(for: baby)
        #expect(snapshot.todayPumpML == 85.5)
        #expect(snapshot.lastPumpEndedAt == unmeasured.endedAt)
        #expect(snapshot.runningPump?.side == .both)
        #expect(try store.lastPumpVolumeML() == 25.5)
    }

    /// The wire keeps milliseconds. A start just short of the next one must
    /// not let a capped end read six hours and a millisecond there.
    @Test func aForgottenPumpEndsWithinSixHoursOnTheWire() throws {
        let clock = TestDateProvider(Date(timeIntervalSinceReferenceDate: 812_345_678))
        let store = try BabyStore.inMemory(dateProvider: clock)
        let wire = Date.ISO8601FormatStyle(includingFractionalSeconds: true)
        let fractions = Array(stride(from: 0.0001, to: 0.001, by: 0.00003)) + [0.0009999, 0.00099999, 0.000999999]
        for (i, fraction) in fractions.enumerated() {
            let start = Date(timeIntervalSinceReferenceDate: 812_345_678 + Double(i) * 86_400 + fraction)
            clock.advance(by: start.addingTimeInterval(1).timeIntervalSince(clock.now))
            let pump = try store.startPump(side: .left, at: start)
            clock.advance(by: PumpRules.maxSeconds + 3600)
            try store.stopPump(pump)
            let sentStart = try wire.parse(MutationFactory.wireTime(pump.startedAt))
            let sentEnd = try wire.parse(MutationFactory.wireTime(try #require(pump.endedAt)))
            #expect(sentEnd.timeIntervalSince(sentStart) <= PumpRules.maxSeconds, "fraction \(fraction)")
            #expect(sentEnd.timeIntervalSince(sentStart) > PumpRules.maxSeconds - 0.01)
            try store.softDelete(pump)
        }
    }

    @Test func aStaleEntryCannotBeEditedInAnotherRealm() throws {
        let (store, _, clock) = try fixture()
        let pump = try store.logPump(side: .left, from: clock.now.addingTimeInterval(-900), to: clock.now, volumeML: nil, note: nil)
        try store.setActiveRealm(Fixture.sharedRealm)
        #expect(throws: RealmWriteError.wrongRealm) { try store.setPumpAmount(pump, volumeML: 40) }
        #expect(pump.volumeML == nil)
        // The other realm's sessions are not this realm's.
        #expect(try store.pumps(from: .distantPast, to: .distantFuture).isEmpty)
    }
}
