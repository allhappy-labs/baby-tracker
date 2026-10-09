import Foundation
import Testing
import BabyCore
import BabyProtocol
@testable import BabyStore

@Test func liveActivityPresentationTracksTheCurrentFeedComponent() {
    #expect(FeedActivityKind.breastLeft.label == "Feeding · Left breast")
    #expect(FeedActivityKind.breastRight.label == "Feeding · Right breast")
    #expect(FeedActivityKind.breastMilkBottle.label == "Feeding · Breast milk bottle")
    #expect(FeedActivityKind.formulaBottle.label == "Feeding · Formula bottle")
    #expect(FeedActivityKind.mixedBottle.label == "Feeding · Mixed bottle")
    #expect(FeedActivityKind.sleep.label == "Asleep")

    // Breast, bottle and sleep each look different; the two sides do too.
    // What is in a bottle is the label's job — see
    // `theActivityGlyphSaysBreastBottleOrSleep`.
    let glyphs = Set([
        FeedActivityKind.breastLeft.icon,
        FeedActivityKind.breastRight.icon,
        FeedActivityKind.formulaBottle.icon,
        FeedActivityKind.sleep.icon,
    ])
    #expect(glyphs.count == 4)

    #expect(FeedActivityKind.resolve(kind: .feed, currentSide: .left, completedComponents: []) == .breastLeft)
    #expect(FeedActivityKind.resolve(kind: .feed, currentSide: .right, completedComponents: []) == .breastRight)
    #expect(FeedActivityKind.resolve(kind: .feed, currentSide: nil, completedComponents: []) == .paused)

    let componentID = UUID()
    #expect(FeedActivityKind.resolve(
        kind: .feed,
        currentSide: nil,
        completedComponents: [.bottle(id: componentID, volumeML: 50, contents: .breastMilk)]
    ) == .breastMilkBottle)
    #expect(FeedActivityKind.resolve(
        kind: .feed,
        currentSide: nil,
        completedComponents: [.bottle(id: componentID, volumeML: 30, contents: .formula)]
    ) == .formulaBottle)
    #expect(FeedActivityKind.resolve(
        kind: .feed,
        currentSide: nil,
        completedComponents: [.bottle(id: componentID, volumeML: 80, contents: .mixed)]
    ) == .mixedBottle)
    #expect(FeedActivityKind.resolve(kind: .sleep, currentSide: nil, completedComponents: []) == .sleep)
}

@MainActor
@Test func timedBottleLiveActivityStartsAtBottleStart() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let timer = try store.startPart(.breast(.left), for: baby.id)

    clock.advance(by: 180)
    // A bottle beginning after a breast part on the same still-running timer
    // can no longer be built live — built directly.
    timer.completedComponents = [.breast(id: UUID(), side: .left, seconds: 180, startedAt: timer.startedAt, endedAt: clock.now)]
    timer.currentSide = nil
    timer.currentBottleStartedAt = clock.now

    let presentation = ActivityPresentation(timer: timer)
    #expect(presentation.activityKind == .timedBottle)
    #expect(presentation.startedAt == timer.currentBottleStartedAt)
    #expect(presentation.startedAt != timer.startedAt)
}

/// What the Live Activity is pushed after each thing a parent does mid-feed.
///
/// ActivityKit cannot run in the macOS test process, so the push itself is not
/// observable here. What IS observable is the state every push is built from,
/// and a push is only ever as current as that state: if a switch or a bottle
/// left it unchanged, the Lock Screen would keep naming a side that stopped.
@MainActor
@Test func eachMidFeedActionChangesWhatTheLiveActivityIsPushed() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let timer = try store.startPart(.breast(.left), for: baby.id)

    let started = ActivityPresentation(timer: timer)
    #expect(started.activityKind == .breastLeft)

    // A side switch, then a bottle, on the SAME still-running timer can no
    // longer be built live — one-part timers give each part its own timer.
    // Built directly here, the way a legacy remote timer would leave it.
    clock.advance(by: 300)
    timer.completedComponents = [.breast(id: UUID(), side: .left, seconds: 300, startedAt: timer.startedAt, endedAt: clock.now)]
    let rightStart = clock.now
    timer.currentSide = .right
    timer.currentSideStartedAt = rightStart
    try store.context.save()
    let switched = ActivityPresentation(timer: timer)
    #expect(switched.activityKind == .breastRight)
    #expect(switched != started)

    clock.advance(by: 120)
    timer.completedComponents.append(.breast(id: UUID(), side: .right, seconds: 120, startedAt: rightStart, endedAt: clock.now))
    timer.currentSide = nil
    timer.completedComponents.append(.bottle(id: UUID(), volumeML: 20, contents: .breastMilk))
    try store.context.save()
    let firstBottle = ActivityPresentation(timer: timer)
    #expect(firstBottle.activityKind == .breastMilkBottle)

    // The second bottle in a row is its own state, not a repeat of the first.
    timer.completedComponents.append(.bottle(id: UUID(), volumeML: 120, contents: .formula))
    try store.context.save()
    let secondBottle = ActivityPresentation(timer: timer)
    #expect(secondBottle.activityKind == .formulaBottle)
    #expect(secondBottle != firstBottle)

    // The elapsed time is the SESSION's, rendered by the system from this one
    // instant, so it never resets as components change.
    #expect(secondBottle.startedAt == started.startedAt)
}

/// The editing phone pushes its Live Activity after a correction; what it
/// pushes is built from the timer, so the timer must carry the new start.
@MainActor
@Test func anEditedStartIsWhatTheLiveActivityIsPushed() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let timer = try store.startPart(.breast(.left), for: baby.id)
    clock.advance(by: 600)
    let before = ActivityPresentation(timer: timer)

    try store.editRunningTimer(id: timer.id, startedAt: timer.startedAt.addingTimeInterval(-300), components: [])

    #expect(ActivityPresentation(timer: timer).startedAt == before.startedAt.addingTimeInterval(-300))
}

/// A corrected bottle is what the Lock Screen names: after a bottle the card
/// says what was in it, so changing the contents must change the card.
@MainActor
@Test func anEditedBottleIsWhatTheLiveActivityIsPushed() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let timer = try store.startPart(.breast(.left), for: baby.id)
    clock.advance(by: 600)
    // A bottle attached to a running breast timer can no longer be built
    // live — built directly, as a legacy remote timer would leave it.
    timer.completedComponents = [
        .breast(id: UUID(), side: .left, seconds: 600, startedAt: timer.startedAt, endedAt: clock.now),
        .bottle(id: UUID(), volumeML: 50, contents: .breastMilk),
    ]
    timer.currentSide = nil
    let bottle = try #require(timer.completedComponents.last)
    #expect(ActivityPresentation(timer: timer).activityKind == .breastMilkBottle)

    try store.editRunningTimer(id: timer.id, startedAt: nil, components: [
        .bottle(id: bottle.id, volumeML: 50, contents: .formula),
    ])

    let edited = ActivityPresentation(timer: timer)
    #expect(edited.activityKind == .formulaBottle)
    #expect(edited.sideLabel == "Feeding · Formula bottle")
}

