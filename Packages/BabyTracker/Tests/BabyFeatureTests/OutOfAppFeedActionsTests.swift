import Testing
import Foundation
import BabyCore
import BabyProtocol
import BabyStore
import BabySync
@testable import BabyFeature

@MainActor
private func store() throws -> (BabyStore, UUID, TestDateProvider) {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    store.breakLimits = { .standard }
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    return (store, baby.id, clock)
}

/// L or R from the widget or a card, while a timed bottle runs: the store's
/// safety net saves the bottle at the usual amount, and — as for a Lock
/// Screen Stop — the app is asked to offer "Set amount" for it.
@MainActor
@Test func startingASideOverARunningBottleAsksForTheBottlesAmount() async throws {
    let (store, baby, clock) = try store()
    let pending = PendingQuickLogStore(defaults: emptyQuickLogDefaults(), center: NotificationCenter())
    _ = try store.startPart(.bottle(.formula), for: baby)
    clock.advance(by: 300)

    let left = try await OutOfAppFeedActions.startBreast(.left, for: baby, in: store, pending: pending)

    #expect(left.currentSide == .left)
    let bottle = try #require(try store.lastFeed(for: baby))
    #expect(bottle.totalBottleVolumeML == LogDefaults.fallbackVolumeML)
    #expect(pending.consume() == PendingQuickLogRequest(.bottleAmount(feedID: bottle.id), babyID: baby))
}

/// Switching from one breast to the other leaves nothing to ask.
@MainActor
@Test func startingASideOverABreastPartAsksNothing() async throws {
    let (store, baby, clock) = try store()
    let pending = PendingQuickLogStore(defaults: emptyQuickLogDefaults(), center: NotificationCenter())
    _ = try store.startPart(.breast(.left), for: baby)
    clock.advance(by: 300)

    _ = try await OutOfAppFeedActions.startBreast(.right, for: baby, in: store, pending: pending)

    #expect(pending.consume() == nil)
}

/// The Lock Screen's Stop on a running bottle: saved at the usual amount,
/// with "Set amount" waiting in the app.
@MainActor
@Test func stoppingABottleOutsideTheAppAsksForItsAmount() async throws {
    let (store, baby, clock) = try store()
    let pending = PendingQuickLogStore(defaults: emptyQuickLogDefaults(), center: NotificationCenter())
    let timer = try store.startPart(.bottle(.formula), for: baby)
    clock.advance(by: 300)

    try await OutOfAppFeedActions.stopFeed(timer, in: store, pending: pending)

    #expect(try store.activeTimers(for: baby).isEmpty)
    let bottle = try #require(try store.lastFeed(for: baby))
    #expect(pending.consume() == PendingQuickLogRequest(.bottleAmount(feedID: bottle.id), babyID: baby))
}

/// Bottle on the feed break card: a timed bottle of what was given last,
/// joining the feed in its break, with nothing to ask until it stops.
@MainActor
@Test func startingABottleInABreakJoinsTheFeedWithTheLastContents() async throws {
    let (store, baby, clock) = try store()
    let pending = PendingQuickLogStore(defaults: emptyQuickLogDefaults(), center: NotificationCenter())
    _ = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: clock.now)
    clock.advance(by: 3_600)
    let left = try store.startPart(.breast(.left), for: baby)
    clock.advance(by: 300)
    _ = try store.stopFeed(left)
    clock.advance(by: 60)

    let bottle = try await OutOfAppFeedActions.startBottle(for: baby, in: store, pending: pending)

    #expect(bottle.currentBottleStartedAt == clock.now)
    #expect(bottle.bottleContents == .formula)
    #expect(bottle.sessionID != nil)
    #expect(pending.consume() == nil)
}

// MARK: - Lock Screen card actions

/// The family server as one card action sees it: every sync answers with the
/// cursor it holds, or fails as an unreachable private network does.
@MainActor
final class CardActionServer: FamilySyncTransport, @unchecked Sendable {
    var cursor: Cursor
    var offline = false
    private(set) var requests: [SyncRequest] = []

