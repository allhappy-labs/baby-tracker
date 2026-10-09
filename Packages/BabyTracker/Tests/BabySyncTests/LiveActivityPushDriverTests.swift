import BabyProtocol
import BabyStore
import Foundation
import Testing
@testable import BabySync

/// The launch-owned driver: it turns the restored account state into a push
/// context with no settings screen, no prompt and no scene involved.
@MainActor
@Suite(.timeLimit(.minutes(1)))
struct LiveActivityPushDriverTests {
    final class Probe: @unchecked Sendable {
        private let lock = NSLock()
        private var answers: [ServerInfo] = []
        private var count = 0
        private var failures = 0
        var gate: Gate?
        init(_ info: ServerInfo) { answers = [info] }
        func answer(_ info: ServerInfo) { lock.withLock { answers = [info] } }
        /// The next `count` probes fail in transport (no answer at all).
        func failNext(_ count: Int) { lock.withLock { failures = count } }
        var calls: Int { lock.withLock { count } }
        func callAsFunction(_ origin: ServerOrigin) async throws -> ServerInfo {
            let (info, gate, fails) = lock.withLock { () -> (ServerInfo, Gate?, Bool) in
                count += 1
                let fails = failures > 0
                if fails { failures -= 1 }
                return (answers[0], self.gate, fails)
            }
            await gate?.wait()
            if fails { throw URLError(.networkConnectionLost) }
            return info
        }
    }

    final class Settings {
        var mode: FamilySyncMode
        var endpoint: ConfirmedEndpoint?
        var feedMinutes = 30
        var environment: LiveActivityAPNsEnvironment? = .sandbox
        init(mode: FamilySyncMode, endpoint: ConfirmedEndpoint? = PushFixture.builtIn) {
            self.mode = mode
            self.endpoint = endpoint
        }
    }

    struct FixedEnvironment: LiveActivityAPNsEnvironmentResolving {
        let value: LiveActivityAPNsEnvironment?
        func environment() -> LiveActivityAPNsEnvironment? { value }
    }

    private func driver(_ harness: PushHarness, client: FakePushClient, settings: Settings, probe: Probe) -> LiveActivityPushDriver {
        LiveActivityPushDriver(
            coordinator: harness.coordinator(clients: [client]),
            builtIn: PushFixture.builtIn,
            mode: { settings.mode },
            endpoint: { settings.endpoint },
            probe: { try await probe($0) },
            activitiesAllowed: { harness.events.activitiesEnabled() },
            environment: FixedEnvironment(value: settings.environment),
            feedBreakMinutes: { settings.feedMinutes },
            sleep: { try await harness.sleeper.sleep($0) }
        )
    }

    // MARK: Transient probe failures

