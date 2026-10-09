import BabyCore
import BabyStore
import BabySync
import Foundation
import OSLog
import Testing
@testable import SettingsFeature

/// Records what the app asked of the engine, in order, so the tests can prove
/// both *which* activation ran and that polling started before it.
@MainActor
final class FakeSyncEngine: FamilySyncDriving {
    var status: FamilySyncEngine.Status = .idle
    var operationalStatus: SyncOperationalStatus = .inactive
    var activationError: (any Error)?
    var onActivation: () -> Void = {}
    private(set) var calls: [String] = []

    func activateAsOwner() async throws {
        calls.append("activateAsOwner")
        onActivation()
        if let activationError { throw activationError }
    }

    func activateAsJoiner() async throws {
        calls.append("activateAsJoiner")
        onActivation()
        if let activationError { throw activationError }
    }

    func syncNow(_ reason: SyncReason) async { calls.append("syncNow(\(reason))") }
    func localWriteHappened() { calls.append("localWriteHappened") }
    func startForegroundPolling() { calls.append("startPolling") }
    func stopForegroundPolling() { calls.append("stopPolling") }
    /// Runs inside `stop()`, so a test can see what had already happened.
    var onStop: () -> Void = {}
    func stop() async {
        onStop()
        calls.append("stop")
    }
}

/// Every engine the model asked for, and the key it asked with.
@MainActor
final class EngineFactoryRecorder {
    private(set) var built: [(key: FamilySessionKey, engine: FakeSyncEngine)] = []
    var nextActivationError: (any Error)?
    var nextOnActivation: () -> Void = {}

    var factory: FamilySyncEngineFactory {
        { [self] key, _ in
            let engine = FakeSyncEngine()
            engine.activationError = nextActivationError
            engine.onActivation = nextOnActivation
            built.append((key, engine))
            return engine
        }
    }

    var last: FakeSyncEngine? { built.last?.engine }
}

@MainActor
final class StartedActivities {
    var ids: [UUID] = []
}

@MainActor
@Suite("Family sync wiring")
struct FamilySyncWiringTests {
    private static func signedInWithoutFamily(
        _ recorder: EngineFactoryRecorder,
        startLiveActivity: @escaping @MainActor (ActiveTimer, BabyStore) -> Void = { _, _ in }
    ) async throws -> FamilySettingsFixture {
        let fixture = try FamilySettingsFixture.make(
            familyInBundle: false, makeEngine: recorder.factory, startLiveActivity: startLiveActivity
        )
        try await fixture.model.signInWithApple()
        return fixture
    }

    /// A cold launch: a new coordinator and model over the same store,
    /// Keychain and App Group defaults.
    private static func relaunch(
        _ fixture: FamilySettingsFixture, recorder: EngineFactoryRecorder
    ) -> FamilySettingsViewModel {
        let coordinator = AccountCoordinator(
            store: fixture.store, keychain: fixture.keychain, identity: fixture.identity,
            apple: fixture.appleClient, credentialState: fixture.credentialStateClient,
            defaults: fixture.defaults
        )
        return FamilySettingsViewModel(
            store: fixture.store, coordinator: coordinator, client: fixture.client,
            defaults: fixture.defaults, makeEngine: recorder.factory
        )
    }

    private struct Offline: Error {}

    // MARK: Entering a family

    @Test func creatingAFamilyActivatesAsOwner() async throws {
        let recorder = EngineFactoryRecorder()
        let fixture = try await Self.signedInWithoutFamily(recorder)

        try await fixture.model.createFamily(name: "Family")

        #expect(recorder.built.count == 1)
        #expect(recorder.built.first?.key.familyID == FamilySettingsFixture.familyID)
        #expect(recorder.last?.calls.contains("activateAsOwner") == true)
        #expect(recorder.last?.calls.contains("activateAsJoiner") == false)
        recorder.last?.operationalStatus = SyncOperationalStatus(
            activity: .active, signIn: .available, live: .dialing, phase: .request
        )
        let asProtocol: any FamilySyncSettingsModel = fixture.model
        #expect(asProtocol.syncOperationalStatus.activity == .active)
        #expect(asProtocol.syncOperationalStatus.live == .dialing)
    }

    @Test func acceptingAnInvitationActivatesAsJoiner() async throws {
        let recorder = EngineFactoryRecorder()
        let fixture = try await Self.signedInWithoutFamily(recorder)

        try await fixture.model.acceptInvitation(
            InvitationIntent(origin: FamilySettingsFixture.origin, token: "abcdefghijklmnop0123456789")
        )

        #expect(recorder.last?.calls.contains("activateAsJoiner") == true)
        #expect(recorder.last?.calls.contains("activateAsOwner") == false)
    }