    init(cursor: Cursor) { self.cursor = cursor }

    func sync(_ request: SyncRequest) async throws -> SyncResponse {
        requests.append(request)
        if offline { throw URLError(.notConnectedToInternet) }
        return SyncResponse(serverInstanceID: cursor.serverInstanceID, recoveryEpoch: cursor.recoveryEpoch, outcomes: [],
                            changes: [], highWater: cursor, nextPageToken: nil)
    }

    func snapshot(pageToken: String?) async throws -> SnapshotResponse { throw URLError(.badServerResponse) }
}

/// A phone in a push-managed family, with one card on its Lock Screen. The
/// card action runs the real preflight: a real `SyncCoordinator` over the
/// fake server, against the real store.
@MainActor
struct CardActionFixture {
    static let instance = UUID(uuidString: "a7a7a7a7-a7a7-4a7a-8a7a-a7a7a7a7a7a7")!
    static let family = UUID(uuidString: "b7b7b7b7-b7b7-4b7b-8b7b-b7b7b7b7b7b7")!
    static let baby = UUID(uuidString: "c7c7c7c7-c7c7-4c7c-8c7c-c7c7c7c7c7c7")!
    static let second: Int64 = 1_800_000_000
    static let accountKey = AccountSessionKey(canonicalOrigin: "https://sync.example", serverInstanceID: instance,
                                              accountID: UUID(uuidString: "d7d7d7d7-d7d7-4d7d-8d7d-d7d7d7d7d7d7")!,
                                              deviceID: UUID(uuidString: "e7e7e7e7-e7e7-4e7e-8e7e-e7e7e7e7e7e7")!, recoveryEpoch: 0)
    static let realm = RealmID.shared(canonicalOrigin: "https://sync.example", serverInstanceID: instance, familyID: family)
    static let fence = LiveActivityRealmFence(serverInstanceID: instance, recoveryEpoch: 1, familyID: family)

    let clock = TestDateProvider(Date(timeIntervalSince1970: TimeInterval(second)))
    let store: BabyStore
    let server: CardActionServer
    let board = MemoryCardBoard()
    let push = LiveActivityPushState()
    let pending = PendingQuickLogStore(defaults: emptyQuickLogDefaults(), center: NotificationCenter())
    /// The publishes handed off after a write, which run once the tap has
    /// returned.
    let background = BackgroundWork()

    /// `managed`: the push coordinator has published the realm. False is a
    /// cold launch whose coordinator has not answered yet.
    init(sequence: Int64 = 10, managed: Bool = true) throws {
        store = try BabyStore.inMemory(dateProvider: clock)
        store.breakLimits = { .standard }
        store.context.insert(Baby(id: Self.baby, name: "Mira", birthDate: .now, realm: Self.realm))
        try store.context.save()
        try store.setActiveRealm(Self.realm)
        try store.markAccountAuthenticated(Self.accountKey.leaseKey, credentialVersion: UUID())
        let cursor = Cursor(serverInstanceID: Self.instance, recoveryEpoch: 1, familyID: Self.family, sequence: sequence)
        // What a sync that pulled through `sequence` leaves behind.
        try store.applySyncPage(SyncResponse(serverInstanceID: Self.instance, recoveryEpoch: 1, outcomes: [], changes: [],
                                             highWater: cursor, nextPageToken: nil))
        server = CardActionServer(cursor: cursor)
        if managed { push.install(Self.fence) }
        // What the coordinator does: the retirement is recorded durably.
        push.setRetireHandler { _ in true }
    }

    func preflight() -> LiveActivityActionPreflight {
        LiveActivityActionPreflight(
            store: store,
            catchUp: SyncCoordinatorCatchUp(store: store, transport: server, accountKey: Self.accountKey, renew: {}),
            sleep: { _ in try await Task.sleep(for: .seconds(86_400)) }
        )
    }

