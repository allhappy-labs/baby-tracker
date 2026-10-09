import BabyCore
import BabyProtocol
@testable import BabyStore
@testable import BabySync
import Foundation
import Observation
@testable import SettingsFeature
import SwiftData
import Testing

/// Actual disk stores and production SettingsFeature composition. The bridge
/// models RootView's revision callback; it does not render SwiftUI or a phone.
@MainActor
@Suite("Foreground durable peer latency", .serialized)
struct ForegroundLatencyE2ETests {
    @Test func foregroundLiveHintAppliesBeforeFallback() async throws {
        guard ForegroundFixture.enabled else { return }
        let fixture = try ForegroundFixture()
        do {
            try await fixture.seedIsolationFamily()
            try await fixture.start()
            let owner = fixture.owner, peer = fixture.peer
            let versions = try fixture.versions()
            _ = try await measureForegroundSaveToPeer(owner: owner, peer: peer, samples: 3)
            let report = try await measureForegroundSaveToPeer(owner: owner, peer: peer, samples: 200)
            report.emit("healthy", expected: 200)
            try #require(report.pairs.count == 200, "healthy observation budget exhausted")
            try #require(report.durableP95 < 1 && report.observableP95 < 1, "healthy foreground latency exceeds one second")
            try #require(try fixture.versions() == versions, "healthy measurement renewed credentials")
            try #require(fixture.ownerRecovery.count == 0 && fixture.peerRecovery.count == 0)
            try #require(fixture.ownerDiagnostics.failures == 0 && fixture.peerDiagnostics.failures == 0)
            try #require(fixture.ownerDiagnostics.eventCount < 4096 && fixture.peerDiagnostics.eventCount < 4096)
            try await fixture.verifyIsolationFamily()
            try #require(owner.traffic.liveDials.count == 1 && peer.traffic.liveDials.count == 1)
            try await fixture.finish()
            print("foreground-latency proof: healthy complete")
        } catch { emitFailure(error); try? await fixture.finish(); throw error }
    }

    @Test func foregroundDisabledHintsFailLatencyGate() async throws {
        guard ForegroundFixture.enabled else { return }
        let fixture = try ForegroundFixture(peerHints: false)
        do {
            try await fixture.start()
            let peerEngine = try fixture.peer.engine()
            try #require(peerEngine.foregroundPollInterval == .seconds(20))
            try #require(peerEngine.liveState == .stopped && fixture.peer.traffic.liveDials.isEmpty)
            let probe = ArrivalProbe(peer: fixture.peer)
            let beforeRequests = fixture.peer.traffic.syncRequests
            let sample = try probe.save(owner: fixture.owner)
            try await Task.sleep(for: .milliseconds(1100))
            try #require(probe.completed == nil, "unavailable-hint path falsely met one-second gate")
            try #require(fixture.peer.traffic.syncRequests == beforeRequests, "fallback fired inside one-second control")
            try #require(try fixture.owner.store.nextDispatchBatch(limit: 100).isEmpty, "owner automatic push never completed")
            // Keep the actual 20-second fallback: prove eventual delivery too.
            try await waitFor(seconds: 23) { probe.completed != nil }
            let pair = try #require(probe.completed)
            try #require(pair.durable >= 19 && pair.observable >= 19)
            ForegroundLatencyReport(pairs: [pair], elapsed: pair.observable).emit("unavailable_live_fallback", expected: 1)
            try probe.verify(sample)
            probe.stop()
            // Disable the equivalent view trigger as a separate negative.
            fixture.owner.foregroundBridge?.enabled = false
            let triggerProbe = ArrivalProbe(peer: fixture.peer)
            _ = try triggerProbe.save(owner: fixture.owner)
            try await Task.sleep(for: .milliseconds(1100))
            try #require(triggerProbe.completed == nil)
            try #require(!fixture.owner.store.nextDispatchBatch(limit: 100).isEmpty)
            fixture.owner.foregroundBridge?.enabled = true
            fixture.owner.model.localWriteHappened()
            triggerProbe.stop()
            try await fixture.finish()
            print("foreground-latency proof: negative complete")
        } catch { emitFailure(error); try? await fixture.finish(); throw error }
    }