/// The compact Dynamic Island has room for one glyph, so the glyph alone has
/// to say breast, bottle or sleep. A drop was the breast's glyph and the
/// breast-milk bottle's too, and every feed's in the app — it said "feed".
@Test func theActivityGlyphSaysBreastBottleOrSleep() {
    #expect(FeedActivityKind.breastLeft.icon == .breastLeft)
    #expect(FeedActivityKind.breastRight.icon == .breastRight)
    for bottle in [FeedActivityKind.breastMilkBottle, .formulaBottle, .mixedBottle] {
        #expect(bottle.icon == .bottle)
    }
    #expect(FeedActivityKind.sleep.icon == .sleep)
}

/// A feed break has a glyph of its own — the compact island shows nothing
/// else, and the feed drop there read as a feed still running. `.paused`, an
/// earlier build's break, wears the same one.
@Test func aBreakWearsThePauseGlyph() {
    #expect(FeedActivityKind.paused.icon == .pause)
}

// MARK: - Break phases (Task 16)

@MainActor @Test func stoppingLeftPutsTheActivityIntoAFeedBreakSuggestingRight() throws {
    let (store, baby, clock) = try fixture()
    store.breakLimits = { .standard }
    let left = try store.startPart(.breast(.left), for: baby)
    clock.advance(by: 300)
    _ = try store.stopFeed(left)
    let snapshot = try store.homeSnapshot(for: baby)
    let presentation = ActivityPresentation.feedBreak(session: try #require(snapshot.feedBreak),
                                                      limit: 1_800, suggested: .right)
    #expect(presentation.phase == .feedBreak)
    #expect(presentation.breakEndsAt == clock.now.addingTimeInterval(1_800))
    #expect(presentation.suggestedSide == "right")
    #expect(presentation.isActionable(now: clock.now.addingTimeInterval(1_799)))
    #expect(!presentation.isActionable(now: clock.now.addingTimeInterval(1_801)))
}

/// What `stopping` pushes is `breakPresentation`: the break card counts from
/// the part's end, lists the parts so far, and outlines the side the dock
/// would suggest.
@MainActor @Test func aStoppedFeedPartLeavesABreakCardWithItsParts() throws {
    let (store, baby, clock) = try fixture()
    let left = try store.startPart(.breast(.left), for: baby)
    clock.advance(by: 542)
    _ = try store.stopFeed(left)
    clock.advance(by: 60)
    let right = try store.startPart(.breast(.right), for: baby)
    clock.advance(by: 120)
    _ = try store.stopFeed(right)

    let card = try #require(try LiveActivityController.breakPresentation(kind: .feed, babyID: baby, in: store))
    #expect(card.phase == .feedBreak)
    #expect(card.startedAt == clock.now)
    #expect(card.breakEndsAt == clock.now.addingTimeInterval(1_800))
    #expect(card.parts == [PartLabel(side: "left", seconds: 542), PartLabel(side: "right", seconds: 120)])
    #expect(card.suggestedSide == "left")
    #expect(card.activityKind == .breastRight)
}

/// A tap under 30 s is discarded at Stop. With no earlier session in range
/// there is no break to show, so the card goes rather than offering L and R
/// over nothing.
@MainActor @Test func aDiscardedTapWithNothingBeforeItLeavesNoBreakCard() throws {
    let (store, baby, clock) = try fixture()
    let tap = try store.startPart(.breast(.left), for: baby)
    clock.advance(by: 10)
    _ = try store.stopFeed(tap)

    #expect(try LiveActivityController.breakPresentation(kind: .feed, babyID: baby, in: store) == nil)
}

/// While a part runs the card is running, never a break — even mid-session.
@MainActor @Test func aRunningPartHasNoBreakCard() throws {
    let (store, baby, clock) = try fixture()
    let left = try store.startPart(.breast(.left), for: baby)
    clock.advance(by: 300)
    _ = try store.stopFeed(left)
    _ = try store.startPart(.breast(.right), for: baby)

    #expect(try LiveActivityController.breakPresentation(kind: .feed, babyID: baby, in: store) == nil)
}

/// A sleep has no break card: there is no time-awake card after Wake up.
@MainActor @Test func wakingLeavesNoSleepBreakCard() throws {
    let (store, baby, clock) = try fixture()
    let sleep = try store.startSleep(for: baby)
    clock.advance(by: 1_200)
    _ = try store.stopSleep(sleep)

    #expect(try LiveActivityController.breakPresentation(kind: .sleep, babyID: baby, in: store) == nil)
}

/// A running card never expires: only a break has a limit.
@MainActor @Test func aRunningCardIsAlwaysActionable() throws {
    let (store, baby, clock) = try fixture()
    let timer = try store.startPart(.breast(.left), for: baby)
    let running = ActivityPresentation.running(timer: timer)
    #expect(running.phase == .running)
    #expect(running.breakEndsAt == nil)
    #expect(running.isActionable(now: clock.now.addingTimeInterval(86_400)))
}

/// What becomes of each card when the app comes back to the foreground.
@Test func cardsAreReconciledWithTheStoreOnActivation() {
    let now = Date(timeIntervalSince1970: 1_800_000_000)
    func fate(_ phase: ActivityPhase, endsIn: TimeInterval? = nil, running: Bool = false,
              owns: Bool = false, open: Bool = false) -> LiveActivityController.Reconciliation {
        LiveActivityController.reconcile(phase: phase, breakEndsAt: endsIn.map { now.addingTimeInterval($0) },
                                         hasRunningTimer: running, timerOwnsCard: owns, isBreakOpen: open, now: now)
    }
    // A break past its limit goes.
    #expect(fate(.feedBreak, endsIn: -1, open: false) == .end)
    // An earlier build's time-awake card goes: a sleep never has a break open.
    #expect(fate(.sleepBreak, endsIn: 60, open: false) == .end)
    // A break still open stays.
    #expect(fate(.feedBreak, endsIn: 60, open: true) == .keep)
    // A running card whose part stopped elsewhere shows the break that followed…
    #expect(fate(.running, open: true) == .enterBreak)
    // …or goes when none did.
    #expect(fate(.running, open: false) == .end)
    // A part started elsewhere during a break takes the card back to running.
    #expect(fate(.feedBreak, endsIn: 60, running: true) == .adopt)
    #expect(fate(.running, running: true, owns: false) == .adopt)
    #expect(fate(.running, running: true, owns: true) == .keep)
}