    /// The server already made this device a member. An activation that
    /// fails — offline, a server hiccup — shows in the status line and is
    /// tried again; it never throws the membership away.
    @Test func failedActivationKeepsMembershipAndRetriesOnForeground() async throws {
        let recorder = EngineFactoryRecorder()
        recorder.nextActivationError = Offline()
        let fixture = try await Self.signedInWithoutFamily(recorder)

        try await fixture.model.createFamily(name: "Family")
        #expect(fixture.model.hasFamily)

        recorder.last?.activationError = nil
        await fixture.model.sceneBecameActive()

        #expect(recorder.last?.calls.filter { $0 == "activateAsOwner" }.count == 2)
    }

    /// A failed activation or member reload is kept off the caller's error
    /// path on purpose, which made it invisible: nothing said why a phone was
    /// not syncing. Each now leaves a line in the sync log — the kind of
    /// failure only, nothing about the family.
    @Test func swallowedActivationAndReloadFailuresAreLogged() async throws {
        let since = Date()
        let recorder = EngineFactoryRecorder()
        recorder.nextActivationError = Offline()
        let fixture = try await Self.signedInWithoutFamily(recorder)
        try await fixture.model.createFamily(name: "Family")
        fixture.client.profileError = Offline()

        await fixture.model.startSyncing()

        let log = try OSLogStore(scope: .currentProcessIdentifier)
        let lines = try log.getEntries(at: log.position(date: since))
            .compactMap { $0 as? OSLogEntryLog }
            .filter { $0.subsystem == AppLog.subsystem && $0.category == "sync" }
            .map(\.composedMessage)
        #expect(lines.contains { $0.contains("family activation failed") })
        #expect(lines.contains { $0.contains("family reload failed") })
    }

    // MARK: Relaunch

    /// The role the device *entered* with decides the activation, not the
    /// server role: an owner's import must resume on every launch until it
    /// completes, and polling must be running before that first sync so a
    /// remote timer it pulls gets its Lock Screen card.
    @Test func relaunchedOwnerResumesAsOwnerWithPollingFirst() async throws {
        let recorder = EngineFactoryRecorder()
        let fixture = try await Self.signedInWithoutFamily(recorder)
        try await fixture.model.createFamily(name: "Family")

        let relaunched = EngineFactoryRecorder()
        let model = Self.relaunch(fixture, recorder: relaunched)
        model.restoreSession()
        await model.startSyncing()

        #expect(model.hasFamily)
        #expect(relaunched.built.count == 1)
        let calls = relaunched.last?.calls ?? []
        #expect(calls.first == "startPolling")
        #expect(calls.contains("activateAsOwner"))
    }

    @Test func relaunchedJoinerResumesAsJoiner() async throws {
        let recorder = EngineFactoryRecorder()
        let fixture = try await Self.signedInWithoutFamily(recorder)
        try await fixture.model.acceptInvitation(
            InvitationIntent(origin: FamilySettingsFixture.origin, token: "abcdefghijklmnop0123456789")
        )

        let relaunched = EngineFactoryRecorder()
        let model = Self.relaunch(fixture, recorder: relaunched)
        model.restoreSession()
        await model.startSyncing()

        #expect(relaunched.last?.calls == ["startPolling", "activateAsJoiner"])
    }

    /// No family: nothing is built, nothing polls, nothing talks to a server.
    @Test func localOnlyLaunchBuildsNoEngine() async throws {
        let recorder = EngineFactoryRecorder()
        let fixture = try FamilySettingsFixture.make(familyInBundle: false, makeEngine: recorder.factory)

        fixture.model.restoreSession()
        await fixture.model.startSyncing()
        await fixture.model.sceneBecameActive()
        fixture.model.sceneEnteredBackground()
        fixture.model.localWriteHappened()

        #expect(recorder.built.isEmpty)
        #expect(fixture.model.syncStatusLine == nil)
    }

    @Test func signedOutCompositionReportsInactive() async throws {
        let recorder = EngineFactoryRecorder()
        let fixture = try FamilySettingsFixture.make(familyInBundle: false, makeEngine: recorder.factory)
        fixture.model.restoreSession()
        await fixture.model.startSyncing()

        #expect(fixture.model.syncOperationalStatus.activity == .inactive)
        #expect(fixture.model.syncOperationalStatus.signIn == .required)
        #expect(fixture.model.syncOperationalStatus.live == .unavailable)
        #expect(recorder.built.isEmpty)
    }

    // MARK: Scene phase and writes

