import BabyStore
import Foundation
import Observation

/// How one sync problem should be shown.
///
/// The distinction that matters is between something still being worked on and
/// something that will never resolve itself. A retry shown as a failure makes
/// a parent re-log an entry that was fine; a permanent rejection shown as a
/// spinner leaves them waiting forever for an answer that is not coming.
public enum SyncIssuePresentation: Sendable, Equatable {
    case retrying
    case permanentRejection(message: String)
    case timerCollision(message: String)
    case importConflict(message: String)
    case deviceContinuityRequired(message: String)
    case revoked

    /// Whether the user has to do something. A retry does not.
    public var needsAttention: Bool {
        switch self {
        case .retrying: false
        case .permanentRejection, .timerCollision, .importConflict, .deviceContinuityRequired, .revoked: true
        }
    }

    public var message: String {
        switch self {
        case .retrying: SyncIssueCopy.retrying
        case .permanentRejection(let message), .timerCollision(let message),
             .importConflict(let message), .deviceContinuityRequired(let message):
            message
        case .revoked: SyncIssueCopy.revoked
        }
    }
}

public struct SyncIssue: Identifiable, Sendable, Equatable {
    public let id: UUID
    public let realm: RealmID
    public let code: String
    public let mutationID: UUID?
    public let presentation: SyncIssuePresentation

    public init(
        id: UUID = UUID(),
        realm: RealmID,
        code: String,
        mutationID: UUID? = nil,
        presentation: SyncIssuePresentation
    ) {
        self.id = id
        self.realm = realm
        self.code = code
        self.mutationID = mutationID
        self.presentation = presentation
    }
}

@MainActor
public protocol SyncIssuePresenting: AnyObject {
    var issues: [SyncIssue] { get }
    func dismiss(issueID: UUID)
}

/// Holds the issues currently worth showing.
///
/// Bounded and de-duplicated by code and mutation: a sync retrying every few
/// seconds must not stack up a hundred identical banners, and the parent
/// should see one problem once.
@MainActor
@Observable
public final class SyncIssueStore: SyncIssuePresenting {
    public private(set) var issues: [SyncIssue] = []
    private var dismissed: Set<UUID> = []
    public let limit: Int

    public init(limit: Int = 5) { self.limit = limit }

    public func record(_ issue: SyncIssue) {
        // The same problem arriving again is the same problem.
        if issues.contains(where: { $0.code == issue.code && $0.mutationID == issue.mutationID }) { return }
        issues.append(issue)
        if issues.count > limit { issues.removeFirst(issues.count - limit) }
    }

    public func dismiss(issueID: UUID) {
        dismissed.insert(issueID)
        issues.removeAll { $0.id == issueID }
    }

    /// Anything the user has already been told about and resolved clears on
    /// the next successful sync; a revoked account does not, because nothing
    /// about it has been resolved.
    public func clearResolved() {
        issues.removeAll { $0.presentation != .revoked }
    }

    /// Translates a server code into something worth showing, or nothing when
    /// the client is simply going to try again.
    public static func issue(for code: SyncErrorCode, realm: RealmID, mutationID: UUID? = nil) -> SyncIssue? {
        switch code.action {
        case .retryAfterBackoff, .refreshSession, .takeSnapshot:
            // Being worked on. Saying so on every attempt trains a parent to
            // ignore the banner that will one day matter.
            nil
        case .quarantineMutation:
            SyncIssue(realm: realm, code: code.rawValue, mutationID: mutationID,
                      presentation: .permanentRejection(message: SyncIssueCopy.rejectedEntry))
        case .freshProofRequired:
            SyncIssue(realm: realm, code: code.rawValue, mutationID: mutationID,
                      presentation: .deviceContinuityRequired(message: SyncIssueCopy.signInAgain))
        case .revokeAndPurge:
            SyncIssue(realm: realm, code: code.rawValue, mutationID: mutationID, presentation: .revoked)
        case .surfaceIssue:
            SyncIssue(realm: realm, code: code.rawValue, mutationID: mutationID,
                      presentation: .permanentRejection(message: SyncIssueCopy.message(for: code)))
        }
    }
}

public enum SyncIssueCopy {
    public static let retrying = String(localized: "Syncing…", bundle: .module)
    public static let revoked = String(localized: "You're no longer in this family. Its shared entries have been removed from this device.", bundle: .module)
    public static let signInAgain = String(localized: "Sign in with Apple again to keep sharing with your family.", bundle: .module)
    public static let readOnly = String(localized: "You have view-only access, so your changes aren't shared.", bundle: .module)
    public static let outOfDate = String(localized: "This version of Baby Tracker is too old to sync with your family's server.", bundle: .module)
    /// Truthful about what happened, and about what was kept. A parent must
    /// not be left thinking an entry vanished.
    public static let rejectedEntry = String(localized: "One entry couldn't be shared with your family and has been set aside. It's still here on this device.", bundle: .module)
    /// The losing side of a timer race. It never claims a feed happened — the
    /// other caregiver's timer is the real one, and fabricating a completed
    /// feed here would invent a feed that nobody gave.
    public static let timerCollision = String(localized: "Another caregiver already started this timer, so theirs is the one that's running. Nothing you logged has been lost.", bundle: .module)
    public static let importConflict = String(localized: "Your family needs a decision about which records belong to which baby.", bundle: .module)
    /// Joining or creating a family stopped part-way. Nothing is lost — the
    /// next attempt resumes where this one stopped.
    /// The server refused part of the owner's history. Nothing is lost on
    /// this phone; the import stays open rather than being called finished.
    public static let importIncomplete = String(localized: "Some of your earlier entries couldn't be shared with your family yet. They're still on this device.", bundle: .module)
    public static let familySetupIncomplete = String(localized: "Couldn't finish setting up sharing yet. Your entries are safe; we'll try again.", bundle: .module)

    public static func message(for code: SyncErrorCode) -> String {
        switch code {
        case .readOnlyCaregiver: readOnly
        case .unsupportedProtocol, .unsupportedCapability: outOfDate
        default: rejectedEntry
        }
    }
}