// MARK: - Payload compatibility

/// ActivityKit hands the new build's widget the state the previous build
/// pushed. It must decode, as a running card.
@Test func contentStateFromThePreviousBuildDecodes() throws {
    let old = #"{"startedAt":780000000,"sideLabel":"Left","activityKind":"breastLeft","canSwitchSide":true}"#
    let state = try JSONDecoder().decode(TimerActivityContentState.self, from: Data(old.utf8))
    #expect(state.phase == .running)
    #expect(state.parts.isEmpty)
    #expect(state.breakEndsAt == nil)
    #expect(state.suggestedSide == nil)
    #expect(state.activityKind == .breastLeft)
    #expect(state.startedAt == Date(timeIntervalSinceReferenceDate: 780_000_000))
}

@Test func aBreakStateRoundTrips() throws {
    let state = TimerActivityContentState(
        startedAt: Date(timeIntervalSinceReferenceDate: 780_000_000),
        sideLabel: "Break",
        activityKind: .breastRight,
        phase: .feedBreak,
        breakEndsAt: Date(timeIntervalSinceReferenceDate: 780_001_800),
        parts: [PartLabel(side: "left", seconds: 542), PartLabel(side: nil, seconds: 300)],
        suggestedSide: "left"
    )
    let decoded = try JSONDecoder().decode(TimerActivityContentState.self, from: JSONEncoder().encode(state))
    #expect(decoded == state)
    #expect(decoded.isActionable(now: Date(timeIntervalSinceReferenceDate: 780_001_800)))
    #expect(!decoded.isActionable(now: Date(timeIntervalSinceReferenceDate: 780_001_801)))
}

// MARK: - Local and pushed cards together

/// One phone in a family on the built-in push server: a shared-realm store,
/// a card board whose ordering follows ActivityKit's timestamps, and the
/// push state the coordinator publishes.
@MainActor
struct PushedCardFixture {
    static let t0 = Date(timeIntervalSince1970: 1_800_000_000)
    let store: BabyStore
    let clock: TestDateProvider
    let board: MemoryCardBoard
    let push = LiveActivityPushState()
    let fence = LiveActivityRealmFence(
        serverInstanceID: LiveActivityReconciliationTests.instance, recoveryEpoch: 2, familyID: LiveActivityReconciliationTests.family
    )
    let baby = Fixture.babyID

    /// `deviceAhead`: how far this phone's clock runs ahead of the server's.
    init(sequence: Int64 = 10, feedLimit: TimeInterval = 1_800, deviceAhead: TimeInterval = 0, eligible: Bool = true) throws {
        let clock = TestDateProvider(Self.t0)
        self.clock = clock
        store = try BabyStore.sharedFixture(dateProvider: clock)
        store.breakLimits = { BreakLimits(feed: feedLimit, sleep: 900) }
        board = MemoryCardBoard(now: { clock.now.addingTimeInterval(deviceAhead) })
        try acknowledge(through: sequence)
        if eligible { push.install(fence) }
        // What the coordinator does: record the retirement, durably.
        push.setRetireHandler { _ in true }
    }

    func run<R>(_ body: () async throws -> R) async rethrows -> R {
        try await LiveCards.$board.withValue(board) {
            try await LiveCards.$pushState.withValue(push) { try await body() }
        }
    }

    /// A sync that delivered every queued mutation and pulled through `sequence`.
    func acknowledge(through sequence: Int64) throws {
        try store.acknowledgeAllForTesting()
        try store.recordCursor(Cursor(serverInstanceID: fence.serverInstanceID, recoveryEpoch: fence.recoveryEpoch,
                                      familyID: fence.familyID, sequence: sequence))
        try store.context.save()
    }

    func scope(_ kind: TimerKind = .feed) -> LiveActivityScope { fence.scope(babyID: baby, kindRaw: kind.rawValue)! }

    func revision(_ kind: TimerKind = .feed, generation: UUID = LiveActivityReconciliationTests.generation, timer: UUID?,
                  session: UUID? = nil, sequence: Int64, version: Int64, at second: Int64, canWrite: Bool = true) -> LiveActivityRevision {
        try! LiveActivityRevision(scope: scope(kind), generationID: generation, timerID: timer, sessionID: session, familySequence: sequence,
                                  presentationVersion: version, presentationTimestamp: second, canWrite: canWrite)
    }

    /// What the server pushes: the canonical presentation plus its revision.
    func pushed(_ presentation: ActivityPresentation, _ revision: LiveActivityRevision) -> TimerActivityContentState {
        TimerActivityContentState(presentation, remote: revision)
    }

    static func second(_ offset: Int64) -> Int64 { Int64(t0.timeIntervalSince1970) + offset }
    static func date(_ second: Int64) -> Date { Date(timeIntervalSince1970: TimeInterval(second)) }
}

@MainActor
@Suite("Live Activity controller with pushed cards")
struct LiveActivityControllerTests {
    typealias F = PushedCardFixture

    /// Only an eligible realm asks ActivityKit for an update token, and the
    /// card carries the realm it belongs to; anywhere else the request is
    /// exactly today's.
    @Test func onlyAnEligibleRealmRequestsATokenCardWithItsScope() async throws {
        let f = try F()
        let timer = try f.store.startPart(.breast(.left), for: f.baby)
        await f.run { await LiveActivityController.adoptOrStart(for: timer, in: f.store) }
        let request = try #require(f.board.requests.last)
        #expect(request.isPushEnabled)
        #expect(request.scope == f.scope())
        #expect(f.board.cards().first?.scope == f.scope())
        #expect(timer.liveActivityID == f.board.cards().first?.id)

        let plain = try F(eligible: false)
        let sleep = try plain.store.startSleep(for: plain.baby)
        await plain.run { await LiveActivityController.adoptOrStart(for: sleep, in: plain.store) }
        let local = try #require(plain.board.requests.last)
        #expect(!local.isPushEnabled)
        #expect(local.scope == nil)
        #expect(plain.board.cards().first?.scope == nil)
    }