    func run<R>(_ body: () async throws -> R) async rethrows -> R {
        try await LiveCards.$board.withValue(board) {
            try await LiveCards.$pushState.withValue(push) { try await body() }
        }
    }

    /// The tap, then whatever it left to run after it.
    func perform(_ action: LiveActivityCardAction, _ target: LiveActivityActionTarget) async throws -> LiveActivityActionOutcome {
        let outcome = try await tap(action, target)
        await run { await background.drain() }
        return outcome
    }

    /// The tap alone: what the intent waits for.
    func tap(_ action: LiveActivityCardAction, _ target: LiveActivityActionTarget) async throws -> LiveActivityActionOutcome {
        try await run {
            try await LiveActivityCardActions.perform(action, target: target, babyID: Self.baby, in: store, gate: preflight(),
                                                      pending: pending, background: background.add)
        }
    }

    func scope(_ kind: TimerKind, family: UUID = Self.family) -> LiveActivityScope {
        try! LiveActivityScope(serverInstanceID: Self.instance, recoveryEpoch: 1, familyID: family, babyID: Self.baby, kindRaw: kind.rawValue)
    }

    /// Puts the server's card for `presentation` on the Lock Screen and
    /// returns what its buttons carry.
    @discardableResult
    func pushCard(_ kind: TimerKind, _ presentation: ActivityPresentation, timer: UUID?, session: UUID? = nil,
                  family: UUID = Self.family, sequence: Int64 = 10) throws -> LiveActivityActionTarget {
        let revision = try LiveActivityRevision(scope: scope(kind, family: family), generationID: UUID(), timerID: timer,
                                                sessionID: session, familySequence: sequence, presentationVersion: 1,
                                                presentationTimestamp: Self.second, canWrite: true)
        board.receiveStart(id: "card", babyID: Self.baby, kindRaw: kind.rawValue, scope: scope(kind, family: family),
                           state: TimerActivityContentState(presentation, remote: revision),
                           timestamp: Date(timeIntervalSince1970: TimeInterval(Self.second)))
        return try #require(LiveActivityActionTarget.card(scope: scope(kind, family: family), remote: revision))
    }

    func outboxIDs() throws -> Set<UUID> { Set(try store.liveOutboxRows().map(\.id)) }
}

@MainActor
@Suite("Lock Screen card actions", .timeLimit(.minutes(1)))
struct LiveActivityCardActionTests {
    typealias F = CardActionFixture

    /// The locked phone's Stop on a pushed timed-bottle card, with the family
    /// reachable: the bottle is saved at the usual amount, "Set amount" is
    /// asked for exactly once, the stop is published in one bounded sync, and
    /// the card shows the break at once — stamped from its own server second.
    @Test func aLockedPhoneBottleStopSavesTheUsualAmountAndPublishesIt() async throws {
        let f = try F()
        _ = try f.store.logBottle(volumeML: 90, contents: .formula, for: F.baby, at: f.clock.now.addingTimeInterval(-7_200))
        _ = try f.store.logBottle(volumeML: 90, contents: .formula, for: F.baby, at: f.clock.now.addingTimeInterval(-3_600))
        let bottle = try f.store.startPart(.bottle(.formula), for: F.baby)
        try f.store.setLiveActivityID("card", on: bottle)
        let target = try f.pushCard(.feed, .running(timer: bottle), timer: bottle.id)
        f.clock.advance(by: 300)

        let outcome = try await f.perform(.stopFeed, target)

        #expect(outcome == .performed)
        #expect(try f.store.activeTimers(for: F.baby).isEmpty)
        let saved = try #require(try f.store.lastFeed(for: F.baby))
        #expect(saved.totalBottleVolumeML == 90)
        #expect(f.pending.consume() == PendingQuickLogRequest(.bottleAmount(feedID: saved.id), babyID: F.baby))
        #expect(f.pending.consume() == nil)
        #expect(f.pending.consumeActivityHandoff() == nil)
        // No catch-up before — the store vouches for the card — and one
        // publish after: the feed the stopped bottle became went out.
        #expect(f.server.requests.count == 1)
        #expect(f.server.requests.last?.mutations.contains { $0.entityKind == .feed } == true)
        let card = try #require(f.board.cards().first)
        #expect(card.state.phase == .feedBreak)
        #expect(f.board.timestamp(of: "card") == Date(timeIntervalSince1970: TimeInterval(F.second) + 0.5))
    }

