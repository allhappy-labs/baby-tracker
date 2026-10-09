import BabySync
import Foundation

/// What to do with a URL the system opened the app with.
public enum InvitationRoute: Equatable, Sendable {
    /// Not an invitation link: nothing to do.
    case ignore
    /// An invitation, but this phone is already in a family. Only an alert —
    /// joining another family means leaving this one first.
    case alreadyInFamily
    /// Open the Join sheet with this invitation. Nothing is redeemed until
    /// the user confirms it there.
    case join(InvitationIntent)
}

/// Decides where a `babytracker://` link goes. Pure, so the decision is
/// tested without a scene; `RootView` only wires it to `.onOpenURL`.
public enum InvitationRouting {
    /// Routes on the phone's sync mode: a family session in recovery is
    /// still a family this phone belongs to, so it only gets the alert.
    public static func route(url: URL, mode: FamilySyncMode) -> InvitationRoute {
        route(url: url, hasFamily: mode.belongsToAFamily)
    }

    public static func route(url: URL, hasFamily: Bool) -> InvitationRoute {
        guard let intent = try? InvitationParser.parse(url.absoluteString) else { return .ignore }
        return hasFamily ? .alreadyInFamily : .join(intent)
    }
}

public extension FamilySyncMode {
    /// Whether this phone holds a family session: in use (`.shared`) or
    /// scheduled for deletion and still recoverable (`.recovery`).
    ///
    /// Not `.signedOut`: that mode keeps only an account key. It is reached
    /// from `.authenticated` as well as from `.shared` (Apple revoked the
    /// credential), the family session record is cleared and the shared realm
    /// purged on the way in, so the phone cannot tell whether the account
    /// still has a family until it signs in again.
    var belongsToAFamily: Bool {
        switch self {
        case .shared, .recovery: true
        case .localOnly, .endpointConfirmed, .authenticated, .signedOut: false
        }
    }
}

/// What the in-app scanner makes of the QR payloads it has just recognised.
enum InvitationScanOutcome: Equatable, Sendable {
    /// No QR payload at all — nothing to say yet.
    case nothing
    /// Only QR codes that are not invitations: say so, keep scanning.
    case notAnInvitation
    /// The first payload that is an invitation. Closes the scanner.
    case invitation(InvitationIntent)
}

/// The scanner's payload handling, kept apart from `DataScannerViewController`
/// so it is tested without a camera.
enum InvitationScanning {
    /// Takes the first payload `InvitationParser` accepts, reading each the
    /// way the paste field does: a link carries its own server, a bare code
    /// is paired with the one this phone confirmed.
    static func outcome(for payloads: [String], confirmedOrigin: ServerOrigin) -> InvitationScanOutcome {
        guard !payloads.isEmpty else { return .nothing }
        for payload in payloads {
            if let intent = try? InvitationParser.intent(fromCodeOrLink: payload, confirmedOrigin: confirmedOrigin) {
                return .invitation(intent)
            }
        }
        return .notAnInvitation
    }
}
