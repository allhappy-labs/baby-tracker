import BabyCore
import BabyProtocol
import BabyStore
@testable import BabySync
import Foundation
import Testing
@testable import SettingsFeature

/// The app's own graph, restored from a saved session the way a cold launch
/// restores it: push registration happens with no settings screen involved,
/// and the account coordinator's mode changes reach it.
@MainActor
@Suite(.timeLimit(.minutes(1)))
struct LiveActivityPushCompositionTests {
    private final class Events: LiveActivityPushEventSource, @unchecked Sendable {
        private let lock = NSLock()
        private var continuation: AsyncStream<LiveActivityPushEvent>.Continuation?
        private var buffered: [LiveActivityPushEvent] = []
        private var sent = 0
        var emitted: Int { lock.withLock { sent } }
        func events() -> AsyncStream<LiveActivityPushEvent> {
            let (stream, continuation) = AsyncStream.makeStream(of: LiveActivityPushEvent.self)
            lock.withLock {
                self.continuation = continuation
                buffered.forEach { continuation.yield($0) }
                buffered = []
            }
            return stream
        }
        func send(_ event: LiveActivityPushEvent) {
            lock.withLock {
                sent += 1
                if let continuation { continuation.yield(event) } else { buffered.append(event) }
            }
        }
        func finish() { lock.withLock { continuation?.finish() } }
        func inventory() -> [LiveActivityObservation] { [] }
        func activitiesEnabled() -> Bool { true }
    }

    private final class Server: LiveActivityPushRegistering, @unchecked Sendable {
        let instanceID: UUID
        init(instanceID: UUID) { self.instanceID = instanceID }
        private let lock = NSLock()
        private var devices: [LiveActivityDeviceRegistration] = []
        var registered: [LiveActivityDeviceRegistration] { lock.withLock { devices } }
        func registerDevice(_ registration: LiveActivityDeviceRegistration) async throws -> LiveActivityRegistrationReceipt {
            lock.withLock { devices.append(registration) }
            return try LiveActivityRegistrationReceipt(serverInstanceID: instanceID,
                                                       recoveryEpoch: 2, acceptedRegistrationVersion: registration.registrationVersion)
        }
        func registerActivity(_ registration: LiveActivityTokenRegistration) async throws -> LiveActivityRegistrationReceipt {
            throw LiveActivityPushClientError.rejected(nil)
        }
        func unregisterDevice(installationID: UUID, version: Int64) async throws {}
    }

    private struct Sandbox: LiveActivityAPNsEnvironmentResolving {
        func environment() -> LiveActivityAPNsEnvironment? { .sandbox }
    }

    /// Every event sent has been handled, every evaluation the driver began
    /// from a callback (the mode change) has finished, and the pump is idle.
    private func settle(_ push: LiveActivityPushDriver, _ events: Events) async {
        await push.coordinator.waitForHandledEvents(events.emitted)
        await push.waitUntilSettled()
    }

    @Test func aRestoredBuiltInFamilyRegistersAutomaticallyAndSignOutStopsIt() async throws {
        let builtIn = try #require(FamilySyncComposition.hostedEndpoint)
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let backend = InMemoryKeychainBackend()
        let defaults = try #require(UserDefaults(suiteName: "LiveActivityPushComposition-\(UUID().uuidString)"))
        let directory = FileManager.default.temporaryDirectory.appending(path: "lap-composition-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: directory) }
        let key = FamilySessionKey(
            canonicalOrigin: builtIn.origin.canonicalString, serverInstanceID: builtIn.serverInstanceID,
            familyID: UUID(), accountID: UUID(), deviceID: UUID(), recoveryEpoch: 2
        )
        // What a previous launch left: a session record and its credentials.
        try KeychainSessionStore(backend: backend).saveFamily(SessionCredentials(
            accessToken: "access", refreshToken: "refresh", expiresAt: .distantFuture, recoveryEpoch: 2,
            deviceContinuity: DeviceContinuity(deviceID: key.deviceID, credential: "continuity")
        ), for: key)
        defaults.set(try JSONEncoder().encode(FamilySessionRecord(endpoint: builtIn, familyKey: key)), forKey: "familySync.session.v1")

        let events = Events()
        let server = Server(instanceID: builtIn.serverInstanceID)
        let info = ServerInfo(serverInstanceID: builtIn.serverInstanceID, canonicalOrigin: builtIn.origin.canonicalString, recoveryEpoch: 2,
                              capabilities: [LiveActivityContract.capability], appleAuthorizationMode: "hosted",
                              maxItemsPerPage: 100, maxEncodedBytes: 1_000_000, sourceVersion: "", sourceURL: "")
        let graph = FamilySyncComposition.assemble(
            store: store, keychainBackend: backend, identity: HTTPIdentityClient(), session: .shared, defaults: defaults,
            liveActivityPush: .init(
                events: events, store: LiveActivityRegistrationStore(directory: directory, keychain: backend),
                builtIn: builtIn, environment: Sandbox(), probe: { _ in info }, makeClient: { _ in server },
                background: InlineLiveActivityPushExecution(), feedBreakMinutes: { 35 }
            )
        )
        let push = try #require(graph.liveActivityPush)
        graph.model.restoreSession()
        events.send(.pushToStartToken("0a1b2c3d"))
        await push.launch()

        await settle(push, events)
        #expect(server.registered.count == 1)
        #expect(server.registered.first?.feedBreakMinutes == 35)
        #expect(push.coordinator.context?.familyKey == key)

        // Sign-out goes through the account coordinator; push follows it.
        try graph.coordinator.finishFamilyCleanup(key)
        await settle(push, events)
        #expect(push.coordinator.context == nil)
        events.send(.pushToStartToken("ffee"))
        await settle(push, events)
        #expect(server.registered.count == 1)
        events.finish()
    }

    @Test func theProductionWiringTargetsOnlyTheHostedEndpoint() throws {
        let wiring = FamilySyncComposition.LiveActivityPushWiring.system(keychainBackend: InMemoryKeychainBackend())
        #expect(wiring.builtIn == FamilySyncComposition.hostedEndpoint)
        #expect(wiring.probe == nil && wiring.makeClient == nil)
        // The package host has no ActivityKit: nothing is ever enabled.
        #expect(wiring.events.activitiesEnabled() == false)
    }
}
