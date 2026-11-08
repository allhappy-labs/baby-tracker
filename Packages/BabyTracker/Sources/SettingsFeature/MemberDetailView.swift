import BabyCore
import BabyStore
import BabySync
import BabyUI
import SwiftUI

/// One member, as the owner manages them: their role, handing them the
/// family, or removing them.
///
/// A page rather than swipe actions on the row: three actions of very
/// different weight were hidden behind the same gesture, with nothing on
/// screen saying any of them existed. Every action here asks first, with the
/// copy for that exact action and member, and returns to the list once done.
struct MemberDetailView<Model: FamilySyncSettingsModel>: View {
    private let model: Model
    private let initialMember: FamilyMember
    @State private var pendingConfirmation: FamilyLifecycleConfirmation?
    @State private var isWorking = false
    @Environment(\.dismiss) private var dismiss
    @Environment(\.dynamicTypeSize) private var typeSize
    @Environment(\.errorReporter) private var errorReporter
    /// An invitation link arrived: "Remove X?" and its kin give way to the
    /// link's Join sheet or alert, and say when they have gone.
    @Environment(\.invitationLinkHandOff) private var linkHandOff
    @State private var handOffID = UUID()

    init(model: Model, member: FamilyMember) {
        self.model = model
        initialMember = member
    }

    /// The member as the model knows them now, so a role changed elsewhere is
    /// not shown stale; the member this page opened for if they have gone.
    private var member: FamilyMember {
        model.members.first { $0.id == initialMember.id } ?? initialMember
    }

    var body: some View {
        SettingsPage {
            CardSection {
                // The name under the avatar at the accessibility sizes:
                // beside it, a name was broken mid-word.
                let layout = typeSize.isAccessibilitySize
                    ? AnyLayout(VStackLayout(alignment: .leading, spacing: Spacing.s))
                    : AnyLayout(HStackLayout(spacing: Spacing.l))
                layout {
                    MemberAvatar(member: member, diameter: 56)
                    VStack(alignment: .leading, spacing: 2) {
                        Text(member.displayName)
                            .font(.title3.weight(.semibold))
                            .fixedSize(horizontal: false, vertical: true)
                        Text(member.familyRole.displayName)
                            .font(.subheadline)
                            .foregroundStyle(Palette.secondaryText)
                    }
                    Spacer(minLength: 0)
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.vertical, Spacing.xs)
                .accessibilityElement(children: .combine)
            }

            if member.familyRole != .owner {
                CardSection(MemberDetailCopy.roleHeader) {
                    Picker(MemberDetailCopy.roleHeader, selection: roleSelection) {
                        Text(FamilyRole.caregiver.displayName).tag(FamilyRole.caregiver)
                        Text(FamilyRole.viewer.displayName).tag(FamilyRole.viewer)
                    }
                    .pickerStyle(.segmented)
                    .frame(minHeight: 44)
                    .accessibilityIdentifier("member.role")
                } footer: {
                    RoleExplanations()
                }
            }

            CardSection {
                if model.canMakeOwner(member) {
                    ActionRow(MemberDetailCopy.makeOwner, identifier: "member.makeOwner") { ask(.makeOwner) }
                }
                DestructiveButton(MemberDetailCopy.remove, identifier: "member.remove") { ask(.remove) }
            }
        }
        .navigationTitle(member.displayName)
        #if os(iOS)
        .navigationBarTitleDisplayMode(.inline)
        #endif
        .disabled(isWorking)
        .fittedSheet(item: $pendingConfirmation, onDismiss: { linkHandOff?.released(handOffID) }) { confirmation in
            FamilyLifecycleConfirmationView(confirmation: confirmation) {
                pendingConfirmation = nil
                run(confirmation.action)
            } onCancel: {
                pendingConfirmation = nil
            }
        }
        .onChange(of: pendingConfirmation != nil) { _, presenting in
            if presenting { linkHandOff?.presenting(handOffID) }
        }
        .onDisappear { linkHandOff?.released(handOffID) }
        .onChange(of: linkHandOff?.arrivals) { _, _ in pendingConfirmation = nil }
    }

    /// Shows the member's current role and never changes it directly: a new
    /// choice only asks, and the control snaps back if the owner cancels.
    private var roleSelection: Binding<FamilyRole> {
        Binding {
            member.familyRole
        } set: { newRole in
            guard newRole != member.familyRole else { return }
            ask(.changeRole(to: newRole))
        }
    }

    private func ask(_ action: MemberAction) {
        let member = member
        let copy = model.confirmation(for: action, member: member)
        switch action {
        case .remove:
            pendingConfirmation = .init(copy, isDestructive: true) { try await model.removeMember(member.id) }
        case .makeOwner:
            pendingConfirmation = .init(copy, isDestructive: true) { try await model.transferOwnership(to: member.id) }
        case .changeRole(let newRole):
            pendingConfirmation = .init(copy, isDestructive: false) {
                try await model.changeRole(of: member.id, to: newRole)
            }
        }
    }

    /// Back to the list on success — which the model has already reloaded —
    /// and the app's standard banner on failure, staying on this page.
    private func run(_ work: @escaping () async throws -> Void) {
        isWorking = true
        Task {
            defer { isWorking = false }
            do {
                try await work()
                errorReporter.clear()
                dismiss()
            } catch {
                errorReporter.report(FamilyCopy.actionFailed, error: error, category: AppLog.store)
            }
        }
    }
}

enum MemberDetailCopy {
    static let roleHeader = String(localized: "Role", bundle: .module)
    static let makeOwner = String(localized: "Make owner", bundle: .module)
    static let remove = String(localized: "Remove from family", bundle: .module)
}
