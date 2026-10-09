import BabyProtocol
import BabyStore
import Foundation
import Testing
@testable import BabySync

/// Shared doubles for the Live Activity push suites. Everything here records
/// what it was asked so the suites can prove the negative requirements: no
/// request into a new realm from an old task, no token in the ledger file.
enum PushFixture {
    static let builtIn = ConfirmedEndpoint(
        origin: try! ServerOrigin(validating: "https://baby-sync.example-tailnet.ts.net"),
        serverInstanceID: UUID(uuidString: "11111111-2222-4333-8444-555555555555")!
    )
    static let familyID = UUID(uuidString: "b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let otherFamilyID = UUID(uuidString: "b2b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let accountID = UUID(uuidString: "d1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let otherAccountID = UUID(uuidString: "e1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let deviceID = UUID(uuidString: "f1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let babyID = UUID(uuidString: "c1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let generationID = UUID(uuidString: "a0b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let canonicalGenerationID = UUID(uuidString: "a9b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let timerID = UUID(uuidString: "a1a2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let attemptID = UUID(uuidString: "a3a2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let startToken = "a1b2c3d4e5f60718"
    static let rotatedStartToken = "ffeeddccbbaa9988"
    static let activityToken = "0123456789abcdef"
    static let epoch: UInt64 = 3

    static func familyKey(familyID: UUID = familyID, accountID: UUID = accountID, deviceID: UUID = deviceID, epoch: UInt64 = epoch) -> FamilySessionKey {
        FamilySessionKey(
            canonicalOrigin: builtIn.origin.canonicalString, serverInstanceID: builtIn.serverInstanceID,
            familyID: familyID, accountID: accountID, deviceID: deviceID, recoveryEpoch: epoch
        )
    }

    static func serverInfo(
        instanceID: UUID = builtIn.serverInstanceID,
        origin: String = builtIn.origin.canonicalString,
        epoch: UInt64 = epoch,
        capabilities: [String] = ["records-v1", "snapshot-v1", LiveActivityContract.capability]
    ) -> ServerInfo {
        ServerInfo(
            serverInstanceID: instanceID, canonicalOrigin: origin, recoveryEpoch: epoch, capabilities: capabilities,
            appleAuthorizationMode: "hosted", maxItemsPerPage: 100, maxEncodedBytes: 1_000_000, sourceVersion: "", sourceURL: ""
        )
    }

    /// `pushAdvertised: false`: the built-in server answered the probe without
    /// `live-activity-push-v1` — its push worker is stopped or push disabled.
    static func context(
        familyID: UUID = familyID,
        accountID: UUID = accountID,
        activitiesAllowed: Bool = true,
        feedBreakMinutes: Int = 30,
        environment: LiveActivityAPNsEnvironment = .sandbox,
        pushAdvertised: Bool = true
    ) -> LiveActivityPushContext {
        let info = pushAdvertised ? serverInfo() : serverInfo(capabilities: ["records-v1", "snapshot-v1"])
        return LiveActivityPushContext(
            familyKey: familyKey(familyID: familyID, accountID: accountID), endpoint: builtIn, serverInfo: info,
            builtIn: builtIn, activitiesAllowed: activitiesAllowed, feedBreakMinutes: feedBreakMinutes, environment: environment
        )!
    }

    static func scope(familyID: UUID = familyID, kind: String = "feed") -> LiveActivityScope {
        try! LiveActivityScope(
            serverInstanceID: builtIn.serverInstanceID, recoveryEpoch: Int64(epoch), familyID: familyID, babyID: babyID, kindRaw: kind
        )
    }

    static func observation(
        id: String = "activity-1",
        familyID: UUID = familyID,
        state: LiveActivityObservedState = .active,
        token: String? = nil,
        generationID: UUID? = generationID
    ) -> LiveActivityObservation {
        let scope = scope(familyID: familyID)
        let remote = generationID.map {
            try! LiveActivityRevision(scope: scope, generationID: $0, timerID: timerID, familySequence: 4,
                                      presentationVersion: 1, presentationTimestamp: 1_700_000_000, canWrite: true)
        }
        return LiveActivityObservation(activityID: id, babyID: babyID, kindRaw: "feed", scope: scope, remote: remote, state: state, pushToken: token)
    }

    static func temporaryDirectory() -> URL {
        let url = FileManager.default.temporaryDirectory.appending(path: "lap-\(UUID().uuidString)", directoryHint: .isDirectory)
        try? FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }
}

/// An ActivityKit stand-in. Events sent before anybody subscribes are kept and
/// replayed on subscription, the way ActivityKit replays a current token.
final class TestPushEvents: LiveActivityPushEventSource, @unchecked Sendable {
    private let lock = NSLock()
    private var continuations: [AsyncStream<LiveActivityPushEvent>.Continuation] = []
    private var buffered: [LiveActivityPushEvent] = []
    private var currentInventory: [LiveActivityObservation] = []
    private var enabled = true
    private var sent = 0
    private(set) var subscriptions = 0

    /// Every event `send` has issued: what a subscriber must have handled
    /// before the suites call the coordinator settled.
    var emitted: Int { lock.withLock { sent } }

    func events() -> AsyncStream<LiveActivityPushEvent> {
        let (stream, continuation) = AsyncStream.makeStream(of: LiveActivityPushEvent.self)
        lock.withLock {
            subscriptions += 1
            continuations.append(continuation)
            for event in buffered { continuation.yield(event) }
            buffered.removeAll()
        }
        return stream
    }

    func send(_ event: LiveActivityPushEvent) {
        lock.withLock {
            sent += 1
            if continuations.isEmpty { buffered.append(event) }
            for continuation in continuations { continuation.yield(event) }
        }
    }

    func setInventory(_ value: [LiveActivityObservation]) { lock.withLock { currentInventory = value } }
    func setEnabled(_ value: Bool) { lock.withLock { enabled = value } }
    func inventory() -> [LiveActivityObservation] { lock.withLock { currentInventory } }
    func activitiesEnabled() -> Bool { lock.withLock { enabled } }

    func finish() { lock.withLock { for continuation in continuations { continuation.finish() } } }
}

/// One suspended response, released by the test.
final class Gate: @unchecked Sendable {
    private let lock = NSLock()
    private var opened = false
    private var waiters: [UUID: CheckedContinuation<Void, Never>] = [:]
    private var arrivalWaiters: [CheckedContinuation<Void, Never>] = []
    private(set) var arrivals = 0

    /// Suspends until `open()`.
    func wait() async {
        await wait(id: UUID())
    }

    /// Suspends until `open()` or until the waiting task is cancelled — the
    /// way a URL request in flight returns when its task is cancelled.
    func waitUnlessCancelled() async {
        let id = UUID()
        await withTaskCancellationHandler {
            await wait(id: id)
        } onCancel: {
            release(id)
        }
    }

    private func wait(id: UUID) async {
        await withCheckedContinuation { continuation in
            let (resumeNow, arrived) = lock.withLock { () -> (Bool, [CheckedContinuation<Void, Never>]) in
                arrivals += 1
                let arrived = arrivalWaiters
                arrivalWaiters.removeAll()
                if opened { return (true, arrived) }
                waiters[id] = continuation
                return (false, arrived)
            }
            for waiter in arrived { waiter.resume() }
            if resumeNow { continuation.resume() }
        }
    }

    private func release(_ id: UUID) {
        let waiter = lock.withLock { waiters.removeValue(forKey: id) }
        waiter?.resume()
    }

    func open() {
        let pending = lock.withLock { () -> [CheckedContinuation<Void, Never>] in
            opened = true
            defer { waiters.removeAll() }
            return Array(waiters.values)
        }
        for waiter in pending { waiter.resume() }
    }

    /// Returns once something has reached the gate — no polling.
    func waitForArrival() async {
        await withCheckedContinuation { continuation in
            let now = lock.withLock { () -> Bool in
                if arrivals > 0 { return true }
                arrivalWaiters.append(continuation)
                return false
            }
            if now { continuation.resume() }
        }
    }
}

/// The registration server, scripted per test. Every request is recorded with
/// the realm of the client that sent it.
final class FakePushClient: LiveActivityPushRegistering, @unchecked Sendable {
    enum Call: Equatable {
        case device(LiveActivityDeviceRegistration)
        case activity(LiveActivityTokenRegistration)
        case unregister(UUID, Int64)
    }

    let familyID: UUID
    private let lock = NSLock()
    private var recorded: [Call] = []
    var onDevice: @Sendable (LiveActivityDeviceRegistration, Int) async throws -> LiveActivityRegistrationReceipt
    var onActivity: @Sendable (LiveActivityTokenRegistration, Int) async throws -> LiveActivityRegistrationReceipt
    var onUnregister: @Sendable (UUID, Int64) async throws -> Void = { _, _ in }

    init(familyID: UUID = PushFixture.familyID) {
        self.familyID = familyID
        onDevice = { registration, _ in try FakePushClient.accept(registration) }
        onActivity = { registration, _ in try FakePushClient.accept(registration, generation: PushFixture.canonicalGenerationID) }
    }

    var calls: [Call] { lock.withLock { recorded } }
    var deviceCalls: [LiveActivityDeviceRegistration] {
        calls.compactMap { if case .device(let value) = $0 { value } else { nil } }
    }
    var activityCalls: [LiveActivityTokenRegistration] {
        calls.compactMap { if case .activity(let value) = $0 { value } else { nil } }
    }
    var unregisterCalls: [(UUID, Int64)] {
        calls.compactMap { if case .unregister(let id, let version) = $0 { (id, version) } else { nil } }
    }

    private func record(_ call: Call) -> Int {
        lock.withLock {
            recorded.append(call)
            return recorded.count
        }
    }

    func registerDevice(_ registration: LiveActivityDeviceRegistration) async throws -> LiveActivityRegistrationReceipt {
        let index = record(.device(registration))
        return try await onDevice(registration, index)
    }

    func registerActivity(_ registration: LiveActivityTokenRegistration) async throws -> LiveActivityRegistrationReceipt {
        let index = record(.activity(registration))
        return try await onActivity(registration, index)
    }

    func unregisterDevice(installationID: UUID, version: Int64) async throws {
        _ = record(.unregister(installationID, version))
        try await onUnregister(installationID, version)
    }

    static func accept(
        _ registration: LiveActivityDeviceRegistration,
        unconfirmed: [LiveActivityUnconfirmedStart] = []
    ) throws -> LiveActivityRegistrationReceipt {
        try LiveActivityRegistrationReceipt(
            serverInstanceID: PushFixture.builtIn.serverInstanceID, recoveryEpoch: Int64(PushFixture.epoch),
            acceptedRegistrationVersion: registration.registrationVersion, unconfirmedStarts: unconfirmed
        )
    }

    static func accept(_ registration: LiveActivityTokenRegistration, generation: UUID) throws -> LiveActivityRegistrationReceipt {
        try LiveActivityRegistrationReceipt(
            serverInstanceID: PushFixture.builtIn.serverInstanceID, recoveryEpoch: Int64(PushFixture.epoch),
            acceptedRegistrationVersion: 1, acceptedTokenVersion: registration.tokenVersion,
            generations: [LiveActivityGenerationMapping(activityID: registration.activityID, scope: registration.scope, generationID: generation)]
        )
    }
}

/// Records the retry schedule without waiting it out.
final class SleepRecorder: @unchecked Sendable {
    private let lock = NSLock()
    private var recorded: [Duration] = []
    var durations: [Duration] { lock.withLock { recorded } }

    func sleep(_ duration: Duration) async throws {
        lock.withLock { recorded.append(duration) }
        try Task.checkCancellation()
        await Task.yield()
    }
}

@MainActor
final class Flag {
    var value: Bool
    init(_ value: Bool) { self.value = value }
}

extension LiveActivityPushCoordinator {
    /// The deterministic point every suite waits for instead of polling a
    /// clock: every event `source` has emitted has been handled, and the pump
    /// those events, `start` or `reconcile` began has finished — its whole
    /// retry schedule included, which the injected sleep makes immediate.
    func settle(_ source: TestPushEvents) async {
        await waitForHandledEvents(source.emitted)
        await waitUntilIdle()
    }
}

extension LiveActivityPushDriver {
    /// As above, plus every evaluation the driver started from a callback.
    func settle(_ source: TestPushEvents) async {
        await coordinator.waitForHandledEvents(source.emitted)
        await waitUntilSettled()
    }
}

@MainActor
struct PushHarness {
    let events = TestPushEvents()
    let directory = PushFixture.temporaryDirectory()
    let keychain = InMemoryKeychainBackend()
    let sleeper = SleepRecorder()
    /// The app's foreground state as the coordinator reads it.
    let foreground = Flag(true)
    /// What the coordinator publishes to the card controller; one per
    /// harness so parallel suites never share it.
    let pushState = LiveActivityPushState()
    var clients: [UUID: FakePushClient] = [:]

    var store: LiveActivityRegistrationStore { LiveActivityRegistrationStore(directory: directory, keychain: keychain) }

    func coordinator(clients: [FakePushClient]) -> LiveActivityPushCoordinator {
        let byFamily = Dictionary(uniqueKeysWithValues: clients.map { ($0.familyID, $0) })
        let sleeper = sleeper
        return LiveActivityPushCoordinator(
            events: events,
            store: store,
            makeClient: { context in byFamily[context.familyKey.familyID] ?? FakePushClient(familyID: context.familyKey.familyID) },
            sleep: { try await sleeper.sleep($0) },
            isForeground: { [foreground] in foreground.value },
            pushState: pushState
        )
    }
}