    /// The card still shows the feed that ended; the feed running now is
    /// another one. Stop must not end it.
    @Test func aStaleStopNeverEndsTheReplacementFeed() async throws {
        let f = try F()
        let old = try f.store.startPart(.breast(.left), for: F.baby)
        let target = try f.pushCard(.feed, .running(timer: old), timer: old.id)
        f.clock.advance(by: 300)
        _ = try f.store.stopFeed(old)
        f.clock.advance(by: 4_000)
        let replacement = try f.store.startPart(.breast(.right), for: F.baby)
        let outbox = try f.outboxIDs()

        let outcome = try await f.perform(.stopFeed, target)

        #expect(outcome == .alreadyFinished)
        let running = try #require(try f.store.activeTimer(kind: .feed, for: F.baby))
        #expect(running.id == replacement.id)
        #expect(running.currentSide == .right)
        #expect(try f.outboxIDs() == outbox)
        #expect(f.pending.consume() == nil)
    }

    /// Wake up on the card of a nap that already ended, with the next nap
    /// running: the old card's button never toggles — never ends the new
    /// nap, never starts another. The card is only reconciled.
    @Test func aStaleWakeNeverTogglesTheReplacementNap() async throws {
        let f = try F()
        let old = try f.store.startSleep(for: F.baby)
        let target = try f.pushCard(.sleep, .running(timer: old), timer: old.id)
        f.clock.advance(by: 1_800)
        _ = try f.store.stopSleep(old)
        f.clock.advance(by: 1_200)
        let replacement = try f.store.startSleep(for: F.baby)
        let outbox = try f.outboxIDs()

        let outcome = try await f.perform(.wake, target)

        #expect(outcome == .alreadyFinished)
        #expect(try f.store.activeTimers(for: F.baby).map(\.id) == [replacement.id])
        #expect(try f.outboxIDs() == outbox)
        #expect(f.push.retired.isEmpty)
        #expect(f.board.ended.isEmpty)
    }

    /// A cold launch with the private network unreachable, on a card the
    /// store already vouches for: the tap acts at once. The bottle is saved
    /// at the usual amount, and the mutation waits in the outbox for the app.
    @Test func aColdLaunchWithoutTheNetworkStillActsOnACurrentCard() async throws {
        let f = try F(managed: false)
        let bottle = try f.store.startPart(.bottle(.formula), for: F.baby)
        try f.store.setLiveActivityID("card", on: bottle)
        let target = try f.pushCard(.feed, .running(timer: bottle), timer: bottle.id)
        f.server.offline = true
        f.clock.advance(by: 300)
        let outbox = try f.outboxIDs()

        let outcome = try await f.perform(.stopFeed, target)

        #expect(outcome == .performed)
        #expect(try f.store.activeTimer(kind: .feed, for: F.baby) == nil)
        let saved = try #require(try f.store.lastFeed(for: F.baby))
        #expect(f.pending.consume() == PendingQuickLogRequest(.bottleAmount(feedID: saved.id), babyID: F.baby))
        #expect(try f.outboxIDs() != outbox)
        #expect(f.pending.consumeActivityHandoff() == nil)
    }

