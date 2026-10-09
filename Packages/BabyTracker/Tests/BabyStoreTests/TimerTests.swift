import Testing
import Foundation
import SwiftData
import BabyCore
@testable import BabyStore

@MainActor
private func makeStore(_ clock: TestDateProvider) throws -> (BabyStore, UUID) {
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    return (store, baby.id)
}

@MainActor
@Test func elapsedSurvivesTheAppBeingKilledMidFeed() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)

    let timer = try store.startPart(.breast(.left), for: babyID)

    // Two hours pass with the process dead. A fresh store over the same
    // container is exactly what a relaunch looks like.
    clock.advance(by: 7_200)
    let relaunched = BabyStore(
        container: store.container,
        dateProvider: clock,
        deviceID: "test-device"
    )
    let recovered = try relaunched.activeTimer(kind: .feed, for: babyID)

    #expect(recovered != nil)
    #expect(recovered!.elapsedSeconds(now: clock.now) == 7_200)
    #expect(recovered!.id == timer.id)
}

@MainActor
@Test func twentyNineSecondBreastPartIsDiscarded() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)
    let timer = try store.startPart(.breast(.left), for: babyID)

    clock.advance(by: 29)
    #expect(try store.stopFeed(timer) == nil)

    #expect(try store.lastFeed(for: babyID) == nil)
}

@MainActor
@Test func thirtySecondBreastPartSurvivesCleanup() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)
    let timer = try store.startPart(.breast(.left), for: babyID)

    clock.advance(by: 30)
    _ = try store.stopFeed(timer)

    let saved = try #require(try store.lastFeed(for: babyID))
    #expect(saved.components.map(\.breastSide) == [.left])
    #expect(saved.totalBreastSeconds == 30)
    #expect(saved.lastSideUsed == .left)
}

@MainActor
@Test func stoppingATimerWithNoComponentsDiscardsIt() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)
    let timer = try store.startPart(.breast(.left), for: babyID)

    // No clock advance: the running side has run for zero seconds, so stop
    // sees nothing worth recording.
    #expect(try store.stopFeed(timer) == nil)

    #expect(try store.lastFeed(for: babyID) == nil)
    #expect(try store.activeTimer(kind: .feed, for: babyID) == nil)
}

@MainActor
@Test func stoppingAFeedClearsTheTimerAndCreatesTheEntry() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)

    let timer = try store.startPart(.breast(.left), for: babyID)
    clock.advance(by: 1_080)
    let feed = try #require(try store.stopFeed(timer))

    #expect(feed.endedAt == clock.now)
    #expect(feed.startedAt == clock.now.addingTimeInterval(-1_080))
    #expect(try store.activeTimer(kind: .feed, for: babyID) == nil)
}

@MainActor
@Test func nextFeedSuggestsTheOppositeSide() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)

    // No history yet: left is as good a guess as any.
    #expect(try store.suggestedSide(for: babyID) == .left)

    let timer = try store.startPart(.breast(.left), for: babyID)
    clock.advance(by: 600)
    _ = try store.stopFeed(timer)

    #expect(try store.suggestedSide(for: babyID) == .right)
}

/// Tied starts use canonical UUID order, independent of local insertion order.
@MainActor
@Test func suggestedSideBreaksTiesByCanonicalUUID() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)
    let larger = FeedEntry(id: UUID(uuidString: "10000000-0000-4000-8000-000000000002")!,
        babyID: babyID, kind: .breast, startedAt: clock.now, timeZoneID: "UTC")
    larger.components = [.breast(id: UUID(), side: .left, seconds: 60)]
    store.context.insert(larger)
    let smaller = FeedEntry(id: UUID(uuidString: "10000000-0000-4000-8000-000000000001")!,
        babyID: babyID, kind: .breast, startedAt: clock.now, timeZoneID: "UTC")
    smaller.components = [.breast(id: UUID(), side: .right, seconds: 60)]
    store.context.insert(smaller)
    try store.context.save()
    #expect(try store.suggestedSide(for: babyID) == .right)
}

@MainActor
@Test func sleepAndFeedCanRunAtTheSameTime() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)

    _ = try store.startSleep(for: babyID)
    clock.advance(by: 120)
    _ = try store.startPart(.breast(.left), for: babyID)

    let running = try store.activeTimers(for: babyID)
    #expect(running.count == 2)
    // Most recently started first — this is what the dial shows.
    #expect(running[0].kind == .feed)
    #expect(running[1].kind == .sleep)
}