    @Test func foregroundSyncsAndBackgroundStopsPolling() async throws {
        let recorder = EngineFactoryRecorder()
        let fixture = try await Self.signedInWithoutFamily(recorder)
        try await fixture.model.createFamily(name: "Family")

        fixture.model.sceneEnteredBackground()
        await fixture.model.sceneBecameActive()
        fixture.model.localWriteHappened()

        let calls = recorder.last?.calls ?? []
        #expect(calls.suffix(4) == ["stopPolling", "startPolling", "syncNow(foreground)", "localWriteHappened"])
    }

    @Test func disconnectingStopsTheEngineAndForgetsTheEntryRole() async throws {
        let recorder = EngineFactoryRecorder()
        let fixture = try await Self.signedInWithoutFamily(recorder)
        try await fixture.model.createFamily(name: "Family")
        let engine = try #require(recorder.last)

        try await fixture.model.disconnectCurrentFamily()

        #expect(engine.calls.last == "stop")
        #expect(fixture.model.syncStatusLine == nil)
        #expect(FamilyEntryRecord.entry(for: try #require(recorder.built.first?.key), in: fixture.defaults) == nil)
    }

    /// The purge that disconnecting does must not race a sync still writing
    /// into the realm it empties: the engine is stopped before the logout.
    @Test func disconnectStopsSyncBeforeLoggingOutAndNothingRunsAfter() async throws {
        let recorder = EngineFactoryRecorder()
        let fixture = try await Self.signedInWithoutFamily(recorder)
        try await fixture.model.createFamily(name: "Family")
        let engine = try #require(recorder.last)
        var logoutsAtStop: Int?
        engine.onStop = { logoutsAtStop = fixture.client.logoutCount }

        try await fixture.model.disconnectCurrentFamily()
        let callsAfterDisconnect = engine.calls
        await fixture.model.sceneBecameActive()
        fixture.model.localWriteHappened()

        #expect(logoutsAtStop == 0)
        #expect(fixture.client.logoutCount == 1)
        #expect(engine.calls == callsAfterDisconnect)
        #expect(recorder.built.count == 1)
    }

    @Test func leavingStopsSyncBeforeTheServerIsAsked() async throws {
        let recorder = EngineFactoryRecorder()
        let fixture = try await Self.signedInWithoutFamily(recorder)
        try await fixture.model.createFamily(name: "Family")
        let engine = try #require(recorder.last)
        var leavesAtStop: Int?
        engine.onStop = { leavesAtStop = fixture.client.leaveCount }

        try await fixture.model.leaveFamily()

        #expect(leavesAtStop == 0)
        #expect(fixture.client.leaveCount == 1)
    }

    /// Still a member after a failed disconnect, so still syncing.
    @Test func failedDisconnectBringsSyncBack() async throws {
        let recorder = EngineFactoryRecorder()
        let fixture = try await Self.signedInWithoutFamily(recorder)
        try await fixture.model.createFamily(name: "Family")
        fixture.client.logoutError = Offline()

        await #expect(throws: Offline.self) { try await fixture.model.disconnectCurrentFamily() }

        #expect(fixture.model.hasFamily)
        #expect(recorder.built.count == 2)
        #expect(recorder.built[0].engine.calls.last == "stop")
        #expect(recorder.last?.calls.contains("activateAsOwner") == true)
    }

    /// After a successful leave, the old session may already be invalid.
    /// Local cleanup must finish without a logout request against it.
    @Test func leaveCompletesCleanupWhenLogoutWouldBeUnauthorized() async throws {
        let recorder = EngineFactoryRecorder()
        let fixture = try await Self.signedInWithoutFamily(recorder)
        try await fixture.model.createFamily(name: "Family")
        fixture.client.logoutError = Offline()

        try await fixture.model.leaveFamily()

        #expect(fixture.client.leaveCount == 1)
        #expect(fixture.client.logoutCount == 0)
        #expect(!fixture.model.hasFamily)
        #expect(recorder.built.count == 1)
        #expect(recorder.last?.calls.last == "stop")
        #expect(fixture.model.syncStatusLine == nil)
    }

    @Test func accountDeletionThatFailsAfterTheServerDeletedStaysStopped() async throws {
        let recorder = EngineFactoryRecorder()
        let fixture = try FamilySettingsFixture.make(
            role: "caregiver", familyInBundle: false, makeEngine: recorder.factory
        )
        try await fixture.model.signInWithApple()
        try await fixture.model.createFamily(name: "Family")
        fixture.client.logoutError = Offline()

        await #expect(throws: Offline.self) { try await fixture.model.deleteAccount() }

        #expect(fixture.client.performed.contains(.deleteAccount))
        #expect(recorder.built.count == 1)
        #expect(fixture.model.syncStatusLine == nil)
    }

