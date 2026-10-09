import Testing
import Foundation
import BabyCore
@testable import BabyStore

private func noonLocal() -> Date {
    var components = DateComponents()
    components.year = 2_025
    components.month = 6
    components.day = 12
    components.hour = 12
    return Calendar.current.date(from: components)!
}

@Test func theNoDataSummaryInventsNothing() {
    let summary = WidgetStatusSummary.noData

    // A placed widget that cannot read the store used to fall back to the
    // gallery's sample values — "2h 14m", "Awake 40m", "55m" — so a fresh
    // install showed a feed, a nap and a change that never happened.
    #expect(summary.feedAge == "—")
    #expect(summary.sleepText == "—")
    #expect(summary.diaperAge == "—")

    // And no name, so nothing can be attributed to a child.
    #expect(summary.babyName.isEmpty)
    #expect(summary.babyName != "Baby")
}

@MainActor
@Test func aBabyWithNothingLoggedYetShowsDashesRatherThanZeroes() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: noonLocal(), referenceSex: .girls, expectedDueDate: nil)

    let summary = WidgetStatusSummary(
        babyName: baby.name,
        snapshot: try store.homeSnapshot(for: baby.id),
        now: clock.now
    )

    #expect(summary.babyName == "Mira")
    #expect(summary.feedAge == "—")
    #expect(summary.sleepText == "—")
    #expect(summary.diaperAge == "—")
}

@MainActor
@Test func theFeedLineIsAnAgeAndTheRunningNapLineIsADuration() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: noonLocal(), referenceSex: .girls, expectedDueDate: nil)

    let feed = try store.startPart(.breast(.left), for: baby.id)
    clock.advance(by: 600)
    _ = try store.stopFeed(feed)
    _ = try store.startSleep(for: baby.id)
    clock.advance(by: 8_040)

    let summary = WidgetStatusSummary(
        babyName: baby.name,
        snapshot: try store.homeSnapshot(for: baby.id),
        now: clock.now
    )

    // Time since the feed ended — an AGE.
    #expect(summary.feedAge == "2h 14m")
    // How long the nap has lasted — a DURATION, so zero would read "0m".
    #expect(summary.sleepText == "Asleep 2h 14m")
    #expect(summary.diaperAge == "—")
}

/// Task 12. `awakeSince` is the end of the last completed sleep PART, not
/// the last SleepEntry by `startedAt` — the widget must read the same field
/// `HomeSnapshotSessionTests.awakeIsMeasuredFromTheLastSleepPart` covers.
@MainActor
@Test func widgetAwakeTimeUsesTheLastSleepPart() throws {
    let (store, baby, clock) = try fixture()
    _ = try store.stopSleep(try store.startSleep(for: baby).advancing(clock, by: 600))
    clock.advance(by: 120)
    let summary = WidgetStatusSummary(babyName: "Mira", snapshot: try store.homeSnapshot(for: baby), now: clock.now)
    #expect(summary.sleepText.contains("2m"))
}