@MainActor
@Test func stoppingSleepWritesTheSleepEntry() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)

    let timer = try store.startSleep(for: babyID)
    clock.advance(by: 6_600)
    let sleep = try store.stopSleep(timer)

    #expect(sleep.durationSeconds == 6_600)
    #expect(try store.activeTimer(kind: .sleep, for: babyID) == nil)
    #expect(try store.changeLog().last?.entityType == "SleepEntry")
}

@MainActor
@Test func repeatingSleepStartKeepsTheOriginalTimerAndStartTime() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)

    let first = try store.startSleep(for: babyID)
    clock.advance(by: 600)
    let repeated = try store.startSleep(for: babyID)

    #expect(repeated.id == first.id)
    #expect(repeated.startedAt == first.startedAt)
    #expect(try store.activeTimer(kind: .sleep, for: babyID)?.id == first.id)
    #expect(try store.activeTimers(for: babyID).count == 1)
}

@MainActor
@Test func bottleFeedsAreLoggedWithoutATimer() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)

    let feed = try store.logBottle(volumeML: 120, contents: .formula, for: babyID)

    #expect(feed.kind == .bottle)
    #expect(feed.bottleComponents.map(\.volumeML) == [120])
    #expect(feed.bottleComponents.map(\.bottleContents) == [.formula])
    #expect(feed.endedAt == clock.now)
    #expect(try store.activeTimers(for: babyID).isEmpty)
}

// MARK: Editing a running timer
//
// The store applies exactly the rules the server does (`editTimer`,
// `Server/internal/records/timers.go`), so an edit this phone accepts is one
// the server will too.

@MainActor
@Test func editingTheStartMovesACurrentSideThatBeganWithIt() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)
    let timer = try store.startPart(.breast(.left), for: babyID)
    let original = timer.startedAt
    clock.advance(by: 600)

    try store.editRunningTimer(id: timer.id, startedAt: original.addingTimeInterval(-300), components: [])

    #expect(timer.startedAt == original.addingTimeInterval(-300))
    // The left side has run since the feed began, so it began when the feed did.
    #expect(timer.currentSideStartedAt == original.addingTimeInterval(-300))
    #expect(timer.elapsedSeconds(now: clock.now) == 900)
}

/// With no side running, the side start is the timer's own start — what
/// remote apply writes (`RemoteTimerState.write`) — so a moved start carries
/// it along, and the two phones agree on the column.
@MainActor
@Test func editingTheStartWithNoSideRunningMovesTheSideStartToo() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)
    let timer = try store.startPart(.bottle(.formula), for: babyID)
    let original = timer.startedAt
    clock.advance(by: 600)
    #expect(timer.currentSide == nil)

    try store.editRunningTimer(id: timer.id, startedAt: original.addingTimeInterval(-300), components: [])

    #expect(timer.startedAt == original.addingTimeInterval(-300))
    #expect(timer.currentSideStartedAt == original.addingTimeInterval(-300))
}

@MainActor
@Test func aStartAfterASideThatBeganLaterIsRefusedAndOneBeforeItLeavesTheSide() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)
    let timer = try store.startPart(.breast(.left), for: babyID)
    let original = timer.startedAt
    clock.advance(by: 600)
    // A side starting later than the timer's own start can no longer happen
    // live — `startPart(.breast)` always ties the side's start to the
    // timer's start, and there is no live switch to move it apart anymore.
    // Built directly so the edit guard (`startAfterCurrentSide`) still has
    // a gap to refuse crossing.
    let sideStart = clock.now
    timer.currentSideStartedAt = sideStart
    try store.context.save()
    clock.advance(by: 300)

    #expect(throws: TimerEditError.startAfterCurrentSide) {
        try store.editRunningTimer(id: timer.id, startedAt: sideStart.addingTimeInterval(60), components: [])
    }
    #expect(timer.startedAt == original)

    // Moving it earlier keeps the right side where it is.
    try store.editRunningTimer(id: timer.id, startedAt: original.addingTimeInterval(-120), components: [])
    #expect(timer.startedAt == original.addingTimeInterval(-120))
    #expect(timer.currentSideStartedAt == sideStart)
}