    @Test func revokedAppleCredentialStopsSyncForGood() async throws {
        let recorder = EngineFactoryRecorder()
        let fixture = try await Self.signedInWithoutFamily(recorder)
        try await fixture.model.createFamily(name: "Family")
        fixture.credentialStateClient.nextState = .revoked

        _ = try await fixture.model.refreshAppleCredentialState()

        #expect(recorder.built.count == 1)
        #expect(recorder.last?.calls.last == "stop")
        #expect(fixture.model.syncStatusLine == nil)
    }

    @Test func authorizedAppleCredentialKeepsSyncing() async throws {
        let recorder = EngineFactoryRecorder()
        let fixture = try await Self.signedInWithoutFamily(recorder)
        try await fixture.model.createFamily(name: "Family")

        _ = try await fixture.model.refreshAppleCredentialState()

        #expect(fixture.model.hasFamily)
        #expect(recorder.built.count == 2)
    }

    @Test func signingInAgainReplacesEngineEvenWhenFamilyKeyIsUnchanged() async throws {
        let recorder = EngineFactoryRecorder()
        let fixture = try await Self.signedInWithoutFamily(recorder)
        try await fixture.model.createFamily(name: "Family")
        let oldEngine = try #require(recorder.last)
        oldEngine.status = .needsSignIn
        fixture.identity.bundle = SessionBundle(
            accountID: FamilySettingsFixture.accountID, deviceID: FamilySettingsFixture.deviceID,
            recoveryEpoch: 0, accessToken: "fresh-access", refreshToken: "fresh-refresh",
            expiresAt: Date(timeIntervalSince1970: 20_000),
            deviceCredential: "device-credential", familyID: FamilySettingsFixture.familyID
        )

        try await fixture.model.signInWithApple()

        #expect(oldEngine.calls.last == "stop")
        #expect(recorder.built.count == 2)
        #expect(recorder.last?.calls.contains("activateAsOwner") == true)
        #expect(recorder.last?.status != .needsSignIn)
    }

    @Test func signingInAgainActivatesBeforeReadingFamilyWithFreshCredentials() async throws {
        let recorder = EngineFactoryRecorder()
        let fixture = try await Self.signedInWithoutFamily(recorder)
        try await fixture.model.createFamily(name: "Family")
        await fixture.model.startSyncing()
        fixture.identity.bundle = SessionBundle(
            accountID: FamilySettingsFixture.accountID, deviceID: FamilySettingsFixture.deviceID,
            recoveryEpoch: 0, accessToken: "fresh-access", refreshToken: "fresh-refresh",
            expiresAt: Date(timeIntervalSince1970: 20_000),
            deviceCredential: "device-credential", familyID: FamilySettingsFixture.familyID
        )
        fixture.client.profileError = HTTPClientError.status(401, "access_expired")
        recorder.nextOnActivation = { fixture.client.profileError = nil }

        try await fixture.model.signInWithApple()

        let calls = try #require(recorder.last?.calls)
        #expect(calls.first == "activateAsOwner")
        #expect(calls.contains("startPolling"))
        #expect(fixture.client.profileError == nil)
    }

    // MARK: Live Activities

