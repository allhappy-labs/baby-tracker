import BabyStore
import BabySync
import Foundation
import Observation

/// Everything the family screens can ask for.
///
/// A protocol rather than the concrete view model so the screens can be
/// rendered — and asserted on — against a scripted model, without a server, a
/// Keychain entitlement, or a real Sign in with Apple sheet.
@MainActor
public protocol FamilySyncSettingsModel: AnyObject, Observable {
    var mode: FamilySyncMode { get }
    var profile: CaregiverProfile { get }
    var members: [FamilyMember] { get }
    /// The copy shown above a destructive confirmation. Read by tests, because
    /// what this says is the whole protection for an irreversible action.
    var confirmationCopy: String { get }
    /// This device's role in the family.
    var role: FamilyRole { get }
    /// Creating or joining needs a name other caregivers will see.
    var displayNameConfirmed: Bool { get }
    /// What sync is doing, in a parent's words. Nil when there is nothing to
    /// say — no family, or an engine that has not run yet.
    var syncStatusLine: FamilySyncStatusLine? { get }
    /// Current engine state for app composition; Home maps it without linking SettingsFeature.
    var syncEngineStatus: FamilySyncEngine.Status? { get }
    var syncOperationalStatus: SyncOperationalStatus { get }
    /// Unacknowledged mutations in this phone's current family realm.
    var pendingDeliveryCount: Int? { get }
    /// Every invitation this family has minted that has not been redeemed or
    /// revoked yet. Reloaded on appear, after every invitation action, and by
    /// pull-to-refresh — never mutated directly by the screen.
    var pendingInvitations: [PendingInvitation] { get }
    /// The last `reloadInvitations()` could not reach the server. The list
    /// above still holds whatever it last knew, so a screen can show both at
    /// once: the last-known invitations and a retry row.
    var invitationsRefreshFailed: Bool { get }
    /// `members`, ordered the way the screen shows them: the owner first,
    /// then everyone else by display name, case-insensitively.
    var sortedMembers: [FamilyMember] { get }

    /// The server this phone confirmed, if any. An invitation for any other
    /// origin is refused before its token is sent anywhere.
    var confirmedOrigin: ServerOrigin? { get }
    /// Last capability probe for this confirmed server. Nil while checking.
    var exportAvailable: Bool? { get }
    func refreshExportAvailability() async
    /// Builds the export transport from this account's confirmed endpoint and current session.
    func makeExportClient() -> (any ExportServing)?

    func confirmEndpoint(_ endpoint: ConfirmedEndpoint) async throws
    func signInWithApple() async throws
    func refreshAppleCredentialState() async throws -> AppleCredentialState
    func confirmDisplayName(_ name: String) async throws
    func updateDisplayName(_ name: String) async throws
    func createFamily(name: String) async throws
    func disconnectCurrentFamily() async throws
    func acceptInvitation(_ invitation: InvitationIntent) async throws
    func acceptInvitation(code: String) async throws
    func createInvitation() async throws -> FamilyInvitation
    func inviteViewer() async throws -> FamilyInvitation
    /// Mints an invitation of the given role, saves its code so this phone
    /// can show it again, and reloads the pending list.
    func createInvitation(role: FamilyRole) async throws -> FamilyInvitation
    func changeRole(of memberID: UUID, to role: FamilyRole) async throws
    func invitationShareURL(_ invitation: FamilyInvitation) throws -> URL
    /// Revokes a still-pending invitation, removes its saved code, and
    /// reloads the list. An invitation the server already considers gone —
    /// redeemed, revoked, or expired — is treated as already done rather than
    /// surfaced as a failure.
    func revokeInvitation(_ invitationID: UUID) async throws
    /// This phone's own saved link for a pending invitation, if it minted it
    /// and still holds the code. Nil for an invitation created on another
    /// phone, or before a reinstall.
    func shareURL(for invitationID: UUID) -> URL?
    /// Refreshes `pendingInvitations` from the server and prunes the saved
    /// codes to match. Never throws: a failure sets `invitationsRefreshFailed`
    /// and leaves the last-known list in place.
    func reloadInvitations() async
    /// Asks the server again for the members and this device's role — what
    /// pull-to-refresh does, since both change on other phones.
    func reloadMembers() async throws
    /// Resolves a scanned or pasted invitation to the family it invites into,
    /// without redeeming it.
    func previewInvitation(_ intent: InvitationIntent) async throws -> InvitationPreview
    func leaveFamily() async throws
    func removeMember(_ memberID: UUID) async throws
    /// Whether `member` may be offered "Make owner": only the owner may make
    /// this offer, and only to a caregiver — the server refuses transfer to
    /// a viewer.
    func canMakeOwner(_ member: FamilyMember) -> Bool
    func transferOwnership(to memberID: UUID) async throws
    /// The copy for a member-detail confirmation, computed fresh for this
    /// action and this member rather than read off a mutable property — the
    /// fix for the sheet showing stale or empty text.
    func confirmation(for action: MemberAction, member: FamilyMember) -> FamilyConfirmationCopy
    func scheduleFamilyDeletion() async throws
    func recoverFamily() async throws
    func forfeitFamilyRecovery() async throws
    func deleteAccount() async throws
    /// Whether `member` is this device's own caregiver.
    func isMe(_ member: FamilyMember) -> Bool
}