    @Test func foregroundReconnectAndRenewalHaveSeparateDistribution() async throws {
        guard ForegroundFixture.enabled else { return }
        // A bounded hint-only diagnostic, explicitly separate from healthy.
        let fixture = try ForegroundFixture(pollOverride: .seconds(120))
        do {
            try await fixture.start()
            let diagnostic = try await measureForegroundSaveToPeer(owner: fixture.owner, peer: fixture.peer, samples: 3)
            diagnostic.emit("hint_only_override_120s", expected: 3)
            let tasks = await fixture.peer.session.allTasks
            let socket = try #require(tasks.compactMap { $0 as? URLSessionWebSocketTask }.first)
            let dialCount = fixture.peer.traffic.liveDials.count
            let dropped = DispatchTime.now().uptimeNanoseconds
            socket.cancel(with: .goingAway, reason: nil)
            try await waitFor(seconds: 15) {
                fixture.peer.traffic.liveDials.count > dialCount && (try? fixture.peer.engine().liveState) == .connected
            }
            emitDuration("dropped_socket_reconnect", since: dropped)
            // Foreground return/offline backlog measured independently.
            fixture.peer.model.sceneEnteredBackground()
            try await waitFor(seconds: 5) { (try? fixture.peer.engine().liveState) == .stopped }
            // A directly invoked background engine pull, not an OS/APNs
            // scheduling claim. Drain reconnect work before starting its clock.
            try await fixture.peer.sync(.backgroundPush)
            let backgroundProbe = ArrivalProbe(peer: fixture.peer)
            let backgroundRecord = try backgroundProbe.save(owner: fixture.owner)
            try await waitFor(seconds: 5) { (try? fixture.owner.store.nextDispatchBatch(limit: 100).isEmpty) == true }
            try #require(backgroundProbe.completed == nil)
            let backgroundPull = DispatchTime.now().uptimeNanoseconds
            try await fixture.peer.sync(.backgroundPush)
            try await waitFor(seconds: 5) { backgroundProbe.completed != nil }
            emitDuration("invoked_background_pull", since: backgroundPull)
            ForegroundLatencyReport(pairs: [try #require(backgroundProbe.completed)], elapsed: secondsSince(backgroundProbe.started))
                .emit("background_engine_pull", expected: 1)
            try backgroundProbe.verify(backgroundRecord)
            backgroundProbe.stop()
            let probe = ArrivalProbe(peer: fixture.peer)
            let record = try probe.save(owner: fixture.owner)
            try await Task.sleep(for: .milliseconds(1100))
            try #require(probe.completed == nil)
            let foreground = DispatchTime.now().uptimeNanoseconds
            await fixture.peer.model.sceneBecameActive()
            try await waitFor(seconds: 10) { probe.completed != nil }
            emitDuration("foreground_return", since: foreground)
            ForegroundLatencyReport(pairs: [try #require(probe.completed)], elapsed: secondsSince(probe.started)).emit("offline_backlog", expected: 1)
            try probe.verify(record)
            probe.stop()
            try await fixture.caughtUp()
            // Expire only the local scheduling metadata; the actual renewer
            // must call the real server and rotate the stored credential.
            let key = try fixture.owner.familyKey()
            let before = try fixture.owner.credentials(key)
            let expired = SessionCredentials(accessToken: before.accessToken, refreshToken: before.refreshToken,
                expiresAt: .distantPast, recoveryEpoch: before.recoveryEpoch, deviceContinuity: before.deviceContinuity,
                credentialVersion: before.credentialVersion, installBinding: before.installBinding)
            try KeychainSessionStore(backend: fixture.owner.keychain).saveFamily(expired, for: key)
            let renewal = try await measureForegroundSaveToPeer(owner: fixture.owner, peer: fixture.peer, samples: 1)
            renewal.emit("access_renewal", expected: 1)
            try #require(try fixture.owner.credentials(key).credentialVersion != before.credentialVersion)
            try #require(fixture.ownerRecovery.count > 0)
            // Durable sign-out gate: automatic local writes must not transmit.
            try await fixture.caughtUp()
            try fixture.owner.store.markAccountSignedOut(key.accountSessionKey.leaseKey)
            let requests = fixture.owner.traffic.syncRequests
            let signedOut = ArrivalProbe(peer: fixture.peer)
            _ = try signedOut.save(owner: fixture.owner)
            try await Task.sleep(for: .milliseconds(1100))
            try #require(signedOut.completed == nil && fixture.owner.traffic.syncRequests == requests)
            signedOut.stop()
            print("foreground-latency proof: signed-out owner refused")
            print("foreground-latency limits: background_execution=unmeasured snapshot_import=setup_only rendered_ui=unmeasured physical_device=unmeasured")
            try await fixture.finish()
            print("foreground-latency proof: recovery complete")
        } catch { emitFailure(error); try? await fixture.finish(); throw error }
    }
}

struct ForegroundLatencyReport {
    struct Pair { let durable: Double; let observable: Double; let publication: Double }
    let pairs: [Pair]
    let elapsed: Double
    var durableP95: Double { percentile(pairs.map(\.durable), 0.95) }
    var observableP95: Double { percentile(pairs.map(\.observable), 0.95) }
    func emit(_ distribution: String, expected: Int) {
        for (index, pair) in pairs.enumerated() {
            let row: [String: Any] = ["distribution": distribution, "ordinal": index + 1,
                "durable_seconds": pair.durable, "observable_seconds": pair.observable, "publication_seconds": pair.publication]
            let data = try! JSONSerialization.data(withJSONObject: row, options: [.sortedKeys])
            print("foreground-latency pair: " + String(decoding: data, as: UTF8.self))
        }
        let row: [String: Any] = ["distribution": distribution, "count": pairs.count, "expected": expected,
            "elapsed_seconds": elapsed, "durable_p50_seconds": percentile(pairs.map(\.durable), 0.5),
            "durable_p95_seconds": durableP95, "durable_p99_seconds": percentile(pairs.map(\.durable), 0.99),
            "observable_p95_seconds": observableP95, "publication_p95_seconds": percentile(pairs.map(\.publication), 0.95)]
        let data = try! JSONSerialization.data(withJSONObject: row, options: [.sortedKeys])
        print("foreground-latency sample: " + String(decoding: data, as: UTF8.self))
    }
}

private func percentile(_ values: [Double], _ fraction: Double) -> Double {
    guard !values.isEmpty else { return 0 }
    return values.sorted()[max(0, Int(ceil(Double(values.count) * fraction)) - 1)]
}
private func secondsSince(_ start: UInt64) -> Double { Double(DispatchTime.now().uptimeNanoseconds - start) / 1e9 }
private func emitDuration(_ distribution: String, since start: UInt64) {
    print("foreground-latency timing: distribution=\(distribution) elapsed_seconds=\(secondsSince(start))")
}

@MainActor
func measureForegroundSaveToPeer(owner: Phone, peer: Phone, samples: Int) async throws -> ForegroundLatencyReport {
    let began = DispatchTime.now().uptimeNanoseconds
    var pairs: [ForegroundLatencyReport.Pair] = []
    let baselineOwner = try owner.history().diapers.count
    let baselinePeer = try peer.history().diapers.count
    let probe = ArrivalProbe(peer: peer)
    defer { probe.stop() }
    for _ in 0..<samples {
        if secondsSince(began) >= 120 { break }
        // Arrival pacing stays below the unchanged 120 pulls/minute quota.
        // It is outside each save-to-peer interval, inside the 120s budget.
        let target = Double(pairs.count) * 0.55
        if samples >= 200 && secondsSince(began) < target {
            try await Task.sleep(for: .seconds(target - secondsSince(began)))
        }
        let id = try probe.save(owner: owner)
        do { try await waitFor(seconds: 10) { probe.completed != nil } } catch {
            ForegroundLatencyReport(pairs: pairs, elapsed: secondsSince(began)).emit("incomplete", expected: samples)
            throw error
        }
        try probe.verify(id)
        pairs.append(try #require(probe.completed))
        try await waitFor(seconds: 5) {
            (try? owner.store.nextDispatchBatch(limit: 100).isEmpty) == true &&
            owner.model.engine?.status != .syncing && peer.model.engine?.status != .syncing
        }
    }
    try #require(try owner.history().diapers.count == baselineOwner + pairs.count)
    try #require(try peer.history().diapers.count == baselinePeer + pairs.count)
    return ForegroundLatencyReport(pairs: pairs, elapsed: secondsSince(began))
}

/// Uses the Task1 post-save milestone, actual Observation notification and a
/// fresh ModelContext. IDs stay in process and are never diagnostic output.
@MainActor
private final class ArrivalProbe {
    let peer: Phone
    var started: UInt64 = 0
    var expected: UUID?
    var durable: UInt64?
    var publication: UInt64?
    var observed: UInt64?
    var completed: ForegroundLatencyReport.Pair? {
        guard let durable, let publication, let observed else { return nil }
        return .init(durable: Double(durable - started) / 1e9, observable: Double(observed - started) / 1e9,
                     publication: Double(publication - started) / 1e9)
    }
    let previous: (@MainActor (StoreSyncMilestone) -> Void)?
    init(peer: Phone) {
        self.peer = peer
        previous = peer.store.onSyncMilestone
        peer.store.onSyncMilestone = { [weak self] milestone in
            guard let self else { return }
            self.previous?(milestone)
            guard self.expected != nil, self.containsExpected() else { return }
            if milestone.phase == .remoteCommit && self.durable == nil { self.durable = milestone.monotonicNanoseconds }
            if milestone.phase == .observablePublish && self.publication == nil { self.publication = milestone.monotonicNanoseconds }
        }
        peer.foregroundBridge?.onRevision = { [weak self] at in
            guard let self, self.expected != nil, self.observed == nil, let durable = self.durable, at >= durable, self.containsExpected() else { return }
            self.observed = at
        }
    }
    func stop() { peer.store.onSyncMilestone = previous; peer.foregroundBridge?.onRevision = nil }
    func save(owner: Phone) throws -> UUID {
        expected = nil; durable = nil; publication = nil; observed = nil
        let baby = try #require(try owner.history().babies.first)
        let ownerHook = owner.store.onSyncMilestone
        var committedAt: UInt64?
        owner.store.onSyncMilestone = { milestone in
            ownerHook?(milestone)
            if milestone.phase == .localCommit { committedAt = milestone.monotonicNanoseconds }
        }
        defer { owner.store.onSyncMilestone = ownerHook }
        let row = try owner.store.logDiaper(.mixed, for: baby.id, consistency: "soft")
        started = try #require(committedAt, "local save milestone missing")
        expected = row.id
        return row.id
    }
    func containsExpected() -> Bool {
        guard let expected else { return false }
        let descriptor = FetchDescriptor<DiaperEntry>(predicate: #Predicate { $0.id == expected })
        return (try? peer.store.context.fetch(descriptor).count) == 1
    }
    func verify(_ id: UUID) throws {
        let context = ModelContext(peer.store.container)
        let rows = try context.fetch(FetchDescriptor<DiaperEntry>(predicate: #Predicate { $0.id == id }))
        try #require(rows.count == 1, "lost or duplicate durable row")
        try #require(rows[0].realmKey == peer.store.activeRealm.encodedKey && !rows[0].isTombstoned, "cross-realm row")
        try #require(rows[0].contents == .mixed && rows[0].consistency == "soft")
    }
}

/// RootView's revision -> localWriteHappened trigger, without a rendered view.
@MainActor
final class ForegroundObservationBridge {
    weak var phone: Phone?
    var enabled = true
    var stopped = false
    var onRevision: ((UInt64) -> Void)?
    init(_ phone: Phone) { self.phone = phone; arm() }
    func arm() {
        guard let phone, !stopped else { return }
        withObservationTracking { _ = phone.store.revision } onChange: { [weak self] in
            let at = DispatchTime.now().uptimeNanoseconds
            Task { @MainActor in
                guard let self, !self.stopped else { return }
                self.onRevision?(at)
                if self.enabled { self.phone?.model.localWriteHappened() }
                self.arm()
            }
        }
    }
}

private final class ForegroundDiagnostics: SyncDiagnosticSink, @unchecked Sendable {
    let lock = NSLock()
    var events: [(SyncDiagnosticEvent, UInt64)] = []
    func record(_ event: SyncDiagnosticEvent) {
        lock.withLock { if events.count < 4096 { events.append((event, DispatchTime.now().uptimeNanoseconds)) } }
    }
    var eventCount: Int { lock.withLock { events.count } }
    var failures: Int { lock.withLock { events.filter { $0.0.status != .success }.count } }
    func emit(_ role: String) {
        let values = lock.withLock { events }
        var starts: [UUID: UInt64] = [:], requests: [Double] = []
        for (event, at) in values {
            if event.phase == .requestStart { starts[event.correlation] = at }
            if event.phase == .requestComplete, let start = starts.removeValue(forKey: event.correlation) {
                requests.append(Double(at - start) / 1e9)
            }
        }
        print("foreground-latency http: scope=fixture failures=\(failures) role=\(role) count=\(requests.count) p95_seconds=\(percentile(requests, 0.95)) events=\(values.count)")
    }
}
@MainActor private final class RecoveryCounter { var count = 0 }

@MainActor
private final class ForegroundFixture {
    static var enabled: Bool { ProcessInfo.processInfo.environment["BABY_TRACKER_FOREGROUND_LATENCY"] == "1" }
    static var latest: [String: SessionCredentials] = [:]
    static var finishedCases = 0
    let scratch: URL
    let owner: Phone
    let peer: Phone
    let ownerRecovery = RecoveryCounter()
    let peerRecovery = RecoveryCounter()
    let ownerDiagnostics = ForegroundDiagnostics()
    let peerDiagnostics = ForegroundDiagnostics()
    let peerHints: Bool
    let live: LiveServer
    let clock: TestDateProvider
    var isolationPhone: Phone?
    var isolationRecord: UUID?
    var finished = false

    init(peerHints: Bool = true, pollOverride: Duration? = nil) throws {
        self.peerHints = peerHints
        let env = ProcessInfo.processInfo.environment
        let base = try #require(env["BABY_TRACKER_E2E_BASE"])
        let statePath = try #require(env["BABY_TRACKER_E2E_STATE"])
        let certPath = try #require(env["BABY_TRACKER_E2E_CERT"])
        let instance = try #require(env["BABY_TRACKER_E2E_INSTANCE"].flatMap(UUID.init(uuidString:)))
        let raw = try #require(JSONSerialization.jsonObject(with: Data(contentsOf: URL(fileURLWithPath: statePath))) as? [String: Any])
        var bundles: [String: SessionBundle] = [:], sessions: [String: Data] = [:]
        for name in ["Owner", "Caregiver"] {
            let data = try JSONSerialization.data(withJSONObject: #require(raw[name]))
            bundles[name] = try JSONDecoder().decode(WireSessionBundle.self, from: data).value
            sessions[name] = data
        }
        let seed = try #require(bundles["Owner"])
        let environment = E2EEnvironment(baseURL: base, serverInstanceID: instance,
            certificate: try Data(contentsOf: URL(fileURLWithPath: certPath)), outsiderAccessToken: seed.accessToken,
            outsiderRefreshToken: seed.refreshToken, outsiderIdentity: SessionIdentityFields(accountID: seed.accountID,
            deviceID: seed.deviceID, recoveryEpoch: seed.recoveryEpoch), sessions: sessions)
        live = try LiveServer(environment)
        scratch = URL(fileURLWithPath: statePath).deletingLastPathComponent().appending(path: "foreground-latency-stores")
        try FileManager.default.createDirectory(at: scratch, withIntermediateDirectories: true)
        clock = TestDateProvider(Date().addingTimeInterval(-3600))
        let ownerCounter = ownerRecovery, peerCounter = peerRecovery
        owner = try Phone(name: "foreground-owner", live: live, clock: clock, directory: scratch,
            appleSubject: "10000000-0000-4000-8000-000000000001", pollOverride: pollOverride,
            diagnostics: ownerDiagnostics, recoveryDiagnostics: { _ in ownerCounter.count += 1 })
        peer = try Phone(name: "foreground-peer", live: live, clock: clock, directory: scratch,
            appleSubject: "10000000-0000-4000-8000-000000000002", pollOverride: pollOverride,
            liveHintsEnabled: peerHints, diagnostics: peerDiagnostics, recoveryDiagnostics: { _ in peerCounter.count += 1 })
        for (name, phone) in [("Owner", owner), ("Caregiver", peer)] {
            let bundle = try #require(bundles[name])
            let key = FamilySessionKey(canonicalOrigin: live.endpoint.origin.canonicalString, serverInstanceID: instance,
                familyID: try #require(bundle.familyID), accountID: bundle.accountID, deviceID: bundle.deviceID, recoveryEpoch: bundle.recoveryEpoch)
            let credentials = try Self.latest[name] ?? SessionCredentials(accessToken: bundle.accessToken, refreshToken: bundle.refreshToken,
                expiresAt: bundle.expiresAt, recoveryEpoch: bundle.recoveryEpoch,
                deviceContinuity: DeviceContinuity(deviceID: bundle.deviceID, credential: #require(bundle.deviceCredential)))
            let keychain = KeychainSessionStore(backend: phone.keychain)
            try phone.store.markAccountSignedIn(key.accountSessionKey.leaseKey, credentialVersion: credentials.credentialVersion) {
                try keychain.saveFamily(credentials, for: key)
                try keychain.saveDeviceContinuity(DeviceContinuityBinding(accountID: key.accountID, continuity: credentials.deviceContinuity), for: key.accountSessionKey.continuityKey)
            }
            FamilySessionRecordStore.save(FamilySessionRecord(endpoint: live.endpoint, familyKey: key), to: phone.defaults)
            phone.model.restoreSession()
            phone.foregroundBridge = ForegroundObservationBridge(phone)
        }
    }
    func seedIsolationFamily() async throws {
        let phone = try Phone(name: "isolation", live: live, clock: clock, directory: scratch,
            appleSubject: UUID().uuidString.lowercased(), pollOverride: nil, liveHintsEnabled: false)
        isolationPhone = phone
        let baby = try phone.store.addBaby(name: "Synthetic control", birthDate: clock.now.addingTimeInterval(-86400), referenceSex: .girls, expectedDueDate: nil)
        isolationRecord = try phone.store.logDiaper(.wet, for: baby.id).id
        _ = try await phone.signIn(displayName: "Synthetic control", live: live)
        try await phone.model.createFamily(name: "Synthetic control")
        try phone.expectUpToDate("after control import")
        try #require(phone.traffic.liveDials.isEmpty)
    }
    func verifyIsolationFamily() async throws {
        let control = try #require(isolationPhone), id = try #require(isolationRecord)
        try await control.sync()
        try #require(try control.history().diapers.map(\.id) == [id])
        try #require(try control.familyKey().familyID != owner.familyKey().familyID)
        for phone in [owner, peer] {
            try #require(try phone.store.context.fetch(FetchDescriptor<DiaperEntry>(predicate: #Predicate { $0.id == id })).isEmpty)
            try #require(try phone.history().babies.count == 1)
        }
        try #require(control.traffic.liveDials.isEmpty)
        print("foreground-latency proof: isolation families=2 measured_sockets=2 cross_family_records=0")
    }
    func start() async throws {
        print("foreground-latency setup: start")
        let setup = DispatchTime.now().uptimeNanoseconds
        // The first case takes a genuine snapshot; later serialized cases
        // reopen the same disk stores and current credentials. This works in
        // any case order and respects the two-active-snapshots/account quota.
        let needsSnapshot = try owner.store.syncCursor(in: owner.familyKey().realmID) == nil
        try await owner.engine().activateAsJoiner()
        print("foreground-latency setup: owner snapshot complete")
        try await peer.engine().activateAsJoiner()
        emitDuration(needsSnapshot ? "initial_snapshot" : "existing_store_activation", since: setup)
        let dial = DispatchTime.now().uptimeNanoseconds
        owner.model.engine?.startForegroundPolling()
        peer.model.engine?.startForegroundPolling()
        try await waitFor(seconds: 15) {
            (try? self.owner.engine().liveState) == .connected &&
            (!self.peerHints || (try? self.peer.engine().liveState) == .connected)
        }
        try await caughtUp()
        emitDuration("initial_connect", since: dial)
        try #require(owner.traffic.liveDials.count == 1)
        if peerHints { try #require(peer.traffic.liveDials.count == 1) }
        ownerRecovery.count = 0; peerRecovery.count = 0
    }
    func caughtUp() async throws {
        try await waitFor(seconds: 10) {
            self.owner.model.engine?.status != .syncing && self.peer.model.engine?.status != .syncing &&
            (try? self.owner.store.nextDispatchBatch(limit: 100).isEmpty) == true &&
            (try? self.peer.store.nextDispatchBatch(limit: 100).isEmpty) == true
        }
        try owner.expectUpToDate("before measurement"); try peer.expectUpToDate("before measurement")
    }
    func versions() throws -> [UUID] {
        try [owner.credentials(owner.familyKey()).credentialVersion, peer.credentials(peer.familyKey()).credentialVersion]
    }
    func finish() async throws {
        guard !finished else { return }; finished = true
        for (name, phone) in [("Owner", owner), ("Caregiver", peer)] {
            if let key = try? phone.familyKey(), let credentials = try? phone.credentials(key) { Self.latest[name] = credentials }
            phone.foregroundBridge?.stopped = true
            await phone.model.engine?.stop()
            phone.session.invalidateAndCancel()
            phone.forgetDefaults()
        }
        if let isolationPhone {
            await isolationPhone.model.engine?.stop()
            isolationPhone.session.invalidateAndCancel(); isolationPhone.forgetDefaults()
        }
        ownerDiagnostics.emit("owner"); peerDiagnostics.emit("peer")
        Self.finishedCases += 1
        if Self.finishedCases == 3 { try FileManager.default.removeItem(at: scratch) }
    }
}

@MainActor
private func waitFor(seconds: Double, _ condition: () -> Bool) async throws {
    let start = DispatchTime.now().uptimeNanoseconds
    while !condition() {
        guard secondsSince(start) < seconds else { throw E2EFailure("bounded foreground wait expired") }
        try await Task.sleep(for: .milliseconds(5))
    }
}

// Only bounded error kinds/codes survive disposable log redaction.
private func emitFailure(_ error: any Error) {
    if let code = error as? SyncErrorCode {
        print("foreground-latency failure: sync_code=\(code.rawValue)")
    } else if error is CredentialBoundAuthFailure {
        print("foreground-latency failure: credential_bound_auth")
    } else if error is DecodingError {
        print("foreground-latency failure: decoding")
    } else if error is E2EFailure {
        print("foreground-latency failure: bounded_wait_or_fixture")
    } else {
        print("foreground-latency failure: assertion_or_store")
    }
}
