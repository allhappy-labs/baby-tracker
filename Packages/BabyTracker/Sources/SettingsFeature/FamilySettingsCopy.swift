import BabyStore
import BabySync
import Foundation

/// One confirmation sheet: what it asks, the words that explain it, and the
/// action it guards.
///
/// The explanation travels with the confirmation, set by whoever asks, rather
/// than being read off the model afterwards: a single shared string was what
/// let a Remove sheet show the last Disconnect's text, or nothing at all.
public struct FamilyLifecycleConfirmation: Identifiable {
    public let id = UUID()
    public let title: String
    public let body: String
    public let confirmLabel: String
    /// Drawn in the destructive colour. A role change is undone by another
    /// role change, so it is not.
    public let isDestructive: Bool
    public let action: () async throws -> Void

    init(
        title: String, body: String, confirmLabel: String, isDestructive: Bool = true,
        action: @escaping () async throws -> Void
    ) {
        self.title = title
        self.body = body
        self.confirmLabel = confirmLabel
        self.isDestructive = isDestructive
        self.action = action
    }

    /// A member-detail confirmation, from the copy computed for that exact
    /// action and member.
    init(_ copy: FamilyConfirmationCopy, isDestructive: Bool, action: @escaping () async throws -> Void) {
        self.init(
            title: copy.title, body: copy.body, confirmLabel: copy.confirmLabel,
            isDestructive: isDestructive, action: action
        )
    }

    static func disconnect(action: @escaping () async throws -> Void) -> Self {
        .init(
            title: String(localized: "Disconnect this device?", bundle: .module), body: FamilyLifecycleCopy.disconnect,
            confirmLabel: String(localized: "Disconnect", bundle: .module), action: action
        )
    }

    static func leave(action: @escaping () async throws -> Void) -> Self {
        .init(title: String(localized: "Leave this family?", bundle: .module), body: FamilyLifecycleCopy.leave, confirmLabel: String(localized: "Leave", bundle: .module), action: action)
    }

    static func scheduleDeletion(action: @escaping () async throws -> Void) -> Self {
        .init(
            title: String(localized: "Schedule this family for deletion?", bundle: .module), body: FamilyLifecycleCopy.scheduleDeletion,
            confirmLabel: String(localized: "Schedule deletion", bundle: .module), action: action
        )
    }
}

/// Maps every way `acceptInvitation(code:)` can fail to the words a caregiver
/// reads next to the field. Free of the view — and of `Model` — so it can be
/// exercised without a running app: a shape that never left the phone, a
/// link for a server this device never confirmed, and the network itself
/// each read differently from a server's own refusal of the invitation.
enum JoinFailureCopy {
    static func message(for error: any Error) -> String {
        if let parseError = error as? InvitationParser.ParseError {
            return describe(parseError)
        }
        if error is URLError {
            return FamilyCopy.joinConnectionProblem
        }
        if (error as? FamilySyncError) == .endpointConfirmationRequired {
            return FamilyCopy.joinWrongServer
        }
        guard case HTTPClientError.status(_, let code) = error else { return FamilyCopy.joinFailed }
        return switch code {
        case "invitation_expired": FamilyCopy.invitationExpired
        case "invitation_consumed": FamilyCopy.invitationConsumed
        case "invitation_revoked": FamilyCopy.invitationRevokedCopy
        default: FamilyCopy.joinFailed
        }
    }

    private static func describe(_ error: InvitationParser.ParseError) -> String {
        switch error {
        case .unsupportedScheme: FamilyCopy.invalidInvitationCode
        case .missingToken, .malformedToken: FamilyCopy.invalidInvitationCode
        case .invalidOrigin(let origin): ServerSelectionView.describe(origin)
        }
    }
}

/// The pending-invitations list, and how `createInvitation(role:)` can fail.
///
/// The one failure that matters is the server's own cap — the plain "that
/// didn't work" banner would leave an owner guessing why an eleventh
/// invitation refuses to mint.
enum PendingInvitationCopy {
    static let header = String(localized: "Pending invitations", bundle: .module)
    static let refreshFailed = String(localized: "Couldn't refresh invitations", bundle: .module)
    static let retry = String(localized: "Retry", bundle: .module)
    static let revoke = String(localized: "Revoke", bundle: .module)
    static let unavailableMessage = String(localized: "This code was created before a reinstall or on another phone. Revoke it and create a new one if you need to share it again.", bundle: .module)
    static let close = String(localized: "Close", bundle: .module)

    static func createFailureMessage(for error: any Error) -> String {
        guard case HTTPClientError.status(_, "invitation_limit") = error else { return FamilyCopy.actionFailed }
        return FamilyCopy.invitationLimitReached
    }

    static func title(for role: FamilyRole) -> String {
        role == .viewer ? String(localized: "View-only invitation", bundle: .module) : String(localized: "Caregiver invitation", bundle: .module)
    }

    static func systemImage(for role: FamilyRole) -> String {
        role == .viewer ? "eye" : "person.badge.plus"
    }