    /// The card shows a feed this phone has not pulled, and the network is
    /// unreachable: nothing can be verified, so nothing is written. The
    /// reason is parked for the app — not the action, which is never
    /// replayed — and the card is left as the server last pushed it.
    @Test func aCardAheadWithoutTheNetworkHandsOffWithoutWriting() async throws {
        let f = try F(managed: false)
        let target = try f.pushCard(.feed, .running(timer: try f.store.startPart(.bottle(.formula), for: F.baby)),
                                    timer: UUID(), sequence: 11)
        let shown = try #require(f.board.cards().first)
        f.server.offline = true
        let outbox = try f.outboxIDs()

        let outcome = try await f.perform(.stopFeed, target)

        #expect(outcome == .handedOff(.refreshFailed))
        #expect(try f.store.activeTimer(kind: .feed, for: F.baby) != nil)
        #expect(try f.outboxIDs() == outbox)
        #expect(f.pending.consumeActivityHandoff() == .refreshFailed)
        #expect(f.pending.consume() == nil)
        #expect(f.board.cards() == [shown])
    }

    /// The tap returns as soon as the nap is written: the publish waits for
    /// nothing the intent awaits, and runs after it.
    @Test func theTapNeverWaitsForThePublish() async throws {
        let f = try F()
        let sleep = try f.store.startSleep(for: F.baby)
        try f.store.setLiveActivityID("card", on: sleep)
        let target = try f.pushCard(.sleep, .running(timer: sleep), timer: sleep.id)
        f.clock.advance(by: 1_800)

        let outcome = try await f.tap(.wake, target)

        #expect(outcome == .performed)
        #expect(try f.store.activeTimer(kind: .sleep, for: F.baby) == nil)
        #expect(f.board.cards().isEmpty)
        #expect(f.server.requests.isEmpty)
        #expect(f.background.pending == 1)

        await f.run { await f.background.drain() }
        // The nap, which this phone's never-sent timer became, went out.
        #expect(f.server.requests.count == 1)
        #expect(f.server.requests.last?.mutations.contains { $0.entityKind == .sleep } == true)
    }

    /// A viewer's phone: the card's own metadata may still say it can write;
    /// the family's current authority says it cannot.
    @Test func aViewersCardNeverWrites() async throws {
        let f = try F()
        let sleep = try f.store.startSleep(for: F.baby)
        let target = try f.pushCard(.sleep, .running(timer: sleep), timer: sleep.id)
        try f.store.setWritePermission(.viewer, for: F.realm)
        let outbox = try f.outboxIDs()

        let outcome = try await f.perform(.wake, target)

        #expect(outcome == .readOnly)
        #expect(try f.store.activeTimer(kind: .sleep, for: F.baby)?.id == sleep.id)
        #expect(try f.outboxIDs() == outbox)
        #expect(f.server.requests.isEmpty)
        #expect(f.pending.consumeActivityHandoff() == nil)
    }

    /// A token card this phone requested, which no push has reached yet, on a
    /// viewer's phone: it may show controls, but its target is still checked
    /// against the family's current authority before anything is written.
    @Test func aViewersUnpushedTokenCardNeverWrites() async throws {
        let f = try F()
        let sleep = try f.store.startSleep(for: F.baby)
        try f.store.setWritePermission(.viewer, for: F.realm)
        let target = try #require(LiveActivityActionTarget.card(scope: f.scope(.sleep), remote: nil))
        #expect(!target.isPushed)

        let outcome = try await f.perform(.wake, target)

        #expect(outcome == .readOnly)
        #expect(try f.store.activeTimer(kind: .sleep, for: F.baby)?.id == sleep.id)
        #expect(f.server.requests.isEmpty)
    }