public extension FamilySyncSettingsModel {
    var syncEngineStatus: FamilySyncEngine.Status? { nil }
    var syncOperationalStatus: SyncOperationalStatus { .inactive }
    var pendingDeliveryCount: Int? { nil }
    var exportAvailable: Bool? { false }
    func refreshExportAvailability() async {}
    func makeExportClient() -> (any ExportServing)? { nil }
    var isOwner: Bool { role.canManageMembers }
    /// A read-only caregiver: sees everything, changes nothing.
    var isReadOnly: Bool { !role.canWrite }

    var isSignedOut: Bool {
        if case .signedOut = mode { return true }
        return false
    }

    var hasFamily: Bool {
        switch mode {
        case .shared: true
        case .localOnly, .endpointConfirmed, .authenticated, .recovery, .signedOut: false
        }
    }

    var isRecoveryOnly: Bool {
        if case .recovery = mode { return true }
        return false
    }
}

/// One line under the family's name saying whether the other caregiver will
/// see what was just logged.
public struct FamilySyncStatusLine: Equatable, Sendable {
    public let text: String
    /// Drawn as something to act on. Offline and a transient retry are not:
    /// the entries are safe and sync resumes by itself.
    public let isProblem: Bool
    /// The session can only be restored with a fresh Apple proof, so the line
    /// carries the button that gets one.
    public let offersSignIn: Bool
}

public enum FamilySyncStatusCopy {
    public static let syncing = String(localized: "Syncing…", bundle: .module)
    public static let offline = String(localized: "Offline — entries are saved and will sync", bundle: .module)
    public static let signInAgain = String(localized: "Sign in again to keep syncing", bundle: .module)
    public static let signInButton = String(localized: "Sign in with Apple", bundle: .module)

    public static func synced(at date: Date, locale: Locale = .autoupdatingCurrent, timeZone: TimeZone = .autoupdatingCurrent) -> String {
        var style = Date.FormatStyle(date: .omitted, time: .shortened)
        style.locale = locale
        style.timeZone = timeZone
        return String(
            format: String(localized: "Synced %@", bundle: .module),
            locale: locale,
            date.formatted(style)
        )
    }

    public static func line(
        for status: FamilySyncEngine.Status,
        locale: Locale = .autoupdatingCurrent,
        timeZone: TimeZone = .autoupdatingCurrent
    ) -> FamilySyncStatusLine? {
        switch status {
        case .idle:
            return nil
        case .syncing:
            return FamilySyncStatusLine(text: syncing, isProblem: false, offersSignIn: false)
        case .upToDate(let date):
            return FamilySyncStatusLine(
                text: synced(at: date, locale: locale, timeZone: timeZone), isProblem: false, offersSignIn: false
            )
        case .offline:
            return FamilySyncStatusLine(text: offline, isProblem: false, offersSignIn: false)
        case .needsSignIn:
            return FamilySyncStatusLine(text: signInAgain, isProblem: true, offersSignIn: true)
        case .failed(let message) where message == SyncIssueCopy.retrying:
            // The engine's backoff state: a server answered with something
            // transient and the next poll retries. Nothing for a parent to do,
            // so it must not read as an error.
            return FamilySyncStatusLine(text: syncing, isProblem: false, offersSignIn: false)
        case .failed(let message):
            return FamilySyncStatusLine(text: message, isProblem: true, offersSignIn: false)
        }
    }
}

