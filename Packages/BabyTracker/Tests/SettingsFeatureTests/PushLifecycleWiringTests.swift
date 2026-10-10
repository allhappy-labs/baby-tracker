import BabyCore
import BabyStore
import BabySync
import Foundation
import Testing
@testable import SettingsFeature

/// What the model told the push registrar, in order.
@MainActor
final class PushLifecycleRecorder {
    private(set) var events: [String] = []
    private(set) var keys: [FamilySessionKey] = []
    /// How often the model said an upload is owed (forget the fingerprint).
    private(set) var invalidations = 0
    /// While true, an activation records itself and then waits, like an
    /// upload stuck on the network, until `releaseActivations()`.
    var holdsActivations = false
    private var held: [CheckedContinuation<Void, Never>] = []

    var lifecycle: PushLifecycle {
        PushLifecycle(
            activated: { [self] key, _, activation in
                keys.append(key)
                events.append("activated(\(activation))")
                if holdsActivations { await withCheckedContinuation { held.append($0) } }
            },
            ending: { [self] in events.append("ending") },
            invalidate: { [self] in invalidations += 1 }
        )
    }

    func releaseActivations() {
        holdsActivations = false
        let waiting = held
        held = []
        for continuation in waiting { continuation.resume() }
    }
}

/// Polls `condition` on the main actor until it holds or `limit` passes.
@MainActor
private func eventually(within limit: Duration = .seconds(2), _ condition: () -> Bool) async -> Bool {
    let end = ContinuousClock.now + limit
    while !condition() {
        if ContinuousClock.now >= end { return false }
        try? await Task.sleep(for: .milliseconds(10))
    }
    return true
}

/// When the model hands the push registrar a session, and how.
@MainActor
@Suite("Push lifecycle wiring")
struct PushLifecycleWiringTests {
    private static func inFamily(
        _ recorder: EngineFactoryRecorder, push: PushLifecycleRecorder
    ) async throws -> FamilySettingsFixture {
        let fixture = try FamilySettingsFixture.make(familyInBundle: false, makeEngine: recorder.factory)
        fixture.model.pushLifecycle = push.lifecycle
        try await fixture.model.signInWithApple()
        try await fixture.model.createFamily(name: "Family")
        await fixture.model.pushSettled()
        return fixture
    }

    /// Creating (or joining) uploads whatever the fingerprint says: the
    /// server's sweep may have dropped this device's token with its old session.
    @Test func enteringAFamilyActivatesPushAsSignedIn() async throws {
        let recorder = EngineFactoryRecorder()
        let push = PushLifecycleRecorder()
        _ = try await Self.inFamily(recorder, push: push)

        #expect(push.events == ["activated(signedIn)"])
        #expect(push.keys.first?.familyID == FamilySettingsFixture.familyID)
    }

    /// A launch only compares fingerprints; a re-authentication uploads again.
    @Test func aLaunchIsRestoredAndASignInAgainIsSignedIn() async throws {
        let recorder = EngineFactoryRecorder()
        let push = PushLifecycleRecorder()
        let fixture = try await Self.inFamily(recorder, push: push)

        let relaunchedPush = PushLifecycleRecorder()
        let coordinator = AccountCoordinator(
            store: fixture.store, keychain: fixture.keychain, identity: fixture.identity,
            apple: fixture.appleClient, credentialState: fixture.credentialStateClient, defaults: fixture.defaults
        )
        let relaunched = FamilySettingsViewModel(
            store: fixture.store, coordinator: coordinator, client: fixture.client,
            defaults: fixture.defaults, makeEngine: EngineFactoryRecorder().factory
        )
        relaunched.pushLifecycle = relaunchedPush.lifecycle
        relaunched.restoreSession()
        await relaunched.startSyncing()
        await relaunched.pushSettled()
        #expect(relaunchedPush.events == ["activated(restored)"])

        // A re-authentication: Apple again, into the same family.
        fixture.identity.bundle = SessionBundle(
            accountID: FamilySettingsFixture.accountID, deviceID: FamilySettingsFixture.deviceID,
            recoveryEpoch: 0, accessToken: "fresh-access", refreshToken: "fresh-refresh",
            expiresAt: Date(timeIntervalSince1970: 20_000),
            deviceCredential: "device-credential", familyID: FamilySettingsFixture.familyID
        )
        try await relaunched.signInWithApple()
        await relaunched.pushSettled()
        #expect(relaunchedPush.events == ["activated(restored)", "activated(signedIn)"])
    }

