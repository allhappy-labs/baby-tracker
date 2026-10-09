import Foundation

/// Closed diagnostic vocabulary. No credential, fingerprint, durable attempt,
/// account, device, endpoint, or care payload is accepted by this interface.
public struct RefreshRecoveryEvent: Sendable, Equatable {
    public enum Phase: String, Sendable { case probing, recovering, preparing, installed, ackPending, acknowledged, terminal, deferred, legacy }
    public enum Reason: String, Sendable {
        case recoveryUnavailableMissingDeviceBinding = "recovery_unavailable_missing_device_binding"
        case legacyServer = "legacy_server"
        case reauthenticationRequired = "reauthentication_required"
        case sessionRevoked = "session_revoked"
        case refreshReused = "refresh_reused"
        case epochChanged = "epoch_changed"
        case receiptUnavailable = "refresh_receipt_unavailable"
        case attemptConflict = "refresh_attempt_conflict"
        case irreconcilableCheckpoint = "irreconcilable_checkpoint"
        case uncertainLegacyAttempt = "uncertain_legacy_attempt"
        case accountGateClosed = "account_gate_closed"
        case retryLater = "retry_later"
    }
    public let phase: Phase
    public let reason: Reason?
    public let durationMilliseconds: Int
    public let retryCount: Int
    public let correlation: UUID
    public init(phase: Phase, reason: Reason? = nil, durationMilliseconds: Int, retryCount: Int = 0, correlation: UUID) {
        self.phase = phase; self.reason = reason
        self.durationMilliseconds = min(max(0, durationMilliseconds), Int(Int32.max))
        self.retryCount = min(max(0, retryCount), 10); self.correlation = correlation
    }
}
