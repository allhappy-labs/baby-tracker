import BabyStore
import BabySync
import BabyUI
import SwiftUI
#if canImport(UIKit)
import UIKit
#endif

/// Inviting someone, in two steps: choose what they may do, then hand them
/// the invitation — as a QR code for a phone in the same room, or as a code
/// and a link for one that is not.
///
/// Opened on the second step for an invitation this phone already holds the
/// code for, so a pending invitation can be shown again rather than minted
/// again.
struct InviteSheet<Model: FamilySyncSettingsModel>: View {
    private let model: Model
    @State private var invitation: FamilyInvitation?
    @State private var role: FamilyRole = .caregiver
    @State private var problem: String?
    @State private var isWorking = false
    @State private var copied = false
    @Environment(\.dismiss) private var dismiss

    init(model: Model, showing invitation: FamilyInvitation? = nil) {
        self.model = model
        _invitation = State(initialValue: invitation)
    }

    var body: some View {
        NavigationStack {
            Group {
                if let invitation {
                    resultStep(invitation)
                } else {
                    roleStep
                }
            }
            .navigationTitle(invitation.map { PendingInvitationCopy.title(for: $0.role) } ?? InviteSheetCopy.title)
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                if invitation == nil {
                    ToolbarItem(placement: .cancellationAction) {
                        Button(InviteSheetCopy.cancel) { dismiss() }
                            .accessibilityIdentifier("invite.cancel")
                    }
                } else {
                    ToolbarItem(placement: .confirmationAction) {
                        Button(InviteSheetCopy.done) { dismiss() }
                            .accessibilityIdentifier("invite.done")
                    }
                }
            }
            .disabled(isWorking)
        }
    }

    // MARK: Step 1 — the role

    private var roleStep: some View {
        Form {
            Section {
                Picker(InviteSheetCopy.roleLabel, selection: $role) {
                    Text(FamilyRole.caregiver.displayName).tag(FamilyRole.caregiver)
                    Text(FamilyRole.viewer.displayName).tag(FamilyRole.viewer)
                }
                .pickerStyle(.segmented)
                .frame(minHeight: 44)
                .accessibilityIdentifier("invite.role")
            } header: {
                Text(InviteSheetCopy.roleLabel)
            } footer: {
                RoleExplanations()
            }

            Section {
                Button {
                    create()
                } label: {
                    Text(InviteSheetCopy.create).frame(maxWidth: .infinity, minHeight: 44)
                }
                .buttonStyle(.borderedProminent)
                .accessibilityIdentifier("invite.create")

                if let problem {
                    Text(problem)
                        .foregroundStyle(Palette.destructive)
                        .accessibilityIdentifier("invite.problem")
                }
            } footer: {
                Text(FamilyCopy.inviteFooter)
            }
        }
    }

    // MARK: Step 2 — the invitation

    private func resultStep(_ invitation: FamilyInvitation) -> some View {
        let code = Self.code(of: invitation)
        return ScrollView {
            VStack(spacing: 20) {
                if let payload = try? InvitationQRCode.payload(for: invitation) {
                    InvitationQRImage(payload: payload)
                }

                Text(InviteSheetCopy.scanHint)
                    .font(.footnote)
                    .foregroundStyle(Palette.secondaryText)
                    .multilineTextAlignment(.center)

                Text(code)
                    .font(.body.monospaced())
                    .multilineTextAlignment(.center)
                    .textSelection(.enabled)
                    .accessibilityLabel(InviteSheetCopy.codeLabel)
                    .accessibilityValue(code)
                    .accessibilityIdentifier("invite.code")

                VStack(spacing: 12) {
                    Button {
                        #if canImport(UIKit)
                        UIPasteboard.general.string = code
                        #endif
                        copied = true
                    } label: {
                        Text(copied ? InviteSheetCopy.copied : InviteSheetCopy.copy)
                            .frame(maxWidth: .infinity, minHeight: 44)
                    }
                    .buttonStyle(.bordered)
                    .accessibilityIdentifier("invite.copy")

                    ShareLink(item: InviteSheetCopy.shareMessage(link: invitation.shareURL, code: code)) {
                        Label(InviteSheetCopy.share, systemImage: "square.and.arrow.up")
                            .frame(maxWidth: .infinity, minHeight: 44)
                    }
                    .buttonStyle(.borderedProminent)
                    .accessibilityIdentifier("invite.share")
                }

                Text(InviteSheetCopy.expires(invitation.expiresAt))
                    .font(.footnote)
                    .foregroundStyle(Palette.secondaryText)
                    .accessibilityIdentifier("invite.expires")

                if let problem {
                    Text(problem)
                        .foregroundStyle(Palette.destructive)
                        .accessibilityIdentifier("invite.problem")
                }

                Button(role: .destructive) {
                    revoke(invitation)
                } label: {
                    Text(InviteSheetCopy.revoke).frame(maxWidth: .infinity, minHeight: 44)
                }
                .buttonStyle(.bordered)
                .accessibilityIdentifier("invite.revoke")
            }
            .padding()
        }
    }

    // MARK: Plumbing

    /// The bare code: the token the share link carries. A link that somehow
    /// fails to re-parse — it never should, this app's server minted it —
    /// still shows something rather than an empty sheet.
    static func code(of invitation: FamilyInvitation) -> String {
        (try? InvitationParser.parse(invitation.shareURL.absoluteString).token) ?? invitation.shareURL.absoluteString
    }

    private func create() {
        isWorking = true
        problem = nil
        Task {
            defer { isWorking = false }
            do {
                invitation = try await model.createInvitation(role: role)
            } catch {
                problem = PendingInvitationCopy.createFailureMessage(for: error)
            }
        }
    }

    private func revoke(_ invitation: FamilyInvitation) {
        isWorking = true
        problem = nil
        Task {
            defer { isWorking = false }
            do {
                try await model.revokeInvitation(invitation.id)
                dismiss()
            } catch {
                problem = FamilyCopy.actionFailed
            }
        }
    }
}

