import Foundation
import Testing
import BabyCore
@testable import BabyStore

/// `LiveActivityController` against an in-memory board: what the cards
/// actually end up showing after a sequence of starts, stops and activations.

@MainActor
private func onBoard<R>(_ board: MemoryCardBoard, _ body: () async throws -> R) async rethrows -> R {
    try await LiveCards.$board.withValue(board) { try await body() }
}

/// A Stop leaves the card up in the break, stale at the break's end.
@MainActor @Test func aStopMovesTheCardIntoTheBreak() async throws {
    let (store, baby, clock) = try fixture()
    let board = MemoryCardBoard()
    try await onBoard(board) {
        let left = try store.startPart(.breast(.left), for: baby)
        LiveActivityController.start(for: left, in: store)
        clock.advance(by: 300)
        try await LiveActivityController.stopping(left, in: store) { _ = try store.stopFeed(left) }
    }
    let card = try #require(board.cards().first)
    #expect(board.cards().count == 1)
    #expect(card.state.phase == .feedBreak)
    #expect(card.state.suggestedSide == "right")
    #expect(card.staleDate == clock.now.addingTimeInterval(1_800))
}

/// Activation with two cards for one baby and kind while a part runs on the
/// second: the second stays, running; the first goes. Judging each card on
/// its own ended both — the first as unowned, the second as a duplicate.
@MainActor @Test func activationKeepsTheCardTheRunningPartOwns() async throws {
    let (store, baby, _) = try fixture()
    let left = try store.startPart(.breast(.left), for: baby)
    let running = TimerActivityContentState(ActivityPresentation(timer: left))
    let stray = LiveCard(id: "stray", babyID: baby, kindRaw: TimerKind.feed.rawValue, state: running)
    let owned = LiveCard(id: "owned", babyID: baby, kindRaw: TimerKind.feed.rawValue, state: running)
    let board = MemoryCardBoard(cards: [stray, owned])
    try store.setLiveActivityID("owned", on: left)

    await onBoard(board) { await LiveActivityController.endExpiredBreaks(in: store) }

    #expect(board.cards().map(\.id) == ["owned"])
    #expect(board.cards().first?.state.phase == .running)
    #expect(left.liveActivityID == "owned")
}

@Test func theCardKeptIsTheOwnedOneElseTheFirst() {
    let state = TimerActivityContentState(startedAt: .now, sideLabel: "", activityKind: .breastLeft)
    let cards = ["a", "b", "c"].map { LiveCard(id: $0, babyID: UUID(), kindRaw: "feed", state: state) }
    #expect(LiveActivityController.chosenCard(among: cards, ownedID: "b")?.id == "b")
    #expect(LiveActivityController.chosenCard(among: cards, ownedID: nil)?.id == "a")
    #expect(LiveActivityController.chosenCard(among: cards, ownedID: "gone")?.id == "a")
    #expect(LiveActivityController.chosenCard(among: [], ownedID: "b") == nil)
}

/// A break card whose content went out of date — here, the break limit
/// changed in Settings — is re-pushed on activation, not merely kept.
@MainActor @Test func activationRefreshesABreakCardThatIsOutOfDate() async throws {
    let (store, baby, clock) = try fixture()
    let board = MemoryCardBoard()
    try await onBoard(board) {
        let left = try store.startPart(.breast(.left), for: baby)
        LiveActivityController.start(for: left, in: store)
        clock.advance(by: 300)
        try await LiveActivityController.stopping(left, in: store) { _ = try store.stopFeed(left) }
    }
    let stoppedAt = clock.now
    store.breakLimits = { BreakLimits(feed: 3_600, sleep: 900) }
    clock.advance(by: 60)

    await onBoard(board) { await LiveActivityController.endExpiredBreaks(in: store) }

    let card = try #require(board.cards().first)
    #expect(card.state.breakEndsAt == stoppedAt.addingTimeInterval(3_600))
    #expect(card.staleDate == stoppedAt.addingTimeInterval(3_600))
}

/// Activation after the break limit passed takes the card down.
@MainActor @Test func activationEndsABreakPastItsLimit() async throws {
    let (store, baby, clock) = try fixture()
    let board = MemoryCardBoard()
    try await onBoard(board) {
        let left = try store.startPart(.breast(.left), for: baby)
        LiveActivityController.start(for: left, in: store)
        clock.advance(by: 300)
        try await LiveActivityController.stopping(left, in: store) { _ = try store.stopFeed(left) }
    }
    clock.advance(by: 1_801)

    await onBoard(board) { await LiveActivityController.endExpiredBreaks(in: store) }

    #expect(board.cards().isEmpty)
}

/// After a stop, a part that already runs is shown, never ended.
@Test func afterAStopTheRunningPartWins() {
    #expect(LiveActivityController.afterStop(hasRunningTimer: true, isBreakOpen: false) == .adoptRunning)
    #expect(LiveActivityController.afterStop(hasRunningTimer: true, isBreakOpen: true) == .adoptRunning)
    #expect(LiveActivityController.afterStop(hasRunningTimer: false, isBreakOpen: true) == .showBreak)
    #expect(LiveActivityController.afterStop(hasRunningTimer: false, isBreakOpen: false) == .end)
}

