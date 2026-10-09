import BabyCore
import BabyProtocol
@testable import BabyStore
import Foundation
import Testing
@testable import BabySync

/// What the registration coordinator tells the card controller: which realm
/// is push-managed, which cards the server knows, which lifecycles the parent
/// dismissed — and what it does with a duplicate the controller retires.
@MainActor
@Suite("Live Activity push card state", .timeLimit(.minutes(1)))
struct LiveActivityPushCardStateTests {
    /// Push-managed only once the server holds this device: before the first
    /// accepted registration nothing could push, so the app keeps its local
    /// cards exactly as before. Stopping withdraws it.
    @Test func theRealmIsPublishedOnlyWhileTheServerHoldsThisDevice() async throws {
        let harness = PushHarness()
        let gate = Gate()
        let client = FakePushClient()
        client.onDevice = { registration, _ in
            await gate.wait()
            return try FakePushClient.accept(registration)
        }
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await gate.waitForArrival()
        #expect(harness.pushState.fence == nil)

        gate.open()
        await coordinator.waitUntilIdle()
        let fence = try #require(harness.pushState.fence)
        #expect(fence.contains(PushFixture.scope()))
        #expect(!fence.contains(PushFixture.scope(familyID: PushFixture.otherFamilyID)))

        // A new device version in flight (a changed break limit) does not
        // flip the realm back to local in between.
        coordinator.updateFeedBreakMinutes(45)
        #expect(harness.pushState.fence == fence)
        await coordinator.waitUntilIdle()

        // A probe that got no answer is not a withdrawal; a definitive no is.
        await coordinator.stop(withdrawingPush: false)
        #expect(harness.pushState.mode == .unknown)
        await coordinator.start(context: PushFixture.context(feedBreakMinutes: 45))
        await coordinator.waitUntilIdle()
        #expect(harness.pushState.fence == fence)
        await coordinator.stop()
        #expect(harness.pushState.fence == nil)
        #expect(harness.pushState.isWithdrawn)
        harness.events.finish()
    }

    @Test func registeredCardsArePublishedWithTheirCanonicalGeneration() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        harness.events.send(.activity(PushFixture.observation(token: PushFixture.activityToken)))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.activityCalls.count == 1)
        await coordinator.waitUntilIdle()

        let registration = try #require(harness.pushState.registration(for: "activity-1"))
        #expect(registration.generationID == PushFixture.canonicalGenerationID)
        #expect(registration.timerID == PushFixture.timerID)
        #expect(registration.accepted)
        harness.events.finish()
    }

    /// A duplicate the controller ends is forgotten: its token and ledger
    /// record go, and its end is never registered — on the server an ended
    /// card of the shared lifecycle reads as the parent dismissing it, which
    /// would stop the surviving card's updates too. Not even a relaunch that
    /// sees it again reports it.
    @Test func aRetiredDuplicateIsForgottenAndNeverReportedEnded() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        harness.events.send(.activity(PushFixture.observation(token: PushFixture.activityToken)))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.activityCalls.count == 1)
        await coordinator.waitUntilIdle()

        #expect(harness.pushState.retire(["activity-1"]))
        let ledger = try harness.store.read()
        let installation = try #require(ledger.installationID)
        #expect(ledger.activities["activity-1"] == nil)
        #expect(try harness.store.token(for: .activity(installation, "activity-1")) == nil)
        #expect(harness.pushState.registration(for: "activity-1") == nil)

        harness.events.send(.activity(PushFixture.observation(state: .ended, token: PushFixture.activityToken)))
        await coordinator.settle(harness.events)
        await coordinator.reconcileForeground()
        #expect(client.activityCalls.map(\.state) == [.active])

        await coordinator.stop()
        let events = TestPushEvents()
        let relaunched = LiveActivityPushCoordinator(events: events, store: harness.store, makeClient: { _ in client },
                                                     sleep: { try await harness.sleeper.sleep($0) }, isForeground: { true })
        events.send(.pushToStartToken(PushFixture.startToken))
        events.send(.activity(PushFixture.observation(state: .dismissed, token: PushFixture.activityToken)))
        await relaunched.start(context: PushFixture.context())
        await relaunched.settle(events)
        await relaunched.reconcile()
        #expect(client.activityCalls.map(\.state) == [.active])
        harness.events.finish()
        events.finish()
    }

    /// A card the parent swiped away names its lifecycle, so reconciliation
    /// does not bring that same feed or sleep back.
    @Test func aDismissedCardsLifecycleIsPublished() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        harness.events.send(.activity(PushFixture.observation(token: PushFixture.activityToken)))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.activityCalls.count == 1)
        #expect(!harness.pushState.isDismissed(lifecycle: PushFixture.timerID))

        harness.events.send(.activity(PushFixture.observation(state: .dismissed)))
        await coordinator.settle(harness.events)
        #expect(harness.pushState.isDismissed(lifecycle: PushFixture.timerID))
        await coordinator.waitUntilIdle()
        harness.events.finish()
    }
}

