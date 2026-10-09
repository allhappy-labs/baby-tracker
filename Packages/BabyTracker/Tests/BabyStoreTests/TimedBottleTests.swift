import Foundation
import Testing
import SwiftData
import BabyCore
@testable import BabyStore

@MainActor
@Test func standaloneBottleTimerRecordsActualElapsedTime() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let timer = try store.startPart(.bottle(.formula), for: baby.id)
    let bottleStartedAt = timer.currentBottleStartedAt
    #expect(bottleStartedAt == clock.now)
    #expect(timer.currentSide == nil)

    clock.advance(by: 420)
    let finishedAt = clock.now
    try store.finishBottle(volumeML: 80, contents: .formula, on: timer)
    #expect(timer.currentBottleStartedAt == nil)
    let saved = try #require(try store.stopFeed(timer))
    #expect(saved.kind == .bottle)
    #expect(saved.bottleComponents.count == 1)
    #expect(saved.bottleComponents[0].bottleSeconds == 420)
    #expect(saved.bottleComponents[0].startedAt == bottleStartedAt)
    #expect(saved.bottleComponents[0].endedAt == finishedAt)
    #expect(saved.totalBreastSeconds == 0)
}

/// `stopFeed` still refuses to finish a feed while a bottle is running — the
/// guard that used to be reachable from `beginBreast`/`switchSide` too, which
/// are gone now that one-part timers never try to switch out of a running
/// bottle on the same timer.
@MainActor
@Test func unfinishedBottleCannotBeStoppedDirectly() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000)))
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let timer = try store.startPart(.bottle(.formula), for: baby.id)
    #expect(throws: BottleTimerError.unfinishedBottle) { _ = try store.stopFeed(timer) }
    #expect(try store.activeTimer(kind: .feed, for: baby.id)?.id == timer.id)
}

@MainActor
@Test func timerStartEditCannotMovePastFinishedBottleDuration() {
    let now = Date(timeIntervalSince1970: 1_800_000_000)
    let component = FeedComponent.bottle(id: UUID(), volumeML: 80, contents: .formula, seconds: 300)
    #expect(throws: TimerEditError.partsExceedElapsed) {
        _ = try RunningTimerEdit(
            startedAt: now.addingTimeInterval(-600), currentSideStartedAt: nil,
            completedComponents: [component], newStart: now.addingTimeInterval(-100),
            changedComponents: [], now: now
        )
    }
}

@MainActor
@Test func editingAStartedBottleMovesTheTiedBottleStart() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let timer = try store.startPart(.bottle(.formula), for: baby.id)
    clock.advance(by: 60)
    let moved = timer.startedAt.addingTimeInterval(-60)
    try store.editRunningTimer(id: timer.id, startedAt: moved, components: [])
    #expect(timer.startedAt == moved)
    #expect(timer.currentBottleStartedAt == moved)
    try store.finishBottle(volumeML: 80, contents: .formula, on: timer)
    #expect(timer.completedComponents.first?.bottleSeconds == 120)
}

@MainActor
@Test func editingAFeedStartCannotCrossItsRunningBottleStart() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    // A bottle beginning later than the timer's own start, on the SAME
    // timer, can no longer happen live — `startPart(.bottle)` always ties
    // the bottle's start to the timer's start. Built directly so the edit
    // guard (`startAfterCurrentBottle`) still has a gap to refuse crossing.
    let timer = ActiveTimer(babyID: baby.id, kind: .feed, startedAt: clock.now, timeZoneID: "UTC")
    let bottleStart = clock.now.addingTimeInterval(120)
    timer.currentBottleStartedAt = bottleStart
    store.context.insert(timer)
    try store.context.save()
    clock.advance(by: 180)
    #expect(throws: TimerEditError.startAfterCurrentBottle) {
        try store.editRunningTimer(id: timer.id, startedAt: bottleStart.addingTimeInterval(1), components: [])
    }
    #expect(timer.startedAt < bottleStart)
    #expect(timer.currentBottleStartedAt == bottleStart)
}

/// Bottles have no timer when logged after the fact: the start is exact but
/// nothing assumes when it ended.
@MainActor
@Test func manuallyLoggedBottleHasKnownStartButNoAssumedEnd() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let feed = try store.logBottle(volumeML: 60, contents: .formula, for: baby.id)
    #expect(feed.components[0].startedAt == clock.now)
    #expect(feed.components[0].endedAt == nil)
}
