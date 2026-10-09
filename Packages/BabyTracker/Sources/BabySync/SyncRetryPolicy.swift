import Foundation

/// What the client does about each server code.
///
/// Frozen as one table on purpose. Scattering these decisions across call
/// sites is how a non-retryable error ends up in a retry loop draining a
/// phone's battery against a server that will never say yes, and how a
/// recoverable one gets surfaced to a parent as a permanent failure.
public enum SyncAction: Sendable, Equatable {
    /// Send again after a delay. The condition is transient.
    case retryAfterBackoff
    /// Stop and surface it. Retrying cannot change the answer.
    case surfaceIssue
    /// Refresh the session, then retry once.
    case refreshSession
    /// Re-authenticate with fresh Apple proof before anything else.
    case freshProofRequired
    /// The cursor is useless; take a snapshot instead.
    case takeSnapshot
    /// Deny this account and purge its shared realm.
    case revokeAndPurge
    /// The mutation itself is bad. Quarantine it rather than blocking the
    /// queue behind something the server will always refuse.
    case quarantineMutation
}

/// Conforms to `Error` so the transport can throw the server's own code and
/// the coordinator's one action table decides what it means.
public enum SyncErrorCode: String, Error, Sendable, CaseIterable {
    case invalidRequest = "invalid_request"
    case unsupportedProtocol = "unsupported_protocol"
    case unsupportedCapability = "unsupported_capability"
    case batchLimit = "batch_limit"
    case bodyTooLarge = "body_too_large"
    case invalidPageToken = "invalid_page_token"
    case pageExpired = "page_expired"
    case snapshotRequired = "snapshot_required"
    case snapshotLimit = "snapshot_limit"
    case epochChanged = "epoch_changed"
    case mutationIDReused = "mutation_id_reused"
    case familyScopeRequired = "family_scope_required"
    case readOnlyCaregiver = "read_only_caregiver"
    case accessExpired = "access_expired"
    case reauthenticationRequired = "reauthentication_required"
    case sessionRevoked = "session_revoked"
    case membershipRevoked = "membership_revoked"
    case ownerRequired = "owner_required"
    case entitlementRequired = "entitlement_required"
    case recoveryOnly = "recovery_only"
    case deviceContinuityInvalid = "device_continuity_invalid"
    case deviceContinuityRequired = "device_continuity_required"
    case serviceUnavailable = "service_unavailable"
    case securityBusy = "security_busy"
    case authorizationQuarantine = "authorization_quarantine"
    case familyUnavailable = "family_unavailable"

    public var action: SyncAction {
        switch self {
        // Transient: the same request will succeed later.
        case .serviceUnavailable, .securityBusy, .authorizationQuarantine, .familyUnavailable:
            .retryAfterBackoff

        // The session is stale but the device is still trusted.
        case .accessExpired:
            .refreshSession

        // The session cannot be refreshed; only Apple can vouch for us again.
        case .reauthenticationRequired, .sessionRevoked,
             .deviceContinuityInvalid, .deviceContinuityRequired:
            .freshProofRequired

        // The cursor no longer means anything against this history.
        case .epochChanged, .snapshotRequired, .pageExpired, .invalidPageToken:
            .takeSnapshot

        // This device is no longer in this family.
        case .membershipRevoked:
            .revokeAndPurge

        // The batch is too big for this server. Sending less is the client's
        // job, so this retries against a smaller batch rather than surfacing.
        case .batchLimit, .bodyTooLarge, .snapshotLimit:
            .retryAfterBackoff

        // The server will never accept this particular mutation. Quarantine it
        // so one bad row cannot block every later one behind it.
        case .invalidRequest, .mutationIDReused:
            .quarantineMutation

        // Nothing the client can do without a human: a wrong protocol, a role
        // that cannot write, a family it may not reach.
        case .unsupportedProtocol, .unsupportedCapability, .familyScopeRequired,
             .readOnlyCaregiver, .ownerRequired, .entitlementRequired, .recoveryOnly:
            .surfaceIssue
        }
    }

    /// Whether a smaller batch is worth trying. Distinguishes "too much at
    /// once" from "come back later", both of which back off.
    public var suggestsSmallerBatch: Bool {
        switch self {
        case .batchLimit, .bodyTooLarge, .snapshotLimit: true
        default: false
        }
    }
}

/// Exponential backoff with full jitter.
///
/// Jittered because every device in a family wakes on the same live hint and
/// the same push. Un-jittered backoff would have them all retry in lockstep
/// and rebuild the thundering herd the backoff exists to prevent.
public struct SyncRetryPolicy: Sendable {
    public let base: TimeInterval
    public let ceiling: TimeInterval
    public let maximumAttempts: Int

    public init(base: TimeInterval = 1, ceiling: TimeInterval = 300, maximumAttempts: Int = 8) {
        self.base = base
        self.ceiling = ceiling
        self.maximumAttempts = maximumAttempts
    }

    /// The window an attempt's delay is drawn from. Exposed separately from
    /// `delay` so it can be asserted without a random draw.
    public func window(forAttempt attempt: Int) -> ClosedRange<TimeInterval> {
        guard attempt > 0 else { return 0...0 }
        let exponential = base * pow(2, Double(min(attempt, 16) - 1))
        return 0...min(exponential, ceiling)
    }

    public func delay(forAttempt attempt: Int, random: (ClosedRange<TimeInterval>) -> TimeInterval = { .random(in: $0) }) -> TimeInterval {
        let window = window(forAttempt: attempt)
        return window.upperBound == 0 ? 0 : random(window)
    }

    public func shouldGiveUp(after attempt: Int) -> Bool { attempt >= maximumAttempts }

    /// Halves the batch on a size complaint, never below one — a single
    /// mutation the server still refuses is quarantined, not shrunk further.
    public func nextBatchSize(after current: Int) -> Int { max(1, current / 2) }
}