extension LiveActivityPushCardStateTests {
    /// A cold launch: the coordinator is built before any probe has answered.
    /// A ledger whose device the server accepted for this realm makes the
    /// realm push-managed from that moment, so the pre-sync pass and an App
    /// Intent never treat its pushed cards as local ones.
    @Test func anAcceptedLedgerMakesTheRealmPushManagedAtConstruction() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let first = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await first.start(context: PushFixture.context())
        await first.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        await first.waitUntilIdle()
        harness.events.finish()

        let relaunchState = LiveActivityPushState()
        _ = LiveActivityPushCoordinator(events: TestPushEvents(), store: harness.store, makeClient: { _ in client },
                                        pushState: relaunchState)
        let fence = try #require(relaunchState.fence)
        #expect(fence.contains(PushFixture.scope()))
    }

    /// A losing duplicate whose retirement cannot be written to the ledger is
    /// not ended: ended, a relaunch would report it to the server, which reads
    /// an ended card of the shared lifecycle as the parent dismissing it.
    @Test func aDuplicateIsNotEndedWhenItsRetirementCannotBeRecorded() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        harness.events.send(.activity(PushFixture.observation(id: "activity-0", token: PushFixture.activityToken)))
        harness.events.send(.activity(PushFixture.observation(id: "activity-1", token: "0123456789abcdee")))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.activityCalls.count == 2)
        await coordinator.waitUntilIdle()
        #expect(harness.pushState.fence != nil)

        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(Date(timeIntervalSince1970: 1_700_000_100)))
        try store.setActiveRealm(.shared(canonicalOrigin: PushFixture.builtIn.origin.canonicalString,
                                         serverInstanceID: PushFixture.builtIn.serverInstanceID, familyID: PushFixture.familyID))
        let board = MemoryCardBoard(now: { Date(timeIntervalSince1970: 1_700_000_100) })
        for id in ["activity-0", "activity-1"] {
            let observation = PushFixture.observation(id: id)
            board.receiveStart(id: id, babyID: PushFixture.babyID, kindRaw: "feed", scope: PushFixture.scope(),
                               state: TimerActivityContentState(startedAt: Date(timeIntervalSince1970: 1_700_000_000), sideLabel: "",
                                                                activityKind: .breastLeft, remote: observation.remote),
                               timestamp: Date(timeIntervalSince1970: 1_700_000_000))
        }

        let path = harness.directory.path
        try FileManager.default.setAttributes([.posixPermissions: 0o555], ofItemAtPath: path)
        await LiveCards.$board.withValue(board) {
            await LiveCards.$pushState.withValue(harness.pushState) {
                await LiveActivityController.reconcileAfterSync(in: store)
            }
        }
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: path)

        #expect(board.cards().map(\.id) == ["activity-0", "activity-1"])
        #expect(try harness.store.read().activities["activity-1"] != nil)
        harness.events.finish()
    }

    /// A pass the parent did not cause — here the post-sync pass, with the
    /// store already past the card's feed — ends a registered card the server
    /// pushed to. The end is the app's, so the card is retired before it
    /// goes: ActivityKit's `ended` for it is never registered. Registered, the
    /// server would read it as the parent dismissing that lifecycle and stop
    /// pushing it to this phone for as long as it runs.
    @Test func aNonOwnEndOfARegisteredCardIsNeverReportedToTheServer() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        let card = PushFixture.observation(token: PushFixture.activityToken)
        harness.events.send(.activity(card))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        await coordinator.waitUntilIdle()
        #expect(client.activityCalls.map(\.state) == [.active])
        #expect(harness.pushState.registration(for: card.activityID)?.accepted == true)

        let now = Date(timeIntervalSince1970: 1_700_000_100)
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(now))
        try store.setActiveRealm(.shared(canonicalOrigin: PushFixture.builtIn.origin.canonicalString,
                                         serverInstanceID: PushFixture.builtIn.serverInstanceID, familyID: PushFixture.familyID))
        try store.recordCursor(Cursor(serverInstanceID: PushFixture.builtIn.serverInstanceID, recoveryEpoch: Int64(PushFixture.epoch),
                                      familyID: PushFixture.familyID, sequence: 10))
        try store.context.save()
        let board = MemoryCardBoard(now: { now })
        board.receiveStart(id: card.activityID, babyID: PushFixture.babyID, kindRaw: "feed", scope: PushFixture.scope(),
                           state: TimerActivityContentState(startedAt: Date(timeIntervalSince1970: 1_700_000_000), sideLabel: "",
                                                            activityKind: .breastLeft, remote: card.remote),
                           timestamp: Date(timeIntervalSince1970: 1_700_000_000))

        await LiveCards.$board.withValue(board) {
            await LiveCards.$pushState.withValue(harness.pushState) {
                await LiveActivityController.reconcileAfterSync(in: store)
            }
        }
        #expect(board.ended == [card.activityID])
        #expect(try harness.store.read().retiredActivityIDs?.contains(card.activityID) == true)

        // ActivityKit reports the end the app made; a foreground pass follows.
        var ended = card
        ended.state = .ended
        harness.events.send(.activity(ended))
        await coordinator.settle(harness.events)
        await coordinator.reconcileForeground()
        await coordinator.waitUntilIdle()
        #expect(client.activityCalls.map(\.state) == [.active])
        harness.events.finish()
    }
}