    /// A timer pulled by a background sync never got its card, because cards
    /// are only requested in the foreground. Coming back to the app fixes it.
    @Test func foregroundGivesARunningTimerWithoutACardItsLiveActivity() async throws {
        let recorder = EngineFactoryRecorder()
        let started = StartedActivities()
        let fixture = try await Self.signedInWithoutFamily(recorder) { timer, _ in started.ids.append(timer.id) }
        try await fixture.model.createFamily(name: "Family")

        let bare = try fixture.store.addBaby(name: "A", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let carded = try fixture.store.addBaby(name: "B", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let needsCard = try fixture.store.startSleep(for: bare.id)
        let hasCard = try fixture.store.startSleep(for: carded.id)
        try fixture.store.setLiveActivityID("existing", on: hasCard)

        await fixture.model.sceneBecameActive()

        #expect(started.ids.contains(needsCard.id))
        #expect(!started.ids.contains(hasCard.id))
    }

    /// A cold launch before the push coordinator has answered: a pushed card
    /// is already up for a running sleep this phone has not given a card.
    /// The launch catch-up must not stack a plain card beside it.
    @Test func startSyncingNeverStacksAPlainCardBesideAPushedOne() async throws {
        let recorder = EngineFactoryRecorder()
        let fixture = try await Self.signedInWithoutFamily(recorder) { timer, store in
            LiveActivityController.start(for: timer, in: store)
        }
        try await fixture.model.createFamily(name: "Family")
        try fixture.store.setActiveRealm(.shared(canonicalOrigin: FamilySettingsFixture.origin.canonicalString,
                                                 serverInstanceID: FamilySettingsFixture.instanceID,
                                                 familyID: FamilySettingsFixture.familyID))
        let baby = try fixture.store.addBaby(name: "A", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let sleep = try fixture.store.startSleep(for: baby.id)
        let scope = try LiveActivityScope(serverInstanceID: FamilySettingsFixture.instanceID, recoveryEpoch: 1,
                                          familyID: FamilySettingsFixture.familyID, babyID: baby.id, kindRaw: "sleep")
        let revision = try LiveActivityRevision(scope: scope, generationID: UUID(), timerID: sleep.id, familySequence: 3,
                                                presentationVersion: 1, presentationTimestamp: 1_800_000_000, canWrite: true)
        let board = MemoryCardBoard()
        board.receiveStart(id: "pushed", babyID: baby.id, kindRaw: "sleep", scope: scope,
                           state: TimerActivityContentState(startedAt: sleep.startedAt, sideLabel: "", activityKind: .sleep, remote: revision),
                           timestamp: Date(timeIntervalSince1970: 1_800_000_000))

        await LiveCards.$board.withValue(board) {
            await LiveCards.$pushState.withValue(LiveActivityPushState()) {
                await fixture.model.startSyncing()
            }
        }
        #expect(board.requests.isEmpty)
        #expect(board.cards().map(\.id) == ["pushed"])
    }

    // MARK: Token lookup off the main actor

    /// URLSession-driven transports ask for the token from a nonisolated
    /// async context. Reading the coordinator's mode there must not assume
    /// main-actor isolation — that traps, on every launch of a family device.
    @Test func tokenLookupFollowsTheCoordinatorFromOffTheMainActor() async throws {
        let fixture = try await Self.signedInWithoutFamily(EngineFactoryRecorder())
        try await fixture.model.createFamily(name: "Family")
        let coordinator = AccountCoordinator(
            store: fixture.store, keychain: fixture.keychain, identity: fixture.identity,
            apple: fixture.appleClient, credentialState: fixture.credentialStateClient,
            defaults: fixture.defaults
        )
        let sessionKey = CurrentSessionKey()
        sessionKey.follow(coordinator)
        let provider = KeychainTokenProvider(keychain: fixture.keychain, key: { sessionKey.value })

        #expect(await Task.detached { await provider.currentAccessToken() }.value == nil)
        coordinator.restoreFamilySession()
        #expect(await Task.detached { await provider.currentAccessToken() }.value == "access")
    }

    // MARK: Status copy

    @Test func upToDateShowsTheTime() throws {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(identifier: "UTC")!
        let date = try #require(calendar.date(from: DateComponents(year: 2026, month: 9, day: 25, hour: 14, minute: 5)))
        let line = try #require(FamilySyncStatusCopy.line(
            for: .upToDate(date), locale: Locale(identifier: "en_GB"), timeZone: TimeZone(identifier: "UTC")!
        ))
        #expect(line.text == "Synced 14:05")
        #expect(!line.isProblem)
        #expect(!line.offersSignIn)
    }

    @Test func offlineReassuresThatEntriesAreSaved() throws {
        let line = try #require(FamilySyncStatusCopy.line(for: .offline))
        #expect(line.text == "Offline — entries are saved and will sync")
        #expect(!line.isProblem)
    }

    @Test func needsSignInOffersTheButton() throws {
        let line = try #require(FamilySyncStatusCopy.line(for: .needsSignIn))
        #expect(line.text == "Sign in again to keep syncing")
        #expect(line.offersSignIn)
        #expect(line.isProblem)
    }

    /// Backing off from a transient server answer is not an error: nothing is
    /// wrong with the entries, and the next poll retries.
    @Test func transientBackoffReadsAsNeutralSyncing() throws {
        let line = try #require(FamilySyncStatusCopy.line(for: .failed(SyncIssueCopy.retrying)))
        #expect(line.text == FamilySyncStatusCopy.syncing)
        #expect(!line.isProblem)
        #expect(FamilySyncStatusCopy.line(for: .syncing)?.isProblem == false)
    }

    @Test func realFailureShowsItsMessageAsAProblem() throws {
        let line = try #require(FamilySyncStatusCopy.line(for: .failed(SyncIssueCopy.importIncomplete)))
        #expect(line.text == SyncIssueCopy.importIncomplete)
        #expect(line.isProblem)
        #expect(FamilySyncStatusCopy.line(for: .idle) == nil)
    }
}