    /// This phone requested a card for its own feed, and the server started
    /// one as well. The registered server card stays; the phone's unregistered
    /// card goes, and is retired so its end never reaches the server as a
    /// dismissal of the shared lifecycle.
    @Test func aLocallyCreatedAndARemotelyStartedCardBecomeOne() async throws {
        let f = try F()
        let timer = try f.store.startPart(.breast(.left), for: f.baby)
        await f.run { await LiveActivityController.adoptOrStart(for: timer, in: f.store) }
        let local = try #require(timer.liveActivityID)
        try f.acknowledge(through: 11)

        let revision = f.revision(timer: timer.id, sequence: 11, version: 1, at: F.second(5))
        f.board.receiveStart(id: "server-card", babyID: f.baby, kindRaw: "feed", scope: f.scope(),
                             state: f.pushed(.running(timer: timer), revision), timestamp: F.date(F.second(5)))
        f.push.publish(registrations: ["server-card": .init(generationID: revision.generationID, timerID: timer.id, sessionID: nil, accepted: true)],
                       dismissedLifecycles: [])

        await f.run { await LiveActivityController.reconcileAfterSync(in: f.store) }
        #expect(f.board.cards().map(\.id) == ["server-card"])
        #expect(f.push.retired == [local])
        #expect(timer.liveActivityID == "server-card")
    }

    /// The app comes back to the foreground with a store that has not synced
    /// since another caregiver stopped the feed. The pushed break must survive
    /// the pre-sync pass and any sync that has not caught up; once the store
    /// has the stop, the card is brought in line at the card's own server
    /// timestamp.
    @Test func aStaleForegroundSnapshotNeverOverwritesANewerPush() async throws {
        let f = try F()
        let timer = try f.store.startPart(.breast(.left), for: f.baby)
        try f.acknowledge(through: 10)
        let running = f.pushed(.running(timer: timer), f.revision(timer: timer.id, sequence: 10, version: 1, at: F.second(1)))
        f.board.receiveStart(id: "card", babyID: f.baby, kindRaw: "feed", scope: f.scope(), state: running, timestamp: F.date(F.second(1)))
        try f.store.setLiveActivityID("card", on: timer)

        // The server's newer break, pushed while this store still shows the part running.
        f.clock.advance(by: 300)
        let pushedBreak = TimerActivityContentState(
            startedAt: f.clock.now, sideLabel: "", activityKind: .breastLeft, phase: .feedBreak,
            breakEndsAt: f.clock.now.addingTimeInterval(1_800), parts: [PartLabel(side: "left", seconds: 300)], suggestedSide: "right",
            remote: f.revision(timer: timer.id, sequence: 12, version: 2, at: F.second(301))
        )
        f.board.receive(id: "card", state: pushedBreak, staleDate: pushedBreak.breakEndsAt, timestamp: F.date(F.second(301)))

        await f.run { await LiveActivityController.endExpiredBreaks(in: f.store) }
        #expect(f.board.cards().first?.state == pushedBreak)
        await f.run { await LiveActivityController.reconcileAfterSync(in: f.store) }
        #expect(f.board.cards().first?.state == pushedBreak)

        // Sync lands the stop.
        _ = try f.store.stopFeed(timer)
        try f.acknowledge(through: 12)
        await f.run { await LiveActivityController.reconcileAfterSync(in: f.store) }
        let card = try #require(f.board.cards().first)
        #expect(card.state.phase == .feedBreak)
        #expect(card.state.remote?.familySequence == 12)
        #expect(f.board.timestamp(of: "card") == F.date(F.second(301)).addingTimeInterval(0.5))
    }

    /// A Stop tapped in the app, offline: the card shows the break at once
    /// — stamped half a second past its last server second, so the server's
    /// push for this stop (at least the next whole second) still replaces it.
    @Test func anOfflineStopIsShownAtOnceAndTheServersPushStillWins() async throws {
        let f = try F()
        let timer = try f.store.startPart(.breast(.left), for: f.baby)
        try f.acknowledge(through: 10)
        let running = f.pushed(.running(timer: timer), f.revision(timer: timer.id, sequence: 10, version: 1, at: F.second(1)))
        f.board.receiveStart(id: "card", babyID: f.baby, kindRaw: "feed", scope: f.scope(), state: running, timestamp: F.date(F.second(1)))
        try f.store.setLiveActivityID("card", on: timer)

        f.clock.advance(by: 300)
        try await f.run { try await LiveActivityController.stopping(timer, in: f.store) { _ = try f.store.stopFeed(timer) } }
        #expect(f.board.cards().first?.state.phase == .feedBreak)
        #expect(f.board.timestamp(of: "card") == F.date(F.second(1)).addingTimeInterval(0.5))

        // Still offline: the pass after an unrelated sync leaves it alone.
        await f.run { await LiveActivityController.reconcileAfterSync(in: f.store) }
        #expect(f.board.cards().first?.state.phase == .feedBreak)

        let serverBreak = TimerActivityContentState(
            startedAt: f.clock.now, sideLabel: "", activityKind: .breastLeft, phase: .feedBreak, breakEndsAt: f.clock.now.addingTimeInterval(1_800),
            remote: f.revision(timer: timer.id, sequence: 11, version: 2, at: F.second(2))
        )
        f.board.receive(id: "card", state: serverBreak, staleDate: nil, timestamp: F.date(F.second(2)))
        #expect(f.board.cards().first?.state == serverBreak)
    }

    /// Wake up, offline: the sleep's card goes at once, and is retired — its
    /// end is never reported to the server as the parent dismissing it.
    @Test func anOfflineWakeEndsTheCardAtOnceAsARetirement() async throws {
        let f = try F()
        let sleep = try f.store.startSleep(for: f.baby)
        try f.acknowledge(through: 10)
        let state = f.pushed(.running(timer: sleep), f.revision(.sleep, timer: sleep.id, sequence: 10, version: 1, at: F.second(1)))
        f.board.receiveStart(id: "card", babyID: f.baby, kindRaw: "sleep", scope: f.scope(.sleep), state: state, timestamp: F.date(F.second(1)))
        try f.store.setLiveActivityID("card", on: sleep)

        f.clock.advance(by: 1_200)
        try await f.run { try await LiveActivityController.stopping(sleep, in: f.store) { _ = try f.store.stopSleep(sleep) } }
        #expect(f.board.cards().isEmpty)
        #expect(f.board.ended == ["card"])
        #expect(f.push.retired == ["card"])
    }

    /// A sleep started here takes the pushed feed break card down at once, as
    /// a retirement, and gets its own token card.
    @Test func startingASleepRetiresThePushedBreakAtOnce() async throws {
        let f = try F()
        let left = try f.store.startPart(.breast(.left), for: f.baby)
        f.clock.advance(by: 300)
        _ = try f.store.stopFeed(left)
        try f.acknowledge(through: 12)
        let open = try #require(try LiveActivityController.breakPresentation(kind: .feed, babyID: f.baby, in: f.store))
        f.board.receiveStart(id: "break", babyID: f.baby, kindRaw: "feed", scope: f.scope(),
                             state: f.pushed(open, f.revision(timer: left.id, sequence: 12, version: 2, at: F.second(301))),
                             timestamp: F.date(F.second(301)))

        let sleep = try f.store.startSleep(for: f.baby)
        await f.run { await LiveActivityController.adoptOrStart(for: sleep, in: f.store) }
        #expect(f.board.cards().map(\.kindRaw) == ["sleep"])
        #expect(f.push.retired == ["break"])
        #expect(f.board.requests.last?.isPushEnabled == true)
    }