@MainActor
@Test func aStartInTheFutureOrMoreThanAWeekBackIsRefused() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)
    let timer = try store.startSleep(for: babyID)
    let original = timer.startedAt
    clock.advance(by: 600)

    #expect(throws: TimerEditError.startInFuture) {
        try store.editRunningTimer(id: timer.id, startedAt: clock.now.addingTimeInterval(1), components: [])
    }
    #expect(throws: TimerEditError.startInFuture) {
        try store.editRunningTimer(id: timer.id, startedAt: clock.now.addingTimeInterval(-7 * 86_400 - 1), components: [])
    }
    #expect(timer.startedAt == original)

    // A sleep's start is editable too.
    try store.editRunningTimer(id: timer.id, startedAt: original.addingTimeInterval(-3_600), components: [])
    #expect(timer.startedAt == original.addingTimeInterval(-3_600))
}

@MainActor
@Test func editedPartsKeepTheirIDsAndPositionsAndTheBridgesFollow() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)
    let timer = try store.startPart(.breast(.left), for: babyID)
    clock.advance(by: 600)
    // Two finished parts plus a running third on one still-running timer can
    // no longer be built live — one-part timers give each part its own
    // timer. Built directly here, the way a legacy remote timer would leave
    // it.
    let left = UUID()
    let bottle = UUID()
    let leftEnd = clock.now
    timer.completedComponents = [
        .breast(id: left, side: .left, seconds: 600, startedAt: timer.startedAt, endedAt: leftEnd),
        .bottle(id: bottle, volumeML: 50, contents: .breastMilk),
    ]
    timer.currentSide = .right
    timer.currentSideStartedAt = leftEnd
    try store.context.save()
    clock.advance(by: 60)

    try store.editRunningTimer(id: timer.id, startedAt: nil, components: [
        .bottle(id: bottle, volumeML: 70, contents: .formula),
        .breast(id: left, side: .left, seconds: 300),
    ])

    #expect(timer.completedComponents == [
        .breast(id: left, side: .left, seconds: 300,
                startedAt: leftEnd.addingTimeInterval(-300), endedAt: leftEnd),
        .bottle(id: bottle, volumeML: 70, contents: .formula),
    ])
    #expect(timer.currentSide == .right)

    // And the finished feed carries the corrected values.
    let feed = try #require(try store.stopFeed(timer))
    #expect(feed.components.prefix(2) == [
        .breast(id: left, side: .left, seconds: 300,
                startedAt: leftEnd.addingTimeInterval(-300), endedAt: leftEnd),
        .bottle(id: bottle, volumeML: 70, contents: .formula),
    ])
    #expect(feed.totalBottleVolumeML == 70)
}

@MainActor
@Test func editingFinishedTimerStepDurationKeepsExactBoundsConsistent() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)
    let timer = try store.startPart(.breast(.left), for: babyID)
    clock.advance(by: 600)
    // One finished part on a still-running timer can no longer be built by a
    // live switch — built directly, as a legacy remote timer would leave it.
    let leftID = UUID()
    let originalEnd = clock.now
    timer.completedComponents = [.breast(id: leftID, side: .left, seconds: 600, startedAt: timer.startedAt, endedAt: originalEnd)]
    timer.currentSide = .right
    timer.currentSideStartedAt = originalEnd
    try store.context.save()
    clock.advance(by: 120)

    try store.editRunningTimer(id: timer.id, startedAt: nil,
                               components: [.breast(id: leftID, side: .left, seconds: 300)])
    #expect(timer.completedComponents[0].startedAt == originalEnd.addingTimeInterval(-300))
    #expect(timer.completedComponents[0].endedAt == originalEnd)
    #expect(timer.completedComponents[0].durationSeconds == 300)
    let feed = try #require(try store.stopFeed(timer))
    #expect(feed.components[0].startedAt == originalEnd.addingTimeInterval(-300))
    #expect(feed.components[0].endedAt == originalEnd)
}

