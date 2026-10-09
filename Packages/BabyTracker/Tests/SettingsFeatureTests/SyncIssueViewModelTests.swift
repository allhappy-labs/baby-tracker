import BabyStore
import BabySync
import Foundation
import Testing
@testable import SettingsFeature

@MainActor
@Suite("Sync issue presentation")
struct SyncIssueViewModelTests {
    private let realm = RealmID.shared(
        canonicalOrigin: "https://sync.example",
        serverInstanceID: UUID(uuidString: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")!,
        familyID: UUID(uuidString: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")!
    )

    /// Something still being worked on is not shown. Saying "sync failed" on
    /// every backoff trains a parent to ignore the banner that will one day
    /// matter.
    @Test(arguments: [
        SyncErrorCode.serviceUnavailable, .securityBusy, .familyUnavailable,
        .accessExpired, .epochChanged, .snapshotRequired, .batchLimit,
    ])
    func transientConditionsAreNotSurfaced(code: SyncErrorCode) {
        #expect(SyncIssueStore.issue(for: code, realm: realm) == nil)
    }

    @Test func revokedMembershipIsItsOwnPresentation() {
        let issue = SyncIssueStore.issue(for: .membershipRevoked, realm: realm)
        #expect(issue?.presentation == .revoked)
        #expect(issue?.presentation.needsAttention == true)
    }

    @Test func continuityProblemsAskForAppleSignIn() {
        let issue = SyncIssueStore.issue(for: .deviceContinuityInvalid, realm: realm)
        guard case .deviceContinuityRequired(let message)? = issue?.presentation else {
            Issue.record("expected a continuity presentation")
            return
        }
        #expect(message.contains("Sign in with Apple"))
    }

    /// A rejected entry must say it was kept. A parent who thinks it vanished
    /// will re-log it, and now the family has it twice.
    @Test func rejectedEntrySaysItIsStillOnTheDevice() {
        let issue = SyncIssueStore.issue(for: .invalidRequest, realm: realm, mutationID: UUID())
        guard case .permanentRejection(let message)? = issue?.presentation else {
            Issue.record("expected a permanent rejection")
            return
        }
        #expect(message.contains("still here on this device"))
    }

    /// The losing side of a timer race never claims a feed happened.
    /// Fabricating a completed feed here would invent a feed nobody gave.
    @Test func timerCollisionCopyDoesNotFabricateAFeed() {
        let copy = SyncIssueCopy.timerCollision
        #expect(copy.contains("Another caregiver already started this timer"))
        #expect(copy.contains("Nothing you logged has been lost"))
        #expect(!copy.lowercased().contains("completed"))
    }

    @Test func readOnlyCaregiverIsToldAboutSharingNotAnError() {
        let issue = SyncIssueStore.issue(for: .readOnlyCaregiver, realm: realm)
        #expect(issue?.presentation.message.contains("view-only") == true)
    }

    // MARK: The store

    /// The same problem arriving again is the same problem. A sync retrying
    /// every few seconds must not stack a hundred identical banners.
    @Test func repeatedIssuesAreDeduplicated() {
        let store = SyncIssueStore()
        let mutationID = UUID()
        for _ in 0..<10 {
            store.record(SyncIssue(
                realm: realm, code: "invalid_request", mutationID: mutationID,
                presentation: .permanentRejection(message: "x")
            ))
        }
        #expect(store.issues.count == 1)
    }

    /// Different entries are different problems.
    @Test func distinctMutationsAreDistinctIssues() {
        let store = SyncIssueStore()
        for _ in 0..<3 {
            store.record(SyncIssue(
                realm: realm, code: "invalid_request", mutationID: UUID(),
                presentation: .permanentRejection(message: "x")
            ))
        }
        #expect(store.issues.count == 3)
    }

    @Test func issuesAreBounded() {
        let store = SyncIssueStore(limit: 2)
        for _ in 0..<6 {
            store.record(SyncIssue(
                realm: realm, code: "invalid_request", mutationID: UUID(),
                presentation: .permanentRejection(message: "x")
            ))
        }
        #expect(store.issues.count == 2)
    }

    @Test func dismissingRemovesOnlyThatIssue() {
        let store = SyncIssueStore()
        let first = SyncIssue(realm: realm, code: "a", mutationID: UUID(), presentation: .revoked)
        let second = SyncIssue(realm: realm, code: "b", mutationID: UUID(), presentation: .revoked)
        store.record(first)
        store.record(second)

        store.dismiss(issueID: first.id)

        #expect(store.issues.map(\.id) == [second.id])
    }

    /// A successful sync clears what it resolved — but not a revocation,
    /// because nothing about that has been resolved.
    @Test func revocationSurvivesAClearButOtherIssuesDoNot() {
        let store = SyncIssueStore()
        store.record(SyncIssue(realm: realm, code: "invalid_request", mutationID: UUID(),
                               presentation: .permanentRejection(message: "x")))
        store.record(SyncIssue(realm: realm, code: "membership_revoked", presentation: .revoked))

        store.clearResolved()

        #expect(store.issues.map(\.presentation) == [.revoked])
    }
}