/// The engine hands the cards over to post-sync reconciliation exactly when
/// a sync succeeded — never on a failed one, whose store says nothing newer.
@MainActor
@Suite("Post-sync card reconciliation")
struct PostSyncCardReconciliationTests {
    @MainActor
    final class Announcer: RemoteTimerAnnouncer {
        private(set) var reconciled = 0
        func started(_ timer: ActiveTimer, in store: BabyStore) async {}
        func changed(_ timer: ActiveTimer, in store: BabyStore) async {}
        func ended(timerID: UUID, liveActivityID: String?, in store: BabyStore) async {}
        func syncSucceeded(in store: BabyStore) async { reconciled += 1 }
    }

    @Test func onlyASuccessfulSyncReconcilesTheCards() async throws {
        let family = try Family()
        try family.seedOwner()
        let owner = family.ownerEngine()
        try await owner.activateAsOwner()
        let announcer = Announcer()
        let joiner = family.joinerEngine(announcer: announcer)
        try await joiner.activateAsJoiner()
        let afterActivation = announcer.reconciled
        #expect(afterActivation == 1)

        family.joinerTransport.offline = true
        await joiner.syncNow(.foreground)
        #expect(announcer.reconciled == afterActivation)

        family.joinerTransport.offline = false
        await joiner.syncNow(.foreground)
        #expect(announcer.reconciled == afterActivation + 1)
    }

    /// The partner edits a running sleep (sequence N) and then changes it
    /// again (N+1). This phone's pull ends at N, but the push for N+1 has
    /// already reached its card. Announcing the remote change at N must not
    /// write the store's older view over the newer pushed card — a remote
    /// change is not this phone's own, and waits until the store catches up.
    @Test func aRemoteChangeNeverOverwritesANewerPushedCard() async throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
        let family = try Family(clock: clock)
        let (baby, _) = try family.seedOwner()
        let owner = family.ownerEngine()
        try await owner.activateAsOwner()
        let joiner = family.joinerEngine(announcer: LiveActivityRemoteTimerAnnouncer())
        try await joiner.activateAsJoiner()

        let started = try family.owner.startSleep(for: baby.id)
        let originalStart = started.startedAt
        await owner.syncNow(.localWrite)
        await joiner.syncNow(.foreground)
        let mirrored = try #require(try family.joiner.activeTimer(kind: .sleep, for: baby.id))
        let cursor = try #require(try family.joiner.syncCursor())
        try #require(cursor.recoveryEpoch >= 1)

        let fence = LiveActivityRealmFence(serverInstanceID: cursor.serverInstanceID, recoveryEpoch: cursor.recoveryEpoch,
                                           familyID: cursor.familyID)
        let scope = try #require(fence.scope(babyID: baby.id, kindRaw: "sleep"))
        // The push for the partner's later change: newer than anything this
        // phone will have pulled.
        let pushed = TimerActivityContentState(
            startedAt: started.startedAt.addingTimeInterval(-1_800), sideLabel: "", activityKind: .sleep,
            remote: try LiveActivityRevision(scope: scope, generationID: UUID(), timerID: mirrored.id, familySequence: cursor.sequence + 50,
                                             presentationVersion: 3, presentationTimestamp: 1_800_000_100, canWrite: true)
        )
        let board = MemoryCardBoard(now: { Date(timeIntervalSince1970: 1_800_000_000) })
        board.receiveStart(id: "pushed", babyID: baby.id, kindRaw: "sleep", scope: scope, state: pushed,
                           timestamp: Date(timeIntervalSince1970: 1_800_000_100))
        try family.joiner.setLiveActivityID("pushed", on: mirrored)
        let push = LiveActivityPushState()
        push.install(fence)

        // The partner's edit at N, pulled here and announced as a change.
        try family.owner.editRunningTimer(id: started.id, startedAt: originalStart.addingTimeInterval(-600), components: [])
        await owner.syncNow(.localWrite)
        await LiveCards.$board.withValue(board) {
            await LiveCards.$pushState.withValue(push) {
                await joiner.syncNow(.foreground)
            }
        }
        let edited = try #require(try family.joiner.activeTimer(kind: .sleep, for: baby.id))
        #expect(abs(edited.startedAt.timeIntervalSince(originalStart.addingTimeInterval(-600))) < 0.001)
        #expect(board.cards().first?.state == pushed)
    }
}