    /// An activation that failed owes the sign-in upload to the one that succeeds.
    @Test func aFailedActivationKeepsTheSignInUploadOwed() async throws {
        let recorder = EngineFactoryRecorder()
        recorder.nextActivationError = URLError(.notConnectedToInternet)
        let push = PushLifecycleRecorder()
        let fixture = try FamilySettingsFixture.make(familyInBundle: false, makeEngine: recorder.factory)
        fixture.model.pushLifecycle = push.lifecycle
        try await fixture.model.signInWithApple()
        try await fixture.model.createFamily(name: "Family")
        await fixture.model.pushSettled()
        #expect(push.events.isEmpty)

        recorder.last?.activationError = nil
        await fixture.model.sceneBecameActive()
        await fixture.model.pushSettled()
        #expect(push.events == ["activated(signedIn)"])
    }

    /// Leaving and disconnecting delete the token while the session can still
    /// authenticate the request: before the engine stops and the server is told.
    @Test func leavingAndDisconnectingEndPushBeforeTheEngineStops() async throws {
        for leave in [true, false] {
            let recorder = EngineFactoryRecorder()
            let push = PushLifecycleRecorder()
            let fixture = try await Self.inFamily(recorder, push: push)
            let engine = try #require(recorder.last)
            var eventsAtStop: [String] = []
            engine.onStop = { eventsAtStop = push.events }

            if leave { try await fixture.model.leaveFamily() } else { try await fixture.model.disconnectCurrentFamily() }

            #expect(eventsAtStop == ["activated(signedIn)", "ending"])
            #expect(push.events == ["activated(signedIn)", "ending"])
        }
    }

    /// An upload stuck on the network must not hold up leaving. The registrar
    /// bounds the DELETE, and waits for an upload in flight within that bound.
    @Test func leavingDoesNotWaitForAStuckActivation() async throws {
        let push = PushLifecycleRecorder()
        push.holdsActivations = true
        let fixture = try FamilySettingsFixture.make(familyInBundle: false, makeEngine: EngineFactoryRecorder().factory)
        fixture.model.pushLifecycle = push.lifecycle
        try await fixture.model.signInWithApple()
        try await fixture.model.createFamily(name: "Family")
        #expect(await eventually { push.events == ["activated(signedIn)"] })

        let leaving = Task { try await fixture.model.leaveFamily() }
        let endedWhileStuck = await eventually { push.events.contains("ending") }
        push.releaseActivations()
        try await leaving.value
        await fixture.model.pushSettled()

        #expect(endedWhileStuck)
        #expect(push.events == ["activated(signedIn)", "ending"])
    }

    /// An activation still queued behind a stuck one when the session ends is
    /// dropped: it must not hand the registrar a session that just ended.
    @Test func anActivationQueuedWhenPushEndsIsDropped() async throws {
        let push = PushLifecycleRecorder()
        push.holdsActivations = true
        let fixture = try FamilySettingsFixture.make(familyInBundle: false, makeEngine: EngineFactoryRecorder().factory)
        fixture.model.pushLifecycle = push.lifecycle
        try await fixture.model.signInWithApple()
        try await fixture.model.createFamily(name: "Family")
        #expect(await eventually { push.events == ["activated(signedIn)"] })
        // A temporary stop restarts the engine: its activation queues behind
        // the stuck one.
        _ = try await fixture.model.refreshAppleCredentialState()

        let leaving = Task { try await fixture.model.leaveFamily() }
        #expect(await eventually { push.events.contains("ending") })
        push.releaseActivations()
        try await leaving.value
        await fixture.model.pushSettled()

        #expect(push.events == ["activated(signedIn)", "ending"])
    }

