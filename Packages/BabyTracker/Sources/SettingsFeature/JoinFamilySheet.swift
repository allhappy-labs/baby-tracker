import BabySync
import BabyUI
import SwiftUI

/// The Caregivers screen, reachable before there is a baby — and wherever an
/// invitation link opens the app.
///
/// The same screen Settings opens — server confirmation, Sign in with Apple,
/// display name, an invitation code to paste — not a second flow that could
/// drift from it. Presented from the first-launch screen, where a joining
/// caregiver would otherwise have to invent a baby to reach Settings, and by
/// a `babytracker://` link, prefilled with its invitation.
public struct JoinFamilySheet<Model: FamilySyncSettingsModel>: View {
    private let model: Model
    private let prefilled: InvitationIntent?
    private let onClose: () -> Void

    /// - Parameter prefilled: an invitation from a link. Its server fills in
    ///   the server step when none is confirmed yet, and it ends at
    ///   "Join {family}?" once the phone is signed in.
    public init(model: Model, prefilled: InvitationIntent? = nil, onClose: @escaping () -> Void) {
        self.model = model
        self.prefilled = prefilled
        self.onClose = onClose
    }

    public var body: some View {
        NavigationStack {
            FamilySettingsView(model: model, invitation: prefilled)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) {
                        Button(JoinFamilyCopy.close, action: onClose)
                            .accessibilityIdentifier("joinFamily.close")
                    }
                }
        }
    }
}

/// How an invitation link gets past whatever the Caregivers screen has on
/// screen.
///
/// Each Caregivers screen registers here the moment it presents something —
/// scanner, invite sheet, a confirmation — and releases only from that
/// presentation's `onDismiss`, once it has actually gone. So `isBusy` is true
/// for exactly as long as a new presentation from `RootView` would be
/// dropped. `RootView` calls `linkArrived()`, which makes every screen
/// dismiss what it has up, and holds the link's alert or Join sheet until
/// `isBusy` turns false — a completion signal, not a guess at how long a
/// dismissal takes.
@MainActor
@Observable
public final class InvitationLinkHandOff {
    /// Bumped per link; the Caregivers screens dismiss their presentations.
    public private(set) var arrivals = 0
    private var presenters: Set<UUID> = []

    public init() {}

    public var isBusy: Bool { !presenters.isEmpty }

    public func linkArrived() { arrivals += 1 }
    func presenting(_ screen: UUID) { presenters.insert(screen) }
    func released(_ screen: UUID) { presenters.remove(screen) }
}

public extension EnvironmentValues {
    @Entry var invitationLinkHandOff: InvitationLinkHandOff?
}

enum JoinFamilyCopy {
    static let close = String(localized: "Close", bundle: .module)
}

/// What `RootView` says when an invitation link arrives on a phone that is
/// already in a family.
public enum InvitationLinkCopy {
    public static let alreadyInFamilyTitle = String(localized: "Already in a family", bundle: .module)
    public static let alreadyInFamily = String(localized: "You're already in a family. Leave it first to join another.", bundle: .module)
    public static let ok = String(localized: "OK", bundle: .module)
}
