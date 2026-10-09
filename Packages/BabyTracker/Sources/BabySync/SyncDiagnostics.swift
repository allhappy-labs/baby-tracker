import BabyStore
import Foundation

/// Fixed vocabulary for one in-process sync trace. No case carries data.
public enum SyncPhase: Sendable, Equatable {
    case localCommit, enqueue, requestStart, requestComplete, hintReceived
    case peerPull, remoteCommit, observablePublish
}

public enum SyncDiagnosticStatus: Sendable, Equatable {
    case success, retry, refused, inactive
}

/// Safe to hand to an opt-in recorder: no care values, credentials, URLs,
/// persistent entity IDs, or raw error descriptions can enter this shape.
public struct SyncDiagnosticEvent: Sendable {
    public let phase: SyncPhase
    public let elapsedNanoseconds: UInt64
    public let status: SyncDiagnosticStatus
    public let correlation: UUID

    public init(phase: SyncPhase, elapsedNanoseconds: UInt64, status: SyncDiagnosticStatus, correlation: UUID) {
        self.phase = phase
        self.elapsedNanoseconds = elapsedNanoseconds
        self.status = status
        self.correlation = correlation
    }
}

public protocol SyncDiagnosticSink: Sendable {
    func record(_ event: SyncDiagnosticEvent)
}

/// A detached, observable view of engine operation. Availability describes
/// local credentials only; it never claims that the server accepted them.
public struct SyncOperationalStatus: Sendable, Equatable {
    public enum Activity: Sendable { case inactive, active }
    public enum SignIn: Sendable { case available, required }
    public enum Live: Sendable { case unavailable, dialing, connected, reconnecting, refused }
    public enum Phase: Sendable { case idle, enqueue, request, peerApply, retryQueued }

    public var activity: Activity
    public var signIn: SignIn
    public var live: Live
    public var phase: Phase

    public init(activity: Activity, signIn: SignIn, live: Live, phase: Phase) {
        self.activity = activity
        self.signIn = signIn
        self.live = live
        self.phase = phase
    }

    public static let inactive = Self(activity: .inactive, signIn: .required, live: .unavailable, phase: .idle)
}

/// Lives only as long as one engine. The correlation never leaves this process
/// unless a caller explicitly supplies a sink; it is never persisted.
@MainActor
final class SyncDiagnosticSession {
    private let sink: any SyncDiagnosticSink
    private struct Trace {
        let correlation: UUID
    }
    private var pending: Trace?
    private var pendingIsLocal = false
    private var active: Trace?
    /// Only current in-process trace origins. Never serialized or keyed by a
    /// persistent entity, account, credential, or family identifier.
    private var origins: [UUID: UInt64] = [:]
    private var stopped = false

    init(sink: any SyncDiagnosticSink) { self.sink = sink }

    func stop() {
        stopped = true
        pending = nil
        pendingIsLocal = false
        active = nil
        origins.removeAll()
    }

    private func newTrace(at now: UInt64) -> Trace {
        let trace = Trace(correlation: UUID())
        origins[trace.correlation] = now
        return trace
    }

    func beginAttempt() {
        guard !stopped else { return }
        active = pending ?? newTrace(at: DispatchTime.now().uptimeNanoseconds)
        pending = nil
        pendingIsLocal = false
    }

    func endAttempt() {
        if let active { origins.removeValue(forKey: active.correlation) }
        active = nil
    }

    func storeMilestone(_ milestone: StoreSyncMilestone) {
        guard !stopped else { return }
        let phase: SyncPhase
        switch milestone.phase {
        case .localCommit:
            if pending == nil || !pendingIsLocal {
                if let pending { origins.removeValue(forKey: pending.correlation) }
                pending = newTrace(at: milestone.monotonicNanoseconds)
            }
            pendingIsLocal = true
            phase = .localCommit
        case .remoteCommit: phase = .remoteCommit
        case .observablePublish: phase = .observablePublish
        }
        record(phase, at: milestone.monotonicNanoseconds, usePending: milestone.phase == .localCommit)
    }

    func record(_ phase: SyncPhase, status: SyncDiagnosticStatus = .success, at now: UInt64 = DispatchTime.now().uptimeNanoseconds, usePending: Bool = false) {
        guard !stopped else { return }
        if active == nil && pending == nil { pending = newTrace(at: now) }
        let trace = usePending ? pending! : (active ?? pending)!
        let start = origins[trace.correlation] ?? now
        sink.record(SyncDiagnosticEvent(
            phase: phase, elapsedNanoseconds: now >= start ? now - start : 0,
            status: status, correlation: trace.correlation
        ))
    }
}