    /// The same store and Keychain, a new process: what `AppEnvironment`
    /// builds at launch, before any scene exists.
    private static func relaunch(
        _ fixture: FamilySettingsFixture, engines: EngineFactoryRecorder, push: PushLifecycleRecorder
    ) -> FamilySettingsViewModel {
        let coordinator = AccountCoordinator(
            store: fixture.store, keychain: fixture.keychain, identity: fixture.identity,
            apple: fixture.appleClient, credentialState: fixture.credentialStateClient, defaults: fixture.defaults
        )
        let model = FamilySettingsViewModel(
            store: fixture.store, coordinator: coordinator, client: fixture.client,
            defaults: fixture.defaults, makeEngine: engines.factory
        )
        model.pushLifecycle = push.lifecycle
        return model
    }

    /// A push to an app the system relaunched in the background. The launch
    /// restored the session, but no scene connected, so nothing called
    /// `startSyncing`. The push does the one pull, an activation because none
    /// ran in this process, and starts no foreground polling.
    @Test func aPushBeforeAnySceneActivatesOnceWithoutPolling() async throws {
        let fixture = try await Self.inFamily(EngineFactoryRecorder(), push: PushLifecycleRecorder())
        let engines = EngineFactoryRecorder()
        let push = PushLifecycleRecorder()
        let model = Self.relaunch(fixture, engines: engines, push: push)
        model.restoreSession()
        let engine = try #require(engines.last)
        engine.onActivation = { [unowned engine] in engine.status = .upToDate(Date()) }
        let wake = BackgroundWakeHandler(sync: { await model.syncNowForBackgroundPush() })

        #expect(await wake.receiveSilentPush() == .newData)
        #expect(engine.calls == ["activateAsOwner"])
        await model.pushSettled()
        #expect(push.events == ["activated(restored)"])

        // Activated now: the next push is an ordinary background sync.
        #expect(await wake.receiveSilentPush() == .newData)
        #expect(engine.calls == ["activateAsOwner", "syncNow(backgroundPush)"])
    }

    /// A cold push's own activation finds this device removed. The realm is
    /// purged and push ended inside the engine call, which still returns
    /// normally. The removed session must not be handed back to push after
    /// the ending.
    @Test func aRemovalInsideAnActivationIsNotHandedBackToPush() async throws {
        let fixture = try await Self.inFamily(EngineFactoryRecorder(), push: PushLifecycleRecorder())
        let engines = EngineFactoryRecorder()
        let push = PushLifecycleRecorder()
        let model = Self.relaunch(fixture, engines: engines, push: push)
        model.restoreSession()
        let engine = try #require(engines.last)
        engine.onActivation = { [unowned model] in model.membershipEndedBySync() }

        _ = await model.syncNowForBackgroundPush()
        await model.pushSettled()

        #expect(engine.calls == ["activateAsOwner"])
        #expect(push.events == ["ending"])
    }

    /// A launch after such a removal restores the Keychain session of an
    /// account the store has marked signed out. The engine declines to sync
    /// and its activation returns normally; push is not handed the session.
    @Test func aSignedOutAccountIsNotHandedToPushAtLaunch() async throws {
        let fixture = try await Self.inFamily(EngineFactoryRecorder(), push: PushLifecycleRecorder())
        guard case .shared(let family) = fixture.model.mode else { Issue.record("fixture family required"); return }
        try fixture.store.markAccountSignedOut(family.leaseKey)
        let engines = EngineFactoryRecorder()
        let push = PushLifecycleRecorder()
        let model = Self.relaunch(fixture, engines: engines, push: push)

        model.restoreSession()
        await model.startSyncing()
        await model.pushSettled()

        #expect(engines.last?.calls.contains("activateAsOwner") == true)
        #expect(push.events.isEmpty)
    }

