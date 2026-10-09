import Foundation
import CryptoKit

public enum RefreshProtocolMode: String, Codable, Sendable { case legacy, recoverableV1 }

/// Non-secret proof tying one credential replacement to one durable attempt.
public struct RefreshInstallBinding: Codable, Sendable, Equatable {
    public let key: AccountLeaseKey
    public let recoveryEpoch: UInt64
    public let attemptID: UUID
    public let parentCredentialVersion: UUID
    public let expectedCredentialVersion: UUID
    public let accessFingerprint: String
    public let refreshFingerprint: String

    public init(key: AccountLeaseKey, recoveryEpoch: UInt64, attemptID: UUID, parentCredentialVersion: UUID, expectedCredentialVersion: UUID, accessFingerprint: String, refreshFingerprint: String) {
        self.key = key; self.recoveryEpoch = recoveryEpoch; self.attemptID = attemptID
        self.parentCredentialVersion = parentCredentialVersion; self.expectedCredentialVersion = expectedCredentialVersion
        self.accessFingerprint = accessFingerprint; self.refreshFingerprint = refreshFingerprint
    }

    public static func fingerprint(_ token: String) -> String {
        SHA256.hash(data: Data(token.utf8)).map { String(format: "%02x", $0) }.joined()
    }

    var isValid: Bool {
        parentCredentialVersion != expectedCredentialVersion &&
        [accessFingerprint, refreshFingerprint].allSatisfy { value in
            value.utf8.count == 64 && value.utf8.allSatisfy { (48...57).contains($0) || (97...102).contains($0) }
        }
    }
}

public struct AckRetry: Codable, Sendable, Equatable {
    public let attempt: RefreshAttempt
    public let binding: RefreshInstallBinding
    public let nextRetryAt: Date
    public let retryCount: Int
    public init(attempt: RefreshAttempt, binding: RefreshInstallBinding, nextRetryAt: Date, retryCount: Int) {
        self.attempt = attempt; self.binding = binding; self.nextRetryAt = nextRetryAt
        self.retryCount = min(max(retryCount, 0), 10)
    }
}

public struct RefreshRecoveryCheckpoint: Codable, Sendable, Equatable {
    public enum Phase: String, Codable, Sendable { case awaitingReply, installPrepared, ackPending }
    public let attempt: RefreshAttempt
    public let recoveryEpoch: UInt64
    public var phase: Phase
    public var installBinding: RefreshInstallBinding?
    public var acknowledgement: AckRetry?
    public var predecessorAcknowledgement: AckRetry?
    public init(attempt: RefreshAttempt, recoveryEpoch: UInt64, phase: Phase, installBinding: RefreshInstallBinding? = nil, acknowledgement: AckRetry? = nil, predecessorAcknowledgement: AckRetry? = nil) {
        self.attempt = attempt; self.recoveryEpoch = recoveryEpoch; self.phase = phase
        self.installBinding = installBinding; self.acknowledgement = acknowledgement; self.predecessorAcknowledgement = predecessorAcknowledgement
    }
}

public enum RefreshRecoveryError: Error, Sendable, Equatable {
    case corruptCheckpoint
    case invalidBinding
}