    /// A cold launch before the push coordinator has answered: whether the
    /// server pushes here is not known yet. The store has not synced since a
    /// partner stopped the feed and started a sleep; the pre-sync pass must
    /// neither replace the pushed break with its stale running feed nor end
    /// the pushed sleep it knows nothing of.
    @Test func pushedCardsAreLeftAloneBeforePushIsKnown() async throws {
        let f = try F(eligible: false)
        let timer = try f.store.startPart(.breast(.left), for: f.baby)
        try f.acknowledge(through: 10)
        try f.store.setLiveActivityID("feed", on: timer)
        let pushedBreak = TimerActivityContentState(
            startedAt: F.t0.addingTimeInterval(300), sideLabel: "", activityKind: .breastLeft, phase: .feedBreak,
            breakEndsAt: F.t0.addingTimeInterval(2_100), parts: [PartLabel(side: "left", seconds: 300)], suggestedSide: "right",
            remote: f.revision(timer: timer.id, sequence: 12, version: 2, at: F.second(301))
        )
        let pushedSleep = TimerActivityContentState(
            startedAt: F.t0.addingTimeInterval(310), sideLabel: "", activityKind: .sleep,
            remote: f.revision(.sleep, generation: LiveActivityReconciliationTests.otherGeneration, timer: UUID(), sequence: 13, version: 1,
                               at: F.second(311))
        )
        f.board.receiveStart(id: "feed", babyID: f.baby, kindRaw: "feed", scope: f.scope(), state: pushedBreak, timestamp: F.date(F.second(301)))
        f.board.receiveStart(id: "sleep", babyID: f.baby, kindRaw: "sleep", scope: f.scope(.sleep), state: pushedSleep,
                             timestamp: F.date(F.second(311)))
        f.clock.advance(by: 400)

        await f.run { await LiveActivityController.reconcileBeforeSync(in: f.store) }
        #expect(f.board.cards().first { $0.id == "feed" }?.state == pushedBreak)
        #expect(f.board.cards().first { $0.id == "sleep" }?.state == pushedSleep)
        #expect(f.board.ended.isEmpty)
        #expect(f.board.requests.isEmpty)
    }

    /// The phone's clock runs ten minutes ahead. Its feed's token card has
    /// had no push yet; an edit made here must not be stamped with the
    /// phone's clock, or the partner's stop five minutes later — stamped with
    /// the server's, which adopted this card through the timer it was
    /// registered with — would be dropped and the card would keep counting.
    @Test func aTokenCardsPhoneWritesNeverOutrankThePartnersPush() async throws {
        let f = try F(deviceAhead: 600)
        let timer = try f.store.startPart(.breast(.left), for: f.baby)
        await f.run { await LiveActivityController.adoptOrStart(for: timer, in: f.store) }
        let card = try #require(timer.liveActivityID)
        #expect(f.board.cards().first?.state.remote == nil)

        timer.startedAt = timer.startedAt.addingTimeInterval(-60)
        await f.run { await LiveActivityController.update(for: timer, in: f.store, own: true) }
        try f.acknowledge(through: 10)
        await f.run { await LiveActivityController.reconcileAfterSync(in: f.store) }
        #expect(f.board.cards().first?.state.startedAt == timer.startedAt)

        let partnerStop = TimerActivityContentState(
            startedAt: F.t0.addingTimeInterval(300), sideLabel: "", activityKind: .breastLeft, phase: .feedBreak,
            breakEndsAt: F.t0.addingTimeInterval(2_100), remote: f.revision(timer: timer.id, sequence: 12, version: 1, at: F.second(300))
        )
        f.board.receive(id: card, state: partnerStop, staleDate: nil, timestamp: F.date(F.second(300)))
        #expect(f.board.cards().first?.state == partnerStop)
    }

    /// The phone's clock runs an hour ahead of the server's. A local apply
    /// stamped with the phone's clock would make ActivityKit drop every push
    /// for the next hour; stamped with the card's server second, the next
    /// push — one second later — still wins.
    @Test func aLocalApplyNeverOutranksTheNextPushDespiteClockSkew() async throws {
        let f = try F(deviceAhead: 3_600)
        let timer = try f.store.startPart(.breast(.left), for: f.baby)
        try f.acknowledge(through: 10)
        let stale = TimerActivityContentState(startedAt: F.t0.addingTimeInterval(-60), sideLabel: "", activityKind: .breastLeft,
                                              remote: f.revision(timer: timer.id, sequence: 9, version: 1, at: F.second(0)))
        f.board.receiveStart(id: "card", babyID: f.baby, kindRaw: "feed", scope: f.scope(), state: stale, timestamp: F.date(F.second(0)))
        try f.store.setLiveActivityID("card", on: timer)

        await f.run { await LiveActivityController.reconcileAfterSync(in: f.store) }
        #expect(f.board.cards().first?.state.startedAt == timer.startedAt)
        #expect(f.board.timestamp(of: "card") == F.date(F.second(0)).addingTimeInterval(0.5))

        let next = TimerActivityContentState(startedAt: timer.startedAt, sideLabel: "", activityKind: .paused,
                                             remote: f.revision(timer: timer.id, sequence: 11, version: 2, at: F.second(1)))
        f.board.receive(id: "card", state: next, staleDate: nil, timestamp: F.date(F.second(1)))
        #expect(f.board.cards().first?.state == next)
    }

    /// A feed and a sleep may run at once for one baby; reconciling one never
    /// takes down the other.
    @Test func simultaneousFeedAndSleepCardsAreReconciledSeparately() async throws {
        let f = try F()
        let feed = try f.store.startPart(.breast(.left), for: f.baby)
        let sleep = try f.store.startSleep(for: f.baby)
        try f.acknowledge(through: 10)
        let feedState = f.pushed(.running(timer: feed), f.revision(timer: feed.id, sequence: 10, version: 1, at: F.second(1)))
        let sleepState = f.pushed(.running(timer: sleep), f.revision(.sleep, generation: LiveActivityReconciliationTests.otherGeneration,
                                                                    timer: sleep.id, sequence: 10, version: 1, at: F.second(1)))
        f.board.receiveStart(id: "feed-card", babyID: f.baby, kindRaw: "feed", scope: f.scope(), state: feedState, timestamp: F.date(F.second(1)))
        f.board.receiveStart(id: "sleep-card", babyID: f.baby, kindRaw: "sleep", scope: f.scope(.sleep), state: sleepState,
                             timestamp: F.date(F.second(1)))

        await f.run {
            await LiveActivityController.endExpiredBreaks(in: f.store)
            await LiveActivityController.reconcileAfterSync(in: f.store)
        }
        #expect(Set(f.board.cards().map(\.id)) == ["feed-card", "sleep-card"])
        #expect(feed.liveActivityID == "feed-card")
        #expect(sleep.liveActivityID == "sleep-card")
        #expect(f.board.cards().first { $0.id == "sleep-card" }?.state.activityKind == .sleep)
    }

