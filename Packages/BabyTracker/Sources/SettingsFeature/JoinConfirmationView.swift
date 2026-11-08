import BabyCore
import BabySync
import BabyUI
import Observation
import SwiftUI

/// The last step before joining a family by QR code or link: the server's
/// preview names the family, and only an explicit Join redeems the
/// invitation.
///
/// Free of the view so each outcome is tested against the real view model:
/// a family named from the preview, a preview the server refuses, and a link
/// for a server this phone never confirmed.
@MainActor
@Observable
final class JoinConfirmation<Model: FamilySyncSettingsModel> {
    enum State: Equatable {
        case loading
        case ready(familyName: String)
        case joining(familyName: String)
        case failed(String)
        case joined
    }

    private(set) var state: State = .loading
    let intent: InvitationIntent
    private let model: Model

    init(model: Model, intent: InvitationIntent) {
        self.model = model
        self.intent = intent
    }

    /// Asks the server which family this is. A link for any server other
    /// than the confirmed one is refused first: the preview would carry its
    /// token to this phone's server rather than the one that minted it.
    func load() async {
        guard let confirmed = model.confirmedOrigin, confirmed == intent.origin else {
            state = .failed(JoinConfirmationCopy.wrongServer(intent.origin))
            return
        }
        do {
            let preview = try await model.previewInvitation(intent)
            state = .ready(familyName: preview.familyName)
        } catch {
            fail(error)
        }
    }

    /// Redeems the invitation — only from a successful preview.
    func join() async {
        guard case .ready(let familyName) = state else { return }
        state = .joining(familyName: familyName)
        do {
            try await model.acceptInvitation(intent)
            state = .joined
        } catch {
            fail(error)
        }
    }

    private func fail(_ error: any Error) {
        let message = JoinFailureCopy.message(for: error)
        state = .failed(message)
        AppLog.sync.error("join confirmation failed: \(message, privacy: .public) \(String(describing: error), privacy: .private)")
    }
}

/// The invitation "Join {family}?" is asking about.
struct JoinConfirmationRequest: Identifiable {
    let intent: InvitationIntent
    var id: String { intent.origin.canonicalString + "#" + intent.token }
}

extension JoinConfirmationRequest {
    /// Whether an invitation waiting for "Join {family}?" is still wanted
    /// after the mode moves from `old` to `new`. It waits through choosing a
    /// server and signing in; once the phone has been able to act on it and
    /// then moves on — joined, created a family, signed out — it is spent.
    /// A join finishes by leaving `.authenticated`, which takes the sheet
    /// down before it can clear the invitation itself; kept, the same
    /// "Join {family}?" would come back after the next sign-in.
    static func invitationSurvives(from old: FamilySyncMode, to new: FamilySyncMode) -> Bool {
        guard case .authenticated = old else { return true }
        if case .authenticated = new { return true }
        return false
    }
}

/// "Join {family}?" with Join and Cancel, or why this invitation cannot be
/// used.
struct JoinConfirmationView<Model: FamilySyncSettingsModel>: View {
    @State private var confirmation: JoinConfirmation<Model>
    private let onFinish: () -> Void

    init(model: Model, intent: InvitationIntent, onFinish: @escaping () -> Void) {
        _confirmation = State(initialValue: JoinConfirmation(model: model, intent: intent))
        self.onFinish = onFinish
    }

    var body: some View {
        // Presented with `fittedSheet`: as tall as what it says in each
        // state — a spinner, a question with two buttons, or a reason with one.
        content
            .interactiveDismissDisabled(confirmation.state.isJoining)
            .task { await confirmation.load() }
            .onChange(of: confirmation.state) { _, state in
                if state == .joined { onFinish() }
            }
    }

    @ViewBuilder private var content: some View {
        switch confirmation.state {
        case .loading:
            ProgressView(JoinConfirmationCopy.checking)
                .accessibilityIdentifier("joinConfirmation.loading")
                .frame(maxWidth: .infinity)
                .padding(Spacing.xl + Spacing.s)
        case .ready(let familyName), .joining(let familyName):
            ConfirmSheet(
                systemImage: "person.2.fill",
                tint: Palette.sleep,
                title: JoinConfirmationCopy.title(familyName: familyName),
                message: "",
                confirm: SheetAction(
                    JoinConfirmationCopy.join, identifier: "joinConfirmation.join",
                    isEnabled: confirmation.state == .ready(familyName: familyName)
                ) { Task { await confirmation.join() } },
                isDestructive: false,
                cancel: SheetAction(
                    JoinConfirmationCopy.cancel, identifier: "joinConfirmation.cancel",
                    isEnabled: !confirmation.state.isJoining, action: onFinish),
                titleIdentifier: "joinConfirmation.title"
            )
        case .failed(let message):
            VStack(spacing: 0) {
                ScrollView {
                    InlineError(message)
                        .font(.callout)
                        .padding(.horizontal, Spacing.xl)
                        .padding(.vertical, Spacing.xl + Spacing.s)
                        .accessibilityIdentifier("joinConfirmation.problem")
                }
                .scrollBounceBehavior(.basedOnSize)
                SheetFooter(secondary: SheetAction(
                    JoinConfirmationCopy.close, identifier: "joinConfirmation.cancel", action: onFinish))
            }
        case .joined:
            EmptyView()
        }
    }
}

private extension JoinConfirmation.State {
    var isJoining: Bool {
        if case .joining = self { return true }
        return false
    }
}

enum JoinConfirmationCopy {
    static let checking = String(localized: "Checking the invitation…", bundle: .module)
    static let join = String(localized: "Join", bundle: .module)
    static let cancel = String(localized: "Cancel", bundle: .module)
    static let close = String(localized: "Close", bundle: .module)

    static func title(familyName: String) -> String { String(format: String(localized: "Join %@?", bundle: .module), familyName) }

    /// `FamilyCopy.joinWrongServer`, with the server the invitation is for.
    static func wrongServer(_ origin: ServerOrigin) -> String {
        String(format: String(localized: "This invitation is for a different server. It's for %@.", bundle: .module), origin.canonicalString)
    }
}
