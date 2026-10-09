import BabyCore
import BabyStore
import Foundation

/// A real, independent process that takes a fenced lease against an on-disk
/// store. The in-process `NSLock` inside `BabySchema.makeContainer` cannot
/// serialise anything here, so whatever these probes prove about takeover and
/// stale-owner rejection is proven by the App Group `flock` and the persisted
/// generation alone — which is the whole point of launching them.
enum ProbeError: Error { case badArgument(String), leaseHeld }

func argument(_ name: String) throws -> String {
    let arguments = CommandLine.arguments
    guard let index = arguments.firstIndex(of: "--\(name)"), index + 1 < arguments.count else {
        throw ProbeError.badArgument(name)
    }
    return arguments[index + 1]
}

func emit(_ payload: [String: String]) {
    let data = try! JSONSerialization.data(withJSONObject: payload)
    FileHandle.standardOutput.write(data)
    FileHandle.standardOutput.write(Data("\n".utf8))
}

func date(_ name: String) throws -> Date {
    guard let seconds = TimeInterval(try argument(name)) else { throw ProbeError.badArgument(name) }
    return Date(timeIntervalSince1970: seconds)
}

do {
    let storeURL = URL(fileURLWithPath: try argument("store"))
    let lockURL = URL(fileURLWithPath: try argument("lock"))
    let bootstrapURL = URL(fileURLWithPath: try argument("bootstrap"))
    let ledgerURL = URL(fileURLWithPath: try argument("ledger"))
    let action = try argument("action")
    // The lease arguments are only meaningful to the lease actions; the import
    // race probes drive the write boundary instead.
    let owner = (try? argument("owner")).flatMap(UUID.init(uuidString:)) ?? UUID()
    let now = (try? date("now")) ?? Date(timeIntervalSince1970: 0)
    let ttl = (try? argument("ttl")).flatMap(TimeInterval.init) ?? 1
    let commitNow = (try? date("commit-now")) ?? Date(timeIntervalSince1970: 0)

    let scope = try (try? argument("scope"))
        .flatMap { Data(base64Encoded: $0) }
        .map { try JSONDecoder().decode(LeaseScope.self, from: $0) } ?? LeaseScope.realm(.local)

    let container = try BabySchema.container(storeURL: storeURL, bootstrapLockURL: bootstrapURL)
    let leases = AppGroupFencedLease(container: container, lockURL: lockURL)

    // The commit body must not suspend. Appending one line to the shared ledger
    // is a durable, observable side effect that records who actually got inside
    // the fenced region, so an ordering violation is visible after the fact.
    func recordCommit(_ label: String) {
        let line = Data("\(label)\n".utf8)
        if let handle = try? FileHandle(forWritingTo: ledgerURL) {
            handle.seekToEndOfFile()
            handle.write(line)
            try? handle.close()
        } else {
            try? line.write(to: ledgerURL)
        }
    }

    func commit(_ token: FencedLeaseToken, at instant: Date, label: String) {
        do {
            try leases.withFencedCommit(token, now: instant) { recordCommit(label) }
            emit(["result": "committed"])
        } catch FencedLeaseError.staleOwner {
            emit(["result": "staleLease"])
        } catch FencedLeaseError.expired {
            emit(["result": "staleLease"])
        } catch {
            emit(["error": "\(error)"])
            exit(1)
        }
    }

    switch action {
    case "recovery-refresh":
        let store = BabyStore(container: container, dateProvider: SystemDateProvider(), deviceID: "recovery-probe", coordinationLockURL: lockURL)
        try await RecoveryProbe.run(store: store, leases: leases, directory: storeURL.deletingLastPathComponent(), now: now)
    case "capture-context-then-write":
        // The widget's shape exactly: capture a write context, park, then come
        // back and try to save against it. While it is parked the app performs
        // the import rewrite, so the context it holds is stale.
        let store = BabyStore(container: container, dateProvider: SystemDateProvider(), deviceID: "probe", coordinationLockURL: lockURL)
        let babyID = UUID(uuidString: try argument("baby"))!
        let captured = try store.captureWriteContext()
        emit(["status": "captured", "generation": "\(captured.transitionGeneration)"])
        _ = readLine(strippingNewline: true)
        do {
            _ = try store.logDiaper(.wet, for: babyID, expectedWriteContext: captured)
            recordCommit("widget-stale:\(babyID.uuidString)")
            emit(["result": "committed"])
        } catch RealmWriteError.retryAfterTransition {
            emit(["result": "retryAfterTransition"])
        } catch {
            emit(["error": "\(error)"])
            exit(1)
        }
    case "recapture-and-write":
        // The retry a well-behaved writer performs: fresh context, then save.
        let store = BabyStore(container: container, dateProvider: SystemDateProvider(), deviceID: "probe", coordinationLockURL: lockURL)
        let babyID = UUID(uuidString: try argument("baby"))!
        do {
            _ = try store.logDiaper(.wet, for: babyID)
            recordCommit("widget-retry:\(babyID.uuidString)")
            emit(["result": "committed"])
        } catch {
            emit(["error": "\(error)"])
            exit(1)
        }
    case "acquire-then-wait":
        guard let token = try leases.acquire(scope: scope, ownerID: owner, now: now, ttl: ttl) else {
            throw ProbeError.leaseHeld
        }
        // Announce, then park outside the commit region — exactly the suspended
        // owner the fence is meant to survive.
        emit(["status": "acquired", "generation": "\(token.generation)"])
        _ = readLine(strippingNewline: true)
        commit(token, at: commitNow, label: "suspended:\(owner.uuidString)")
    case "take-and-commit":
        guard let token = try leases.acquire(scope: scope, ownerID: owner, now: now, ttl: ttl) else {
            throw ProbeError.leaseHeld
        }
        emit(["status": "acquired", "generation": "\(token.generation)"])
        commit(token, at: commitNow, label: "replacement:\(owner.uuidString)")
    default:
        throw ProbeError.badArgument("action")
    }
} catch {
    emit(["error": "\(error)"])
    exit(1)
}