    /// The server withdrew push: nothing will update the pushed card any
    /// more, so the app takes it back — immediately, and stamped after the
    /// card's last server second so ActivityKit accepts it.
    @Test func aWithdrawnCapabilityHandsCardsBackToLocalControl() async throws {
        let f = try F()
        let timer = try f.store.startPart(.breast(.left), for: f.baby)
        try f.acknowledge(through: 10)
        let running = f.pushed(.running(timer: timer), f.revision(timer: timer.id, sequence: 10, version: 1, at: F.second(120)))
        f.board.receiveStart(id: "card", babyID: f.baby, kindRaw: "feed", scope: f.scope(), state: running, timestamp: F.date(F.second(120)))
        try f.store.setLiveActivityID("card", on: timer)
        f.push.withdraw()

        f.clock.advance(by: 60)
        try await f.run { try await LiveActivityController.stopping(timer, in: f.store) { _ = try f.store.stopFeed(timer) } }
        #expect(f.board.cards().first?.state.phase == .feedBreak)
        #expect(f.board.cards().first?.id == "card")
    }

    /// An upgrade finds a card from the build before push: it has no realm
    /// and no update token. Rather than wait for the server to start a second
    /// card (with an alert), the app replaces it quietly with a token card
    /// for the same feed.
    @Test func aLegacyCardIsReplacedByATokenCardWithoutWaitingForAServerStart() async throws {
        let f = try F()
        let timer = try f.store.startPart(.breast(.left), for: f.baby)
        try f.acknowledge(through: 10)
        let legacy = LiveCard(id: "legacy", babyID: f.baby, kindRaw: "feed", state: TimerActivityContentState(.running(timer: timer)))
        f.board.add(legacy)
        try f.store.setLiveActivityID("legacy", on: timer)

        await f.run { await LiveActivityController.endExpiredBreaks(in: f.store) }
        let card = try #require(f.board.cards().first)
        #expect(f.board.cards().count == 1)
        #expect(card.id != "legacy")
        #expect(card.scope == f.scope())
        #expect(f.board.requests.last?.isPushEnabled == true)
        #expect(timer.liveActivityID == card.id)
        #expect(f.board.ended == ["legacy"])
    }

    /// A usable pushed card already shows the feed: the legacy card simply
    /// goes, and nothing new is requested.
    @Test func aLegacyCardBesideAPushedCardSimplyEnds() async throws {
        let f = try F()
        let timer = try f.store.startPart(.breast(.left), for: f.baby)
        try f.acknowledge(through: 10)
        f.board.add(LiveCard(id: "legacy", babyID: f.baby, kindRaw: "feed", state: TimerActivityContentState(.running(timer: timer))))
        try f.store.setLiveActivityID("legacy", on: timer)
        let revision = f.revision(timer: timer.id, sequence: 10, version: 1, at: F.second(1))
        f.board.receiveStart(id: "pushed", babyID: f.baby, kindRaw: "feed", scope: f.scope(), state: f.pushed(.running(timer: timer), revision),
                             timestamp: F.date(F.second(1)))
        f.push.publish(registrations: ["pushed": .init(generationID: revision.generationID, timerID: timer.id, sessionID: nil, accepted: true)],
                       dismissedLifecycles: [])

        await f.run { await LiveActivityController.reconcileAfterSync(in: f.store) }
        #expect(f.board.cards().map(\.id) == ["pushed"])
        #expect(f.board.requests.isEmpty)
        #expect(timer.liveActivityID == "pushed")
    }

    /// Two phones in one family keep different break limits. Each receives
    /// the break its own limit gives, and reconciling after sync keeps it to
    /// that limit: the five-minute phone's card goes at ten minutes, the
    /// two-hour phone's stays.
    @Test func eachPhoneKeepsTheBreakItsOwnLimitGives() async throws {
        var survivors: [TimeInterval: Bool] = [:]
        for limit in [TimeInterval(300), 7_200] {
            let f = try F(feedLimit: limit)
            let timer = try f.store.startPart(.breast(.left), for: f.baby)
            f.clock.advance(by: 120)
            _ = try f.store.stopFeed(timer)
            try f.acknowledge(through: 12)
            let open = try #require(try LiveActivityController.breakPresentation(kind: .feed, babyID: f.baby, in: f.store))
            #expect(open.breakEndsAt == f.clock.now.addingTimeInterval(limit))
            let state = f.pushed(open, f.revision(timer: timer.id, sequence: 12, version: 2, at: F.second(121)))
            f.board.receiveStart(id: "card", babyID: f.baby, kindRaw: "feed", scope: f.scope(), state: state, timestamp: F.date(F.second(121)))

            f.clock.advance(by: 600)
            await f.run { await LiveActivityController.reconcileAfterSync(in: f.store) }
            survivors[limit] = f.board.cards().first?.state.breakEndsAt == open.breakEndsAt
        }
        #expect(survivors == [300: false, 7_200: true])
    }

    /// Registration receipts arrive in either order; the card kept is the
    /// same one either way — the one the server treats as primary.
    @Test func reorderedRegistrationReceiptsKeepTheSameCard() async throws {
        for order in [["card-a", "card-b"], ["card-b", "card-a"]] {
            let f = try F()
            let timer = try f.store.startPart(.breast(.left), for: f.baby)
            try f.acknowledge(through: 10)
            let revision = f.revision(timer: timer.id, sequence: 10, version: 1, at: F.second(1))
            var registrations: [String: LiveActivityPushState.Registration] = [:]
            for id in order {
                f.board.receiveStart(id: id, babyID: f.baby, kindRaw: "feed", scope: f.scope(), state: f.pushed(.running(timer: timer), revision),
                                     timestamp: F.date(F.second(1)))
                registrations[id] = .init(generationID: revision.generationID, timerID: timer.id, sessionID: nil, accepted: true)
                f.push.publish(registrations: registrations, dismissedLifecycles: [])
            }
            await f.run { await LiveActivityController.reconcileAfterSync(in: f.store) }
            #expect(f.board.cards().map(\.id) == ["card-a"])
            #expect(f.push.retired == ["card-b"])
            #expect(timer.liveActivityID == "card-a")
        }
    }

