import BabyCore
import BabyProtocol
@testable import BabyStore
@testable import BabySync
import Foundation
@testable import SettingsFeature
import SwiftData
import Testing

/// Owned synthetic source harness. No rendered UI or physical-phone claim.
@MainActor
@Suite("Bounded foreground capacity matrix", .serialized)
struct CapacityMatrixE2ETests {
    @Test func childProtocolRejectsOverflowWrongCompletionAndTimeout() async throws {
        let root = FileManager.default.temporaryDirectory.appending(path: "capacity-child-" + UUID().uuidString)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        do {
            try await capacityControl(root: root, cell: 1, starting: true, acknowledgementSeconds: 0.02)
            Issue.record("missing control ACK accepted")
        } catch is CapacityTimeout {}
        let child = try CapacityChild(executable: "/usr/bin/python3", arguments: ["-u", "-c", "import json,time; print(json.dumps({'kind':'cell_start'})); time.sleep(60)"], directory: root)
        defer { child.stop() }
        try await capacityWait(Date().addingTimeInterval(2)) { try child.poll(); return child.started }
        for operation in 1...64 { try child.send(["operation": operation]) }
        #expect(throws: CapacityProtocolFailure.self) { try child.send(["operation": 65]) }
        do {
            try await capacityWait(Date().addingTimeInterval(0.02)) { try child.poll(); return !child.takeCompleted().isEmpty }
            Issue.record("missing completion did not time out")
        } catch is CapacityTimeout {}
        child.stop()
        #expect(!child.process.isRunning)
        let wrong = try CapacityChild(executable: "/usr/bin/python3", arguments: ["-u", "-c", "import json,time; print(json.dumps({'kind':'cell_start'})); print(json.dumps({'kind':'operation_complete','operation':999})); time.sleep(60)"], directory: root)
        defer { wrong.stop() }
        do {
            try await capacityWait(Date().addingTimeInterval(2)) { try wrong.poll(); return wrong.finished }
            Issue.record("unrequested completion accepted")
        } catch is CapacityProtocolFailure {}
        wrong.stop()
        #expect(!wrong.process.isRunning)
    }

    @Test func pacedBudgetNeverClaimsCompletedHistoryOrPairs() async throws {
        #expect(capacityPacedUpperBound(head: 7, seconds: 116, spacing: 6) == 27)
        #expect(capacityPacedUpperBound(head: 7, seconds: 0, spacing: 6) == 7)
        #expect(capacityPacedUpperBound(head: 247, seconds: 6, spacing: 6) == 248)
        // The server's configured refresh quotas set the pace, not a constant:
        // at the matrix's 6 ms address spacing a cell can age past 2048.
        #expect(capacityPacedUpperBound(head: 2, seconds: 100, spacing: 0.006) >= 2048)
        let pacing = try CapacityRotationPacing(["BABY_TRACKER_MATRIX_ROTATION_IP_SECONDS": "0.006", "BABY_TRACKER_MATRIX_ROTATION_DEVICE_SECONDS": "0.06"])
        #expect(pacing.address == 0.006 && pacing.device == 0.06)
        for missing in [[:], ["BABY_TRACKER_MATRIX_ROTATION_IP_SECONDS": "0.006"], ["BABY_TRACKER_MATRIX_ROTATION_IP_SECONDS": "0", "BABY_TRACKER_MATRIX_ROTATION_DEVICE_SECONDS": "1"], ["BABY_TRACKER_MATRIX_ROTATION_IP_SECONDS": "nan", "BABY_TRACKER_MATRIX_ROTATION_DEVICE_SECONDS": "1"]] {
            #expect(throws: CapacityProtocolFailure.self) { try CapacityRotationPacing(missing) }
        }
        let cell = CapacityCell(ordinal: 1, events: 248, families: 1, concurrency: 1)
        cell.actualEvents = 7
        cell.plannedEventUpperBound = capacityPacedUpperBound(head: 7, seconds: 116, spacing: 6)
        cell.completion = "paced_setup_budget_insufficient"
        #expect(cell.actualEvents == 7 && cell.plannedEventUpperBound < cell.events)
        #expect(cell.pairs.isEmpty && cell.requestSeconds.isEmpty && cell.attempted.values.allSatisfy { $0 == 0 })
        // A completed pair followed by a thrown timeout retains its interval.
        do {
            cell.beginMeasurement()
            defer { cell.finishMeasurement() }
            cell.pairs.append(["operation": 1, "durable_seconds": 0.001, "observable_seconds": 0.002])
            try await Task.sleep(for: .milliseconds(10))
            throw CapacityTimeout()
        } catch is CapacityTimeout {}
        #expect(cell.elapsed >= 0.01 && cell.pairs.count == 1)
    }