/// The QR code, rendered once per payload rather than on every redraw of
/// the sheet — CoreImage work that a Copy tap has no reason to repeat.
private struct InvitationQRImage: View {
    let payload: String
    @State private var rendered: (payload: String, image: Image?)?

    var body: some View {
        Group {
            if let image = rendered?.image {
                image
                    .resizable()
                    .interpolation(.none)
            } else {
                Color.white
            }
        }
        .aspectRatio(1, contentMode: .fit)
        // The quiet zone a camera needs, and a light ground on this app's
        // dark one.
        .padding(14)
        .background(Color.white, in: RoundedRectangle(cornerRadius: 16))
        .frame(maxWidth: 280)
        .accessibilityElement()
        .accessibilityLabel(InviteSheetCopy.qrLabel)
        .accessibilityAddTraits(.isImage)
        .accessibilityIdentifier("invite.qr")
        .task(id: payload) {
            guard rendered?.payload != payload else { return }
            rendered = (payload, InvitationQRCode.image(for: payload))
        }
    }
}

/// What each role may do, one line each, wherever a role is chosen.
struct RoleExplanations: View {
    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(InviteSheetCopy.caregiverExplanation)
            Text(InviteSheetCopy.viewerExplanation)
        }
    }
}

enum InviteSheetCopy {
    static let title = String(localized: "Invite someone", bundle: .module)
    static let roleLabel = String(localized: "Role", bundle: .module)
    static let caregiverExplanation = String(localized: "Caregiver: can add and change records.", bundle: .module)
    static let viewerExplanation = String(localized: "View only: can see records, but not add or change them.", bundle: .module)
    static let create = String(localized: "Create invitation", bundle: .module)
    static let cancel = String(localized: "Cancel", bundle: .module)
    static let done = String(localized: "Done", bundle: .module)
    static let qrLabel = String(localized: "Invitation QR code", bundle: .module)
    static let scanHint = String(localized: "Scan with the other phone's camera, or share the code.", bundle: .module)
    static let codeLabel = String(localized: "Invitation code", bundle: .module)
    static let copy = String(localized: "Copy code", bundle: .module)
    static let copied = String(localized: "Copied", bundle: .module)
    static let share = String(localized: "Share", bundle: .module)
    static let revoke = String(localized: "Revoke invitation", bundle: .module)

    static func expires(_ date: Date) -> String {
        String(format: String(localized: "Expires %@", bundle: .module), date.formatted(date: .abbreviated, time: .shortened))
    }

    static func shareMessage(link: URL, code: String) -> String {
        String(format: String(localized: "Join our family in Baby Tracker: %@. Or open Baby Tracker → Settings → Caregivers → Join and paste this code: %@", bundle: .module), link.absoluteString, code)
    }
}

/// Why the invite sheet is up: to mint a new invitation, or to show one this
/// phone already holds the code for.
enum InviteSheetRequest: Identifiable {
    case newInvitation
    case existing(FamilyInvitation)

    var id: String {
        switch self {
        case .newInvitation: "new"
        case .existing(let invitation): invitation.id.uuidString
        }
    }

    var invitation: FamilyInvitation? {
        if case .existing(let invitation) = self { return invitation }
        return nil
    }
}