// MARK: - A break card only while nothing runs

/// The feed's break card goes the moment a sleep starts; the sleep gets its
/// own running card.
@MainActor @Test func startingASleepEndsTheFeedBreakCard() async throws {
    let (store, baby, clock) = try fixture()
    let board = MemoryCardBoard()
    try await onBoard(board) {
        let left = try store.startPart(.breast(.left), for: baby)
        LiveActivityController.start(for: left, in: store)
        clock.advance(by: 300)
        try await LiveActivityController.stopping(left, in: store) { _ = try store.stopFeed(left) }
        #expect(board.cards().first?.state.phase == .feedBreak)

        let sleep = try store.startSleep(for: baby)
        await LiveActivityController.adoptOrStart(for: sleep, in: store)
    }
    #expect(board.cards().map(\.kindRaw) == [TimerKind.sleep.rawValue])
    #expect(board.cards().first?.state.phase == .running)
}

/// Wake up takes the sleep's card down rather than leaving a time-awake card.
@MainActor @Test func wakingEndsTheSleepCard() async throws {
    let (store, baby, clock) = try fixture()
    let board = MemoryCardBoard()
    try await onBoard(board) {
        let sleep = try store.startSleep(for: baby)
        LiveActivityController.start(for: sleep, in: store)
        clock.advance(by: 1_200)
        try await LiveActivityController.stopping(sleep, in: store) { _ = try store.stopSleep(sleep) }
    }
    #expect(board.cards().isEmpty)
}

/// A time-awake card an earlier build left up is taken down on activation.
@MainActor @Test func activationEndsAnEarlierBuildsAwakeCard() async throws {
    let (store, baby, clock) = try fixture()
    let sleep = try store.startSleep(for: baby)
    clock.advance(by: 1_200)
    _ = try store.stopSleep(sleep)
    let awake = TimerActivityContentState(startedAt: clock.now, sideLabel: "Awake", activityKind: .sleep,
                                          phase: .sleepBreak, breakEndsAt: clock.now.addingTimeInterval(900))
    let board = MemoryCardBoard(cards: [LiveCard(id: "awake", babyID: baby, kindRaw: TimerKind.sleep.rawValue, state: awake)])
    await onBoard(board) {
        await LiveActivityController.endExpiredBreaks(in: store)
    }
    #expect(board.cards().isEmpty)
}

/// A feed part stopped while the baby sleeps leaves no break card, only the
/// sleep's.
@MainActor @Test func stoppingAFeedWhileAsleepLeavesNoBreakCard() async throws {
    let (store, baby, clock) = try fixture()
    let board = MemoryCardBoard()
    try await onBoard(board) {
        let left = try store.startPart(.breast(.left), for: baby)
        await LiveActivityController.adoptOrStart(for: left, in: store)
        let sleep = try store.startSleep(for: baby)
        await LiveActivityController.adoptOrStart(for: sleep, in: store)
        clock.advance(by: 300)
        try await LiveActivityController.stopping(left, in: store) { _ = try store.stopFeed(left) }
    }
    #expect(board.cards().map(\.kindRaw) == [TimerKind.sleep.rawValue])
}

/// A sleep started on another phone, or before this build: activation takes
/// down the feed break card it left beside the sleep.
@MainActor @Test func activationEndsABreakCardWhileAnotherTimerRuns() async throws {
    let (store, baby, clock) = try fixture()
    let board = MemoryCardBoard()
    try await onBoard(board) {
        let left = try store.startPart(.breast(.left), for: baby)
        LiveActivityController.start(for: left, in: store)
        clock.advance(by: 300)
        try await LiveActivityController.stopping(left, in: store) { _ = try store.stopFeed(left) }
    }
    _ = try store.startSleep(for: baby)

    await onBoard(board) { await LiveActivityController.endExpiredBreaks(in: store) }

    #expect(board.cards().isEmpty)
}

/// A sleep that continues the same sleep counts the whole sleep's time
/// asleep on its card, as Home's hero does; separated, it counts its own.
@MainActor @Test func aJoinedSleepsCardCountsTheWholeSleep() async throws {
    let (store, baby, clock) = try fixture()
    let board = MemoryCardBoard()
    _ = try store.stopSleep(try store.startSleep(for: baby).advancing(clock, by: 1_200))
    clock.advance(by: 300)
    let sleep = try store.startSleep(for: baby)
    await onBoard(board) { LiveActivityController.start(for: sleep, in: store) }
    #expect(board.cards().first?.state.startedAt == sleep.startedAt.addingTimeInterval(-1_200))

    try store.detachFromSession(sleep)
    await onBoard(board) { await LiveActivityController.update(for: sleep, in: store, own: true) }
    #expect(board.cards().first?.state.startedAt == sleep.startedAt)
}