    /// A card from another family, server instance or recovery epoch says
    /// nothing true here, in an eligible realm or not.
    @Test func cardsFromAnotherRealmAreEnded() async throws {
        let f = try F()
        let timer = try f.store.startPart(.breast(.left), for: f.baby)
        try f.acknowledge(through: 10)
        let state = TimerActivityContentState(.running(timer: timer))
        let otherFamily = try LiveActivityScope(serverInstanceID: f.fence.serverInstanceID, recoveryEpoch: 2, familyID: UUID(),
                                                babyID: f.baby, kindRaw: "feed")
        let otherEpoch = try LiveActivityScope(serverInstanceID: f.fence.serverInstanceID, recoveryEpoch: 1, familyID: f.fence.familyID,
                                               babyID: f.baby, kindRaw: "feed")
        f.board.add(LiveCard(id: "other-family", babyID: f.baby, kindRaw: "feed", state: state, scope: otherFamily))
        f.board.add(LiveCard(id: "other-epoch", babyID: f.baby, kindRaw: "feed", state: state, scope: otherEpoch))
        f.board.add(LiveCard(id: "current", babyID: f.baby, kindRaw: "feed", state: state, scope: f.scope()))
        try f.store.setLiveActivityID("current", on: timer)

        await f.run { await LiveActivityController.endExpiredBreaks(in: f.store) }
        #expect(f.board.cards().map(\.id) == ["current"])
        #expect(Set(f.board.ended) == ["other-family", "other-epoch"])

        // Signed out of the family: the store is local, and a card scoped to
        // any family goes.
        let local = try F(eligible: false)
        try local.store.pointAtRealmForTesting(.local)
        local.board.add(LiveCard(id: "family-card", babyID: local.baby, kindRaw: "sleep", state: state, scope: local.scope(.sleep)))
        await local.run { await LiveActivityController.endExpiredBreaks(in: local.store) }
        #expect(local.board.cards().isEmpty)
    }

    /// The parent swiped the card for this sleep away. Reconciliation must
    /// not bring the same lifecycle back; a new sleep still gets its card.
    @Test func aDismissedLifecycleIsNotRecreatedByReconciliation() async throws {
        let f = try F()
        let sleep = try f.store.startSleep(for: f.baby)
        try f.acknowledge(through: 10)
        f.push.publish(registrations: [:], dismissedLifecycles: [sleep.id])

        await f.run {
            LiveActivityController.start(for: sleep, in: f.store)
            await LiveActivityController.reconcileAfterSync(in: f.store)
            await LiveActivityController.endExpiredBreaks(in: f.store)
        }
        #expect(f.board.requests.isEmpty)
        #expect(f.board.cards().isEmpty)

        f.clock.advance(by: 600)
        _ = try f.store.stopSleep(sleep)
        let next = try f.store.startSleep(for: f.baby)
        await f.run { await LiveActivityController.adoptOrStart(for: next, in: f.store) }
        #expect(f.board.requests.count == 1)
    }

    /// Another caregiver's sleep appears in a sync while the app is open. In
    /// an eligible realm its card comes from the server's push-to-start; the
    /// app adopts that card and never stacks a second one of its own.
    @Test func aRemoteTimerWaitsForTheServersCardAndAdoptsIt() async throws {
        let f = try F()
        let sleep = try f.store.startSleep(for: f.baby)
        try f.acknowledge(through: 10)

        await f.run { await LiveActivityController.adoptRemoteTimer(sleep, in: f.store) }
        #expect(f.board.requests.isEmpty)
        #expect(sleep.liveActivityID == nil)

        let revision = f.revision(.sleep, timer: sleep.id, sequence: 10, version: 1, at: F.second(1), canWrite: false)
        f.board.receiveStart(id: "pushed", babyID: f.baby, kindRaw: "sleep", scope: f.scope(.sleep),
                             state: f.pushed(.running(timer: sleep), revision), timestamp: F.date(F.second(1)))
        await f.run { await LiveActivityController.adoptRemoteTimer(sleep, in: f.store) }
        #expect(f.board.requests.isEmpty)
        #expect(sleep.liveActivityID == "pushed")
    }

    /// The Lock Screen's sleep toggle finds nothing to do: the card is behind
    /// the store, or the store behind the card. The parent changed nothing,
    /// so the store's possibly stale view must not overwrite what the server
    /// pushed after it.
    @Test func aLockScreenToggleThatChangesNothingLeavesNewerPushedContent() async throws {
        let f = try F()
        let sleep = try f.store.startSleep(for: f.baby)
        try f.acknowledge(through: 10)
        try f.store.setLiveActivityID("card", on: sleep)
        let pushed = TimerActivityContentState(
            startedAt: sleep.startedAt.addingTimeInterval(-900), sideLabel: "", activityKind: .sleep,
            remote: f.revision(.sleep, timer: sleep.id, sequence: 12, version: 2, at: F.second(40))
        )
        f.board.receiveStart(id: "card", babyID: f.baby, kindRaw: "sleep", scope: f.scope(.sleep), state: pushed,
                             timestamp: F.date(F.second(40)))

        await f.run { await LiveActivityController.showSleepAsStored(running: sleep, babyID: f.baby, in: f.store) }
        #expect(f.board.cards().first?.state == pushed)
    }

    /// While push is not known yet, a pass the parent did not cause — here
    /// another caregiver's sleep appearing — must not end this phone's token
    /// card for a feed break: unretired, the server would read that end as
    /// the parent dismissing the feed.
    @Test func aNonUserPassNeverEndsARealmTokenCardBeforePushIsKnown() async throws {
        let f = try F(eligible: false)
        let left = try f.store.startPart(.breast(.left), for: f.baby)
        f.clock.advance(by: 300)
        _ = try f.store.stopFeed(left)
        let open = try #require(try LiveActivityController.breakPresentation(kind: .feed, babyID: f.baby, in: f.store))
        f.board.add(LiveCard(id: "token", babyID: f.baby, kindRaw: "feed", state: TimerActivityContentState(open),
                             staleDate: open.breakEndsAt, scope: f.scope()))
        let sleep = try f.store.startSleep(for: f.baby)
        try f.acknowledge(through: 12)

        await f.run { await LiveActivityController.adoptRemoteTimer(sleep, in: f.store) }
        #expect(f.board.cards().contains { $0.id == "token" })
        #expect(f.board.ended.isEmpty)
    }