    /// L on a break card after the next part already started: the break the
    /// card offered is over. The running part is neither stopped nor
    /// replaced.
    @Test func aStaleBreakCardNeverReplacesTheRunningPart() async throws {
        let f = try F()
        let first = try f.store.startPart(.breast(.left), for: F.baby)
        f.clock.advance(by: 300)
        let feed = try #require(try f.store.stopFeed(first))
        f.clock.advance(by: 60)
        guard let open = try LiveActivityController.breakPresentation(kind: .feed, babyID: F.baby, in: f.store) else {
            Issue.record("no break open"); return
        }
        let target = try f.pushCard(.feed, open, timer: nil, session: feed.effectiveFeedingSessionID)
        let next = try f.store.startPart(.breast(.right), for: F.baby)
        let outbox = try f.outboxIDs()

        let outcome = try await f.perform(.startBreast(.left), target)

        #expect(outcome == .alreadyFinished)
        let running = try #require(try f.store.activeTimer(kind: .feed, for: F.baby))
        #expect(running.id == next.id)
        #expect(running.currentSide == .right)
        #expect(try f.outboxIDs() == outbox)
    }

    /// A card from a family this phone has since left — its baby carrying the
    /// same identifier as one here. Nothing is written, and the card goes.
    @Test func aCardFromAnotherFamilyIsEndedNotActedOn() async throws {
        let f = try F()
        let sleep = try f.store.startSleep(for: F.baby)
        let target = try f.pushCard(.sleep, .running(timer: sleep), timer: sleep.id, family: UUID())
        let outbox = try f.outboxIDs()

        let outcome = try await f.perform(.wake, target)

        #expect(outcome == .wrongRealm)
        #expect(try f.store.activeTimer(kind: .sleep, for: F.baby)?.id == sleep.id)
        #expect(try f.outboxIDs() == outbox)
        #expect(f.board.ended == ["card"])
    }

    /// Wake up on a current card: the nap ends, and the card this phone's own
    /// stop takes down is retired first — never reported as the parent
    /// dismissing it.
    @Test func aCurrentWakeEndsTheNapAndRetiresItsCard() async throws {
        let f = try F()
        let sleep = try f.store.startSleep(for: F.baby)
        try f.store.setLiveActivityID("card", on: sleep)
        let target = try f.pushCard(.sleep, .running(timer: sleep), timer: sleep.id)
        f.clock.advance(by: 1_800)

        let outcome = try await f.perform(.wake, target)

        #expect(outcome == .performed)
        #expect(try f.store.activeTimer(kind: .sleep, for: F.baby) == nil)
        #expect(f.push.retired == ["card"])
        #expect(f.board.ended == ["card"])
        #expect(f.server.requests.count == 1)
    }

    /// The button's target crosses the intent as one string; anything that
    /// does not read back is no target at all. A card without a realm keeps
    /// today's path.
    @Test func aCardsTargetSurvivesTheIntentParameter() throws {
        let f = try F()
        let revision = try LiveActivityRevision(scope: f.scope(.feed), generationID: UUID(), timerID: UUID(), sessionID: UUID(),
                                                familySequence: 42, presentationVersion: 3, presentationTimestamp: F.second, canWrite: true)
        let target = try #require(LiveActivityActionTarget.card(scope: f.scope(.feed), remote: revision))
        #expect(target.isPushed)
        #expect(target.minimumFamilySequence == 42)
        #expect(LiveActivityActionTarget(parameterValue: target.parameterValue) == target)
        #expect(LiveActivityActionTarget(parameterValue: "{\"scope\":1}") == nil)
        #expect(LiveActivityActionTarget(parameterValue: nil) == nil)
        #expect(LiveActivityActionTarget.card(scope: nil, remote: nil) == nil)
    }

    // MARK: After `ready`: the write boundary

