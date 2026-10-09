import BabyCore
import BabyProtocol
@testable import BabyStore
import Foundation
import SwiftData
import Testing
@testable import BabySync

private final class RecordingSyncDiagnostics: SyncDiagnosticSink, @unchecked Sendable {
    private let lock = NSLock()
    private var recorded: [SyncDiagnosticEvent] = []

    func record(_ event: SyncDiagnosticEvent) {
        lock.lock(); defer { lock.unlock() }
        recorded.append(event)
    }

    var events: [SyncDiagnosticEvent] {
        lock.lock(); defer { lock.unlock() }
        return recorded
    }
}

@MainActor
@Suite("Bounded sync diagnostics")
struct SyncDiagnosticsTests {
    private struct AbortedWrite: Error {}

    @Test func diagnosticsCoverSaveThroughObservableApply() async throws {
        let store = try SyncFixtures.store()
        let transport = FakeSyncTransport()
        let sink = RecordingSyncDiagnostics()
        let coordinator = SyncCoordinator(
            store: store, transport: transport, accountKey: SyncFixtures.accountKey,
            diagnostics: sink
        )
        let baby = try #require(store.context.fetch(FetchDescriptor<Baby>()).first)
        baby.name = "private-care-value"
        try store.record(.update, on: baby)
        await coordinator.syncOnce(reason: .localWrite)

        let events = sink.events
        let phases = events.map(\.phase)
        #expect(phases == [.localCommit, .enqueue, .requestStart, .requestComplete,
                           .peerPull, .remoteCommit, .observablePublish])
        #expect(Set(events.map(\.correlation)).count == 1)
        #expect(events.first?.elapsedNanoseconds == 0)
        #expect(events.allSatisfy { $0.status == .success })
        await coordinator.syncOnce(reason: .foreground)
        #expect(sink.events.last?.correlation != events.first?.correlation)
    }

    @Test func diagnosticsContainNoCareOrCredentialValues() async throws {
        let store = try SyncFixtures.store()
        let sink = RecordingSyncDiagnostics()
        let coordinator = SyncCoordinator(
            store: store, transport: FakeSyncTransport(), accountKey: SyncFixtures.accountKey,
            diagnostics: sink
        )
        let baby = try #require(store.context.fetch(FetchDescriptor<Baby>()).first)
        coordinator.recordHintReceived()
        baby.name = "sentinel-care-secret"
        try store.record(.update, on: baby)
        #expect(sink.events.last?.phase == .localCommit)
        #expect(sink.events.last?.elapsedNanoseconds == 0)
        await coordinator.syncOnce(reason: .localWrite)

        let artifact = String(reflecting: sink.events)
        #expect(!artifact.contains("sentinel-care-secret"))
        #expect(!artifact.contains(SyncFixtures.babyID.uuidString))
        #expect(!artifact.contains(SyncFixtures.accountID.uuidString))
        #expect(!artifact.contains("access"))
        #expect(!artifact.contains("refresh"))
    }

    @Test func groupedCareWritesStartAtCommitAndRollbackLeavesNoSignal() async throws {
        let store = try SyncFixtures.store()
        let sink = RecordingSyncDiagnostics()
        let coordinator = SyncCoordinator(
            store: store, transport: FakeSyncTransport(), accountKey: SyncFixtures.accountKey,
            diagnostics: sink
        )
        let baby = try store.addBaby(name: "new", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        #expect(sink.events.map(\.phase) == [.localCommit])
        _ = try store.startSleep(for: baby.id)
        #expect(sink.events.map(\.phase) == [.localCommit, .localCommit])

        let expected = try store.captureWriteContext()
        #expect(throws: AbortedWrite.self) {
            try store.withRealmWrite(expected: expected) {
                try store.recordLocked(.update, on: baby)
                throw AbortedWrite()
            }
        }
        #expect(sink.events.count == 2)
        try store.withRealmWrite(expected: expected) {}
        #expect(sink.events.count == 2)
        try store.record(.update, on: baby)
        #expect(sink.events.map(\.phase) == [.localCommit, .localCommit, .localCommit])

        await coordinator.syncOnce(reason: .localWrite)
        #expect(sink.events.map(\.phase) == [
            .localCommit, .localCommit, .localCommit, .enqueue,
            .requestStart, .requestComplete, .peerPull, .remoteCommit, .observablePublish,
        ])
    }

    @Test func remoteCallbackReentrantLocalWriteKeepsExactClassificationAndOrder() throws {
        let store = try SyncFixtures.store()
        var phases: [StoreSyncMilestone.Phase] = []
        var callbackError: (any Error)?
        var didReenter = false
        store.onSyncMilestone = { milestone in
            phases.append(milestone.phase)
            if milestone.phase == .remoteCommit && !didReenter {
                didReenter = true
                do {
                    _ = try store.addBaby(name: "reentrant", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
                } catch {
                    callbackError = error
                }
            }
        }

        try store.applySyncPage(SyncFixtures.page(highWater: SyncFixtures.cursor(1)))
        #expect(callbackError == nil)
        #expect(phases == [.remoteCommit, .localCommit, .observablePublish])
        #expect(try store.syncCursor()?.sequence == 1)

        try store.finishSnapshot(snapshotID: UUID())
        #expect(phases == [.remoteCommit, .localCommit, .observablePublish])
        let expected = try store.captureWriteContext()
        #expect(throws: AbortedWrite.self) {
            try store.withRealmWrite(expected: expected) {
                store.syncWriteScope?.appliedRemoteData = true
                throw AbortedWrite()
            }
        }
        #expect(phases == [.remoteCommit, .localCommit, .observablePublish])
        _ = try store.addBaby(name: "after rollback", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        #expect(phases == [.remoteCommit, .localCommit, .observablePublish, .localCommit])
    }

    @Test func timerIntentsAndExplicitEnqueueEmitOnceButDispatchBookkeepingDoesNot() throws {
        let store = try SyncFixtures.store()
        let sink = RecordingSyncDiagnostics()
        let coordinator = SyncCoordinator(
            store: store, transport: FakeSyncTransport(), accountKey: SyncFixtures.accountKey,
            diagnostics: sink
        )
        let baby = try store.addBaby(name: "new", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let baseline = sink.events.count
        let timer = try store.startPart(.breast(.left), for: baby.id)
        try store.editRunningTimer(id: timer.id, startedAt: timer.startedAt.addingTimeInterval(-1), components: [])
        _ = try store.stopFeed(timer)
        #expect(Array(sink.events.dropFirst(baseline)).map(\.phase) == [.localCommit, .localCommit, .localCommit])

        let beforeDispatch = sink.events.count
        try store.markDispatched(mutationIDs: try store.pendingOutbox().map(\.id))
        #expect(sink.events.count == beforeDispatch)

        let mutation = WireMutation(
            mutationID: UUID(), entityID: baby.id, entityKind: .baby, operation: .update,
            payloadVersion: 1, observedEntityVersion: 0, clientMutationTime: "2026-10-01T00:00:00Z",
            importID: nil, payload: .object(["name": .string("new")])
        )
        try store.enqueueIntent(mutation, overlay: EntityOverlay(entityID: baby.id, entityKind: .baby, orderedMutationIDs: []))
        #expect(sink.events.count == beforeDispatch + 1)
        #expect(sink.events.last?.phase == .localCommit)
        withExtendedLifetime(coordinator) {}
    }

    @Test func operationalTraceCoversEstablishedDialingReconnectingRefusedAndSignOut() async throws {
        let family = try Family()
        let rig = LiveRig()
        let socket = FakeLiveSocket()
        let sink = RecordingSyncDiagnostics()
        rig.connector.script = [.socket(socket)]
        let engine = FamilySyncEngine(
            store: family.joiner, familyKey: family.joinerKey, transport: family.joinerTransport,
            importClient: EngineFixture.resolver(), renew: {}, live: rig.channel,
            diagnostics: sink
        )
        #expect(engine.operationalStatus.activity == .active)
        #expect(engine.operationalStatus.signIn == .available)
        #expect(engine.operationalStatus.live == .unavailable)
        print("SYNC_STATUS_TRACE established=\(engine.operationalStatus)")
        try await engine.activateAsJoiner()
        #expect(engine.operationalStatus.phase == .idle)

        engine.startForegroundPolling()
        #expect(engine.operationalStatus.live == .dialing)
        print("SYNC_STATUS_TRACE dialing=\(engine.operationalStatus)")
        try await until { await engine.liveState == .connected }
        #expect(engine.operationalStatus.live == .connected)
        print("SYNC_STATUS_TRACE connected=\(engine.operationalStatus)")
        _ = await engine.liveRefused()
        #expect(engine.operationalStatus.live == .refused)
        #expect(engine.operationalStatus.signIn == .available)
        print("SYNC_STATUS_TRACE refused=\(engine.operationalStatus)")
        socket.fail(.rateLimited(30))
        try await until { await engine.liveState == .backingOff }
        #expect(engine.operationalStatus.live == .reconnecting)
        print("SYNC_STATUS_TRACE reconnecting=\(engine.operationalStatus)")

        await engine.stop()
        #expect(engine.operationalStatus == .inactive)
        print("SYNC_STATUS_TRACE signedOut=\(engine.operationalStatus)")
        let afterStop = sink.events.count
        let added = try family.joiner.addBaby(name: "after stop", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        added.name = "edited after stop"
        try family.joiner.record(.update, on: added)
        #expect(sink.events.count == afterStop)
    }

    @Test func rateLimitedAndCapabilityMismatchRemainRetryStatesWithoutFalseLiveHealth() async throws {
        let family = try Family()
        let rig = LiveRig()
        rig.connector.script = [.fail(.rateLimited(30))]
        let engine = FamilySyncEngine(
            store: family.joiner, familyKey: family.joinerKey, transport: family.joinerTransport,
            importClient: EngineFixture.resolver(), renew: {}, live: rig.channel
        )
        try await engine.activateAsJoiner()
        engine.startForegroundPolling()
        try await until { await engine.liveState == .backingOff }
        #expect(engine.operationalStatus.live == .reconnecting)
        #expect(engine.operationalStatus.signIn == .available)
        await engine.stop()

        let wrongServer = LiveRig(instanceID: UUID())
        let mismatchedLive = FamilySyncEngine(
            store: family.joiner, familyKey: family.joinerKey, transport: family.joinerTransport,
            importClient: EngineFixture.resolver(), renew: {}, live: wrongServer.channel
        )
        mismatchedLive.startForegroundPolling()
        try await until { wrongServer.probes > 0 }
        try await until { await mismatchedLive.operationalStatus.live == .unavailable }
        #expect(mismatchedLive.operationalStatus.signIn == .available)
        await mismatchedLive.stop()

        let store = try SyncFixtures.store()
        let transport = FakeSyncTransport()
        let sink = RecordingSyncDiagnostics()
        let key = FamilySessionKey(
            canonicalOrigin: "https://sync.example", serverInstanceID: SyncFixtures.instanceID,
            familyID: SyncFixtures.familyID, accountID: SyncFixtures.accountID,
            deviceID: SyncFixtures.deviceID, recoveryEpoch: 0
        )
        let mismatch = FamilySyncEngine(
            store: store, familyKey: key, transport: transport,
            importClient: EngineFixture.resolver(), renew: {}, diagnostics: sink
        )
        transport.failWith = .unsupportedCapability
        await mismatch.syncNow(.foreground)
        #expect(mismatch.operationalStatus.phase == .retryQueued)
        #expect(mismatch.operationalStatus.signIn == .available)
        #expect(mismatch.operationalStatus.live == .unavailable)
        print("SYNC_STATUS_TRACE retryQueued=\(mismatch.operationalStatus)")
        #expect(sink.events.last(where: { $0.phase == .requestComplete })?.status == .refused)
        transport.failWith = .serviceUnavailable
        await mismatch.syncNow(.foreground)
        #expect(sink.events.last(where: { $0.phase == .requestComplete })?.status == .retry)
        await mismatch.stop()
    }
}
