import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import HomeFeature

@MainActor @Suite struct HomeViewModelTests {
    @Test func beforeTheFirstRefreshHomeIsEmptyRatherThanStale() throws {
        let (store, baby, _) = try fixture()
        let model = HomeViewModel(store: store, babyID: baby)
        #expect(model.snapshot == nil)
        #expect(model.screen.hero == .awake(since: nil))
        #expect(model.screen.dock == .start(suggested: .left))
    }

    @Test func refreshPicksUpAWriteMadeWithoutTellingTheViewModel() async throws {
        let (store, baby, _) = try fixture()
        let model = HomeViewModel(store: store, babyID: baby)
        try model.refresh()
        #expect(model.screen.status.isEmpty)

        let revision = store.revision
        _ = try store.logDiaper(.wet, for: baby)
        #expect(store.revision != revision)

        try model.refresh()
        #expect(model.screen.status.map(\.tracker) == [.diaper])
    }

    @Test func refreshingTwiceWithoutChangesProducesAnEqualScreen() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.startPart(.breast(.left), for: baby)
        let model = HomeViewModel(store: store, babyID: baby)
        try model.refresh()
        let first = model.screen
        // A running part's numbers tick in the view; the screen itself does
        // not change with the clock, so a later refresh has nothing to redraw.
        clock.advance(by: 30)
        try model.refresh()
        #expect(model.screen == first)
    }

    @Test func startingABreastShowsItAsTheHeroAndStopOnTheDock() async throws {
        let (store, baby, _) = try fixture()
        let model = HomeViewModel(store: store, babyID: baby)
        try await model.startBreast(.left)
        try model.refresh()

        let timer = try #require(try store.activeTimer(kind: .feed, for: baby))
        #expect(model.screen.hero == .feedRunning(icon: .breastLeft, title: "Left breast", startedAt: timer.startedAt))
        #expect(model.screen.dock == .feedRunning(timerID: timer.id, isBottle: false))
    }

    @Test func tappingTheOtherSideWhileOneRunsStopsItAndJoinsTheSameFeed() async throws {
        let (store, baby, clock) = try fixture()
        let model = HomeViewModel(store: store, babyID: baby)
        try await model.startBreast(.left)
        clock.advance(by: 300)
        try await model.startBreast(.right)
        try model.refresh()

        let timer = try #require(try store.activeTimer(kind: .feed, for: baby))
        #expect(timer.currentSide == .right)
        #expect(model.screen.joinChip == JoinChip(kind: .feed, previousParts: [PartSummary(icon: .breastLeft, seconds: 300)]))
    }

    @Test func startingTheSideAlreadyRunningKeepsItsTimer() async throws {
        let (store, baby, clock) = try fixture()
        let model = HomeViewModel(store: store, babyID: baby)
        try await model.startBreast(.left)
        let first = try #require(try store.activeTimer(kind: .feed, for: baby)).id
        clock.advance(by: 60)
        try await model.startBreast(.left)
        #expect(try store.activeTimer(kind: .feed, for: baby)?.id == first)
    }

    @Test func stopSavesOnePartAndLeavesTheFeedInItsBreak() async throws {
        let (store, baby, clock) = try fixture()
        let model = HomeViewModel(store: store, babyID: baby)
        try await model.startBreast(.left)
        clock.advance(by: 542)
        #expect(try await model.stopFeed() == .stopped)
        try model.refresh()

        #expect(try store.activeTimers(for: baby).isEmpty)
        #expect(try store.lastFeed(for: baby)?.components.count == 1)
        guard case .feedBreak(_, let parts) = model.screen.hero else { Issue.record("expected break hero"); return }
        #expect(parts == [PartSummary(icon: .breastLeft, seconds: 542)])
        #expect(model.screen.dock == .start(suggested: .right))
    }

    @Test func stoppingARunningBottleAsksForItsAmountAndLeavesItRunning() async throws {
        let (store, baby, clock) = try fixture()
        let bottle = try store.startPart(.bottle(.formula), for: baby)
        clock.advance(by: 300)
        let model = HomeViewModel(store: store, babyID: baby)
        #expect(try await model.stopFeed() == .needsBottleAmount(timerID: bottle.id))
        #expect(try store.activeTimer(kind: .feed, for: baby)?.id == bottle.id)
    }

    @Test func sleepStartsAndWakeUpSavesIt() async throws {
        let (store, baby, clock) = try fixture()
        let model = HomeViewModel(store: store, babyID: baby)
        try await model.startSleep()
        try model.refresh()
        let timer = try #require(try store.activeTimer(kind: .sleep, for: baby))
        #expect(model.screen.hero == .sleepRunning(clockFrom: timer.startedAt, since: timer.startedAt))
        #expect(model.screen.dock == .sleepRunning(timerID: timer.id))

        clock.advance(by: 1_200)
        try await model.stopSleep()
        try model.refresh()
        #expect(try store.lastSleep(for: baby)?.durationSeconds == 1_200)
        #expect(model.screen.hero == .awake(since: clock.now))
    }

    @Test func separatingTheRunningPartRemovesTheChip() async throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300))
        let model = HomeViewModel(store: store, babyID: baby)
        try await model.startBreast(.right)
        try model.refresh()
        #expect(model.screen.joinChip != nil)

        try await model.detachRunning(.feed)
        try model.refresh()
        #expect(model.screen.joinChip == nil)
        #expect(try store.activeTimer(kind: .feed, for: baby)?.sessionID == nil)
    }

    @Test func aJoinedSleepShowsTheSameSleepChip() async throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopSleep(try store.startSleep(for: baby).advancing(clock, by: 1_200))
        clock.advance(by: 300)
        let model = HomeViewModel(store: store, babyID: baby)
        try await model.startSleep()
        try model.refresh()
        #expect(model.screen.joinChip == JoinChip(kind: .sleep, previousParts: [PartSummary(icon: .sleep, seconds: 1_200)]))
    }

    /// The same sleep, woken briefly: the hero counts the whole sleep's time
    /// asleep — the earlier part plus this one, not the awake gap — since
    /// the sleep first began.
    @Test func aJoinedSleepsHeroCountsTheWholeSleep() async throws {
        let (store, baby, clock) = try fixture()
        let firstStart = clock.now
        _ = try store.stopSleep(try store.startSleep(for: baby).advancing(clock, by: 1_200))
        clock.advance(by: 300)
        let model = HomeViewModel(store: store, babyID: baby)
        try await model.startSleep()
        try model.refresh()
        let timer = try #require(try store.activeTimer(kind: .sleep, for: baby))
        #expect(model.screen.hero == .sleepRunning(clockFrom: timer.startedAt.addingTimeInterval(-1_200), since: firstStart))

        try await model.detachRunning(.sleep)
        try model.refresh()
        #expect(model.screen.hero == .sleepRunning(clockFrom: timer.startedAt, since: timer.startedAt))
    }

    /// The sleep chip beside a running feed counts the whole sleep too.
    @Test func aJoinedSleepsChipCountsTheWholeSleep() async throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopSleep(try store.startSleep(for: baby).advancing(clock, by: 1_200))
        clock.advance(by: 300)
        let sleep = try store.startSleep(for: baby)
        clock.advance(by: 60)
        _ = try store.startPart(.breast(.left), for: baby)
        let model = HomeViewModel(store: store, babyID: baby)
        try model.refresh()
        #expect(model.screen.sleepChip == sleep.startedAt.addingTimeInterval(-1_200))
    }

    @Test func verbsAreNoOpsWhenNothingIsRunning() async throws {
        let (store, baby, _) = try fixture()
        let model = HomeViewModel(store: store, babyID: baby)
        #expect(try await model.stopFeed() == .stopped)
        try await model.stopSleep()
        try await model.detachRunning(.feed)
        #expect(try store.activeTimers(for: baby).isEmpty)
        #expect(try store.lastFeed(for: baby) == nil)
        #expect(try store.lastSleep(for: baby) == nil)
    }

    @Test func theHeroEditorOpensTheFeedWhenAFeedAndASleepBothRun() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.startSleep(for: baby)
        clock.advance(by: 60)
        let feed = try store.startPart(.breast(.left), for: baby)
        let editor = try #require(HomeViewModel(store: store, babyID: baby).makeTimerEditor())
        #expect(editor.id == feed.id)
    }

    @Test func theEditorFormatsVolumesInThePreferredUnit() throws {
        let (store, baby, _) = try fixture()
        _ = try store.startPart(.bottle(.formula), for: baby)
        let model = HomeViewModel(store: store, babyID: baby, volumeUnit: .millilitres)
        // The preference can change while Home is alive; the next editor
        // must use the new one.
        model.volumeUnit = .ounces
        let editor = try #require(model.makeTimerEditor())
        #expect(editor.unit == .ounces)
    }
}