    /// A 128-family cell warmed its families one save-to-peer (~0.4 s) at a
    /// time: 55 s, leaving 24 s of the 116 s cell for a 30 s measurement.
    @Test func warmupOverlapsFamiliesWithinItsWidth() async throws {
        var active = 0, peak = 0, warmed: [Int] = []
        let began = ContinuousClock.now
        try await capacityEachFamily(families: 16, width: 8) { family in
            active += 1; peak = max(peak, active)
            try await Task.sleep(for: .milliseconds(100))
            active -= 1; warmed.append(family)
        }
        #expect(began.duration(to: .now) < .milliseconds(700))
        #expect(peak == 8 && warmed.sorted() == Array(0..<16))
        await #expect(throws: CapacityTimeout.self) {
            try await capacityEachFamily(families: 4, width: 2) { family in if family == 2 { throw CapacityTimeout() } }
        }
    }

    @Test func capacityMatrix() async throws {
        let env = ProcessInfo.processInfo.environment
        guard env["BABY_TRACKER_CAPACITY_MATRIX"] == "1" else { return }
        print("capacity-matrix stage: configuration_started"); fflush(stdout)
        let required = ["BABY_TRACKER_MATRIX_PRIVATE", "BABY_TRACKER_E2E_BASE", "BABY_TRACKER_E2E_INSTANCE", "BABY_TRACKER_E2E_CERT", "BABY_TRACKER_MATRIX_DEADLINE", "BABY_TRACKER_MATRIX_ROTATION_IP_SECONDS", "BABY_TRACKER_MATRIX_ROTATION_DEVICE_SECONDS"]
        guard required.allSatisfy({ env[$0] != nil }) else {
            print("capacity-matrix failure: missing_configuration"); fflush(stdout)
            throw CapacityProtocolFailure()
        }
        let root = URL(fileURLWithPath: try #require(env["BABY_TRACKER_MATRIX_PRIVATE"]))
        let live = try LiveServer(E2EEnvironment(baseURL: try #require(env["BABY_TRACKER_E2E_BASE"]),
            serverInstanceID: try #require(env["BABY_TRACKER_E2E_INSTANCE"].flatMap(UUID.init(uuidString:))),
            certificate: try Data(contentsOf: URL(fileURLWithPath: #require(env["BABY_TRACKER_E2E_CERT"]))),
            outsiderAccessToken: "", outsiderRefreshToken: "",
            outsiderIdentity: SessionIdentityFields(accountID: UUID(), deviceID: UUID(), recoveryEpoch: 1), sessions: [:]))
        let globalDeadline = Date(timeIntervalSince1970: try #require(env["BABY_TRACKER_MATRIX_DEADLINE"].flatMap(Double.init)))
        let pacing = try CapacityRotationPacing(env)
        print("capacity-matrix stage: configuration_ready"); fflush(stdout)
        var ordinal = 0
        var stoppedForInvariant = false
        for events in [248, 512, 2048] {
            for families in [1, 32, 128] {
                for concurrency in [1, 16, 64] {
                    ordinal += 1
                    let cell = CapacityCell(ordinal: ordinal, events: events, families: families, concurrency: concurrency)
                    if stoppedForInvariant { cell.completion = "unfinished_after_invariant"; cell.emit(); continue }
                    if Date() >= globalDeadline {
                        cell.completion = "global_budget_exhausted"; cell.emit(); continue
                    }
                    try await capacityControl(root: root, cell: ordinal, starting: true)
                    cell.checkpointURL = root.appending(path: "checkpoint-\(ordinal).json")
                    try cell.checkpoint()
                    print("capacity-matrix start: \(ordinal)"); fflush(stdout)
                    let deadline = min(globalDeadline, Date().addingTimeInterval(116))
                    var phones: [Phone] = []
                    let directory = root.appending(path: "cell-\(ordinal)")
                    try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
                    do {
                        // Families are independent: provisioning them one at a time took
                        // 37-80 s of a 128-family cell (host-dependent) and once left no
                        // 30 s measurement. Four at a time; phones keep family order.
                        var provisioned: [Int: [Phone]] = [:]
                        try await capacityEachFamily(families: families, width: 4) { family in
                            try capacityCheck(deadline)
                            let clock = TestDateProvider(Date().addingTimeInterval(-3600))
                            let owner = try Phone(name: "owner-\(family)", live: live, clock: clock, directory: directory,
                                appleSubject: String(format: "82000000-0000-4000-%04d-%012d", ordinal, family * 2), pollOverride: nil, recoveryDiagnostics: { cell.recovery($0) })
                            let peer = try Phone(name: "peer-\(family)", live: live, clock: clock, directory: directory,
                                appleSubject: String(format: "82000000-0000-4000-%04d-%012d", ordinal, family * 2 + 1), pollOverride: nil, recoveryDiagnostics: { cell.recovery($0) })
                            phones += [owner, peer]; provisioned[family] = [owner, peer]
                            let domains = root.appending(path: "defaults.jsonl")
                            if !FileManager.default.fileExists(atPath: domains.path) { FileManager.default.createFile(atPath: domains.path, contents: nil, attributes: [.posixPermissions: 0o600]) }
                            let handle = try FileHandle(forWritingTo: domains); try handle.seekToEnd()
                            try handle.write(contentsOf: Data((owner.suiteName + "\n" + peer.suiteName + "\n").utf8)); try handle.close()
                            let baby = try owner.store.addBaby(name: "Synthetic", birthDate: clock.now.addingTimeInterval(-86400), referenceSex: .girls, expectedDueDate: nil)
                            for _ in 0..<10 { _ = try owner.store.logDiaper(.wet, for: baby.id) }
                            try cell.stage("owner_sign_in", family: family)
                            _ = try await owner.signIn(displayName: "Synthetic", live: live)
                            try capacityCheck(deadline)
                            try cell.stage("owner_family_activation", family: family)
                            try await owner.model.createFamily(name: "Synthetic")
                            try cell.stage("invitation_create", family: family)
                            let invitation = try await owner.model.createInvitation()
                            try cell.stage("peer_sign_in", family: family)
                            _ = try await peer.signIn(displayName: "Synthetic", live: live)
                            try capacityCheck(deadline)
                            try cell.stage("peer_join_activation", family: family)
                            try await peer.model.acceptInvitation(InvitationParser.parse(invitation.shareURL.absoluteString))
                            try cell.stage("scene_activation", family: family)
                            for phone in [owner, peer] {
                                phone.foregroundBridge = ForegroundObservationBridge(phone)
                                await phone.model.sceneBecameActive()
                            }
                            try cell.stage("two_socket_admission", family: family)
                            try await capacityWait(deadline) { (try? owner.engine().liveState) == .connected && (try? peer.engine().liveState) == .connected }
                            for phone in [owner, peer] {
                                let sockets = await phone.session.allTasks.compactMap { $0 as? URLSessionWebSocketTask }.filter { $0.state == .running }
                                guard sockets.count == 1 else { throw CapacityInvariant() }
                            }
                            try cell.stage("connected_poll_default", family: family)
                            guard try owner.engine().foregroundPollInterval == FamilySyncEngine.defaultLivePollInterval && peer.engine().foregroundPollInterval == FamilySyncEngine.defaultLivePollInterval else { throw CapacityInvariant() }
                            try cell.stage("family_ready", family: family, state: "complete")
                            cell.actualFamilies += 1
                        }
                        phones = try (0..<families).flatMap { try #require(provisioned[$0]) }
                        // Age by genuine credential rotations through the app renewer.
                        // Never insert SQL rows, bypass quotas, or manufacture events.
                        try cell.stage("authority_aging", family: 0)
                        cell.actualEvents = try capacityHead(env)
                        cell.plannedEventUpperBound = capacityPacedUpperBound(head: cell.actualEvents, seconds: deadline.timeIntervalSinceNow, spacing: pacing.address)
                        if cell.plannedEventUpperBound < events { throw CapacityPacedBudgetInsufficient() }
                        var rotation = 0
                        var lastRotation: [Int: Date] = [:]
                        var lastIPRotation = Date()
                        while cell.actualEvents < events {
                            try capacityCheck(deadline)
                            let phoneIndex = rotation % phones.count
                            let earliest = max((lastRotation[phoneIndex] ?? .distantPast).addingTimeInterval(pacing.device), lastIPRotation.addingTimeInterval(pacing.address))
                            guard earliest < deadline else { throw CapacityTimeout() }
                            if earliest > Date() { try await Task.sleep(for: .seconds(earliest.timeIntervalSinceNow)) }
                            try capacityCheck(deadline)
                            lastRotation[phoneIndex] = Date(); lastIPRotation = Date()
                            let phone = phones[phoneIndex]
                            let key = try phone.familyKey(), old = try phone.credentials(key)
                            let expired = SessionCredentials(accessToken: old.accessToken, refreshToken: old.refreshToken,
                                expiresAt: .distantPast, recoveryEpoch: old.recoveryEpoch, deviceContinuity: old.deviceContinuity,
                                credentialVersion: old.credentialVersion, installBinding: old.installBinding)
                            try KeychainSessionStore(backend: phone.keychain).saveFamily(expired, for: key)
                            do { try await phone.sync() } catch {
                                switch phone.model.engine?.status {
                                case .idle: cell.engineFailureState = "idle"
                                case .syncing: cell.engineFailureState = "syncing"
                                case .upToDate: cell.engineFailureState = "up_to_date"
                                case .offline: cell.engineFailureState = "offline"
                                case .serverUnreachable: cell.engineFailureState = "server_unreachable"
                                case .needsSignIn: cell.engineFailureState = "needs_sign_in"
                                case .failed: cell.engineFailureState = "failed"
                                case nil: cell.engineFailureState = "unavailable"
                                }
                                throw error
                            }
                            guard try phone.credentials(key).credentialVersion != old.credentialVersion else { throw CapacityInvariant() }
                            rotation += 1
                            try cell.stage("authority_aging", family: 0)
                        cell.actualEvents = try capacityHead(env)
                        }
                        for phone in phones {
                            try await capacityWait(deadline) { (try? phone.engine().liveState) == .connected }
                        }
                        // Care history is already seeded. Warm complete app operations.
                        // Families are independent; warming them serially left a
                        // 128-family cell without time to measure. The width keeps
                        // the one harness address within its body-read share.
                        try await capacityEachFamily(families: families, width: 8) { family in
                            try capacityCheck(deadline)
                            _ = try await measureForegroundSaveToPeer(owner: phones[family * 2], peer: phones[family * 2 + 1], samples: 1)
                        }
                        try capacityCheck(deadline)
                        try cell.stage("measurement", family: 0)
                        try await cell.measure(phones: phones, directory: directory, env: env, deadline: deadline)
                    } catch {
                        if error is CapacityPacedBudgetInsufficient {
                            cell.completion = "paced_setup_budget_insufficient"
                        } else {
                            cell.record(error)
                            if error is CapacityInvariant { stoppedForInvariant = true }
                            cell.completion = Date() >= deadline ? "cell_budget_exhausted" : "setup_or_measurement_refused"
                        }
                    }
                    cell.actualEvents = (try? capacityHead(env)) ?? cell.actualEvents
                    for phone in phones {
                        for dial in phone.traffic.liveDials {
                            cell.liveStatuses[dial.status.map(String.init) ?? "no_response", default: 0] += 1
                        }
                    }
                    try cell.checkpoint()
                    for phone in phones {
                        phone.foregroundBridge?.stopped = true
                        await phone.model.engine?.stop()
                        phone.session.invalidateAndCancel(); phone.forgetDefaults()
                    }
                    try cell.checkpoint()
                    try await capacityControl(root: root, cell: ordinal, starting: false)
                    cell.emit()
                }
            }
        }
        live.session.invalidateAndCancel()
        print("capacity-matrix proof: 27 cells reported")
    }
}

@MainActor
private final class CapacityCell {
    let ordinal: Int, events: Int, families: Int, concurrency: Int
    var actualEvents = 0, actualFamilies = 0, plannedEventUpperBound = 0
    var completion = "not_started"
    private let began = ContinuousClock.now
    var stageCode = "not_started", failureCode = "none"
    var engineFailureState = "unobserved", recoveryPhase = "unobserved", recoveryReason = "unobserved"
    var recoveryRetries = 0
    var liveStatuses: [String: Int] = [:]
    func recovery(_ event: RefreshRecoveryEvent) {
        recoveryPhase = event.phase.rawValue
        recoveryReason = event.reason?.rawValue ?? "none"
        recoveryRetries += event.retryCount
    }
    var checkpointURL: URL?
    func stage(_ code: String, family: Int, state: String = "started") throws {
        stageCode = code
        let elapsed = began.duration(to: .now).components
        let seconds = Double(elapsed.seconds) + Double(elapsed.attoseconds) / 1e18
        let row: [String: Any] = ["cell": ordinal, "family_ordinal": family, "stage": code, "state": state, "elapsed_seconds": seconds]
        let data = try! JSONSerialization.data(withJSONObject: row, options: [.sortedKeys])
        print("capacity-matrix stage: " + String(decoding: data, as: UTF8.self)); fflush(stdout)
        try checkpoint()
    }
    var errors = 0, rateLimits = 0, timeouts = 0
    var attempted = ["empty_pull": 0, "caught_up_read": 0, "save_apply": 0]
    var successful = ["empty_pull": 0, "caught_up_read": 0, "save_apply": 0]
    var requestSeconds: [Double] = [], pairs: [[String: Any]] = []
    var actualNonempty = 0, notCaughtUp = 0
    private var measurementBegan: ContinuousClock.Instant?
    private var finalizedElapsed = 0.0
    var elapsed: Double {
        guard let measurementBegan else { return finalizedElapsed }
        let span = measurementBegan.duration(to: .now).components
        return Double(span.seconds) + Double(span.attoseconds) / 1e18
    }
    func beginMeasurement() { measurementBegan = .now }
    func finishMeasurement() { finalizedElapsed = elapsed; measurementBegan = nil }
    var peakActive = 0, peakWrites = 0, peakRequests = 0
    init(ordinal: Int, events: Int, families: Int, concurrency: Int) {
        self.ordinal = ordinal; self.events = events; self.families = families; self.concurrency = concurrency
    }
    func record(_ error: any Error) {
        if case HTTPClientError.status(429, _) = error { rateLimits += 1; failureCode = "rate_limit" }
        else if error is CapacityTimeout { timeouts += 1; failureCode = "timeout" }
        else if error is CapacityInvariant { errors += 1; failureCode = "invariant" }
        else { errors += 1; failureCode = "setup_or_operation_error" }
    }
    func measure(phones: [Phone], directory: URL, env: [String: String], deadline: Date) async throws {
        let duration = Double(env["BABY_TRACKER_MATRIX_SECONDS"] ?? "30") ?? 30
        try #require(duration >= 30 && duration <= 60)
        guard deadline.timeIntervalSinceNow >= duration else { throw CapacityTimeout() }
        let manifest = directory.appending(path: "credentials.json")
        let tokens = try stride(from: 0, to: phones.count, by: 2).map { ["token": try phones[$0].credentials(phones[$0].familyKey()).accessToken] }
        FileManager.default.createFile(atPath: manifest.path, contents: try JSONSerialization.data(withJSONObject: tokens), attributes: [.posixPermissions: 0o600])
        let child = try CapacityChild(executable: #require(env["BABY_TRACKER_MATRIX_CLIENT"]), arguments: ["--base", #require(env["BABY_TRACKER_E2E_BASE"]), "--cert", #require(env["BABY_TRACKER_E2E_CERT"]), "--cell-manifest", manifest.path, "--concurrency", "\(concurrency)"], directory: directory)
        defer { child.stop(); try? FileManager.default.removeItem(at: manifest) }
        try await capacityWait(min(deadline, Date().addingTimeInterval(10))) { try child.poll(); return child.started }
        beginMeasurement()
        defer { finishMeasurement(); try? checkpoint() }
        let end = min(deadline, Date().addingTimeInterval(duration))
        // Each family has one probe: queued operations in the same family are
        // intentionally serialized through that family's actual single store.
        var busy = Set<Int>(), cursors: [Int: Cursor] = [:], pending: [Int: (Int, String)] = [:], operation = 0
        var saves: [Int: (Int, Task<Void, Never>)] = [:]
        var saveResults: [Int: Result<ForegroundLatencyReport, any Error>] = [:]
        defer { for (_, task) in saves.values { task.cancel() } }
        while Date() < end || !pending.isEmpty || !saves.isEmpty {
            try capacityCheck(deadline)
            try child.poll()
            for row in child.takeCompleted() {
                let id = try #require(row["operation"] as? Int)
                guard let (family, kind) = pending.removeValue(forKey: id) else { throw CapacityProtocolFailure() }
                let status = row["status"] as? String
                if status == "success" {
                    requestSeconds.append(try #require(row["seconds"] as? Double))
                    successful[kind, default: 0] += 1
                    let cursor = try JSONDecoder().decode(Cursor.self, from: JSONSerialization.data(withJSONObject: #require(row["cursor"])))
                    if let previous = cursors[family] {
                        guard previous.serverInstanceID == cursor.serverInstanceID && previous.recoveryEpoch == cursor.recoveryEpoch && previous.familyID == cursor.familyID else { throw CapacityProtocolFailure() }
                        if cursor.sequence > previous.sequence { cursors[family] = cursor }
                    } else { cursors[family] = cursor }
                    if (row["changes"] as? Int ?? 0) > 0 { actualNonempty += 1 }
                } else if status == "rate_limit" { rateLimits += 1 }
                else if status == "timeout" { timeouts += 1 }
                else { errors += 1 }
            }
            for (id, result) in saveResults {
                guard let (family, _) = saves.removeValue(forKey: id) else { throw CapacityProtocolFailure() }
                busy.remove(family)
                switch result {
                case .success(let report):
                    for pair in report.pairs { pairs.append(["operation": id, "durable_seconds": pair.durable, "observable_seconds": pair.observable]) }
                    successful["save_apply", default: 0] += report.pairs.count
                case .failure(let error): record(error)
                }
            }
            let hadSaves = !saveResults.isEmpty
            saveResults.removeAll()
            if hadSaves || !pending.isEmpty { try checkpoint(force: false) }
            // Schedule cap covers both Go requests and complete Swift applies.
            if Date() < end && pending.count + saves.count < concurrency {
                let family = operation % families
                let nextKind = ["empty_pull", "empty_pull", "caught_up_read", "caught_up_read", "save_apply"][operation % 5]
                if nextKind != "save_apply" || !busy.contains(family) {
                    operation += 1
                    let kind = nextKind
                    attempted[kind, default: 0] += 1
                    let owner = phones[family * 2], peer = phones[family * 2 + 1]
                    if kind == "save_apply" {
                        busy.insert(family)
                        let id = operation
                        let task = Task { @MainActor in
                            do { saveResults[id] = .success(try await measureForegroundSaveToPeer(owner: owner, peer: peer, samples: 1)) }
                            catch { saveResults[id] = .failure(error) }
                        }
                        saves[id] = (family, task)
                    } else {
                        let key = try owner.familyKey(), storeCursor = try #require(try owner.store.syncCursor(in: key.realmID))
                        let cursor = (cursors[family]?.sequence ?? -1) > storeCursor.sequence ? cursors[family]! : storeCursor
                        if kind == "caught_up_read" {
                            if try peer.store.syncCursor(in: peer.familyKey().realmID) != cursor || peer.history().diapers.count != owner.history().diapers.count || storeCursor != cursor { notCaughtUp += 1 }
                        }
                        let cursorValue = try JSONSerialization.jsonObject(with: JSONEncoder().encode(cursor))
                        try child.send(["operation": operation, "family": family, "kind": kind, "cursor": cursorValue])
                        pending[operation] = (family, kind)
                    }
                }
            }
            peakActive = max(peakActive, pending.count + saves.count)
            peakWrites = max(peakWrites, saves.count)
            try await Task.sleep(for: .milliseconds(5))
        }
        finishMeasurement()
        try checkpoint()
        child.finishInput()
        try await capacityWait(min(deadline, Date().addingTimeInterval(2))) { try child.poll(); return child.finished }
        peakRequests = child.peakRequests
        completion = "measurement_complete"
        // A fixed offline backlog is a separate observation, never folded into
        // healthy peer percentiles.
        let owner = phones[0], peer = phones[1]
        peer.model.sceneEnteredBackground()
        let baby = try #require(try owner.history().babies.first)
        for _ in 0..<100 { _ = try owner.store.logDiaper(.wet, for: baby.id) }
        try await capacityWait(deadline) { (try? owner.store.nextDispatchBatch(limit: 100).isEmpty) == true }
        let backlogStart = Date()
        await peer.model.sceneBecameActive()
        try await capacityWait(deadline) { (try? peer.history().diapers.count) == (try? owner.history().diapers.count) }
        print("capacity-matrix backlog: {\"cell\":\(ordinal),\"changes\":100,\"seconds\":\(Date().timeIntervalSince(backlogStart))}")
    }
    func snapshot() -> [String: Any] {
        let span = began.duration(to: .now).components
        let row: [String: Any] = ["cell": ordinal, "families": families, "actual_families": actualFamilies, "concurrency": concurrency,
            "requested_events": events, "actual_events": stageCode == "not_started" ? NSNull() : actualEvents as Any, "contention": concurrency > families ? "same_family_contention" : "isolated_families",
            "completion": completion, "failure_stage": stageCode, "failure_code": failureCode, "actual_families_semantics": "fully_ready_families", "engine_failure_state": engineFailureState, "recovery_phase": recoveryPhase, "recovery_reason": recoveryReason, "recovery_retries_observed": recoveryRetries, "errors": errors, "rate_limits": rateLimits, "timeouts": timeouts, "retries": NSNull(),
            "attempted": attempted, "successful": successful, "request_seconds": requestSeconds, "pairs": pairs,
            "elapsed_seconds": Double(span.seconds) + Double(span.attoseconds) / 1e18, "measurement_seconds": elapsed, "planned_event_upper_bound": plannedEventUpperBound, "live_status_counts": liveStatuses, "actual_nonempty_pulls": actualNonempty, "not_caught_up_reads": notCaughtUp,
            "request_categories_wire_equivalent": true, "same_family_serialized": true, "peak_pending_complete_operations": peakActive, "peak_active_requests": peakRequests, "peak_active_writes": peakWrites]
        return row
    }
    private var lastCheckpoint = ContinuousClock.now.advanced(by: .seconds(-1))
    func checkpoint(force: Bool = true) throws {
        guard let checkpointURL else { return }
        if !force && lastCheckpoint.duration(to: .now) < .milliseconds(250) { return }
        // Atomic replacement keeps the latest bounded progress through a kill.
        guard pairs.count <= 10000 && requestSeconds.count <= 10000 else { throw CapacityProtocolFailure() }
        let data = try JSONSerialization.data(withJSONObject: snapshot(), options: [.sortedKeys])
        guard data.count <= 2_000_000 else { throw CapacityProtocolFailure() }
        try data.write(to: checkpointURL, options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: checkpointURL.path)
        lastCheckpoint = .now
    }
    func emit() {
        let data = try! JSONSerialization.data(withJSONObject: snapshot(), options: [.sortedKeys])
        print("capacity-matrix cell: " + String(decoding: data, as: UTF8.self))
        fflush(stdout)
    }
}

private func capacityControl(root: URL, cell: Int, starting: Bool, acknowledgementSeconds: Double = 2) async throws {
    let control = root.appending(path: "control.jsonl")
    if !FileManager.default.fileExists(atPath: control.path) {
        FileManager.default.createFile(atPath: control.path, contents: nil, attributes: [.posixPermissions: 0o600])
    }
    let handle = try FileHandle(forWritingTo: control)
    try handle.seekToEnd()
    var data = try JSONSerialization.data(withJSONObject: ["kind": starting ? "start" : "finish", "cell": cell])
    data.append(10); try handle.write(contentsOf: data); try handle.close()
    guard starting else { return }
    let deadline = ContinuousClock.now.advanced(by: .seconds(acknowledgementSeconds))
    let ack = root.appending(path: "control.ack")
    while ContinuousClock.now < deadline {
        if let data = try? Data(contentsOf: ack), let row = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
           row["kind"] as? String == "start_ack", row["cell"] as? Int == cell { return }
        try await Task.sleep(for: .milliseconds(5))
    }
    throw CapacityTimeout()
}

// Conservative bound for this harness's intentional aging schedule: at least
// `spacing` seconds per address rotation (the server's configured refresh
// quota for the harness's one address, window over count), ignoring the
// stricter per-device spacing and request time. One successful recoverable rotation commits at most one event (none
// since owner decision D1, 2026-10-16: a routine rotation is not audited);
// acknowledgments only update recovery receipt storage. This is a setup plan
// bound, never a measured service-capacity or observed-quota result.
private func capacityPacedUpperBound(head: Int, seconds: Double, spacing: Double) -> Int {
    head + max(0, Int(min(1e9, ceil(max(0, seconds) / spacing))))
}
/// Renewal spacing the runner derives from the refresh quotas it gave the
/// disposable server (auth-refresh-ip and auth-refresh-device, window over
/// count), so aging never meets a refusal and never assumes another pace.
private struct CapacityRotationPacing {
    let address: Double, device: Double
    init(_ env: [String: String]) throws {
        func seconds(_ name: String) throws -> Double {
            guard let value = env[name].flatMap(Double.init), value.isFinite, value > 0 else { throw CapacityProtocolFailure() }
            return value
        }
        address = try seconds("BABY_TRACKER_MATRIX_ROTATION_IP_SECONDS")
        device = try seconds("BABY_TRACKER_MATRIX_ROTATION_DEVICE_SECONDS")
    }
}
/// Runs `body` once per family in batches of `width`; a batch's first error
/// is rethrown after the batch settles, and no later batch starts.
@MainActor private func capacityEachFamily(families: Int, width: Int, _ body: @escaping @MainActor (Int) async throws -> Void) async throws {
    var next = 0
    while next < families {
        let tasks = (next..<min(families, next + width)).map { family in Task { @MainActor in try await body(family) } }
        var failure: (any Error)?
        for task in tasks {
            do { try await task.value } catch { failure = failure ?? error }
        }
        if let failure { throw failure }
        next += width
    }
}
private struct CapacityPacedBudgetInsufficient: Error {}
private struct CapacityInvariant: Error {}
private struct CapacityTimeout: Error {}
private struct CapacityProtocolFailure: Error {}
private func capacityCheck(_ deadline: Date) throws { if Date() >= deadline { throw CapacityTimeout() } }
@MainActor private func capacityWait(_ deadline: Date, _ predicate: () throws -> Bool) async throws {
    while try !predicate() { try capacityCheck(deadline); try await Task.sleep(for: .milliseconds(5)) }
}
private func capacityHead(_ env: [String: String]) throws -> Int {
    let process = Process(), output = Pipe()
    process.executableURL = URL(fileURLWithPath: try #require(env["BABY_TRACKER_MATRIX_HEAD_READER"]))
    process.standardOutput = output; process.standardError = FileHandle.nullDevice
    try process.run()
    let data = output.fileHandleForReading.readDataToEndOfFile(); process.waitUntilExit()
    return try #require(process.terminationStatus == 0 ? Int(String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)) : nil)
}

/// Bounded regular-file stdout avoids pipe backpressure deadlocks. Files live
/// only under this run's owned private directory and are removed by its owner.
@MainActor private final class CapacityChild {
    let process = Process(), input = Pipe(), output: FileHandle, reader: FileHandle
    var buffer = Data(), rows: [[String: Any]] = [], pending = Set<Int>()
    var started = false, finished = false
    var peakRequests = 0
    init(executable: String, arguments: [String], directory: URL) throws {
        let path = directory.appending(path: "child.jsonl")
        FileManager.default.createFile(atPath: path.path, contents: nil, attributes: [.posixPermissions: 0o600])
        output = try FileHandle(forWritingTo: path); reader = try FileHandle(forReadingFrom: path)
        process.executableURL = URL(fileURLWithPath: executable); process.arguments = arguments
        process.standardInput = input; process.standardOutput = output; process.standardError = FileHandle.nullDevice
        try process.run()
    }
    func send(_ row: [String: Any]) throws {
        let id = try #require(row["operation"] as? Int)
        guard !pending.contains(id), pending.count < 64 else { throw CapacityProtocolFailure() }
        pending.insert(id)
        var data = try JSONSerialization.data(withJSONObject: row); data.append(10)
        try input.fileHandleForWriting.write(contentsOf: data)
    }
    func poll() throws {
        buffer.append(try reader.read(upToCount: 65536) ?? Data())
        guard buffer.count <= 131072 else { throw CapacityProtocolFailure() }
        while let end = buffer.firstIndex(of: 10) {
            let line = buffer.prefix(upTo: end); buffer.removeSubrange(...end)
            guard line.count <= 65536, let row = try JSONSerialization.jsonObject(with: line) as? [String: Any] else { throw CapacityProtocolFailure() }
            switch row["kind"] as? String {
            case "cell_start": guard !started else { throw CapacityProtocolFailure() }; started = true
            case "operation_complete":
                guard let id = row["operation"] as? Int, pending.remove(id) != nil, rows.count < 64 else { throw CapacityProtocolFailure() }
                rows.append(row)
            case "cell_finish": guard pending.isEmpty, !finished else { throw CapacityProtocolFailure() }; peakRequests = row["peak_active_requests"] as? Int ?? 0; finished = true
            default: throw CapacityProtocolFailure()
            }
        }
        if !process.isRunning && !finished { throw CapacityProtocolFailure() }
    }
    func takeCompleted() -> [[String: Any]] { defer { rows.removeAll() }; return rows }
    func finishInput() { try? input.fileHandleForWriting.close() }
    func stop() {
        finishInput()
        if process.isRunning { process.terminate(); kill(process.processIdentifier, SIGKILL); process.waitUntilExit() }
        try? reader.close(); try? output.close()
    }
}