    /// Push-managed: another caregiver's sleep arrives in a sync and takes
    /// this phone's registered feed break card down — a pass the parent did
    /// not cause. Ended unretired, the card's end would be registered, and
    /// the server reads an ended card of a lifecycle that goes on as the
    /// parent dismissing it: that phone would silently stop receiving it. So
    /// the end is retired first — pushed to yet or not — and when the
    /// retirement cannot be recorded the card stays up.
    @Test func aNonUserPassRetiresTheRegisteredCardItEnds() async throws {
        for pushed in [false, true] {
            for recordable in [true, false] {
                let label = "pushed=\(pushed) recordable=\(recordable)"
                let f = try F()
                if !recordable { f.push.setRetireHandler { _ in false } }
                let left = try f.store.startPart(.breast(.left), for: f.baby)
                f.clock.advance(by: 300)
                _ = try f.store.stopFeed(left)
                try f.acknowledge(through: 12)
                let open = try #require(try LiveActivityController.breakPresentation(kind: .feed, babyID: f.baby, in: f.store))
                let revision = f.revision(timer: left.id, sequence: 12, version: 2, at: F.second(301))
                if pushed {
                    f.board.receiveStart(id: "break", babyID: f.baby, kindRaw: "feed", scope: f.scope(), state: f.pushed(open, revision),
                                         staleDate: open.breakEndsAt, timestamp: F.date(F.second(301)))
                } else {
                    f.board.add(LiveCard(id: "break", babyID: f.baby, kindRaw: "feed", state: TimerActivityContentState(open),
                                         staleDate: open.breakEndsAt, scope: f.scope()))
                }
                f.push.publish(registrations: ["break": .init(generationID: pushed ? revision.generationID : UUID(),
                                                              timerID: pushed ? left.id : nil, sessionID: nil, accepted: true)],
                               dismissedLifecycles: [])
                let sleep = try f.store.startSleep(for: f.baby)
                try f.acknowledge(through: 13)

                await f.run { await LiveActivityController.adoptRemoteTimer(sleep, in: f.store) }
                if recordable {
                    #expect(f.board.ended == ["break"], "\(label)")
                    #expect(f.push.retired == ["break"], "\(label)")
                } else {
                    #expect(f.board.cards().map(\.id) == ["break"], "\(label)")
                    #expect(f.board.ended.isEmpty, "\(label)")
                }
            }
        }
    }

    /// The card this phone requests for its own feed has no server revision:
    /// the lifecycle it shows is published for the push coordinator, which
    /// registers the card with it so the server adopts the card and pushes
    /// the partner's changes to it. Each write that moves the card to another
    /// lifecycle — here the break that follows Stop — publishes that one.
    @Test func aRequestedTokenCardPublishesTheLifecycleItShows() async throws {
        let f = try F()
        let left = try f.store.startPart(.breast(.left), for: f.baby)
        await f.run { await LiveActivityController.adoptOrStart(for: left, in: f.store) }
        let card = try #require(left.liveActivityID)
        #expect(f.board.cards().first?.state.remote == nil)
        #expect(f.push.references(for: card) == .init(timerID: left.id, sessionID: nil))

        f.clock.advance(by: 300)
        try await f.run { try await LiveActivityController.stopping(left, in: f.store) { _ = try f.store.stopFeed(left) } }
        let open = try #require(try LiveActivityController.lifecycle(kind: .feed, babyID: f.baby, in: f.store).sessionID)
        #expect(f.board.cards().first?.state.phase == .feedBreak)
        #expect(f.push.references(for: card) == .init(timerID: nil, sessionID: open))

        // A retired card is nobody's to register.
        #expect(f.push.retire([card]))
        f.push.setReferences(.init(timerID: left.id, sessionID: nil), for: card)
        #expect(f.push.references(for: card) == nil)
    }

    /// Push was withdrawn, or the pass runs before push is known, and the app
    /// ends a realm card it may have registered. The end is still the app's:
    /// retired first, so a coordinator that later gets push back never
    /// reports ActivityKit's `ended` as the parent dismissing that lifecycle.
    /// Not recordable, the card stays up.
    @Test func aWithdrawnRealmStillRetiresTheRealmCardsItEnds() async throws {
        for recordable in [true, false] {
            let f = try F()
            if !recordable { f.push.setRetireHandler { _ in false } }
            let sleep = try f.store.startSleep(for: f.baby)
            try f.acknowledge(through: 10)
            let revision = f.revision(.sleep, timer: sleep.id, sequence: 10, version: 1, at: F.second(1))
            f.board.receiveStart(id: "card", babyID: f.baby, kindRaw: "sleep", scope: f.scope(.sleep),
                                 state: f.pushed(.running(timer: sleep), revision), timestamp: F.date(F.second(1)))
            f.push.withdraw()
            f.clock.advance(by: 600)
            _ = try f.store.stopSleep(sleep)

            await f.run { await LiveActivityController.endExpiredBreaks(in: f.store) }
            if recordable {
                #expect(f.board.ended == ["card"])
                #expect(f.push.retired == ["card"])
            } else {
                #expect(f.board.cards().map(\.id) == ["card"])
                #expect(f.board.ended.isEmpty)
            }
        }
    }

    /// A viewer's phone: the partner's nap arrives in a sync before the
    /// server's card does, and this phone requests a token card of its own
    /// for it. No push has reached that card, so it carries no server
    /// metadata saying who may write — the family's current authority
    /// decides, and the card offers no controls.
    @Test func aViewersTokenCardOffersNoControlsBeforeTheFirstPush() async throws {
        let f = try F()
        let sleep = try f.store.startSleep(for: f.baby)
        try f.acknowledge(through: 10)
        try f.store.setWritePermission(.viewer, for: f.store.activeRealm)
        #expect(!f.store.canWriteInActiveRealm)

        await f.run { LiveActivityController.start(for: sleep, in: f.store) }

        let card = try #require(f.board.cards().first)
        #expect(card.scope == f.scope(.sleep))
        #expect(card.state.remote == nil)
        #expect(card.state.isReadOnly)
        #expect(!card.state.offersControls(now: f.clock.now))
    }

    /// The same phone, promoted back to caregiver: the next write to the
    /// token card offers its controls again.
    @Test func aCaregiversTokenCardOffersItsControls() async throws {
        let f = try F()
        let sleep = try f.store.startSleep(for: f.baby)
        try f.acknowledge(through: 10)

        await f.run { LiveActivityController.start(for: sleep, in: f.store) }

        let card = try #require(f.board.cards().first)
        #expect(card.state.remote == nil)
        #expect(!card.state.isReadOnly)
        #expect(card.state.offersControls(now: f.clock.now))
    }
}