    /// Demoted between the gate's check and the write: the store's boundary
    /// refuses the write under its own lock and rolls it back. Nothing is
    /// published, nothing is parked, and the card is reconciled read-only.
    @Test func aDemotionBetweenTheCheckAndTheWriteIsRefusedByTheBoundary() async throws {
        let f = try F()
        let sleep = try f.store.startSleep(for: F.baby)
        let target = try f.pushCard(.sleep, .running(timer: sleep), timer: sleep.id)
        let outbox = try f.outboxIDs()
        let store = f.store
        let gate = ScriptedGate {
            let context = try store.captureWriteContext()
            try store.setWritePermission(.viewer, for: F.realm)
            return .ready(context)
        }

        let outcome = try await f.run {
            try await LiveActivityCardActions.perform(.wake, target: target, babyID: F.baby, in: store, gate: gate, pending: f.pending)
        }

        #expect(outcome == .readOnly)
        #expect(gate.prepareCalls == 1)
        #expect(try store.activeTimer(kind: .sleep, for: F.baby)?.id == sleep.id)
        #expect(try f.outboxIDs() == outbox)
        #expect(gate.publishCalls == 0)
        #expect(gate.finished)
        #expect(f.pending.consumeActivityHandoff() == nil)
        #expect(f.board.cards().count == 1)
    }

    /// The realm moved between the check and the write — the generation the
    /// gate checked is no longer the store's — so the boundary refuses the
    /// write. Nothing is written; the reason is parked for the app.
    @Test func aRealmMoveBetweenTheCheckAndTheWriteHandsOff() async throws {
        let f = try F()
        let part = try f.store.startPart(.breast(.left), for: F.baby)
        let target = try f.pushCard(.feed, .running(timer: part), timer: part.id)
        let outbox = try f.outboxIDs()
        let current = try f.store.captureWriteContext()
        let gate = ScriptedGate { .ready(RealmWriteContext(realm: current.realm, transitionGeneration: current.transitionGeneration &- 1)) }

        let outcome = try await f.run {
            try await LiveActivityCardActions.perform(.stopFeed, target: target, babyID: F.baby, in: f.store, gate: gate, pending: f.pending)
        }

        #expect(outcome == .handedOff(.sessionChanged))
        #expect(f.pending.consumeActivityHandoff() == .sessionChanged)
        #expect(try f.store.activeTimer(kind: .feed, for: F.baby)?.id == part.id)
        #expect(try f.outboxIDs() == outbox)
        #expect(gate.publishCalls == 0)
        #expect(gate.finished)
    }

    // MARK: The other Task 10 gaps

    /// R on the server's break card while that break is open: the next part
    /// starts, joins the break's session, is published, and the card shows
    /// it at once as this phone's own change.
    @Test func aStartOnAPushedBreakCardJoinsTheBreaksSession() async throws {
        let f = try F()
        let first = try f.store.startPart(.breast(.left), for: F.baby)
        f.clock.advance(by: 300)
        let feed = try #require(try f.store.stopFeed(first))
        f.clock.advance(by: 60)
        let open = try #require(try LiveActivityController.breakPresentation(kind: .feed, babyID: F.baby, in: f.store))
        let session = feed.effectiveFeedingSessionID
        let target = try f.pushCard(.feed, open, timer: nil, session: session)

        let outcome = try await f.perform(.startBreast(.right), target)

        #expect(outcome == .performed)
        let running = try #require(try f.store.activeTimer(kind: .feed, for: F.baby))
        #expect(running.currentSide == .right)
        #expect(running.sessionID == session)
        // No catch-up before, one publish after.
        #expect(f.server.requests.count == 1)
        #expect(f.server.requests.last?.mutations.contains { $0.entityKind == .timer } == true)
        let card = try #require(f.board.cards().first)
        #expect(card.state.phase == .running)
        #expect(card.state.activityKind == .breastRight)
        #expect(f.pending.consume() == nil)
        #expect(f.pending.consumeActivityHandoff() == nil)
    }