    @Test func aBackgroundWakeRetriesATransientProbeFailure() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let probe = Probe(PushFixture.serverInfo())
        probe.failNext(1)
        let driver = driver(harness, client: client, settings: Settings(mode: .shared(PushFixture.familyKey())), probe: probe)
        // A push-to-start wake: the new card's update token is waiting.
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        harness.events.send(.activity(PushFixture.observation(token: PushFixture.activityToken)))
        await driver.launch()
        #expect(probe.calls == 2)
        #expect(harness.sleeper.durations == [.seconds(2)])
        await driver.settle(harness.events)
        #expect(client.activityCalls.count == 1)
        harness.events.finish()
    }

    @Test func aForegroundProbeBlipKeepsTheHealthySession() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let probe = Probe(PushFixture.serverInfo())
        let driver = driver(harness, client: client, settings: Settings(mode: .shared(PushFixture.familyKey())), probe: probe)
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await driver.launch()
        await driver.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        await driver.coordinator.waitUntilIdle()

        probe.failNext(1)
        await driver.sceneBecameActive()
        #expect(driver.coordinator.context?.familyKey == PushFixture.familyKey())
        // The session proven earlier keeps working: the foreground replay ran.
        #expect(client.deviceCalls.map(\.registrationVersion) == [1, 1])
        #expect(harness.sleeper.durations.isEmpty)
        harness.events.send(.pushToStartToken(PushFixture.rotatedStartToken))
        await driver.settle(harness.events)
        #expect(client.deviceCalls.count == 3)
        harness.events.finish()
    }

    @Test func aTokenEventReevaluatesOnceTheProbeRetriesRanOut() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let probe = Probe(PushFixture.serverInfo())
        probe.failNext(4)
        let driver = driver(harness, client: client, settings: Settings(mode: .shared(PushFixture.familyKey())), probe: probe)
        await driver.launch()
        #expect(probe.calls == 4)
        #expect(harness.sleeper.durations == [.seconds(2), .seconds(5), .seconds(15)])
        #expect(driver.coordinator.context == nil)

        // The network is back; ActivityKit hands over a token.
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await driver.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        #expect(probe.calls == 5)
        harness.events.finish()
    }

    @Test func aDefinitiveAnswerIsNotReprobedOnEveryEvent() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let probe = Probe(PushFixture.serverInfo(capabilities: ["records-v1"]))
        let driver = driver(harness, client: client, settings: Settings(mode: .shared(PushFixture.familyKey())), probe: probe)
        await driver.launch()
        // A take-down-only session: the server answered, so nothing re-asks.
        #expect(driver.coordinator.context?.canRegister == false)
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        harness.events.send(.activity(PushFixture.observation(token: PushFixture.activityToken)))
        await driver.settle(harness.events)
        #expect(probe.calls == 1)
        #expect(harness.sleeper.durations.isEmpty)
        #expect(client.calls.isEmpty)
        harness.events.finish()
    }

    @Test func launchFromARestoredFamilySessionRegistersAutomatically() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let probe = Probe(PushFixture.serverInfo())
        let driver = driver(harness, client: client, settings: Settings(mode: .shared(PushFixture.familyKey())), probe: probe)
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await driver.launch()
        await driver.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        #expect(probe.calls == 1)
        #expect(driver.coordinator.context?.familyKey == PushFixture.familyKey())
        harness.events.finish()
    }

    @Test func everyModeWithoutASharedFamilyLeavesPushInert() async throws {
        let key = PushFixture.familyKey()
        let account = AccountSessionKey(canonicalOrigin: key.canonicalOrigin, serverInstanceID: key.serverInstanceID,
                                        accountID: key.accountID, deviceID: key.deviceID, recoveryEpoch: key.recoveryEpoch)
        for mode in [FamilySyncMode.localOnly, .endpointConfirmed(PushFixture.builtIn), .authenticated(account), .signedOut(account), .recovery(key)] {
            let harness = PushHarness()
            let client = FakePushClient()
            let probe = Probe(PushFixture.serverInfo())
            let driver = driver(harness, client: client, settings: Settings(mode: mode), probe: probe)
            harness.events.send(.pushToStartToken(PushFixture.startToken))
            await driver.launch()
            await driver.settle(harness.events)
            #expect(probe.calls == 0)
            #expect(client.calls.isEmpty)
            #expect(driver.coordinator.context == nil)
            harness.events.finish()
        }
    }

    @Test func aSelfHostedFamilyIsNeverProbedOrRegistered() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let selfHosted = ConfirmedEndpoint(origin: try ServerOrigin(validating: "https://tracker.home.example"), serverInstanceID: UUID())
        let key = FamilySessionKey(canonicalOrigin: selfHosted.origin.canonicalString, serverInstanceID: selfHosted.serverInstanceID,
                                   familyID: PushFixture.familyID, accountID: PushFixture.accountID, deviceID: PushFixture.deviceID, recoveryEpoch: 1)
        let probe = Probe(PushFixture.serverInfo(instanceID: selfHosted.serverInstanceID, origin: selfHosted.origin.canonicalString))
        let driver = driver(harness, client: client, settings: Settings(mode: .shared(key), endpoint: selfHosted), probe: probe)
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await driver.launch()
        await driver.sceneBecameActive()
        #expect(probe.calls == 0)
        #expect(client.calls.isEmpty)
        harness.events.finish()
    }

    @Test func aBuildWithoutAPushEnvironmentNeverRegisters() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let settings = Settings(mode: .shared(PushFixture.familyKey()))
        settings.environment = nil
        let probe = Probe(PushFixture.serverInfo())
        let driver = driver(harness, client: client, settings: settings, probe: probe)
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await driver.launch()
        #expect(probe.calls == 0)
        #expect(client.calls.isEmpty)
        harness.events.finish()
    }

    @Test func systemDisabledActivitiesNeverRegister() async throws {
        let harness = PushHarness()
        harness.events.setEnabled(false)
        let client = FakePushClient()
        let probe = Probe(PushFixture.serverInfo())
        let driver = driver(harness, client: client, settings: Settings(mode: .shared(PushFixture.familyKey())), probe: probe)
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await driver.launch()
        await driver.settle(harness.events)
        await driver.coordinator.waitUntilIdle()
        #expect(client.deviceCalls.isEmpty)
        #expect(driver.coordinator.context?.canRegister == false)
        harness.events.finish()
    }

    @Test func signingOutStopsFurtherRegistration() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let key = PushFixture.familyKey()
        let settings = Settings(mode: .shared(key))
        let probe = Probe(PushFixture.serverInfo())
        let driver = driver(harness, client: client, settings: settings, probe: probe)
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await driver.launch()
        await driver.settle(harness.events)
        #expect(client.deviceCalls.count == 1)

        settings.mode = .signedOut(key.accountSessionKey)
        await driver.modeChanged()
        #expect(driver.coordinator.context == nil)
        harness.events.send(.pushToStartToken(PushFixture.rotatedStartToken))
        await driver.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        harness.events.finish()
    }

    @Test func aModeChangeDuringTheProbeDiscardsTheStaleAnswer() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let key = PushFixture.familyKey()
        let settings = Settings(mode: .shared(key))
        let probe = Probe(PushFixture.serverInfo())
        let gate = Gate()
        probe.gate = gate
        let driver = driver(harness, client: client, settings: settings, probe: probe)
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        let launching = Task { await driver.launch() }
        await gate.waitForArrival()
        settings.mode = .signedOut(key.accountSessionKey)
        await driver.modeChanged()
        gate.open()
        await launching.value
        await driver.settle(harness.events)
        #expect(driver.coordinator.context == nil)
        #expect(client.calls.isEmpty)
        harness.events.finish()
    }

    @Test func foregroundReprobesAndAWithdrawnCapabilityStopsWithoutUnregistering() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let probe = Probe(PushFixture.serverInfo())
        let driver = driver(harness, client: client, settings: Settings(mode: .shared(PushFixture.familyKey())), probe: probe)
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await driver.launch()
        await driver.settle(harness.events)
        #expect(client.deviceCalls.count == 1)
        await driver.coordinator.waitUntilIdle()

        await driver.sceneBecameActive()
        #expect(probe.calls == 2)
        // The foreground replay re-sends the accepted state.
        #expect(client.deviceCalls.map(\.registrationVersion) == [1, 1])

        probe.answer(PushFixture.serverInfo(capabilities: ["records-v1"]))
        await driver.sceneBecameActive()
        // The session stays, able only to take down; the cards go back to the app.
        #expect(driver.coordinator.context?.canRegister == false)
        #expect(harness.pushState.mode == .withdrawn)
        // A worker restart on the server is transient: nothing is torn down.
        #expect(client.unregisterCalls.isEmpty)
        #expect(client.deviceCalls.count == 2)
        harness.events.finish()
    }

    @Test func aChangedFeedBreakIsObservedAndRegistered() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let settings = Settings(mode: .shared(PushFixture.familyKey()))
        let probe = Probe(PushFixture.serverInfo())
        let driver = driver(harness, client: client, settings: settings, probe: probe)
        let center = NotificationCenter()
        driver.observe(center, becameActive: nil, preferencesChanged: UserDefaults.didChangeNotification)
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await driver.launch()
        await driver.settle(harness.events)
        #expect(client.deviceCalls.count == 1)

        settings.feedMinutes = 55
        center.post(name: UserDefaults.didChangeNotification, object: nil)
        await driver.settle(harness.events)
        #expect(client.deviceCalls.count == 2)
        #expect(client.deviceCalls.last?.feedBreakMinutes == 55)
        // An unrelated defaults change is not a registration.
        center.post(name: UserDefaults.didChangeNotification, object: nil)
        await driver.settle(harness.events)
        #expect(client.deviceCalls.count == 2)
        harness.events.finish()
    }

    /// The family changes while a probe is in flight. The evaluation that
    /// started it no longer owns the outcome: it stops the old session but
    /// says nothing about push for the new family, which stays unknown —
    /// not withdrawn — until its own probe answers.
    @Test func aModeChangeDuringTheProbeLeavesPushUnknownNotWithdrawn() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let probe = Probe(PushFixture.serverInfo())
        let gate = Gate()
        probe.gate = gate
        let settings = Settings(mode: .shared(PushFixture.familyKey()))
        let driver = driver(harness, client: client, settings: settings, probe: probe)
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        let launch = Task { await driver.launch() }
        await gate.waitForArrival()

        settings.mode = .shared(PushFixture.familyKey(familyID: PushFixture.otherFamilyID))
        gate.open()
        await launch.value
        #expect(harness.pushState.mode == .unknown)
        harness.events.finish()
    }

    // MARK: Cancelled background evaluations

    /// Background execution that runs out while the work is in flight: the
    /// first `count` evaluations are cancelled once their probe is on the
    /// wire, the way iOS ends a background task's time.
    final class CancellingExecution: LiveActivityPushBackgroundExecution, @unchecked Sendable {
        private let lock = NSLock()
        private var remaining: Int
        let probeGate: Gate

        init(cancelling count: Int, whileProbing probeGate: Gate) {
            remaining = count
            self.probeGate = probeGate
        }

        @MainActor func perform(_ work: @escaping @MainActor () async -> Void) async {
            let cancels = lock.withLock { () -> Bool in
                guard remaining > 0 else { return false }
                remaining -= 1
                return true
            }
            guard cancels else { await work(); return }
            let running = Task { @MainActor in await work() }
            await probeGate.waitForArrival()
            running.cancel()
            probeGate.open()
            await running.value
        }
    }

    /// Background time runs out during the launch probe. The evaluation was
    /// cancelled and decided nothing — exactly like a probe that got no
    /// answer — so the next ActivityKit event must evaluate again rather than
    /// leave push inert until the next launch or foreground.
    @Test func aCancelledBackgroundEvaluationReprobesOnTheNextEvent() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let probe = Probe(PushFixture.serverInfo())
        let gate = Gate()
        probe.gate = gate
        let settings = Settings(mode: .shared(PushFixture.familyKey()))
        let driver = LiveActivityPushDriver(
            coordinator: harness.coordinator(clients: [client]),
            builtIn: PushFixture.builtIn,
            mode: { settings.mode },
            endpoint: { settings.endpoint },
            probe: { try await probe($0) },
            activitiesAllowed: { harness.events.activitiesEnabled() },
            environment: FixedEnvironment(value: settings.environment),
            feedBreakMinutes: { settings.feedMinutes },
            background: CancellingExecution(cancelling: 1, whileProbing: gate),
            sleep: { try await harness.sleeper.sleep($0) }
        )
        await driver.launch()
        #expect(probe.calls == 1)
        #expect(driver.coordinator.context == nil)
        #expect(harness.sleeper.durations.isEmpty)

        // A push-to-start token arrives: the new card's registration is waiting.
        probe.gate = nil
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await driver.settle(harness.events)
        #expect(probe.calls == 2)
        #expect(driver.coordinator.context?.familyKey == PushFixture.familyKey())
        #expect(client.deviceCalls.count == 1)
        harness.events.finish()
    }

    // MARK: Removals while the server does not advertise push

    /// The server's push worker is stopped when the user switches Live
    /// Activities off. The server still honours the DELETE, so the registration
    /// comes down — exactly one removal, no registration — instead of staying
    /// on the server until push happens to return.
    @Test func turningLiveActivitiesOffWhilePushIsUnavailableTakesTheRegistrationDown() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let probe = Probe(PushFixture.serverInfo())
        let driver = driver(harness, client: client, settings: Settings(mode: .shared(PushFixture.familyKey())), probe: probe)
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await driver.launch()
        await driver.settle(harness.events)
        #expect(client.deviceCalls.count == 1)

        probe.answer(PushFixture.serverInfo(capabilities: ["records-v1"]))
        await driver.sceneBecameActive()
        #expect(driver.coordinator.context?.canRegister == false)
        #expect(harness.pushState.mode == .withdrawn)
        #expect(client.calls.count == 1)

        harness.events.setEnabled(false)
        harness.events.send(.activitiesEnabled(false))
        await driver.settle(harness.events)
        let installation = try #require(try harness.store.read().installationID)
        #expect(client.unregisterCalls.count == 1)
        #expect(client.unregisterCalls.first?.0 == installation)
        #expect(client.unregisterCalls.first?.1 == 2)
        #expect(client.deviceCalls.count == 1)
        #expect(try harness.store.read().pendingRemovals.isEmpty)

        // Later triggers owe nothing more.
        await driver.sceneBecameActive()
        harness.events.send(.pushToStartToken(PushFixture.rotatedStartToken))
        await driver.settle(harness.events)
        #expect(client.calls.count == 2)
        harness.events.finish()
    }

    /// Without the capability and with nothing owed, the built-in server is
    /// never contacted — not for a token, a card, or Live Activities going off
    /// on a device that was never registered.
    @Test func aServerWithoutPushAndNothingOwedIsNeverContacted() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let probe = Probe(PushFixture.serverInfo(capabilities: ["records-v1"]))
        let driver = driver(harness, client: client, settings: Settings(mode: .shared(PushFixture.familyKey())), probe: probe)
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await driver.launch()
        await driver.settle(harness.events)
        #expect(driver.coordinator.context?.canRegister == false)
        #expect(harness.pushState.mode == .withdrawn)

        harness.events.send(.activity(PushFixture.observation(token: PushFixture.activityToken)))
        harness.events.setEnabled(false)
        harness.events.send(.activitiesEnabled(false))
        await driver.sceneBecameActive()
        await driver.settle(harness.events)
        #expect(probe.calls == 2)
        #expect(client.calls.isEmpty)
        #expect(try harness.store.read().pendingRemovals.isEmpty)
        harness.events.finish()
    }

    /// A self-hosted realm whose ledger still owes a removal: nothing is
    /// probed and nothing is sent, to it or to the built-in server.
    @Test func aSelfHostedFamilyWithARemovalOwedSendsNothingAnywhere() async throws {
        let harness = PushHarness()
        let client = FakePushClient()
        let selfHosted = ConfirmedEndpoint(origin: try ServerOrigin(validating: "https://tracker.home.example"), serverInstanceID: UUID())
        let key = FamilySessionKey(canonicalOrigin: selfHosted.origin.canonicalString, serverInstanceID: selfHosted.serverInstanceID,
                                   familyID: PushFixture.familyID, accountID: PushFixture.accountID, deviceID: PushFixture.deviceID, recoveryEpoch: 1)
        let installation = try harness.store.bind(LiveActivityRegistrationIdentity(familyKey: key))
        try harness.store.update { ledger in
            ledger.registrationVersion = 2
            ledger.pendingRemovals = [.init(familyID: PushFixture.familyID, installationID: installation, version: 2)]
        }
        let probe = Probe(PushFixture.serverInfo(
            instanceID: selfHosted.serverInstanceID, origin: selfHosted.origin.canonicalString, capabilities: ["records-v1"]
        ))
        let driver = driver(harness, client: client, settings: Settings(mode: .shared(key), endpoint: selfHosted), probe: probe)
        harness.events.send(.pushToStartToken(PushFixture.startToken))
        await driver.launch()
        await driver.sceneBecameActive()
        await driver.settle(harness.events)
        #expect(probe.calls == 0)
        #expect(client.calls.isEmpty)
        #expect(driver.coordinator.context == nil)
        #expect(try harness.store.read().pendingRemovals.count == 1)
        harness.events.finish()
    }
}