/// One member action a member-detail page can ask to confirm.
///
/// Kept apart from `FamilyLifecycleCommand`, which is what actually gets
/// sent: this is only ever the *reason* to show a confirmation, decided
/// before any request goes out.
public enum MemberAction: Equatable, Sendable {
    case remove
    case makeOwner
    case changeRole(to: FamilyRole)
}

/// The copy for one member-detail confirmation: what the sheet says, and
/// what its destructive button reads.
///
/// A value computed fresh by `confirmation(for:member:)`, not a property the
/// model mutates and the screen reads back later — the earlier version of
/// this screen set a single `confirmationCopy` string for some actions and
/// never for "Remove" or "Make owner", so their sheets showed whatever the
/// last action had left behind, or nothing.
public struct FamilyConfirmationCopy: Equatable, Sendable {
    public let title: String
    public let body: String
    public let confirmLabel: String

    public init(title: String, body: String, confirmLabel: String) {
        self.title = title
        self.body = body
        self.confirmLabel = confirmLabel
    }

    public static func remove(_ member: FamilyMember) -> Self {
        .init(
            title: String(format: String(localized: "Remove %@?", bundle: .module), member.displayName),
            body: String(
                format: String(localized: "This removes %@ from the family. Their devices lose access to this family's shared records. This does not restore a local copy on any device.", bundle: .module),
                member.displayName
            ),
            confirmLabel: String(localized: "Remove", bundle: .module)
        )
    }

    public static func makeOwner(_ member: FamilyMember) -> Self {
        .init(
            title: String(format: String(localized: "Make %@ the owner?", bundle: .module), member.displayName),
            body: String(
                format: String(localized: "%@ becomes the owner and can manage caregivers and invitations. You become a caregiver.", bundle: .module),
                member.displayName
            ),
            confirmLabel: String(localized: "Make owner", bundle: .module)
        )
    }

    public static func changeRole(_ member: FamilyMember, to newRole: FamilyRole) -> Self {
        .init(
            title: String(format: String(localized: "Change %@ to %@?", bundle: .module), member.displayName, newRole.displayName),
            body: newRole == .viewer
                ? String(format: String(localized: "%@ will be able to see this family's records, but not add or change them.", bundle: .module), member.displayName)
                : String(format: String(localized: "%@ will be able to add and change this family's records.", bundle: .module), member.displayName),
            confirmLabel: String(localized: "Change role", bundle: .module)
        )
    }
}

/// The copy for every irreversible, family-wide action, in one place.
///
/// These strings are load-bearing, not decoration: disconnecting, leaving and
/// scheduling deletion all destroy the shared copy on this device, and none of
/// them put anything back into the local realm. A user who expects their data
/// to "come back" after disconnecting has been misled by the button, so the
/// sentence that says otherwise is asserted in tests.
public enum FamilyLifecycleCopy {
    public static let disconnect = String(localized: "Disconnecting removes this family's shared records from this device. It does not restore a local copy, and it does not delete anything for other caregivers. Ask the family owner to export the family's data first if you want to keep a copy.", bundle: .module)

    public static let leave = String(localized: "Leaving removes you from this family and removes its shared records from this device. It does not restore a local copy. Everyone else keeps the family and its history.", bundle: .module)

    public static let scheduleDeletion = String(localized: "Scheduling deletion removes the family for every caregiver after the recovery window. Until then you can recover it by signing in with Apple again. This does not restore a local copy on any device.", bundle: .module)

    public static let selfHostRevocation = String(localized: "This Apple account no longer authorizes Baby Tracker, so syncing has stopped on this device. Your family's data on the server is untouched. To restore access, sign in with Apple again. To review or undo the revocation, open Apple account settings on this device and look under apps using Sign in with Apple.", bundle: .module)

    public static let deleteAccount = String(localized: "Deleting your account removes your caregiver profile and this device's access. You must transfer ownership to another caregiver first if you own a family.", bundle: .module)
}
