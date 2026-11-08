import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import HomeFeature

@MainActor @Suite struct PumpHomeTests {
    private func screen(_ store: BabyStore, _ baby: UUID, now: Date) throws -> HomeScreen {
        HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .left, canLog: true, now: now)
    }

    /// The strip is its own thing: a bottle stays the hero with its own Stop,
    /// and the dock is exactly what it is without a pump.
    @Test func theStripShowsBesideARunningBottle() throws {
        let (store, baby, clock) = try fixture()
        let bottle = try store.startPart(.bottle(.formula), for: baby)
        let without = try screen(store, baby, now: clock.now)
        let pump = try store.startPump(side: .both)
        let with = try screen(store, baby, now: clock.now)
        #expect(with.pump == RunningPump(id: pump.id, side: .both, startedAt: pump.startedAt))
        #expect(with.dock == without.dock)
        #expect(with.hero == without.hero)
        if case .feedRunning(let timerID, let isBottle, _) = with.dock {
            #expect(timerID == bottle.id)
            #expect(isBottle)
        } else {
            Issue.record("the bottle no longer owns the dock")
        }
    }

    @Test func everyBabysHomeShowsTheSameStrip() throws {
        let (store, mira, clock) = try fixture()
        let theo = try store.addBaby(name: "Theo", birthDate: .now, referenceSex: .boys, expectedDueDate: nil).id
        let pump = try store.startPump(side: .left)
        for baby in [mira, theo] {
            #expect(try screen(store, baby, now: clock.now).pump?.id == pump.id)
        }
    }

    @Test func stopEndsThePumpLeavesTheBottleAndNamesTheSessionForHowMuch() async throws {
        let (store, baby, clock) = try fixture()
        let bottle = try store.startPart(.bottle(.formula), for: baby)
        let pump = try store.startPump(side: .right)
        clock.advance(by: 900)
        let model = HomeViewModel(store: store, babyID: baby)
        let stopped = try await model.stopPump()
        #expect(stopped == pump.id)
        #expect(pump.endedAt == clock.now)
        #expect(try store.activeTimer(kind: .feed, for: baby)?.id == bottle.id)
        // Stopped a moment ago on the other phone: nothing to stop, nothing to ask.
        #expect(try await model.stopPump() == nil)
        try model.refresh()
        #expect(model.screen.pump == nil)
    }

    /// The Today card carries today's pumped amount and nothing about when
    /// the last session was; a session without an amount adds no line.
    @Test func theTodayCardCarriesTodaysPumpTotal() throws {
        let (store, baby, clock) = try fixture()
        #expect(try screen(store, baby, now: clock.now).strip?.showsPump == false)
        _ = try store.logPump(side: .left, from: clock.now.addingTimeInterval(-600), to: clock.now.addingTimeInterval(-300), volumeML: nil, note: nil)
        var made = try screen(store, baby, now: clock.now)
        #expect(made.strip?.pumpML == 0)
        #expect(made.strip?.showsPump == false)
        _ = try store.logPump(side: .both, from: clock.now.addingTimeInterval(-200), to: clock.now.addingTimeInterval(-100), volumeML: 85.5, note: nil)
        made = try screen(store, baby, now: clock.now)
        #expect(made.strip?.pumpML == 85.5)
        #expect(made.strip?.showsPump == true)
        #expect(made.pump == nil)
    }

    /// Yesterday's pumping leaves no line on today's card.
    @Test func thePumpLineIsTodayOnly() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.logPump(side: .left, from: clock.now.addingTimeInterval(-900), to: clock.now.addingTimeInterval(-600), volumeML: 40, note: nil)
        #expect(try screen(store, baby, now: clock.now).strip?.showsPump == true)
        clock.advance(by: 25 * 3600)
        #expect(try screen(store, baby, now: clock.now).strip?.showsPump == false)
    }
}