    /// Ordinary sync finding this device removed ends push through the
    /// model, in its order: an activation still queued is dropped rather than
    /// handing the registrar the removed session again.
    @Test func anEngineDetectedRemovalEndsPushThroughTheModel() async throws {
        let push = PushLifecycleRecorder()
        push.holdsActivations = true
        let fixture = try FamilySettingsFixture.make(familyInBundle: false, makeEngine: EngineFactoryRecorder().factory)
        fixture.model.pushLifecycle = push.lifecycle
        try await fixture.model.signInWithApple()
        try await fixture.model.createFamily(name: "Family")
        #expect(await eventually { push.events == ["activated(signedIn)"] })
        // A restart's activation queues behind the stuck one.
        _ = try await fixture.model.refreshAppleCredentialState()

        let removed = FamilySyncComposition.revocationHandler(
            codeStoreBackend: fixture.backend, model: FamilyModelReference(fixture.model)
        )
        await removed(fixture.store.activeRealm)
        #expect(await eventually { push.events.contains("ending") })
        push.releaseActivations()
        await fixture.model.pushSettled()

        #expect(push.events == ["activated(signedIn)", "ending"])
    }

    /// The sign-in upload is owed from the moment the model decides it. The
    /// fingerprint is forgotten then, not when the activation finally runs:
    /// an app killed after a failed activation still uploads at next launch.
    @Test func enteringAFamilyForgetsTheFingerprintBeforeActivating() async throws {
        let recorder = EngineFactoryRecorder()
        recorder.nextActivationError = URLError(.notConnectedToInternet)
        let push = PushLifecycleRecorder()
        let fixture = try FamilySettingsFixture.make(familyInBundle: false, makeEngine: recorder.factory)
        fixture.model.pushLifecycle = push.lifecycle
        try await fixture.model.signInWithApple()
        #expect(push.invalidations == 0)

        try await fixture.model.createFamily(name: "Family")
        await fixture.model.pushSettled()

        #expect(push.events.isEmpty)
        #expect(push.invalidations == 1)
    }

    /// `withSyncStopped`'s temporary stop is not a sign-out.
    @Test func aTemporaryStopDoesNotEndPush() async throws {
        let recorder = EngineFactoryRecorder()
        let push = PushLifecycleRecorder()
        let fixture = try await Self.inFamily(recorder, push: push)

        _ = try await fixture.model.refreshAppleCredentialState()
        await fixture.model.pushSettled()

        #expect(recorder.built.count == 2)
        #expect(push.events == ["activated(signedIn)", "activated(restored)"])
    }

    /// Apple revoking the credential ends the family on this phone: the
    /// registrar forgets the session, and nothing activates it again.
    @Test func aRevokedAppleCredentialEndsPush() async throws {
        let recorder = EngineFactoryRecorder()
        let push = PushLifecycleRecorder()
        let fixture = try await Self.inFamily(recorder, push: push)
        fixture.credentialStateClient.nextState = .revoked

        _ = try await fixture.model.refreshAppleCredentialState()
        await fixture.model.pushSettled()

        #expect(push.events == ["activated(signedIn)", "ending"])
    }

    /// A background push is one `.backgroundPush` sync, answered by whether
    /// the engine ended up to date.
    @Test func aBackgroundPushRunsOneBackgroundSync() async throws {
        let recorder = EngineFactoryRecorder()
        let fixture = try await Self.inFamily(recorder, push: PushLifecycleRecorder())
        let engine = try #require(recorder.last)
        let before = engine.calls.count

        engine.status = .upToDate(Date())
        #expect(await fixture.model.syncNowForBackgroundPush())
        engine.status = .offline
        #expect(await fixture.model.syncNowForBackgroundPush() == false)

        #expect(Array(engine.calls.dropFirst(before)) == ["syncNow(backgroundPush)", "syncNow(backgroundPush)"])
    }
}
