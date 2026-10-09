import Testing
import Foundation
import SwiftData
import BabyCore
@testable import BabyStore

/// Noon local, fixed on a June date so `DayBucket.bounds` sees an ordinary
/// 24-hour day. Tests that assert on "today" totals need headroom in both
/// directions from local midnight, which a raw `timeIntervalSince1970` does
/// not reliably give — the same instant lands on different local calendar
/// days depending on the machine's timezone.
@MainActor
private func noonLocal() -> Date {
    var parts = DateComponents()
    parts.year = 2026; parts.month = 6; parts.day = 15
    parts.hour = 12; parts.minute = 0
    return Calendar.current.date(from: parts)!
}

@MainActor
@Test func homeSnapshotReportsTheLastOfEachKind() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    _ = try store.logBottle(volumeML: 90, contents: .formula, for: baby.id)
    clock.advance(by: 3_600)
    _ = try store.logDiaper(.wet, for: baby.id)
    clock.advance(by: 3_600)
    _ = try store.logBottle(volumeML: 120, contents: .formula, for: baby.id)

    let snapshot = try store.homeSnapshot(for: baby.id)

    #expect(snapshot.lastFeed?.totalBottleVolumeML == 120)
    #expect(snapshot.lastDiaper?.contents == .wet)
    #expect(snapshot.lastSleep == nil)
    #expect(snapshot.todayFeedCount == 2)
    #expect(snapshot.todayVolumeML == 210)
    #expect(snapshot.todayDiaperCount == 1)
}

@MainActor
@Test func deletedEntriesAreInvisibleToQueries() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    let diaper = try store.logDiaper(.wet, for: baby.id)
    try store.softDelete(diaper)

    let snapshot = try store.homeSnapshot(for: baby.id)
    #expect(snapshot.lastDiaper == nil)
    #expect(snapshot.todayDiaperCount == 0)
}

@MainActor
@Test func snapshotIsScopedToOneBaby() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let mira = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let theo = try store.addBaby(name: "Theo", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    _ = try store.logDiaper(.wet, for: mira.id)
    _ = try store.logDiaper(.dirty, for: theo.id)
    _ = try store.logDiaper(.mixed, for: theo.id)

    #expect(try store.homeSnapshot(for: mira.id).todayDiaperCount == 1)
    #expect(try store.homeSnapshot(for: theo.id).todayDiaperCount == 2)
}

@MainActor
@Test func runningTimersAppearInTheSnapshotMostRecentFirst() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    _ = try store.startSleep(for: baby.id)
    clock.advance(by: 300)
    _ = try store.startPart(.breast(.left), for: baby.id)

    let snapshot = try store.homeSnapshot(for: baby.id)
    #expect(snapshot.runningTimers.count == 2)
    #expect(snapshot.runningTimers.first?.kind == .feed)
}

/// A distinct-timestamps test cannot catch this: `lastFeed`/`lastSleep`/
/// `lastDiaper` used to fetch descending by `startedAt` and read `.first`,
/// but SwiftData's sort is stable, so ties preserve insertion order
/// regardless of the requested direction — the descending fetch's "first"
/// on a tie was actually the *first-inserted* entry. Backdating (Task 23)
/// makes identical `startedAt` values reachable in normal use, and these
/// three queries populate the Home screen's status rows.
@MainActor
@Test func lastFeedBreaksTiesByInsertionOrderNotSortOrder() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    _ = try store.logBottle(volumeML: 90, contents: .formula, for: baby.id)
    let mostRecent = try store.logBottle(volumeML: 120, contents: .breastMilk, for: baby.id)

    #expect(try store.lastFeed(for: baby.id)?.id == mostRecent.id)
}

@MainActor
@Test func lastSleepBreaksTiesByInsertionOrderNotSortOrder() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    _ = try store.logSleep(from: clock.now, to: clock.now, for: baby.id)
    let mostRecent = try store.logSleep(from: clock.now, to: clock.now, for: baby.id)

    #expect(try store.lastSleep(for: baby.id)?.id == mostRecent.id)
}

@MainActor
@Test func lastDiaperBreaksTiesByInsertionOrderNotSortOrder() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    _ = try store.logDiaper(.wet, for: baby.id)
    let mostRecent = try store.logDiaper(.dirty, for: baby.id)

    #expect(try store.lastDiaper(for: baby.id)?.id == mostRecent.id)
}

@MainActor
@Test func historyMergesAllKindsNewestFirst() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    _ = try store.logBottle(volumeML: 90, contents: .formula, for: baby.id)
    clock.advance(by: 600)
    _ = try store.logDiaper(.wet, for: baby.id)

    let day = DayBucket.key(for: clock.now, timeZoneID: TimeZone.current.identifier)
    let items = try store.entries(for: baby.id, on: day)

    #expect(items.count == 2)
    #expect(items[0].startedAt > items[1].startedAt)
}