/// The finished breast parts all happened before the running side began —
/// or before now when none runs.
@MainActor
@Test func partsLongerThanTheTimeBeforeTheRunningSideAreRefused() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)
    let timer = try store.startPart(.breast(.left), for: babyID)
    let original = timer.startedAt
    clock.advance(by: 600)
    // A finished part on a still-running timer can no longer be built by a
    // live switch — built directly, as a legacy remote timer would leave it.
    let left = UUID()
    timer.completedComponents = [.breast(id: left, side: .left, seconds: 600, startedAt: original, endedAt: clock.now)]
    timer.currentSide = .right
    timer.currentSideStartedAt = clock.now
    try store.context.save()
    clock.advance(by: 900)

    #expect(throws: TimerEditError.partsExceedElapsed) {
        try store.editRunningTimer(id: timer.id, startedAt: nil, components: [.breast(id: left, side: .left, seconds: 601)])
    }
    // Moving the start later shrinks the room the parts have.
    #expect(throws: TimerEditError.partsExceedElapsed) {
        try store.editRunningTimer(id: timer.id, startedAt: original.addingTimeInterval(1), components: [])
    }
    try store.editRunningTimer(id: timer.id, startedAt: original.addingTimeInterval(100), components: [.breast(id: left, side: .left, seconds: 500)])
    #expect(timer.completedComponents[0].id == left)
    #expect(timer.completedComponents[0].durationSeconds == 500)
    #expect(timer.completedComponents[0].startedAt == original.addingTimeInterval(100))

    // With no side running the bound is now. A second finished part on one
    // still-running timer can no longer be built live — appended directly.
    let right = UUID()
    timer.completedComponents.append(.breast(id: right, side: .right, seconds: 900, startedAt: original.addingTimeInterval(600), endedAt: clock.now))
    timer.currentSide = nil
    try store.context.save()
    #expect(throws: TimerEditError.partsExceedElapsed) {
        try store.editRunningTimer(id: timer.id, startedAt: nil, components: [.breast(id: right, side: .right, seconds: 901)])
    }
}

@MainActor
@Test func anEditNamingNoPartOfThisTimerOrTheWrongKindIsRefused() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)
    let timer = try store.startPart(.breast(.left), for: babyID)
    clock.advance(by: 600)
    // One finished part on a still-running timer can no longer be built by a
    // live switch — built directly, as a legacy remote timer would leave it.
    let left = UUID()
    timer.completedComponents = [.breast(id: left, side: .left, seconds: 600, startedAt: timer.startedAt, endedAt: clock.now)]
    timer.currentSide = .right
    timer.currentSideStartedAt = clock.now
    try store.context.save()

    #expect(throws: TimerEditError.unknownComponent) {
        try store.editRunningTimer(id: timer.id, startedAt: nil, components: [.breast(id: UUID(), side: .left, seconds: 10)])
    }
    #expect(throws: TimerEditError.unknownComponent) {
        try store.editRunningTimer(id: timer.id, startedAt: nil, components: [.bottle(id: left, volumeML: 30, contents: .formula)])
    }
    #expect(timer.completedComponents[0].id == left)
    #expect(timer.completedComponents[0].durationSeconds == 600)
    #expect(timer.completedComponents[0].startedAt == timer.startedAt)
}

@MainActor
@Test func anEditThatChangesNothingOrTargetsAStoppedTimerIsRefused() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)
    let timer = try store.startPart(.breast(.left), for: babyID)
    clock.advance(by: 600)
    // One finished part on a still-running timer can no longer be built by a
    // live switch — built directly, as a legacy remote timer would leave it.
    let left = FeedComponent.breast(id: UUID(), side: .left, seconds: 600, startedAt: timer.startedAt, endedAt: clock.now)
    timer.completedComponents = [left]
    timer.currentSide = .right
    timer.currentSideStartedAt = clock.now
    try store.context.save()

    #expect(throws: TimerEditError.nothingChanged) {
        try store.editRunningTimer(id: timer.id, startedAt: nil, components: [])
    }
    #expect(throws: TimerEditError.nothingChanged) {
        try store.editRunningTimer(id: timer.id, startedAt: timer.startedAt, components: [left])
    }

    let timerID = timer.id
    _ = try store.stopFeed(timer)
    #expect(throws: TimerEditError.timerEnded) {
        try store.editRunningTimer(id: timerID, startedAt: clock.now.addingTimeInterval(-60), components: [])
    }
}