    /// A button whose target names another kind, or another baby, than the
    /// intent was built for: the gate is never asked, nothing is written, and
    /// the reason is parked for the app.
    @Test func aTargetForAnotherBabyOrKindHandsOffWithoutAGateCheck() async throws {
        let f = try F()
        let sleep = try f.store.startSleep(for: F.baby)
        let outbox = try f.outboxIDs()
        let sleepTarget = try #require(LiveActivityActionTarget.card(scope: f.scope(.sleep), remote: nil))
        let store = f.store

        let kindGate = ScriptedGate { .ready(try store.captureWriteContext()) }
        let mismatchedKind = try await f.run {
            try await LiveActivityCardActions.perform(.stopFeed, target: sleepTarget, babyID: F.baby, in: store, gate: kindGate, pending: f.pending)
        }
        #expect(mismatchedKind == .handedOff(.unverifiable))
        #expect(kindGate.prepareCalls == 0)
        #expect(kindGate.finished)
        #expect(f.pending.consumeActivityHandoff() == .unverifiable)

        let babyGate = ScriptedGate { .ready(try store.captureWriteContext()) }
        let mismatchedBaby = try await f.run {
            try await LiveActivityCardActions.perform(.wake, target: sleepTarget, babyID: UUID(), in: store, gate: babyGate, pending: f.pending)
        }
        #expect(mismatchedBaby == .handedOff(.unverifiable))
        #expect(babyGate.prepareCalls == 0)
        #expect(babyGate.finished)
        #expect(f.pending.consumeActivityHandoff() == .unverifiable)

        #expect(try store.activeTimer(kind: .sleep, for: F.baby)?.id == sleep.id)
        #expect(try f.outboxIDs() == outbox)
    }

    /// A token card no push has reached shows only what this phone wrote, so
    /// its button needs no catch-up: off the private network, Wake up still
    /// ends the nap at once, the mutation waits in the outbox for the app,
    /// and the card is retired — never reported as the parent dismissing it.
    @Test func aTokenCardsButtonWorksOffline() async throws {
        let f = try F()
        let sleep = try f.store.startSleep(for: F.baby)
        // The phone's own token card for the nap, requested as the app does.
        await f.run { await LiveActivityController.adoptOrStart(for: sleep, in: f.store) }
        let card = try #require(sleep.liveActivityID)
        #expect(f.board.cards().first?.state.remote == nil)
        f.server.offline = true
        let target = try #require(LiveActivityActionTarget.card(scope: f.scope(.sleep), remote: nil))
        let outbox = try f.outboxIDs()
        f.clock.advance(by: 1_200)

        let outcome = try await f.perform(.wake, target)

        #expect(outcome == .performed)
        #expect(try f.store.activeTimer(kind: .sleep, for: F.baby) == nil)
        #expect(try f.outboxIDs() != outbox)
        // No catch-up for a card with nothing newer to wait for; the one
        // publish after the write fails offline and is left to the app.
        #expect(f.server.requests.count == 1)
        #expect(f.board.cards().isEmpty)
        #expect(f.push.retired == [card])
        #expect(f.pending.consumeActivityHandoff() == nil)
    }
}

/// A gate whose answer the test scripts. For the branches after `ready`:
/// what the store's write boundary refuses is decided by the store under its
/// own lock, whatever the gate said a moment earlier.
@MainActor
final class ScriptedGate: LiveActivityActionGate {
    private let decision: @MainActor () throws -> LiveActivityActionDecision
    private(set) var prepareCalls = 0
    private(set) var publishCalls = 0
    private(set) var finished = false

    init(_ decision: @escaping @MainActor () throws -> LiveActivityActionDecision) { self.decision = decision }

    func prepare(target: LiveActivityActionTarget, timeout: Duration) async -> LiveActivityActionDecision {
        prepareCalls += 1
        return (try? decision()) ?? .openApp(reason: .unverifiable)
    }

    func publish(timeout: Duration) async { publishCalls += 1 }

    func finish() { finished = true }
}

/// Work a card action hands off to run after the tap, held until the test
/// runs it.
@MainActor
final class BackgroundWork {
    private var work: [@MainActor () async -> Void] = []

    var pending: Int { work.count }

    func add(_ item: @escaping @MainActor () async -> Void) { work.append(item) }

    func drain() async {
        while !work.isEmpty { await work.removeFirst()() }
    }
}
