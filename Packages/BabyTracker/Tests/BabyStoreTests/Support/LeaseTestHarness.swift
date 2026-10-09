import Foundation
@testable import BabyCore
@testable import BabyStore

@MainActor
struct LeaseTestHarness {
    enum ProbeResult: String, Codable { case committed, staleLease, retryAfterTransition }
    struct TakeoverResult { let replacement: ProbeResult; let suspended: ProbeResult }

    let directory: URL
    let storeURL: URL
    let lockURL: URL
    let store: BabyStore
    let leases: AppGroupFencedLease
    let ownerA = UUID(uuidString: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")!
    let ownerB = UUID(uuidString: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")!
    let accountKey = AccountLeaseKey(
        canonicalOrigin: "https://sync.example",
        serverInstanceID: UUID(uuidString: "cccccccc-cccc-4ccc-8ccc-cccccccccccc")!,
        accountID: UUID(uuidString: "dddddddd-dddd-4ddd-8ddd-dddddddddddd")!,
        deviceID: UUID(uuidString: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee")!
    )

    init() throws {
        directory = FileManager.default.temporaryDirectory.appending(path: "babytracker-lease-\(UUID().uuidString)", directoryHint: .isDirectory)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        storeURL = directory.appending(path: "store.sqlite")
        lockURL = directory.appending(path: "coordination.lock")
        let container = try BabySchema.container(storeURL: storeURL, bootstrapLockURL: directory.appending(path: "bootstrap.lock"))
        store = BabyStore(container: container, dateProvider: TestDateProvider(.now), deviceID: "lease-test", coordinationLockURL: lockURL)
        leases = AppGroupFencedLease(container: container, lockURL: lockURL)
    }

    func reopenStore() throws -> BabyStore {
        let container = try BabySchema.container(storeURL: storeURL, bootstrapLockURL: directory.appending(path: "bootstrap.lock"))
        return BabyStore(container: container, dateProvider: TestDateProvider(.now), deviceID: "lease-test", coordinationLockURL: lockURL)
    }

    func runTwoProcessTakeover(scope: LeaseScope) throws -> TakeoverResult {
        try LeaseProbeRunner.runTakeover(storeURL: storeURL, lockURL: lockURL, scope: scope, ownerA: ownerA, ownerB: ownerB)
    }

    /// Launches a probe that captures a write context and then parks, so the
    /// caller can perform an exclusive phase while a real second process holds
    /// a now-stale context.
    func launchStaleWriter(babyID: UUID) throws -> StaleWriterProbe {
        try LeaseProbeRunner.launchStaleWriter(storeURL: storeURL, lockURL: lockURL, babyID: babyID)
    }

    /// A fresh process that captures its context and writes immediately.
    func runFreshWriter(babyID: UUID) throws -> ProbeResult {
        try LeaseProbeRunner.runFreshWriter(storeURL: storeURL, lockURL: lockURL, babyID: babyID)
    }
}

/// A live probe process parked mid-write.
@MainActor
final class StaleWriterProbe {
    let process: Process
    let input: Pipe
    let reader: AnyObject
    private let resume: (String) throws -> LeaseTestHarness.ProbeResult

    init(process: Process, input: Pipe, reader: AnyObject, resume: @escaping (String) throws -> LeaseTestHarness.ProbeResult) {
        self.process = process
        self.input = input
        self.reader = reader
        self.resume = resume
    }

    /// Lets the parked writer attempt its save, then waits for it to exit so
    /// the database is quiet before anything is asserted about it.
    func resumeAndWait() throws -> LeaseTestHarness.ProbeResult {
        try resume("go")
    }
}

enum LeaseProbeRunner {
    enum HarnessError: Error, CustomStringConvertible {
        case probeMissing(String)
        case probeFailed(String)
        case malformedOutput(String)

        var description: String {
            switch self {
            case .probeMissing(let path): "BabyStoreLeaseProbe not found at \(path); build the package before running this test."
            case .probeFailed(let detail): "lease probe failed: \(detail)"
            case .malformedOutput(let detail): "lease probe emitted unreadable output: \(detail)"
            }
        }
    }

    /// Reads whole newline-delimited JSON objects from a pipe without losing
    /// bytes that arrived in the same chunk as the previous line.
    fileprivate final class LineReader {
        private let handle: FileHandle
        private var buffer = Data()

        init(_ handle: FileHandle) { self.handle = handle }

        func next() throws -> [String: String] {
            while !buffer.contains(0x0A) {
                let chunk = handle.availableData
                if chunk.isEmpty { throw HarnessError.malformedOutput(String(decoding: buffer, as: UTF8.self)) }
                buffer.append(chunk)
            }
            let newline = buffer.firstIndex(of: 0x0A)!
            let line = Data(buffer[..<newline])
            buffer = Data(buffer[buffer.index(after: newline)...])
            guard let object = try? JSONSerialization.jsonObject(with: line) as? [String: String] else {
                throw HarnessError.malformedOutput(String(decoding: line, as: UTF8.self))
            }
            return object
        }
    }

    /// `swift test` builds every target in the package, so the probe executable
    /// exists — but the Swift Testing runner exposes no `.xctest` bundle to
    /// locate it from, and `Bundle.main` points into the toolchain. Walk up to
    /// the package root from this source file and search the build directory,
    /// which holds for both `swift test` and Xcode.
    private static var probeCandidates: [URL] {
        var candidates: [URL] = []
        if let override = ProcessInfo.processInfo.environment["BABY_STORE_LEASE_PROBE"] {
            candidates.append(URL(fileURLWithPath: override))
        }
        if let products = Bundle.allBundles.first(where: { $0.bundlePath.hasSuffix(".xctest") })?
            .bundleURL.deletingLastPathComponent() {
            candidates.append(products.appending(path: "BabyStoreLeaseProbe"))
        }

        var root = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
        while root.path != "/" {
            if FileManager.default.fileExists(atPath: root.appending(path: "Package.swift").path) { break }
            root.deleteLastPathComponent()
        }
        let build = root.appending(path: ".build", directoryHint: .isDirectory)
        candidates.append(build.appending(path: "debug/BabyStoreLeaseProbe"))
        candidates.append(build.appending(path: "release/BabyStoreLeaseProbe"))
        // Arch-triple layouts, e.g. .build/arm64-apple-macosx/debug/.
        let contents = (try? FileManager.default.contentsOfDirectory(at: build, includingPropertiesForKeys: nil)) ?? []
        for entry in contents {
            candidates.append(entry.appending(path: "debug/BabyStoreLeaseProbe"))
            candidates.append(entry.appending(path: "release/BabyStoreLeaseProbe"))
        }
        return candidates
    }

    private static func arguments(
        storeURL: URL,
        lockURL: URL,
        ledgerURL: URL,
        encodedScope: String,
        owner: UUID,
        action: String,
        now: TimeInterval,
        ttl: TimeInterval,
        commitNow: TimeInterval
    ) -> [String] {
        [
            "--store", storeURL.path,
            "--lock", lockURL.path,
            "--bootstrap", storeURL.deletingLastPathComponent().appending(path: "bootstrap.lock").path,
            "--ledger", ledgerURL.path,
            "--scope", encodedScope,
            "--owner", owner.uuidString,
            "--action", action,
            "--now", "\(now)",
            "--ttl", "\(ttl)",
            "--commit-now", "\(commitNow)",
        ]
    }

    private static func probeArguments(storeURL: URL, lockURL: URL, babyID: UUID, action: String) -> [String] {
        [
            "--store", storeURL.path,
            "--lock", lockURL.path,
            "--bootstrap", storeURL.deletingLastPathComponent().appending(path: "bootstrap.lock").path,
            "--ledger", storeURL.deletingLastPathComponent().appending(path: "commits.ledger").path,
            "--baby", babyID.uuidString,
            "--action", action,
        ]
    }

    private static func resolvedProbe() throws -> URL {
        let candidates = probeCandidates
        guard let probe = candidates.first(where: { FileManager.default.isExecutableFile(atPath: $0.path) }) else {
            throw HarnessError.probeMissing(candidates.map(\.path).joined(separator: ", "))
        }
        return probe
    }

    @MainActor
    static func launchStaleWriter(storeURL: URL, lockURL: URL, babyID: UUID) throws -> StaleWriterProbe {
        let process = Process()
        process.executableURL = try resolvedProbe()
        process.arguments = probeArguments(storeURL: storeURL, lockURL: lockURL, babyID: babyID, action: "capture-context-then-write")
        let input = Pipe()
        let output = Pipe()
        process.standardInput = input
        process.standardOutput = output
        try process.run()

        let reader = LineReader(output.fileHandleForReading)
        let captured = try reader.next()
        guard captured["status"] == "captured" else {
            throw HarnessError.probeFailed("writer did not capture a context: \(captured)")
        }

        return StaleWriterProbe(process: process, input: input, reader: reader) { line in
            input.fileHandleForWriting.write(Data("\(line)\n".utf8))
            try? input.fileHandleForWriting.close()
            let payload = try reader.next()
            // Wait for exit before the caller reads the database, so nothing is
            // asserted while the probe still holds it open.
            process.waitUntilExit()
            guard let raw = payload["result"], let value = LeaseTestHarness.ProbeResult(rawValue: raw) else {
                throw HarnessError.probeFailed("stale writer: \(payload)")
            }
            return value
        }
    }

    @MainActor
    static func runFreshWriter(storeURL: URL, lockURL: URL, babyID: UUID) throws -> LeaseTestHarness.ProbeResult {
        let process = Process()
        process.executableURL = try resolvedProbe()
        process.arguments = probeArguments(storeURL: storeURL, lockURL: lockURL, babyID: babyID, action: "recapture-and-write")
        let output = Pipe()
        process.standardOutput = output
        try process.run()
        let reader = LineReader(output.fileHandleForReading)
        let payload = try reader.next()
        process.waitUntilExit()
        guard let raw = payload["result"], let value = LeaseTestHarness.ProbeResult(rawValue: raw) else {
            throw HarnessError.probeFailed("fresh writer: \(payload)")
        }
        return value
    }

    @MainActor
    static func runTakeover(
        storeURL: URL,
        lockURL: URL,
        scope: LeaseScope,
        ownerA: UUID,
        ownerB: UUID
    ) throws -> LeaseTestHarness.TakeoverResult {
        let candidates = probeCandidates
        guard let probe = candidates.first(where: { FileManager.default.isExecutableFile(atPath: $0.path) }) else {
            throw HarnessError.probeMissing(candidates.map(\.path).joined(separator: ", "))
        }

        let ledgerURL = storeURL.deletingLastPathComponent().appending(path: "commits.ledger")
        FileManager.default.createFile(atPath: ledgerURL.path, contents: Data())
        let encodedScope = try JSONEncoder().encode(scope).base64EncodedString()

        // Owner A takes the lease at t=0 for one second and then parks outside
        // the commit region, holding no OS lock.
        let suspended = Process()
        suspended.executableURL = probe
        suspended.arguments = arguments(
            storeURL: storeURL, lockURL: lockURL, ledgerURL: ledgerURL, encodedScope: encodedScope,
            owner: ownerA, action: "acquire-then-wait", now: 0, ttl: 1, commitNow: 2
        )
        let suspendedInput = Pipe()
        let suspendedOutput = Pipe()
        suspended.standardInput = suspendedInput
        suspended.standardOutput = suspendedOutput
        try suspended.run()

        let suspendedReader = LineReader(suspendedOutput.fileHandleForReading)
        let acquired = try suspendedReader.next()
        guard acquired["status"] == "acquired" else {
            throw HarnessError.probeFailed("owner A did not acquire: \(acquired)")
        }

        // Owner B runs to completion while A is parked: the lease has expired at
        // t=2, so B supersedes the generation and commits.
        let replacement = Process()
        replacement.executableURL = probe
        replacement.arguments = arguments(
            storeURL: storeURL, lockURL: lockURL, ledgerURL: ledgerURL, encodedScope: encodedScope,
            owner: ownerB, action: "take-and-commit", now: 2, ttl: 10, commitNow: 2
        )
        let replacementOutput = Pipe()
        replacement.standardOutput = replacementOutput
        try replacement.run()
        let replacementReader = LineReader(replacementOutput.fileHandleForReading)
        _ = try replacementReader.next()
        let replacementResult = try replacementReader.next()
        replacement.waitUntilExit()

        // Only now does A wake up and try to commit against its superseded fence.
        suspendedInput.fileHandleForWriting.write(Data("go\n".utf8))
        try? suspendedInput.fileHandleForWriting.close()
        let suspendedResult = try suspendedReader.next()
        suspended.waitUntilExit()

        func outcome(_ payload: [String: String], _ label: String) throws -> LeaseTestHarness.ProbeResult {
            guard let raw = payload["result"], let value = LeaseTestHarness.ProbeResult(rawValue: raw) else {
                throw HarnessError.probeFailed("\(label): \(payload)")
            }
            return value
        }

        return LeaseTestHarness.TakeoverResult(
            replacement: try outcome(replacementResult, "replacement"),
            suspended: try outcome(suspendedResult, "suspended")
        )
    }
}
