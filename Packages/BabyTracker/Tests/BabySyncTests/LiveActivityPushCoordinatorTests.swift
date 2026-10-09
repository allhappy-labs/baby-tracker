import BabyProtocol
import BabyStore
import Foundation
import Testing
@testable import BabySync

/// The registration lifecycle, end to end against a scripted server: what is
/// registered, with which version, when it retries, and — the part that
/// matters most — what an old realm's task can no longer do.
@MainActor
@Suite(.timeLimit(.minutes(1)))
struct LiveActivityPushCoordinatorTests {
    // MARK: Automatic registration

    @Test func registersThePushToStartTokenWithoutAnyUserAction() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let coordinator = harness.coordinator(clients: [client])
        await coordinator.start(context: PushFixture.context(feedBreakMinutes: 45))
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        await coordinator.waitUntilIdle()

        let sent = try #require(client.deviceCalls.first)
        let ledger = try harness.store.read()
        #expect(sent.registrationVersion == 1)
        #expect(sent.pushToStartToken == PushFixture.startToken)
        #expect(sent.apnsEnvironment == .sandbox)
        #expect(sent.feedBreakMinutes == 45)
        #expect(sent.installationID == ledger.installationID)
        #expect(ledger.device?.accepted == true)
        #expect(try harness.store.token(for: .pushToStart(sent.installationID)) == PushFixture.startToken)
        let file = String(decoding: try Data(contentsOf: harness.store.ledgerURL), as: UTF8.self)
        #expect(!file.contains(PushFixture.startToken))
        harness.events.finish()
    }

    @Test func tokenEmittedBeforeTheContextExistsIsRegisteredOnceItDoes() async throws {
        // Launch order without a scene: ActivityKit hands over the token while
        // the account and server probe are still being restored.
        let harness = PushHarness()
        let client = FakePushClient()
        let coordinator = harness.coordinator(clients: [client])
        coordinator.observeEvents()
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.settle(harness.events)
        #expect(coordinator.pushToStartToken == PushFixture.startToken)
        #expect(client.calls.isEmpty)

        await coordinator.start(context: PushFixture.context())
        await coordinator.waitUntilIdle()
        #expect(client.deviceCalls.map(\.pushToStartToken) == [PushFixture.startToken])
        harness.events.finish()
    }

    @Test func tokenEmittedAfterRegistrationRotatesWithANewerVersion() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        await coordinator.waitUntilIdle()

        harness.events.send(.pushToStartToken(PushFixture.rotatedStartToken))
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 2)
        await coordinator.waitUntilIdle()
        #expect(client.deviceCalls.map(\.registrationVersion) == [1, 2])
        #expect(client.deviceCalls.last?.pushToStartToken == PushFixture.rotatedStartToken)
        harness.events.finish()
    }

    @Test func simultaneousInitialTokenAndStreamEmissionRegisterOnce() async throws {
        let harness = PushHarness()
        let gate = Gate()
        let client = FakePushClient()
        client.onDevice = { registration, _ in
            await gate.wait()
            return try FakePushClient.accept(registration)
        }
        let coordinator = harness.coordinator(clients: [client])
        // The current value and the stream's first element are the same token.
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await gate.waitForArrival()
        // …and a third copy lands while the first request is still in flight.
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.waitForHandledEvents(harness.events.emitted)
        gate.open()
        await coordinator.waitUntilIdle()
        #expect(client.deviceCalls.count == 1)
        harness.events.finish()
    }

    // MARK: Failure, retry and relaunch

    @Test func failuresRetryAtTwoFiveFifteenSecondsThenWaitForTheNextTrigger() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        client.onDevice = { _, _ in throw LiveActivityPushClientError.unavailable }
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 4)
        await coordinator.waitUntilIdle()
        #expect(harness.sleeper.durations == [.seconds(2), .seconds(5), .seconds(15)])
        // An unchanged request is retried identically, never re-versioned.
        #expect(Set(client.deviceCalls.map(\.registrationVersion)) == [1])
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 4)

        client.onDevice = { registration, _ in try FakePushClient.accept(registration) }
        await coordinator.reconcile()
        #expect(client.deviceCalls.count == 5)
        #expect(try harness.store.read().device?.accepted == true)
        harness.events.finish()
    }

    @Test func relaunchAfterAFailedRegistrationContinuesTheSameInstallationMonotonically() async throws {
        let harness = PushHarness()
        let failing = FakePushClient()
        failing.onDevice = { _, _ in throw LiveActivityPushClientError.rejected("invalid_request") }
        let first = harness.coordinator(clients: [failing])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await first.start(context: PushFixture.context())
        await first.settle(harness.events)
        #expect(failing.deviceCalls.count == 1)
        await first.waitUntilIdle()
        await first.stop()
        let installation = try #require(try harness.store.read().installationID)

        // A new process: new coordinator, new event stream, same ledger file
        // and Keychain. The token arrives again from ActivityKit.
        let relaunchedEvents = TestPushEvents()
        let relaunchedClient = FakePushClient()
        let relaunched = LiveActivityPushCoordinator(
            events: relaunchedEvents, store: harness.store, makeClient: { _ in relaunchedClient },
            sleep: { try await harness.sleeper.sleep($0) }
        )
        await relaunched.start(context: PushFixture.context(feedBreakMinutes: 60))
        relaunchedEvents.send(.pushToStartToken(PushFixture.startToken))
        await relaunched.settle(relaunchedEvents)
        #expect(relaunchedClient.deviceCalls.count == 1)
        await relaunched.waitUntilIdle()
        let sent = try #require(relaunchedClient.deviceCalls.first)
        #expect(sent.installationID == installation)
        // The feed limit changed, so the content changed: a strictly newer version.
        #expect(sent.registrationVersion == 2)
        #expect(try harness.store.read().device?.accepted == true)
        harness.events.finish()
        relaunchedEvents.finish()
    }

    @Test func aLateResponseForAnOldTokenDoesNotWinOverTheNewOne() async throws {
        let harness = PushHarness()
        let gate = Gate()
        let client = FakePushClient()
        client.onDevice = { registration, index in
            if index == 1 { await gate.wait() }
            return try FakePushClient.accept(registration)
        }
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await gate.waitForArrival()
        harness.events.send(.pushToStartToken(PushFixture.rotatedStartToken))
        await coordinator.waitForHandledEvents(harness.events.emitted)
        gate.open()
        await coordinator.waitUntilIdle()

        #expect(client.deviceCalls.map(\.pushToStartToken) == [PushFixture.startToken, PushFixture.rotatedStartToken])
        #expect(client.deviceCalls.map(\.registrationVersion) == [1, 2])
        let ledger = try harness.store.read()
        #expect(ledger.device?.version == 2)
        #expect(ledger.device?.accepted == true)
        #expect(try harness.store.token(for: .pushToStart(try #require(ledger.installationID))) == PushFixture.rotatedStartToken)
        harness.events.finish()
    }

    @Test func aVersionConflictForTheCurrentRequestReversionsOnce() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        client.onDevice = { registration, index in
            if index == 1 { throw LiveActivityPushClientError.versionConflict }
            return try FakePushClient.accept(registration)
        }
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 2)
        await coordinator.waitUntilIdle()
        #expect(client.deviceCalls.map(\.registrationVersion) == [1, 2])
        #expect(try harness.store.read().device?.accepted == true)
        harness.events.finish()
    }

    /// Makes the ledger unwritable for the duration of `body`: the atomic
    /// replace needs a writable directory, the lock file stays usable.
    private func withUnwritableLedger(_ harness: PushHarness, _ body: () async throws -> Void) async rethrows {
        let path = harness.directory.path
        try? FileManager.default.setAttributes([.posixPermissions: 0o555], ofItemAtPath: path)
        defer { try? FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: path) }
        try await body()
    }

    @Test func aRotatedStartTokenSurvivesAFailedLedgerCommit() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        await coordinator.waitUntilIdle()
        let installation = try #require(try harness.store.read().installationID)

        await withUnwritableLedger(harness) {
            harness.events.send(.pushToStartToken(PushFixture.rotatedStartToken))
            await coordinator.settle(harness.events)
            #expect(coordinator.pushToStartToken == PushFixture.rotatedStartToken)
            await coordinator.waitUntilIdle()
        }
        // Nothing was sent, and the Keychain still names what the ledger's
        // accepted version carried.
        #expect(client.deviceCalls.count == 1)
        #expect(try harness.store.token(for: .pushToStart(installation)) == PushFixture.startToken)

        await coordinator.reconcile()
        let last = try #require(client.deviceCalls.last)
        #expect(last.pushToStartToken == PushFixture.rotatedStartToken)
        #expect(last.registrationVersion > 1)
        harness.events.finish()
    }

    @Test func aRotatedActivityTokenSurvivesAFailedLedgerCommit() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        harness.events.send(.activity(PushFixture.observation(token: PushFixture.activityToken)))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.activityCalls.count == 1)
        await coordinator.waitUntilIdle()
        let installation = try #require(try harness.store.read().installationID)
        let rotated = "fedcba9876543210"

        await withUnwritableLedger(harness) {
            harness.events.send(.activity(PushFixture.observation(token: rotated)))
            await coordinator.settle(harness.events)
            #expect(coordinator.observed["activity-1"]?.pushToken == rotated)
            await coordinator.waitUntilIdle()
        }
        #expect(client.activityCalls.count == 1)
        #expect(try harness.store.token(for: .activity(installation, "activity-1")) == PushFixture.activityToken)

        await coordinator.reconcile()
        let last = try #require(client.activityCalls.last)
        #expect(last.token == rotated)
        #expect(last.tokenVersion == 2)
        harness.events.finish()
    }

    // MARK: Realm changes

    @Test func signOutDuringAnAwaitedRequestCannotUploadIntoTheNextRealm() async throws {
        let harness = PushHarness()
        let gate = Gate()
        let oldFamily = FakePushClient(familyID: PushFixture.familyID)
        oldFamily.onDevice = { registration, _ in
            // Held until the session ends; then the server's acceptance
            // arrives anyway, after the sign-out, the way a response already
            // on the wire does when its task is cancelled.
            await gate.waitUnlessCancelled()
            return try FakePushClient.accept(registration)
        }
        let newFamily = FakePushClient(familyID: PushFixture.otherFamilyID)
        let coordinator = harness.coordinator(clients: [oldFamily, newFamily])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        harness.events.send(.activity(PushFixture.observation(token: PushFixture.activityToken)))
        await coordinator.start(context: PushFixture.context())
        await coordinator.waitForHandledEvents(harness.events.emitted)
        await gate.waitForArrival()

        // The old family's request is in flight when the session ends; its
        // acceptance is returned only once the sign-out has begun.
        await coordinator.stop()
        #expect(coordinator.context == nil)

        await coordinator.start(context: PushFixture.context(familyID: PushFixture.otherFamilyID))
        await coordinator.waitUntilIdle()

        // The old task neither registered its activity nor recorded its
        // receipt once it lost the realm.
        #expect(oldFamily.deviceCalls.count == 1)
        #expect(oldFamily.activityCalls.isEmpty)
        let fresh = try #require(newFamily.deviceCalls.first)
        #expect(fresh.registrationVersion > 1)
        // An activity scoped to the old family is never sent to the new one.
        #expect(newFamily.activityCalls.isEmpty)
        let ledger = try harness.store.read()
        #expect(ledger.device?.familyID == PushFixture.otherFamilyID)
        #expect(ledger.device?.accepted == true)
        harness.events.finish()
    }

    @Test func anEnvironmentChangeReregistersDeviceAndActivitiesWithNewerVersions() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        harness.events.send(.activity(PushFixture.observation(token: PushFixture.activityToken)))
        await coordinator.start(context: PushFixture.context(environment: .sandbox))
        await coordinator.settle(harness.events)
        #expect(client.activityCalls.count == 1)
        await coordinator.waitUntilIdle()

        await coordinator.start(context: PushFixture.context(environment: .production))
        await coordinator.waitUntilIdle()
        #expect(client.deviceCalls.map(\.apnsEnvironment) == [.sandbox, .production])
        #expect(client.deviceCalls.map(\.registrationVersion) == [1, 2])
        // Same raw token, newer version: an equal-version replay from the old
        // environment would be refused by the server's namespace fence.
        #expect(client.activityCalls.map(\.token) == [PushFixture.activityToken, PushFixture.activityToken])
        #expect(client.activityCalls.map(\.tokenVersion) == [1, 2])
        harness.events.finish()
    }

    @Test func feedBreakChangeIsReplayedWithANewerVersion() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context(feedBreakMinutes: 30))
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        await coordinator.waitUntilIdle()
        coordinator.updateFeedBreakMinutes(50)
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 2)
        await coordinator.waitUntilIdle()
        #expect(client.deviceCalls.map(\.feedBreakMinutes) == [30, 50])
        #expect(client.deviceCalls.map(\.registrationVersion) == [1, 2])
        // A foreground replay of the same state reuses the accepted version.
        await coordinator.reconcile()
        #expect(client.deviceCalls.map(\.registrationVersion) == [1, 2, 2])
        harness.events.finish()
    }

    // MARK: Activities

    @Test func remoteStartWhoseTokenArrivesLateRegistersWhenTheTokenDoes() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 1)

        // ActivityKit reports the pushed card before its update token exists.
        harness.events.send(.activity(PushFixture.observation()))
        await coordinator.settle(harness.events)
        await coordinator.waitUntilIdle()
        #expect(client.activityCalls.isEmpty)
        // A missing token is not an absence: no foreground inventory is sent.
        #expect(client.deviceCalls.allSatisfy { $0.foregroundInventory == nil })

        harness.events.send(.activity(PushFixture.observation(token: PushFixture.activityToken)))
        await coordinator.settle(harness.events)
        #expect(client.activityCalls.count == 1)
        await coordinator.waitUntilIdle()
        let sent = try #require(client.activityCalls.first)
        #expect(sent.activityID == "activity-1")
        #expect(sent.token == PushFixture.activityToken)
        #expect(sent.tokenVersion == 1)
        #expect(sent.state == .active)
        #expect(sent.knownGenerationID == PushFixture.generationID)
        #expect(sent.timerID == PushFixture.timerID)
        #expect(sent.scope == PushFixture.scope())
        let ledger = try harness.store.read()
        // Provisional → canonical generation, kept for the next registration.
        #expect(ledger.activities["activity-1"]?.canonicalGenerationID == PushFixture.canonicalGenerationID)
        #expect(try harness.store.token(for: .activity(try #require(ledger.installationID), "activity-1")) == PushFixture.activityToken)
        // Learning the canonical generation is not a change of its own.
        await coordinator.reconcile()
        #expect(client.activityCalls.count == 1)
        harness.events.finish()
    }

    @Test func aCardFromAnotherRealmIsNeverRegistered() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        harness.events.send(.activity(PushFixture.observation(id: "foreign", familyID: PushFixture.otherFamilyID, token: PushFixture.activityToken)))
        harness.events.send(.activity(LiveActivityObservation(activityID: "legacy", babyID: PushFixture.babyID, kindRaw: "feed",
                                                              scope: nil, state: .active, pushToken: PushFixture.activityToken)))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        await coordinator.waitUntilIdle()
        #expect(client.activityCalls.isEmpty)
        harness.events.finish()
    }

    @Test func endedAndDismissedCardsRegisterTheirStateWithNewerVersions() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        harness.events.send(.activity(PushFixture.observation(token: PushFixture.activityToken)))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.activityCalls.count == 1)
        harness.events.send(.activity(PushFixture.observation(state: .ended)))
        await coordinator.settle(harness.events)
        #expect(client.activityCalls.count == 2)
        harness.events.send(.activity(PushFixture.observation(state: .dismissed)))
        await coordinator.settle(harness.events)
        #expect(client.activityCalls.count == 3)
        await coordinator.waitUntilIdle()
        #expect(client.activityCalls.map(\.state) == [.active, .ended, .dismissed])
        #expect(client.activityCalls.map(\.tokenVersion) == [1, 2, 3])
        // The canonical generation learned from the first receipt is what the
        // later registrations claim.
        #expect(client.activityCalls.dropFirst().allSatisfy { $0.knownGenerationID == PushFixture.canonicalGenerationID })
        #expect(client.activityCalls.allSatisfy { $0.token == PushFixture.activityToken })
        harness.events.finish()
    }

    @Test func anUnknownActivityStateIsNeverActedOn() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        // A state a future ActivityKit adds: not live, not ended, not dismissed.
        harness.events.send(.activity(PushFixture.observation(state: .unknown, token: PushFixture.activityToken)))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        await coordinator.waitUntilIdle()
        #expect(client.activityCalls.isEmpty)

        harness.events.send(.activity(PushFixture.observation(state: .active)))
        await coordinator.settle(harness.events)
        #expect(client.activityCalls.count == 1)
        // An unknown update neither ends the card nor re-registers it.
        harness.events.send(.activity(PushFixture.observation(state: .unknown)))
        await coordinator.settle(harness.events)
        await coordinator.waitUntilIdle()
        #expect(client.activityCalls.map(\.state) == [.active])
        #expect(coordinator.observed["activity-1"]?.state == .active)
        harness.events.finish()
    }

    @Test func aCardDismissedWhileUnobservedIsRegisteredDismissedThenForgotten() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let first = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        harness.events.send(.activity(PushFixture.observation(token: PushFixture.activityToken)))
        await first.start(context: PushFixture.context())
        await first.settle(harness.events)
        #expect(client.activityCalls.count == 1)
        await first.waitUntilIdle()
        await first.stop()

        // Next launch: the card was swiped away while the app was not running,
        // so ActivityKit neither lists nor reports it.
        let events = TestPushEvents()
        let relaunched = LiveActivityPushCoordinator(events: events, store: harness.store, makeClient: { _ in client },
                                                     sleep: { try await harness.sleeper.sleep($0) }, isForeground: { true })
        events.send(.pushToStartToken(PushFixture.startToken))
        await relaunched.start(context: PushFixture.context())
        await relaunched.reconcile()
        #expect(client.activityCalls.count == 1)
        events.setInventory([])
        await relaunched.reconcileForeground()
        let dismissal = try #require(client.activityCalls.last)
        #expect(client.activityCalls.count == 2)
        #expect(dismissal.state == .dismissed)
        #expect(dismissal.tokenVersion == 2)
        #expect(dismissal.token == PushFixture.activityToken)
        #expect(dismissal.knownGenerationID == PushFixture.canonicalGenerationID)

        await relaunched.reconcileForeground()
        let ledger = try harness.store.read()
        #expect(ledger.activities["activity-1"] == nil)
        #expect(try harness.store.token(for: .activity(try #require(ledger.installationID), "activity-1")) == nil)
        #expect(client.activityCalls.count == 2)
        harness.events.finish()
        events.finish()
    }

    /// A card absent from the foreground inventory whose token is gone — a
    /// Keychain write that failed, an item lost — can never be registered
    /// dismissed: no token, no request. It is forgotten once, rather than
    /// re-marked dismissed on every foreground and carried in the ledger for
    /// good.
    @Test func anAbsentCardWithNoTokenIsForgottenOnceNotRemarkedEveryForeground() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let first = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        harness.events.send(.activity(PushFixture.observation(token: PushFixture.activityToken)))
        await first.start(context: PushFixture.context())
        await first.settle(harness.events)
        #expect(client.activityCalls.count == 1)
        await first.waitUntilIdle()
        await first.stop()
        let installation = try #require(try harness.store.read().installationID)
        try harness.store.deleteToken(for: .activity(installation, "activity-1"))

        let events = TestPushEvents()
        let relaunched = LiveActivityPushCoordinator(events: events, store: harness.store, makeClient: { _ in client },
                                                     sleep: { try await harness.sleeper.sleep($0) }, isForeground: { true })
        events.send(.pushToStartToken(PushFixture.startToken))
        await relaunched.start(context: PushFixture.context())
        events.setInventory([])
        await relaunched.reconcileForeground()
        #expect(client.activityCalls.count == 1)
        #expect(try harness.store.read().activities["activity-1"] == nil)
        #expect(relaunched.observed["activity-1"] == nil)

        await relaunched.reconcileForeground()
        #expect(client.activityCalls.count == 1)
        #expect(try harness.store.read().activities["activity-1"] == nil)
        #expect(relaunched.observed["activity-1"] == nil)
        harness.events.finish()
        events.finish()
    }

    @Test func aRefusedActivityRenewsTheDeviceRegistrationThenRetries() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        client.onActivity = { registration, index in
            // The server lost this device's row (a new session lineage).
            if index == 2 { throw LiveActivityPushClientError.registrationDenied }
            return try FakePushClient.accept(registration, generation: PushFixture.canonicalGenerationID)
        }
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        harness.events.send(.activity(PushFixture.observation(token: PushFixture.activityToken)))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.activityCalls.count == 2)
        await coordinator.waitUntilIdle()
        #expect(client.deviceCalls.map(\.registrationVersion) == [1, 2])
        #expect(client.calls.count == 4)
        #expect(try harness.store.read().activities["activity-1"]?.accepted == true)
        harness.events.finish()
    }

    // MARK: Permission revocation

    @Test func turningLiveActivitiesOffTakesTheRegistrationDownAndOnRestoresIt() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        await coordinator.waitUntilIdle()

        harness.events.send(.activitiesEnabled(false))
        await coordinator.settle(harness.events)
        #expect(client.unregisterCalls.count == 1)
        await coordinator.waitUntilIdle()
        let installation = try #require(try harness.store.read().installationID)
        #expect(client.unregisterCalls.first?.0 == installation)
        #expect(client.unregisterCalls.first?.1 == 2)
        #expect(try harness.store.read().pendingRemovals.isEmpty)
        // Nothing is registered while disallowed, whatever ActivityKit says.
        harness.events.send(.pushToStartToken(PushFixture.rotatedStartToken))
        await coordinator.settle(harness.events)
        await coordinator.waitUntilIdle()
        #expect(client.deviceCalls.count == 1)

        harness.events.send(.activitiesEnabled(true))
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 2)
        await coordinator.waitUntilIdle()
        #expect(client.deviceCalls.last?.registrationVersion == 3)
        #expect(client.deviceCalls.last?.pushToStartToken == PushFixture.rotatedStartToken)
        harness.events.finish()
    }

    @Test func aPendingRemovalNeverRunsInAnotherFamily() async throws {
        let harness = PushHarness()
        let failing = FakePushClient(familyID: PushFixture.familyID)
        failing.onUnregister = { _, _ in throw LiveActivityPushClientError.rejected("invalid_request") }
        let other = FakePushClient(familyID: PushFixture.otherFamilyID)
        let coordinator = harness.coordinator(clients: [failing, other])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(failing.deviceCalls.count == 1)
        await coordinator.start(context: PushFixture.context(activitiesAllowed: false))
        await coordinator.waitUntilIdle()
        #expect(failing.unregisterCalls.count == 1)

        await coordinator.start(context: PushFixture.context(familyID: PushFixture.otherFamilyID))
        await coordinator.waitUntilIdle()
        #expect(other.unregisterCalls.isEmpty)
        #expect(other.deviceCalls.count == 1)
        harness.events.finish()
    }

    // MARK: Foreground inventory

    @Test func onlyAForegroundInventoryAttestsAnUnconfirmedStartAbsent() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let unconfirmed = try LiveActivityUnconfirmedStart(scope: PushFixture.scope(), generationID: PushFixture.generationID, attemptID: PushFixture.attemptID)
        client.onDevice = { registration, _ in try FakePushClient.accept(registration, unconfirmed: [unconfirmed]) }
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        await coordinator.reconcile()
        // Background passes never claim absence.
        #expect(client.deviceCalls.allSatisfy { $0.foregroundInventory == nil })

        harness.events.setInventory([])
        await coordinator.reconcileForeground()
        let proof = try #require(client.deviceCalls.last?.foregroundInventory)
        #expect(proof.inventoryVersion == 1)
        #expect(proof.absentGenerations == [try LiveActivityAbsentGeneration(generationID: PushFixture.generationID, observedAttemptID: PushFixture.attemptID)])
        // The proof rides an identical device registration.
        #expect(Set(client.deviceCalls.map(\.registrationVersion)) == [1])
        #expect(try harness.store.read().pendingInventory == nil)

        // A card for that generation on screen is presence, not absence.
        let before = client.deviceCalls.count
        harness.events.setInventory([PushFixture.observation()])
        await coordinator.reconcileForeground()
        #expect(client.deviceCalls.count > before)
        #expect(client.deviceCalls.dropFirst(before).allSatisfy { $0.foregroundInventory == nil })
        harness.events.finish()
    }

    @Test func aForegroundRequestIsNotSpentByALaterRetryPass() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let unconfirmed = try LiveActivityUnconfirmedStart(scope: PushFixture.scope(), generationID: PushFixture.generationID, attemptID: PushFixture.attemptID)
        client.onDevice = { registration, index in
            // The foreground replay fails; the retry runs ~2 s later, when
            // the app may already be in the background.
            if index == 2 { throw LiveActivityPushClientError.unavailable }
            return try FakePushClient.accept(registration, unconfirmed: [unconfirmed])
        }
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        await coordinator.waitUntilIdle()

        harness.events.setInventory([])
        await coordinator.reconcileForeground()
        #expect(client.deviceCalls.count == 3)
        #expect(client.deviceCalls.allSatisfy { $0.foregroundInventory == nil })
        let ledger = try harness.store.read()
        #expect(ledger.pendingInventory == nil)
        #expect(ledger.inventoryVersion == 0)
        harness.events.finish()
    }

    @Test func anInventoryReadWhileBackgroundedProvesNothing() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let unconfirmed = try LiveActivityUnconfirmedStart(scope: PushFixture.scope(), generationID: PushFixture.generationID, attemptID: PushFixture.attemptID)
        client.onDevice = { registration, _ in try FakePushClient.accept(registration, unconfirmed: [unconfirmed]) }
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        await coordinator.waitUntilIdle()

        // didBecomeActive asked, but the app left the foreground before the pass read.
        harness.foreground.value = false
        harness.events.setInventory([])
        await coordinator.reconcileForeground()
        #expect(client.deviceCalls.allSatisfy { $0.foregroundInventory == nil })
        #expect(try harness.store.read().inventoryVersion == 0)
        harness.events.finish()
    }

    @Test func anUnacknowledgedInventoryIsRetriedIdentically() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let unconfirmed = try LiveActivityUnconfirmedStart(scope: PushFixture.scope(), generationID: PushFixture.generationID, attemptID: PushFixture.attemptID)
        client.onDevice = { registration, _ in
            if registration.foregroundInventory != nil { throw LiveActivityPushClientError.unavailable }
            return try FakePushClient.accept(registration, unconfirmed: [unconfirmed])
        }
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        await coordinator.waitUntilIdle()
        await coordinator.reconcileForeground()
        let proofs = client.deviceCalls.compactMap(\.foregroundInventory)
        #expect(proofs.count == 4)
        #expect(Set(proofs.map(\.inventoryVersion)) == [1])
        #expect(try harness.store.read().pendingInventory?.inventoryVersion == 1)
        harness.events.finish()
    }

    @Test func aProofRefusedOutrightIsDroppedSoTheDeviceCanRegisterAgain() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let unconfirmed = try LiveActivityUnconfirmedStart(scope: PushFixture.scope(), generationID: PushFixture.generationID, attemptID: PushFixture.attemptID)
        client.onDevice = { registration, _ in
            if registration.foregroundInventory != nil { throw LiveActivityPushClientError.rejected("invalid_request") }
            return try FakePushClient.accept(registration, unconfirmed: [unconfirmed])
        }
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        await coordinator.waitUntilIdle()
        harness.events.setInventory([])
        await coordinator.reconcileForeground()
        #expect(client.deviceCalls.compactMap(\.foregroundInventory).count == 1)
        var ledger = try harness.store.read()
        #expect(ledger.pendingInventory == nil)
        // The version stays spent: a later proof is strictly newer.
        #expect(ledger.inventoryVersion == 1)

        // Later registrations are no longer weighed down by it.
        let before = client.deviceCalls.count
        await coordinator.reconcile()
        #expect(client.deviceCalls.count == before + 1)
        #expect(client.deviceCalls.last?.foregroundInventory == nil)
        ledger = try harness.store.read()
        #expect(ledger.device?.accepted == true)

        await coordinator.reconcileForeground()
        #expect(client.deviceCalls.compactMap(\.foregroundInventory).last?.inventoryVersion == 2)
        harness.events.finish()
    }

    @Test func aPendingAbsenceProofNeverTravelsToAnotherFamily() async throws {
        let harness = PushHarness()
        let first = FakePushClient(familyID: PushFixture.familyID)
        let unconfirmed = try LiveActivityUnconfirmedStart(scope: PushFixture.scope(), generationID: PushFixture.generationID, attemptID: PushFixture.attemptID)
        first.onDevice = { registration, _ in
            // Retryable: the proof stays pending, which is what is under test.
            if registration.foregroundInventory != nil { throw LiveActivityPushClientError.unavailable }
            return try FakePushClient.accept(registration, unconfirmed: [unconfirmed])
        }
        let second = FakePushClient(familyID: PushFixture.otherFamilyID)
        let coordinator = harness.coordinator(clients: [first, second])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(first.deviceCalls.count == 1)
        await coordinator.waitUntilIdle()
        await coordinator.reconcileForeground()
        #expect(try harness.store.read().pendingInventory != nil)

        await coordinator.start(context: PushFixture.context(familyID: PushFixture.otherFamilyID))
        await coordinator.reconcileForeground()
        #expect(!second.deviceCalls.isEmpty)
        #expect(second.deviceCalls.allSatisfy { $0.foregroundInventory == nil })
        let ledger = try harness.store.read()
        #expect(ledger.pendingInventory == nil)
        #expect(ledger.unconfirmedStarts.isEmpty)
        harness.events.finish()
    }

    // MARK: Final review fixes

    /// A card this app requested has no server revision. It registers with
    /// the lifecycle the card controller published for it — so the server
    /// adopts it into the lifecycle this phone started and pushes the
    /// partner's changes to it — and a changed lifecycle is a newer version.
    @Test func anAppRequestedCardRegistersTheLifecycleTheControllerPublished() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 1)

        // What LiveActivityController publishes when it requests the card.
        harness.pushState.setReferences(.init(timerID: PushFixture.timerID, sessionID: nil), for: "own-card")
        harness.events.send(.activity(PushFixture.observation(id: "own-card", token: PushFixture.activityToken, generationID: nil)))
        await coordinator.settle(harness.events)
        let first = try #require(client.activityCalls.first)
        #expect(client.activityCalls.count == 1)
        #expect(first.timerID == PushFixture.timerID && first.sessionID == nil)
        #expect(first.knownGenerationID == nil && first.tokenVersion == 1)

        // The part stops: the controller shows the break on the same card.
        let session = UUID()
        harness.pushState.setReferences(.init(timerID: nil, sessionID: session), for: "own-card")
        await coordinator.waitUntilIdle()
        #expect(client.activityCalls.count == 2)
        let second = try #require(client.activityCalls.last)
        #expect(second.timerID == nil && second.sessionID == session && second.tokenVersion == 2)
        #expect(second.knownGenerationID == PushFixture.canonicalGenerationID)

        // The same lifecycle again is no change.
        harness.pushState.setReferences(.init(timerID: nil, sessionID: session), for: "own-card")
        await coordinator.reconcile()
        #expect(client.activityCalls.count == 2)
        harness.events.finish()
    }

    /// A card the server still refuses after this pass renewed the device
    /// registration is refused for itself: it waits for the next trigger,
    /// and every other card registers in the same pass.
    @Test func aCardRefusedAgainAfterRenewalWaitsWhileTheOthersRegister() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        client.onActivity = { registration, _ in
            if registration.activityID == "activity-0" { throw LiveActivityPushClientError.registrationDenied }
            return try FakePushClient.accept(registration, generation: PushFixture.canonicalGenerationID)
        }
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        harness.events.send(.activity(PushFixture.observation(id: "activity-0", token: PushFixture.activityToken)))
        harness.events.send(.activity(PushFixture.observation(id: "activity-1", token: "0123456789abcdee")))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        var ledger = try harness.store.read()
        #expect(ledger.activities["activity-1"]?.accepted == true)
        #expect(ledger.activities["activity-0"]?.accepted == false)
        #expect(ledger.device?.accepted == true)
        #expect(client.deviceCalls.count >= 2)

        // The next trigger tries the refused card again, at the same version.
        let refused = client.activityCalls.filter { $0.activityID == "activity-0" }
        let others = client.activityCalls.filter { $0.activityID == "activity-1" }.count
        await coordinator.reconcile()
        let again = client.activityCalls.filter { $0.activityID == "activity-0" }
        #expect(again.count > refused.count && again.allSatisfy { $0.tokenVersion == 1 })
        #expect(client.activityCalls.filter { $0.activityID == "activity-1" }.count == others)
        ledger = try harness.store.read()
        #expect(ledger.activities["activity-1"]?.accepted == true)
        harness.events.finish()
    }

    /// The server keeps one card per lifecycle — the registered card with the
    /// smallest activity ID — and ends the others itself. ActivityKit then
    /// reports the loser ended: that is the server's retirement, never the
    /// parent dismissing the lifecycle, so it is retired here, not reported.
    @Test func aDuplicateTheServerEndedIsRetiredNotReported() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        harness.events.send(.activity(PushFixture.observation(id: "activity-0", token: PushFixture.activityToken)))
        harness.events.send(.activity(PushFixture.observation(id: "activity-1", token: "0123456789abcdee")))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.activityCalls.map(\.activityID).sorted() == ["activity-0", "activity-1"])

        harness.events.send(.activity(PushFixture.observation(id: "activity-1", state: .ended)))
        await coordinator.settle(harness.events)
        await coordinator.reconcile()
        #expect(client.activityCalls.count == 2)
        #expect(try harness.store.read().retiredActivityIDs?.contains("activity-1") == true)

        // The survivor's own ending is the parent's, and is reported.
        harness.events.send(.activity(PushFixture.observation(id: "activity-0", state: .dismissed)))
        await coordinator.settle(harness.events)
        #expect(client.activityCalls.count == 3)
        #expect(client.activityCalls.last?.activityID == "activity-0" && client.activityCalls.last?.state == .dismissed)
        harness.events.finish()
    }

    // MARK: Refused removals

    /// A removal the server refuses outright — not a version conflict, not a
    /// denial, not a stopped worker, but a request it will never accept as
    /// sent — is finished here: the row is left to the server's own sweep.
    /// Carried on every later pass it would wedge this family and
    /// installation for good: no device registration, no card, until a
    /// receipt from another family purged it. (A stopped worker's 409 stays
    /// pending: `LiveActivityPushClientTests.aRemovalRefusedForAWithdrawnCapabilityStaysPending`.)
    @Test func aRefusedRemovalIsFinishedAndNeverWedgesLaterPasses() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        client.onUnregister = { _, _ in throw LiveActivityPushClientError.rejected("invalid_request") }
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        await coordinator.waitUntilIdle()

        harness.events.send(.activitiesEnabled(false))
        await coordinator.settle(harness.events)
        #expect(client.unregisterCalls.count == 1)
        #expect(try harness.store.read().pendingRemovals.isEmpty)

        // Live Activities back on: the refused removal is not sent again, and
        // the device registers with a newer version as if it had gone through.
        harness.events.send(.activitiesEnabled(true))
        await coordinator.settle(harness.events)
        #expect(client.unregisterCalls.count == 1)
        #expect(client.deviceCalls.count == 2)
        #expect(client.deviceCalls.last?.registrationVersion == 3)
        #expect(try harness.store.read().device?.accepted == true)
        harness.events.finish()
    }

    /// A removal the server could not take right now — unavailable — is a
    /// different matter: it stays pending and is retried on the next pass.
    @Test func anUnavailableServerKeepsTheRemovalPending() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        client.onUnregister = { _, _ in throw LiveActivityPushClientError.unavailable }
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        await coordinator.waitUntilIdle()

        harness.events.send(.activitiesEnabled(false))
        await coordinator.settle(harness.events)
        // The attempt and its whole retry schedule.
        #expect(client.unregisterCalls.count == 4)
        #expect(try harness.store.read().pendingRemovals.count == 1)

        client.onUnregister = { _, _ in }
        await coordinator.reconcile()
        #expect(client.unregisterCalls.count == 5)
        #expect(try harness.store.read().pendingRemovals.isEmpty)
        harness.events.finish()
    }

    /// A refusal with no envelope of the server's — a proxy's error page, a
    /// truncated body — is not this server's verdict on the removal. It stays
    /// pending like an unavailable server's, not written off as refused for
    /// good; the pass fails closed and the next trigger sends it again.
    @Test func aRefusalWithoutAServerCodeKeepsTheRemovalPending() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        client.onUnregister = { _, _ in throw LiveActivityPushClientError.rejected(nil) }
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        await coordinator.waitUntilIdle()

        harness.events.send(.activitiesEnabled(false))
        await coordinator.settle(harness.events)
        // Not retried within the pass (a refusal is not retryable), and not
        // finished either.
        #expect(client.unregisterCalls.count == 1)
        #expect(try harness.store.read().pendingRemovals.count == 1)

        client.onUnregister = { _, _ in }
        await coordinator.reconcile()
        #expect(client.unregisterCalls.count == 2)
        #expect(try harness.store.read().pendingRemovals.isEmpty)
        harness.events.finish()
    }

    // MARK: Removals while the server does not advertise push

    /// The built-in server stops advertising push — its worker stopped, push
    /// disabled — while a removal is still owed. The server honours a DELETE
    /// without push, so the removal goes out under this family's session; no
    /// device, inventory or card is registered until the capability is back,
    /// and then the device registers again with a newer version.
    @Test func aPendingRemovalIsSentWhileTheServerDoesNotAdvertisePush() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        client.onUnregister = { _, _ in throw LiveActivityPushClientError.unavailable }
        let coordinator = harness.coordinator(clients: [client])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        harness.events.send(.activitiesEnabled(false))
        await coordinator.settle(harness.events)
        #expect(client.unregisterCalls.count == 4)
        #expect(try harness.store.read().pendingRemovals.count == 1)

        // The server is back, without the capability.
        client.onUnregister = { _, _ in }
        await coordinator.start(context: PushFixture.context(pushAdvertised: false))
        await coordinator.waitUntilIdle()
        #expect(client.unregisterCalls.count == 5)
        #expect(client.unregisterCalls.last?.1 == 2)
        #expect(try harness.store.read().pendingRemovals.isEmpty)
        #expect(client.deviceCalls.count == 1)
        #expect(harness.pushState.mode == .withdrawn)

        // Live Activities back on, a rotated token: nothing registers while
        // the server is not pushing.
        harness.events.send(.activitiesEnabled(true))
        harness.events.send(.pushToStartToken(PushFixture.rotatedStartToken))
        harness.events.send(.activity(PushFixture.observation(token: PushFixture.activityToken)))
        await coordinator.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        #expect(client.activityCalls.isEmpty)
        #expect(client.unregisterCalls.count == 5)

        // Push advertised again: the same realm, a newer device version.
        await coordinator.start(context: PushFixture.context())
        await coordinator.waitUntilIdle()
        #expect(client.deviceCalls.count == 2)
        #expect(client.deviceCalls.last?.registrationVersion == 3)
        #expect(client.deviceCalls.last?.pushToStartToken == PushFixture.rotatedStartToken)
        #expect(client.activityCalls.count == 1)
        harness.events.finish()
    }

    /// The family changes while a push-less removal is on the wire: the old
    /// task records nothing once it has lost the realm, the removal stays the
    /// old family's, and the new family's session — also without push — sends
    /// nothing at all.
    @Test func aRealmSwitchDuringAPushlessRemovalUploadsNothingIntoTheNewRealm() async throws {
        let harness = PushHarness()
        let gate = Gate()
        let oldFamily = FakePushClient(familyID: PushFixture.familyID)
        oldFamily.onUnregister = { _, _ in await gate.waitUnlessCancelled() }
        let newFamily = FakePushClient(familyID: PushFixture.otherFamilyID)
        let coordinator = harness.coordinator(clients: [oldFamily, newFamily])
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await coordinator.start(context: PushFixture.context())
        await coordinator.settle(harness.events)
        #expect(oldFamily.deviceCalls.count == 1)

        // Push withdrawn and Live Activities switched off: the removal is
        // sent, and held on the wire.
        await coordinator.start(context: PushFixture.context(activitiesAllowed: false, pushAdvertised: false))
        await gate.waitForArrival()
        #expect(oldFamily.unregisterCalls.count == 1)

        await coordinator.start(context: PushFixture.context(familyID: PushFixture.otherFamilyID, pushAdvertised: false))
        await coordinator.waitUntilIdle()
        harness.events.send(.pushToStartToken(PushFixture.rotatedStartToken))
        await coordinator.settle(harness.events)
        #expect(newFamily.calls.isEmpty)
        #expect(oldFamily.unregisterCalls.count == 1)
        #expect(oldFamily.deviceCalls.count == 1)
        let ledger = try harness.store.read()
        #expect(ledger.pendingRemovals.map(\.familyID) == [PushFixture.familyID])
        #expect(ledger.device?.familyID == PushFixture.familyID)
        harness.events.finish()
    }
}
