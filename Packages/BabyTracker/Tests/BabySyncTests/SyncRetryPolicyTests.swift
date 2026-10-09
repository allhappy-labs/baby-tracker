import Foundation
import Testing
@testable import BabySync

@Suite("Sync retry policy and action table")
struct SyncRetryPolicyTests {
    /// Every code the server can send has a decision. A missing one would
    /// default to whatever the call site felt like, which is how a
    /// non-retryable error ends up in a retry loop.
    @Test func everyServerCodeHasAnAction() {
        for code in SyncErrorCode.allCases {
            _ = code.action
        }
        #expect(SyncErrorCode.allCases.count >= 26)
    }

    /// The codes that must never be retried. Each of these would otherwise
    /// drain a phone against a server that will never say yes.
    @Test(arguments: [
        SyncErrorCode.unsupportedProtocol, .unsupportedCapability,
        .familyScopeRequired, .readOnlyCaregiver, .ownerRequired,
        .entitlementRequired, .recoveryOnly,
    ])
    func permanentRefusalsAreSurfaced(code: SyncErrorCode) {
        #expect(code.action == .surfaceIssue)
    }

    @Test(arguments: [
        SyncErrorCode.serviceUnavailable, .securityBusy,
        .authorizationQuarantine, .familyUnavailable,
    ])
    func transientConditionsBackOff(code: SyncErrorCode) {
        #expect(code.action == .retryAfterBackoff)
    }

    /// An expired access token is refreshable; a revoked session is not, and
    /// asking the server to refresh it would send a token that cannot work.
    @Test func expiredAccessRefreshesButRevokedSessionNeedsFreshProof() {
        #expect(SyncErrorCode.accessExpired.action == .refreshSession)
        #expect(SyncErrorCode.sessionRevoked.action == .freshProofRequired)
        #expect(SyncErrorCode.reauthenticationRequired.action == .freshProofRequired)
    }

    /// Continuity problems need Apple, not a refresh: the device binding is
    /// what is in doubt.
    @Test func continuityProblemsRequireFreshProof() {
        #expect(SyncErrorCode.deviceContinuityInvalid.action == .freshProofRequired)
        #expect(SyncErrorCode.deviceContinuityRequired.action == .freshProofRequired)
    }

    /// A cursor that no longer means anything is replaced by a snapshot rather
    /// than retried — retrying it asks the same impossible question again.
    @Test(arguments: [SyncErrorCode.epochChanged, .snapshotRequired, .pageExpired, .invalidPageToken])
    func uselessCursorsTakeASnapshot(code: SyncErrorCode) {
        #expect(code.action == .takeSnapshot)
    }

    @Test func revokedMembershipPurges() {
        #expect(SyncErrorCode.membershipRevoked.action == .revokeAndPurge)
    }

    /// One mutation the server will always refuse must not block every later
    /// one behind it.
    @Test(arguments: [SyncErrorCode.invalidRequest, .mutationIDReused])
    func hopelessMutationsAreQuarantined(code: SyncErrorCode) {
        #expect(code.action == .quarantineMutation)
    }

    @Test(arguments: [SyncErrorCode.batchLimit, .bodyTooLarge, .snapshotLimit])
    func sizeComplaintsRetryWithASmallerBatch(code: SyncErrorCode) {
        #expect(code.action == .retryAfterBackoff)
        #expect(code.suggestsSmallerBatch)
    }

    // MARK: Backoff

    @Test func backoffGrowsExponentiallyAndIsCapped() {
        let policy = SyncRetryPolicy(base: 1, ceiling: 16)
        #expect(policy.window(forAttempt: 1).upperBound == 1)
        #expect(policy.window(forAttempt: 2).upperBound == 2)
        #expect(policy.window(forAttempt: 3).upperBound == 4)
        #expect(policy.window(forAttempt: 5).upperBound == 16)
        // Capped, not unbounded: a week-long backoff is indistinguishable from
        // sync being broken.
        #expect(policy.window(forAttempt: 40).upperBound == 16)
    }

    /// Full jitter, because every device in a family wakes on the same live
    /// hint. Lockstep backoff rebuilds the herd it exists to prevent.
    @Test func delayIsDrawnFromZeroToTheWindow() {
        let policy = SyncRetryPolicy(base: 1, ceiling: 16)
        #expect(policy.delay(forAttempt: 3) { $0.lowerBound } == 0)
        #expect(policy.delay(forAttempt: 3) { $0.upperBound } == 4)
    }

    @Test func zeroAttemptsMeansNoDelay() {
        #expect(SyncRetryPolicy().delay(forAttempt: 0) == 0)
    }

    @Test func givingUpIsBounded() {
        let policy = SyncRetryPolicy(maximumAttempts: 3)
        #expect(!policy.shouldGiveUp(after: 2))
        #expect(policy.shouldGiveUp(after: 3))
    }

    /// Halving never reaches zero: a single mutation the server still refuses
    /// is quarantined, not shrunk into nothing.
    @Test func batchHalvingBottomsOutAtOne() {
        let policy = SyncRetryPolicy()
        #expect(policy.nextBatchSize(after: 100) == 50)
        #expect(policy.nextBatchSize(after: 2) == 1)
        #expect(policy.nextBatchSize(after: 1) == 1)
    }
}