    /// "Expires today", "Expires tomorrow", "Expires in 6 days" — counted in
    /// calendar days, because "in 1 day" at 23:00 would really mean tonight.
    static func expiry(_ expiresAt: Date, now: Date = .now, calendar: Calendar = .autoupdatingCurrent) -> String {
        let days = calendar.dateComponents(
            [.day], from: calendar.startOfDay(for: now), to: calendar.startOfDay(for: expiresAt)
        ).day ?? 0
        switch days {
        case ..<1: return String(localized: "Expires today", bundle: .module)
        case 1: return String(localized: "Expires tomorrow", bundle: .module)
        default:
            // The plural form follows the language the string resolved to,
            // as in Insights and History.
            let language = Locale(identifier: Bundle.module.preferredLocalizations.first ?? "en")
            return String(format: NSLocalizedString("Expires in %d days", bundle: .module, comment: ""), locale: language, days)
        }
    }
}

enum FamilyCopy {
    static let title = String(localized: "Caregivers", bundle: .module)
    static let serverHeader = String(localized: "Synchronization server", bundle: .module)
    static let chooseServer = String(localized: "Choose server", bundle: .module)
    static let serverFooter = String(localized: "The server address and identity are shown for confirmation before Sign in with Apple.", bundle: .module)
    static let signIn = String(localized: "Sign in with Apple", bundle: .module)
    static let signInAgain = String(localized: "Sign in with Apple again", bundle: .module)
    static let signedOutHeader = String(localized: "Sync stopped", bundle: .module)
    static let localOnlyFooter = String(localized: "Baby Tracker works fully without an account. Sign in only if you want to share tracking with another caregiver.", bundle: .module)
    static let noFamilyHeader = String(localized: "Start or join a family", bundle: .module)
    static let noFamilyFooter = String(localized: "A family shares one set of babies and records between caregivers.", bundle: .module)
    static let familyNameField = String(localized: "Family name", bundle: .module)
    static let createFamily = String(localized: "Create family", bundle: .module)
    static let chooseName = String(localized: "Choose the name caregivers see", bundle: .module)
    static let nameLabel = String(localized: "Your name", bundle: .module)
    static let scanQRCode = String(localized: "Scan QR code", bundle: .module)
    static let orPasteCode = String(localized: "Or paste a code", bundle: .module)
    static let invitationCodeField = String(localized: "Invitation code", bundle: .module)
    static let pasteCode = String(localized: "Paste", bundle: .module)
    static let joinFamily = String(localized: "Join", bundle: .module)
    static let invalidInvitationCode = String(localized: "That code isn't valid. Check it and try again.", bundle: .module)
    static let joinFailed = String(localized: "That invitation could not be used. Ask for a new one.", bundle: .module)
    static let joinConnectionProblem = String(localized: "Couldn't reach your family's server. Check you're connected, then try again.", bundle: .module)
    static let joinWrongServer = String(localized: "This invitation is for a different server.", bundle: .module)
    static let invitationExpired = String(localized: "That invitation has expired. Ask for a new one.", bundle: .module)
    static let invitationConsumed = String(localized: "That invitation was already used. Ask for a new one.", bundle: .module)
    static let invitationRevokedCopy = String(localized: "That invitation was revoked. Ask for a new one.", bundle: .module)
    static let membersHeader = String(localized: "Members", bundle: .module)
    static let ownerFooter = String(localized: "You own this family. Tap a member to change their role or remove them.", bundle: .module)
    static let caregiverFooter = String(localized: "The family owner can add and remove caregivers.", bundle: .module)
    static let readOnlyHeader = String(localized: "View only", bundle: .module)
    static let readOnlyExplanation = String(localized: "You can see everything this family logs, but you cannot add or change entries. Ask the family owner if you need to start logging.", bundle: .module)
    static let inviteSomeone = String(localized: "Invite someone", bundle: .module)
    static let inviteFooter = String(localized: "Each invitation works once and expires after 7 days. Revoking it disables the code.", bundle: .module)
    static let leavingHeader = String(localized: "Leaving", bundle: .module)
    static let disconnect = String(localized: "Disconnect this device", bundle: .module)
    static let leaveFamily = String(localized: "Leave family", bundle: .module)
    static let scheduleDeletion = String(localized: "Schedule family deletion", bundle: .module)
    static let recoveryHeader = String(localized: "Family deletion scheduled", bundle: .module)
    static let recoveryExplanation = String(localized: "This family is scheduled for deletion. You can recover it, or give up the recovery claim and let the deletion complete.", bundle: .module)
    static let recoverFamily = String(localized: "Recover family", bundle: .module)
    static let forfeitRecovery = String(localized: "Give up recovery", bundle: .module)
    static let actionFailed = String(localized: "That didn't work. Nothing was changed.", bundle: .module)
    static let invitationLimitReached = String(localized: "You have 10 open invitations. Revoke one to create another.", bundle: .module)

    /// A member's role as their row shows it, with "You · " on your own.
    static func roleLabel(_ role: FamilyRole, isMe: Bool) -> String {
        isMe ? String(format: String(localized: "You · %@", bundle: .module), role.displayName) : role.displayName
    }
}
